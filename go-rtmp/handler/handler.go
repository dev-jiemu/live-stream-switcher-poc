package handler

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dev-jiemu/live-stream-switcher-poc/config"
	"github.com/dev-jiemu/live-stream-switcher-poc/store"
	"github.com/yutopp/go-rtmp"
	rtmpmsg "github.com/yutopp/go-rtmp/message"
)

var _ rtmp.Handler = (*Handler)(nil)

type Event int

const (
	EventBecameActive    Event = iota // 내가 active 됨 (연결/재연결/스위칭 성공)
	EventActiveExpired                // 상대방 heartbeat 만료
	EventForwardingFail               // Active 중 Wowza 전송 실패
	EventReconnectFailed              // Active 중 재연결 모두 실패
	EventSwitchFail                   // Standby → Active 스위칭 실패
	EventStreamClose                  // 스트림 종료
)

type State int

const (
	StateStandby State = iota // 대기중
	StateActive               // 포워딩중
)

type Handler struct {
	rtmp.DefaultHandler
	ConnectionId int64
	conn         *rtmp.Conn
	NetConn      net.Conn
	wowzaMu      sync.RWMutex
	wowzaConn    *rtmp.ClientConn
	wowzaStream  *rtmp.Stream
	lastSeen     atomic.Value // time.Time
	cancel       context.CancelFunc
	wowzaApp     string
	streamKey    string
	eventCh      chan Event
	state        State

	// sequence header 캐시
	videoSeqHeader []byte
	audioSeqHeader []byte
	seqMu          sync.RWMutex
	lastVideoTS    atomic.Uint32
	lastAudioTS    atomic.Uint32
}

// OnServe : 초기 설정
func (v *Handler) OnServe(conn *rtmp.Conn) {
	v.conn = conn
	v.lastSeen.Store(time.Now())
}

// watchdog : OBS 등 소스가 비정상 종료됐을 때 감지 (OnClose 가 안 불리는 케이스 대비)
// 고루틴 1 - NetConn.Close() 가 블로킹이라 별도 고루틴 필수
func (v *Handler) watchdog(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			last := v.lastSeen.Load().(time.Time)
			if time.Since(last) > 15*time.Second {
				log.Println("watchdog: 타임아웃 감지, 연결 강제 종료")
				_ = v.NetConn.Close()
				return
			}
		}
	}
}

func (v *Handler) OnConnect(timestamp uint32, cmd *rtmpmsg.NetConnectionConnect) error {
	appName := cmd.Command.App
	if len(appName) == 0 {
		return errors.New("app name is empty")
	}

	log.Printf("stream connect - appName : %v\n", appName)
	v.wowzaApp = appName

	return nil
}

// OnPublish : stream start → Wowza 동기 연결 후 FSM 시작
func (v *Handler) OnPublish(ctx *rtmp.StreamContext, timestamp uint32, cmd *rtmpmsg.NetStreamPublish) error {
	v.lastSeen.Store(time.Now())
	v.streamKey = cmd.PublishingName
	log.Printf("stream start : %s", v.streamKey)

	watchCtx, cancel := context.WithCancel(context.Background())
	v.cancel = cancel
	v.eventCh = make(chan Event, 4)

	go v.watchdog(watchCtx)     // 고루틴 1
	go v.runEventLoop(watchCtx) // 고루틴 2

	serverId, _ := store.Client.GetActive(watchCtx, v.wowzaApp, v.streamKey)
	switch {
	case serverId == "" || serverId == config.EnvConfig.ServerID:
		ok, _ := store.Client.SetActive(watchCtx, v.wowzaApp, v.streamKey, config.EnvConfig.ServerID)
		if ok {
			if err := v.connectWowza(); err != nil {
				store.Client.DeleteActive(watchCtx, v.wowzaApp, v.streamKey)
				return err
			}
			v.eventCh <- EventBecameActive
		}

	default:
		// 다른 서버가 active → StateStandby 로 watching 시작
	}

	return nil
}

