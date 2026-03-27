package lal

import (
	"context"
	"log"
	"time"

	"github.com/dev-jiemu/live-stream-switcher-poc/config"
	"github.com/dev-jiemu/live-stream-switcher-poc/store"
	"github.com/q191201771/lal/pkg/base"
	"github.com/q191201771/lal/pkg/rtmp"
)

type Event int

const (
	EventBecameActive    Event = iota // 내가 active 됨
	EventActiveExpired                // 상대방 heartbeat 만료
	EventForwardingFail               // Wowza 전송/연결 실패
	EventReconnectFailed              // 재연결 모두 실패
	EventSwitchFail                   // Standby → Active 스위칭 실패
	EventStreamClose                  // 스트림 종료
)

type State int

const (
	StateStandby State = iota
	StateActive
)

// streamSession : ICustomizeHookSessionContext 구현
// 스트림 1개당 1개 생성됨
//
// 생성 흐름:
//
//	WithOnHookSession 콜백 → newStreamSession() : appName 없이 생성
//	OnPubStart            → setAppName() + start() : appName 주입 후 FSM 시작
//
// sequence header(SPS/PPS, AAC config)는 캐싱해두고 Wowza 연결 시 재전송
type streamSession struct {
	appName    string
	streamName string

	pushSession *rtmp.PushSession

	ctx    context.Context
	cancel context.CancelFunc

	eventCh chan Event
	state   State
	started bool // start() 호출 여부

	// sequence header 캐시 (Wowza 연결/재연결 시 재전송용)
	videoSeqHeader base.RtmpMsg
	audioSeqHeader base.RtmpMsg
	metaData       base.RtmpMsg
	hasVideoSH     bool
	hasAudioSH     bool
	hasMetaData    bool
	lastVideoTS    uint32 // 재전송 시 타임스탬프 맞춤용
	lastAudioTS    uint32
}

// newStreamSession : appName 없이 placeholder로 생성
// start()가 불리기 전까지 OnMsg는 state=StateStandby라 포워딩 안 됨
func newStreamSession(streamName string) *streamSession {
	ctx, cancel := context.WithCancel(context.Background())
	return &streamSession{
		streamName: streamName,
		ctx:        ctx,
		cancel:     cancel,
		eventCh:    make(chan Event, 4),
		state:      StateStandby,
	}
}

// setAppName : OnPubStart에서 appName 확정 후 주입
func (s *streamSession) setAppName(appName string) {
	s.appName = appName
}

// start : appName 확정 후 FSM + Wowza 연결 시작
// OnPubStart에서 호출
func (s *streamSession) start() {
	s.started = true
	go s.runEventLoop()

	serverId, _ := store.Client.GetActive(s.ctx, s.appName, s.streamName)
	switch {
	case serverId == "" || serverId == config.EnvConfig.ServerID:
		ok, _ := store.Client.SetActive(s.ctx, s.appName, s.streamName, config.EnvConfig.ServerID)
		if ok {
			if err := s.connectWowza(); err != nil {
				store.Client.DeleteActive(s.ctx, s.appName, s.streamName)
				log.Printf("[lal][%s/%s] 초기 Wowza 연결 실패, Standby로 대기: %v", s.appName, s.streamName, err)
			} else {
				s.eventCh <- EventBecameActive
			}
		}
	default:
		log.Printf("[lal][%s/%s] 다른 서버가 active, Standby 대기", s.appName, s.streamName)
	}
}

// OnMsg : lal이 스트림 패킷 수신할 때마다 호출 (오디오/비디오 모두)
// 주의: 빠르게 처리해야 함 — 블로킹 금지
func (s *streamSession) OnMsg(msg base.RtmpMsg) {
	// sequence header / metadata 캐싱 (연결/재연결 시 재전송용)
	switch msg.Header.MsgTypeId {
	case base.RtmpTypeIdMetadata:
		s.metaData = msg.Clone()
		s.hasMetaData = true
	case base.RtmpTypeIdVideo:
		s.lastVideoTS = msg.Header.TimestampAbs
		// AVC sequence header: 0x17 0x00
		if len(msg.Payload) >= 2 && msg.Payload[0] == 0x17 && msg.Payload[1] == 0x00 {
			s.videoSeqHeader = msg.Clone()
			s.hasVideoSH = true
		}
	case base.RtmpTypeIdAudio:
		s.lastAudioTS = msg.Header.TimestampAbs
		// AAC sequence header: 0xAF 0x00
		if len(msg.Payload) >= 2 && msg.Payload[0] == 0xAF && msg.Payload[1] == 0x00 {
			s.audioSeqHeader = msg.Clone()
			s.hasAudioSH = true
		}
	}

	if !s.started || s.state != StateActive || s.pushSession == nil {
		return
	}

	if err := s.pushSession.WriteMsg(msg); err != nil {
		log.Printf("[lal][%s/%s] Wowza 전송 실패: %v", s.appName, s.streamName, err)
		select {
		case s.eventCh <- EventForwardingFail:
		default:
		}
	}
}

