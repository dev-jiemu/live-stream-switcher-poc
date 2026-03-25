package handler

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
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
	EventBecameActive   Event = iota // 내가 active 됨
	EventActiveExpired               // 상대방 heartbeat 만료
	EventForwardingFail              // forwarding 실패
	EventHeartbeatFail               // heartbeat 실패
	EventStreamClose                 // 스트림 종료
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
	wowzaConn    *rtmp.ClientConn
	wowzaStream  *rtmp.Stream
	lastSeen     atomic.Value // time.Time
	cancel       context.CancelFunc
	wowzaApp     string
	streamKey    string
	eventCh      chan Event
	state        State
}

// OnServe : 초기 설정
func (v *Handler) OnServe(conn *rtmp.Conn) {
	v.conn = conn
	v.lastSeen.Store(time.Now())
}

// watchdog : OBS 등 소스가 비정상 종료됐을 때 감지 (OnClose 가 안 불리는 케이스 대비)
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

	go v.watchdog(watchCtx)
	go v.runEventLoop(watchCtx)

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

// runEventLoop : FSM 루프
func (v *Handler) runEventLoop(ctx context.Context) {
	v.state = StateStandby

	serverId, _ := store.Client.GetActive(ctx, v.wowzaApp, v.streamKey)
	if serverId != config.EnvConfig.ServerID {
		v.startWatching(ctx)
	}

	for {
		select {
		case <-ctx.Done():
			return
		case event := <-v.eventCh:
			v.state = v.transition(v.state, event, ctx)
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
			v.startHeartbeatAsync(ctx)
			return StateActive

		case EventActiveExpired:
			log.Println("FSM: 상대방 만료 감지 → 경합 시도")
			ok, err := store.Client.SetActive(ctx, v.wowzaApp, v.streamKey, config.EnvConfig.ServerID)
			if err != nil || !ok {
				log.Println("FSM: 경합 패배 → 다시 watching")
				v.startWatching(ctx)
				return StateStandby
			}
			v.startForwardingAsync(ctx)
			return StateStandby

		case EventStreamClose:
			// standby 상태에서 종료 - Redis 정리 불필요 (active 아님)
			return StateStandby
		}

	case StateActive:
		switch event {
		case EventHeartbeatFail, EventForwardingFail:
			log.Printf("FSM: Active → Standby (포워딩 중단)")
			// ctx 가 살아있으므로 여기서 정리 가능
			store.Client.DeleteActive(ctx, v.wowzaApp, v.streamKey)
			store.Client.DelHeartbeat(ctx, v.wowzaApp, v.streamKey, config.EnvConfig.ServerID)
			if v.wowzaConn != nil {
				v.wowzaConn.Close()
				v.wowzaConn = nil
			}
			v.wowzaStream = nil
			v.startWatching(ctx)
			return StateStandby

		case EventStreamClose:
			// OnClose 에서 이미 Redis 정리했으므로 여기선 상태만 전이
			log.Println("FSM: Active → 정상 종료")
			return StateStandby
		}
	}

	return state
}

// startWatching : 상대방 heartbeat 감시 → 만료 시 EventActiveExpired 발행
func (v *Handler) startWatching(ctx context.Context) {
	go func() {
		ch := store.Client.WatchHeartbeat(ctx, v.wowzaApp, v.streamKey)
		select {
		case <-ctx.Done():
		case <-ch:
			v.eventCh <- EventActiveExpired
		}
	}()
}

// startHeartbeatAsync : heartbeat 실패 시 EventHeartbeatFail 발행
func (v *Handler) startHeartbeatAsync(ctx context.Context) {
	go func() {
		if err := v.startHeartbeat(ctx); err != nil {
			v.eventCh <- EventHeartbeatFail
		}
	}()
}

// startForwardingAsync : 스위칭 시 사용
func (v *Handler) startForwardingAsync(ctx context.Context) {
	go func() {
		if err := v.connectWowza(); err != nil {
			v.eventCh <- EventForwardingFail
			return
		}
		v.eventCh <- EventBecameActive
	}()
}