// runEventLoop : FSM + heartbeat ticker + watching ticker 를 단일 루프에서 처리
// 고루틴 2 - watching/heartbeat 를 인라인 통합해 별도 고루틴 제거
func (v *Handler) runEventLoop(ctx context.Context) {
	v.state = StateStandby

	heartbeatTicker := time.NewTicker(store.HeartbeatInterval)
	watchTicker := time.NewTicker(store.PollingInterval)
	defer heartbeatTicker.Stop()
	defer watchTicker.Stop()

	heartbeatFailCount := 0
	const maxHeartbeatFail = 3

	// 시작 시 이미 내가 active 면 isActive=true 로 초기화
	isActive := false
	serverId, _ := store.Client.GetActive(ctx, v.wowzaApp, v.streamKey)
	if serverId == config.EnvConfig.ServerID {
		isActive = true
	}

	for {
		select {
		case <-ctx.Done():
			return

		case <-watchTicker.C:
			if isActive {
				continue // active 중엔 watching 불필요
			}
			activeServer, err := store.Client.GetActive(ctx, v.wowzaApp, v.streamKey)
			if err != nil {
				continue
			}
			if activeServer == "" {
				v.eventCh <- EventActiveExpired
				continue
			}
			alive, err := store.Client.IsAlive(ctx, v.wowzaApp, v.streamKey, activeServer)
			if err != nil {
				continue
			}
			if !alive {
				store.Client.DeleteActive(ctx, v.wowzaApp, v.streamKey)
				v.eventCh <- EventActiveExpired
			}

		case <-heartbeatTicker.C:
			if !isActive {
				continue // standby 중엔 heartbeat 불필요
			}
			if err := store.Client.RefreshHeartbeat(ctx, v.wowzaApp, v.streamKey, config.EnvConfig.ServerID); err != nil {
				heartbeatFailCount++
				log.Printf("heartbeat 실패 (%d/%d): %v", heartbeatFailCount, maxHeartbeatFail, err)
				if heartbeatFailCount >= maxHeartbeatFail {
					heartbeatFailCount = 0
					v.eventCh <- EventForwardingFail
				}
			} else {
				heartbeatFailCount = 0
			}

		case event := <-v.eventCh:
			next := v.transition(v.state, event, ctx)
			isActive = (next == StateActive)
			v.state = next
		}
	}
}

// transition : 상태 전이 - 모든 상태 변경은 여기서만
func (v *Handler) transition(state State, event Event, ctx context.Context) State {
	switch state {
	case StateStandby:
		switch event {
		case EventBecameActive:
			log.Println("FSM: Standby → Active")
			return StateActive

		case EventActiveExpired:
			log.Println("FSM: 상대방 만료 감지 → 경합 시도")
			ok, err := store.Client.SetActive(ctx, v.wowzaApp, v.streamKey, config.EnvConfig.ServerID)
			if err != nil || !ok {
				log.Println("FSM: 경합 패배 → watching 계속")
				return StateStandby
			}
			v.startConnectAsync(ctx, true) // 고루틴 3 (일시적)
			return StateStandby

		case EventSwitchFail:
			log.Println("FSM: 스위칭 실패 → Redis 정리 후 watching 계속")
			store.Client.DeleteActive(ctx, v.wowzaApp, v.streamKey)
			store.Client.DelHeartbeat(ctx, v.wowzaApp, v.streamKey, config.EnvConfig.ServerID)
			return StateStandby

		case EventStreamClose:
			return StateStandby
		}

	case StateActive:
		switch event {
		case EventBecameActive:
			log.Println("FSM: Wowza 재연결 성공, Active 유지")
			return StateActive

		case EventForwardingFail:
			log.Println("FSM: 포워딩/heartbeat 실패 → Wowza 재연결 시도")
			v.wowzaMu.Lock()
			if v.wowzaConn != nil {
				v.wowzaConn.Close()
				v.wowzaConn = nil
			}
			v.wowzaStream = nil
			v.wowzaMu.Unlock()
			v.startConnectAsync(ctx, false) // 고루틴 3 (일시적)
			return StateActive

		case EventReconnectFailed:
			log.Printf("FSM: Active → Standby (복구 불가)")
			store.Client.DeleteActive(ctx, v.wowzaApp, v.streamKey)
			store.Client.DelHeartbeat(ctx, v.wowzaApp, v.streamKey, config.EnvConfig.ServerID)
			v.wowzaMu.Lock()
			if v.wowzaConn != nil {
				v.wowzaConn.Close()
				v.wowzaConn = nil
			}
			v.wowzaStream = nil
			v.wowzaMu.Unlock()
			return StateStandby

		case EventStreamClose:
			log.Println("FSM: Active → 정상 종료")
			return StateStandby
		}
	}

	return state
}

// startConnectAsync : Wowza 연결을 백그라운드에서 시도 (retry 포함)
// isSwitching=true  → Standby→Active 스위칭, 실패 시 EventSwitchFail
// isSwitching=false → Active 중 재연결,       실패 시 EventReconnectFailed
// 고루틴 3 - TCP dial + RTMP handshake 블로킹이라 불가피, 연결 완료 후 소멸
func (v *Handler) startConnectAsync(ctx context.Context, isSwitching bool) {
	go func() {
		const maxRetries = 3
		const baseBackoff = 1 * time.Second

		for i := 0; i < maxRetries; i++ {
			if i > 0 {
				backoff := baseBackoff * time.Duration(1<<uint(i-1))
				log.Printf("Wowza 연결 재시도 대기 %v (%d/%d)", backoff, i, maxRetries)
				select {
				case <-ctx.Done():
					return
				case <-time.After(backoff):
				}
			}
			if err := v.connectWowza(); err != nil {
				log.Printf("Wowza 연결 실패 (%d/%d): %v", i+1, maxRetries, err)
				continue
			}
			log.Printf("Wowza 연결 성공 (%d/%d)", i+1, maxRetries)
			select {
			case v.eventCh <- EventBecameActive:
			default:
			}
			return
		}

		log.Printf("Wowza 연결 최종 실패 (isSwitching=%v)", isSwitching)
		failEvent := EventReconnectFailed
		if isSwitching {
			failEvent = EventSwitchFail
		}
		select {
		case v.eventCh <- failEvent:
		default:
		}
	}()
}