// OnStop : pub 세션 종료 시 lal이 호출
func (s *streamSession) OnStop() {
	log.Printf("[lal][%s/%s] 스트림 종료", s.appName, s.streamName)
	s.eventCh <- EventStreamClose
}

// ---------------------------------------------------------------------------------------------------------------------

func (s *streamSession) connectWowza() error {
	pushSession := rtmp.NewPushSession(func(option *rtmp.PushSessionOption) {
		option.PushTimeoutMs = 10000
		option.WriteAvTimeoutMs = 10000
	})

	url := "rtmp://" + config.EnvConfig.Wowza.WowzaHost + "/" + s.appName + "/" + s.streamName

	// Push()는 Wowza로부터 onStatus("NetStream.Publish.Start") 응답을 받을 때까지 블로킹
	// 즉 여기서 에러 없이 리턴 = Wowza가 스트림 수락 확인
	if err := pushSession.Push(url); err != nil {
		return err
	}

	log.Printf("[lal][%s/%s] ✅ Wowza onStatus 확인, 포워딩 시작: %s", s.appName, s.streamName, url)
	s.pushSession = pushSession

	// sequence header / metadata 재전송 (Wowza가 스트림을 올바르게 인식하도록)
	if s.hasMetaData {
		_ = pushSession.WriteMsg(s.metaData)
	}
	if s.hasVideoSH {
		_ = pushSession.WriteMsg(s.videoSeqHeader)
	}
	if s.hasAudioSH {
		_ = pushSession.WriteMsg(s.audioSeqHeader)
	}

	// Wowza 연결 끊김 감지 (EOF, 네트워크 단절 등)
	go func() {
		err := <-pushSession.WaitChan()
		log.Printf("[lal][%s/%s] Wowza 연결 종료: %v", s.appName, s.streamName, err)
		s.pushSession = nil
		select {
		case s.eventCh <- EventForwardingFail:
		default:
		}
	}()

	return nil
}

func (s *streamSession) startConnectAsync(isSwitching bool) {
	go func() {
		const maxRetries = 3
		const baseBackoff = 1 * time.Second

		for i := 0; i < maxRetries; i++ {
			if i > 0 {
				backoff := baseBackoff * time.Duration(1<<uint(i-1))
				log.Printf("[lal][%s/%s] Wowza 재연결 대기 %v (%d/%d)", s.appName, s.streamName, backoff, i, maxRetries)
				select {
				case <-s.ctx.Done():
					return
				case <-time.After(backoff):
				}
			}
			if err := s.connectWowza(); err != nil {
				log.Printf("[lal][%s/%s] Wowza 연결 실패 (%d/%d): %v", s.appName, s.streamName, i+1, maxRetries, err)
				continue
			}
			select {
			case s.eventCh <- EventBecameActive:
			default:
			}
			return
		}

		failEvent := EventReconnectFailed
		if isSwitching {
			failEvent = EventSwitchFail
		}
		log.Printf("[lal][%s/%s] Wowza 최종 연결 실패 (isSwitching=%v)", s.appName, s.streamName, isSwitching)
		select {
		case s.eventCh <- failEvent:
		default:
		}
	}()
}