// connectWowza : Wowza 연결 및 publish 설정
func (v *Handler) connectWowza() error {
	wowzaConn, err := rtmp.Dial("rtmp", config.EnvConfig.Wowza.WowzaHost, &rtmp.ConnConfig{})
	if err != nil {
		log.Printf("Wowza 연결 실패: %v", err)
		return err
	}
	v.wowzaConn = wowzaConn

	if err := wowzaConn.Connect(&rtmpmsg.NetConnectionConnect{
		Command: rtmpmsg.NetConnectionConnectCommand{
			App: v.wowzaApp,
		},
	}); err != nil {
		log.Printf("Wowza Connect 실패: %v", err)
		return err
	}

	stream, err := wowzaConn.CreateStream(&rtmpmsg.NetConnectionCreateStream{}, 128)
	if err != nil {
		log.Printf("Wowza 스트림 생성 실패: %v", err)
		return err
	}
	v.wowzaStream = stream

	if err := stream.Publish(&rtmpmsg.NetStreamPublish{
		PublishingName: v.streamKey,
		PublishingType: "live",
	}); err != nil {
		log.Printf("Wowza publish 실패: %v", err)
		return err
	}

	log.Printf("✅ Wowza 포워딩 시작: %s/%s", v.wowzaApp, v.streamKey)
	return nil
}

// startHeartbeat : heartbeat 주기적 갱신, 연속 실패 시 에러 반환
func (v *Handler) startHeartbeat(ctx context.Context) error {
	ticker := time.NewTicker(store.HeartbeatInterval)
	defer ticker.Stop()

	failCount := 0
	const maxFail = 3

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := store.Client.RefreshHeartbeat(ctx, v.wowzaApp, v.streamKey, config.EnvConfig.ServerID); err != nil {
				failCount++
				log.Printf("heartbeat 실패 (%d/%d): %v", failCount, maxFail, err)
				if failCount >= maxFail {
					return fmt.Errorf("heartbeat 연속 %d회 실패: %w", maxFail, err)
				}
			} else {
				failCount = 0
			}
		}
	}
}

func (v *Handler) OnAudio(timestamp uint32, payload io.Reader) error {
	v.lastSeen.Store(time.Now())

	buf := new(bytes.Buffer)
	if _, err := io.Copy(buf, payload); err != nil {
		return err
	}

	if v.wowzaStream != nil {
		if err := v.wowzaStream.Write(4, timestamp, &rtmpmsg.AudioMessage{Payload: buf}); err != nil {
			log.Printf("Wowza 오디오 전송 실패: %v", err)
		}
	}

	return nil
}

func (v *Handler) OnVideo(timestamp uint32, payload io.Reader) error {
	v.lastSeen.Store(time.Now())

	buf := new(bytes.Buffer)
	if _, err := io.Copy(buf, payload); err != nil {
		return err
	}

	if v.wowzaStream != nil {
		if err := v.wowzaStream.Write(6, timestamp, &rtmpmsg.VideoMessage{Payload: buf}); err != nil {
			log.Printf("Wowza 비디오 전송 실패: %v", err)
		}
	}

	return nil
}

// OnClose : 연결 종료 - ctx 취소 전에 Redis 정리 먼저
func (v *Handler) OnClose() {
	log.Println("Connection Close")

	// ctx 취소 전에 별도 context 로 Redis 정리
	cleanCtx, cleanCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cleanCancel()

	if v.state == StateActive {
		store.Client.DeleteActive(cleanCtx, v.wowzaApp, v.streamKey)
		store.Client.DelHeartbeat(cleanCtx, v.wowzaApp, v.streamKey, config.EnvConfig.ServerID)
		log.Println("Redis 정리 완료")
	}

	// FSM 종료
	if v.eventCh != nil {
		v.eventCh <- EventStreamClose
	}

	// ctx 취소 → 모든 고루틴 종료
	if v.cancel != nil {
		v.cancel()
	}

	if v.wowzaStream != nil {
		v.wowzaStream.Close()
	}
	if v.wowzaConn != nil {
		v.wowzaConn.Close()
	}
}