// connectWowza : Wowza 연결 및 publish 설정
func (v *Handler) connectWowza() error {
	conn, err := rtmp.Dial("rtmp", config.EnvConfig.Wowza.WowzaHost, &rtmp.ConnConfig{})
	if err != nil {
		log.Printf("Wowza 연결 실패: %v", err)
		return err
	}

	if err := conn.Connect(&rtmpmsg.NetConnectionConnect{
		Command: rtmpmsg.NetConnectionConnectCommand{
			App: v.wowzaApp,
		},
	}); err != nil {
		conn.Close()
		log.Printf("Wowza Connect 실패: %v", err)
		return err
	}

	stream, err := conn.CreateStream(&rtmpmsg.NetConnectionCreateStream{}, 128)
	if err != nil {
		conn.Close()
		log.Printf("Wowza 스트림 생성 실패: %v", err)
		return err
	}

	if err := stream.Publish(&rtmpmsg.NetStreamPublish{
		PublishingName: v.streamKey,
		PublishingType: "live",
	}); err != nil {
		conn.Close()
		log.Printf("Wowza publish 실패: %v", err)
		return err
	}

	v.wowzaMu.Lock()
	v.wowzaConn = conn
	v.wowzaStream = stream
	v.wowzaMu.Unlock()

	// 신규 연결 시 sequence header 재전송
	v.seqMu.RLock()
	videoSH := append([]byte(nil), v.videoSeqHeader...)
	audioSH := append([]byte(nil), v.audioSeqHeader...)
	v.seqMu.RUnlock()

	if len(videoSH) > 0 {
		stream.Write(6, v.lastVideoTS.Load(), &rtmpmsg.VideoMessage{Payload: bytes.NewReader(videoSH)})
	}
	if len(audioSH) > 0 {
		stream.Write(4, v.lastAudioTS.Load(), &rtmpmsg.AudioMessage{Payload: bytes.NewReader(audioSH)})
	}

	log.Printf("✅ Wowza 포워딩 시작: %s/%s", v.wowzaApp, v.streamKey)
	return nil
}

func (v *Handler) OnAudio(timestamp uint32, payload io.Reader) error {
	v.lastSeen.Store(time.Now())

	buf := new(bytes.Buffer)
	if _, err := buf.ReadFrom(payload); err != nil {
		return err
	}
	data := buf.Bytes()

	// AAC sequence header 캐싱: 0xAF 0x00
	if len(data) >= 2 && data[0] == 0xAF && data[1] == 0x00 {
		v.seqMu.Lock()
		v.audioSeqHeader = append([]byte(nil), data...)
		v.seqMu.Unlock()
	}
	v.lastAudioTS.Store(timestamp)

	v.wowzaMu.RLock()
	stream := v.wowzaStream
	v.wowzaMu.RUnlock()

	if stream != nil {
		if err := stream.Write(4, timestamp, &rtmpmsg.AudioMessage{Payload: buf}); err != nil {
			log.Printf("Wowza 오디오 전송 실패: %v", err)
			if v.eventCh != nil {
				select {
				case v.eventCh <- EventForwardingFail:
				default:
				}
			}
		}
	}

	return nil
}

func (v *Handler) OnVideo(timestamp uint32, payload io.Reader) error {
	v.lastSeen.Store(time.Now())

	buf := new(bytes.Buffer)
	if _, err := buf.ReadFrom(payload); err != nil {
		return err
	}
	data := buf.Bytes()

	// AVC sequence header 캐싱: 0x17 0x00
	if len(data) >= 2 && data[0] == 0x17 && data[1] == 0x00 {
		v.seqMu.Lock()
		v.videoSeqHeader = append([]byte(nil), data...)
		v.seqMu.Unlock()
	}
	v.lastVideoTS.Store(timestamp)

	v.wowzaMu.RLock()
	stream := v.wowzaStream
	v.wowzaMu.RUnlock()

	if stream != nil {
		if err := stream.Write(6, timestamp, &rtmpmsg.VideoMessage{Payload: buf}); err != nil {
			log.Printf("Wowza 비디오 전송 실패: %v", err)
			if v.eventCh != nil {
				select {
				case v.eventCh <- EventForwardingFail:
				default:
				}
			}
		}
	}

	return nil
}

// OnClose : 연결 종료
func (v *Handler) OnClose() {
	log.Println("Connection Close")

	if v.eventCh != nil {
		v.eventCh <- EventStreamClose
	}

	if v.cancel != nil {
		v.cancel()
	}

	v.wowzaMu.Lock()
	if v.wowzaStream != nil {
		v.wowzaStream.Close()
		v.wowzaStream = nil
	}
	if v.wowzaConn != nil {
		v.wowzaConn.Close()
		v.wowzaConn = nil
	}
	v.wowzaMu.Unlock()
}