func (s *streamSession) runEventLoop() {
	heartbeatTicker := time.NewTicker(store.HeartbeatInterval)
	watchTicker := time.NewTicker(store.PollingInterval)
	defer heartbeatTicker.Stop()
	defer watchTicker.Stop()

	heartbeatFailCount := 0
	const maxHeartbeatFail = 3

	// isActive는 FSM 전이(EventBecameActive 등)에만 의존해서 관리
	// runEventLoop 진입 시점에 Redis 조회로 초기화하면 start()의 connectWowza()와
	// 타이밍 경합이 생겨 EventBecameActive가 와도 isActive=false인 채로 남는 버그 발생
	isActive := false

	for {
		select {
		case <-s.ctx.Done():
			return

		case <-watchTicker.C:
			if isActive {
				continue
			}
			activeServer, err := store.Client.GetActive(s.ctx, s.appName, s.streamName)
			if err != nil {
				continue
			}
			if activeServer == "" {
				s.eventCh <- EventActiveExpired
				continue
			}
			alive, err := store.Client.IsAlive(s.ctx, s.appName, s.streamName, activeServer)
			if err != nil {
				continue
			}
			if !alive {
				store.Client.DeleteActive(s.ctx, s.appName, s.streamName)
				s.eventCh <- EventActiveExpired
			}

		case <-heartbeatTicker.C:
			if !isActive {
				continue
			}
			if err := store.Client.RefreshHeartbeat(s.ctx, s.appName, s.streamName, config.EnvConfig.ServerID); err != nil {
				heartbeatFailCount++
				log.Printf("[lal][%s/%s] heartbeat 실패 (%d/%d): %v", s.appName, s.streamName, heartbeatFailCount, maxHeartbeatFail, err)
				if heartbeatFailCount >= maxHeartbeatFail {
					heartbeatFailCount = 0
					s.eventCh <- EventForwardingFail
				}
			} else {
				heartbeatFailCount = 0
			}

		case event := <-s.eventCh:
			next := s.transition(s.state, event)
			isActive = next == StateActive
			s.state = next
		}
	}
}

func (s *streamSession) transition(state State, event Event) State {
	switch state {
	case StateStandby:
		switch event {
		case EventBecameActive:
			log.Printf("[lal][%s/%s] FSM: Standby → Active", s.appName, s.streamName)
			return StateActive

		case EventActiveExpired:
			log.Printf("[lal][%s/%s] FSM: 상대방 만료 → 경합 시도", s.appName, s.streamName)
			ok, err := store.Client.SetActive(s.ctx, s.appName, s.streamName, config.EnvConfig.ServerID)
			if err != nil || !ok {
				log.Printf("[lal][%s/%s] FSM: 경합 패배 → Standby 유지", s.appName, s.streamName)
				return StateStandby
			}
			s.startConnectAsync(true)
			return StateStandby

		case EventSwitchFail:
			log.Printf("[lal][%s/%s] FSM: 스위칭 실패 → Redis 정리", s.appName, s.streamName)
			store.Client.DeleteActive(s.ctx, s.appName, s.streamName)
			store.Client.DelHeartbeat(s.ctx, s.appName, s.streamName, config.EnvConfig.ServerID)
			return StateStandby

		case EventStreamClose:
			s.cleanup()
			return StateStandby
		}

	case StateActive:
		switch event {
		case EventBecameActive:
			log.Printf("[lal][%s/%s] FSM: Wowza 재연결 성공, Active 유지", s.appName, s.streamName)
			return StateActive

		case EventForwardingFail:
			log.Printf("[lal][%s/%s] FSM: 포워딩 실패 → Wowza 재연결 시도", s.appName, s.streamName)
			if s.pushSession != nil {
				s.pushSession.Dispose()
				s.pushSession = nil
			}
			s.startConnectAsync(false)
			return StateActive

		case EventReconnectFailed:
			log.Printf("[lal][%s/%s] FSM: Active → Standby (복구 불가)", s.appName, s.streamName)
			store.Client.DeleteActive(s.ctx, s.appName, s.streamName)
			store.Client.DelHeartbeat(s.ctx, s.appName, s.streamName, config.EnvConfig.ServerID)
			if s.pushSession != nil {
				s.pushSession.Dispose()
				s.pushSession = nil
			}
			return StateStandby

		case EventStreamClose:
			log.Printf("[lal][%s/%s] FSM: Active → 정상 종료", s.appName, s.streamName)
			s.cleanup()
			return StateStandby
		}
	}

	return state
}

func (s *streamSession) cleanup() {
	store.Client.DeleteActive(s.ctx, s.appName, s.streamName)
	store.Client.DelHeartbeat(s.ctx, s.appName, s.streamName, config.EnvConfig.ServerID)
	if s.pushSession != nil {
		s.pushSession.Dispose()
		s.pushSession = nil
	}
	s.cancel()
}
