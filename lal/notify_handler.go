package lal

import (
	"log"
	"sync"

	"github.com/q191201771/lal/pkg/base"
	"github.com/q191201771/lal/pkg/logic"
)

// sessionRegistry : OnPubStart에서 생성한 streamSession을 streamName 기준으로 보관
// WithOnHookSession 콜백보다 OnPubStart가 항상 나중에 불리기 때문에 사용 불가 →
// 대신 OnPubStart에서 미리 streamSession을 생성해두고, WithOnHookSession에서 꺼내는 구조
type sessionRegistry struct {
	mu sync.RWMutex
	m  map[string]*streamSession // streamName → streamSession
}

func newSessionRegistry() *sessionRegistry {
	return &sessionRegistry{m: make(map[string]*streamSession)}
}

func (r *sessionRegistry) set(streamName string, s *streamSession) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.m[streamName] = s
}

func (r *sessionRegistry) get(streamName string) *streamSession {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.m[streamName]
}

func (r *sessionRegistry) delete(streamName string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.m, streamName)
}

// notifyHandler : INotifyHandler 구현
type notifyHandler struct {
	registry *sessionRegistry
}

func (h *notifyHandler) OnServerStart(info base.LalInfo) {
	log.Printf("[lal] 서버 시작: %+v", info)
}

func (h *notifyHandler) OnUpdate(info base.UpdateInfo) {}

// OnPubStart : WithOnHookSession 콜백보다 나중에 불리므로
// 여기서 streamSession을 생성하면 이미 OnMsg가 흘러들어오는 시점이라 못 씀
// → 반대로 여기서 생성하되, WithOnHookSession에서 꺼내가도록 registry에 먼저 저장
// 실제 호출 순서: WithOnHookSession → OnPubStart
// 그래서 WithOnHookSession에서는 nil을 반환하는 placeholder를 넣고,
// OnPubStart에서 진짜 streamSession으로 교체하는 구조로 처리
func (h *notifyHandler) OnPubStart(info base.PubStartInfo) {
	log.Printf("[lal] pub 시작: app=%s stream=%s remote=%s",
		info.AppName, info.StreamName, info.RemoteAddr)

	// WithOnHookSession 콜백에서 이미 placeholder가 생성돼 있음
	// 거기서 appName을 못 받으니, 여기서 appName을 주입
	if s := h.registry.get(info.StreamName); s != nil {
		s.setAppName(info.AppName)
		s.start() // appName 확정 후 FSM + Wowza 연결 시작
	}
}

func (h *notifyHandler) OnPubStop(info base.PubStopInfo) {
	log.Printf("[lal] pub 종료: app=%s stream=%s", info.AppName, info.StreamName)
	h.registry.delete(info.StreamName)
}

func (h *notifyHandler) OnSubStart(info base.SubStartInfo)        {}
func (h *notifyHandler) OnSubStop(info base.SubStopInfo)          {}
func (h *notifyHandler) OnRelayPullStart(info base.PullStartInfo) {}
func (h *notifyHandler) OnRelayPullStop(info base.PullStopInfo)   {}
func (h *notifyHandler) OnRtmpConnect(info base.RtmpConnectInfo)  {}
func (h *notifyHandler) OnHlsMakeTs(info base.HlsMakeTsInfo)      {}

// newHookSessionFn : server.go의 WithOnHookSession에서 사용
// streamSession을 생성해 registry에 저장 후 반환
// 이 시점엔 appName을 모르므로 placeholder로 생성, OnPubStart에서 appName 주입
func newHookSessionFn(registry *sessionRegistry) func(string, string) logic.ICustomizeHookSessionContext {
	return func(uniqueKey string, streamName string) logic.ICustomizeHookSessionContext {
		log.Printf("[lal] 스트림 훅: uniqueKey=%s streamName=%s (appName은 OnPubStart에서 주입)", uniqueKey, streamName)
		s := newStreamSession(streamName) // appName은 빈 채로 생성
		registry.set(streamName, s)
		return s
	}
}
