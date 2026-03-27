# lal 패키지 기반 RTMP 포워딩 서버

## 핵심 인터페이스

### INotifyHandler
서버 레벨 이벤트 수신. `logic.NewLalServer`의 `option.NotifyHandler`에 등록.
pub/sub 세션의 시작·종료, 서버 상태 등을 콜백으로 받는다.

### ICustomizeHookSessionContext
스트림 레벨 데이터 수신. `server.WithOnHookSession`에 등록.
스트림에 흐르는 실제 패킷(`OnMsg`)과 스트림 종료(`OnStop`)를 콜백으로 받는다.

---

## 핸들러 호출 순서 (중요)

lal 내부 `server_manager__.go` 기준으로 pub 세션이 들어올 때 호출 순서는 다음과 같다.

```
1. group.AddRtmpPubSession()
       └─ group.addIn()
               └─ onHookSession()  ←  WithOnHookSession 콜백 (ICustomizeHookSessionContext 생성)
                                       이 시점부터 OnMsg() 호출 시작

2. sm.nhOnPubStart()
       └─ NotifyHandler.OnPubStart()  ←  INotifyHandler 콜백
                                          이 시점에 비로소 appName을 알 수 있음
```

**WithOnHookSession이 OnPubStart보다 항상 먼저 불린다.**

---

## appName을 얻을 수 없는 문제

`WithOnHookSession` 콜백 시그니처는 `(uniqueKey string, streamName string)` 으로,
**appName이 파라미터에 없다.**

lal의 `SimpleGroupManager`가 appName을 무시하고 streamName만으로 그룹을 관리하는 설계이기 때문이다.
(`group_manager.go` 주석: "SimpleGroupManager 忽略appName，只使用streamName")

appName은 `OnPubStart`의 `info.AppName`에서만 얻을 수 있는데,
이 콜백이 `WithOnHookSession`보다 나중에 불리기 때문에 아래와 같은 2단계 초기화 구조가 필요하다.

```
WithOnHookSession 콜백
  → newStreamSession(streamName)    // appName 없이 placeholder 생성
  → registry.set(streamName, s)     // sessionRegistry에 등록
  → OnMsg 수신 시작 (아직 포워딩 안 함)

OnPubStart 콜백 (이후에 호출)
  → registry.get(streamName)        // 등록된 streamSession 조회
  → s.setAppName(info.AppName)      // appName 주입
  → s.start()                       // FSM + Wowza 연결 시작
```

`start()` 호출 전까지는 `started = false` 상태라 `OnMsg`에서 포워딩을 하지 않는다.
단, sequence header(SPS/PPS, AAC config)와 metadata는 이 구간에도 수신되므로 별도로 캐싱한다.

---

## sequence header 캐싱 및 재전송

`OnMsg`가 `start()` 이전부터 호출되기 때문에, Wowza 연결이 완료되는 시점에는
이미 초반 패킷(metadata, video/audio sequence header)이 지나간 상태다.

Wowza가 스트림을 올바르게 디코딩하려면 연결 직후에 이 헤더들을 재전송해야 한다.
재전송 없이 일반 프레임만 보내면 플레이어에서 3012(디코더 초기화 실패) 에러가 발생한다.

```
OnMsg에서 항상 캐싱
  - metadata       : RtmpTypeIdMetadata (0x12)
  - video SH       : RtmpTypeIdVideo (0x09) + payload[0]==0x17, payload[1]==0x00  (AVC sequence header)
  - audio SH       : RtmpTypeIdAudio (0x08) + payload[0]==0xAF, payload[1]==0x00  (AAC sequence header)

connectWowza() 성공 직후 재전송
  metadata → videoSeqHeader → audioSeqHeader 순으로 WriteMsg
```

재연결(`startConnectAsync`) 시에도 캐시에 최신 값이 있으므로 동일하게 재전송된다.

---

## 전체 흐름 요약

```
ffmpeg/OBS
  │  rtmp push
  ▼
[lal RTMP 서버 :1935]
  │
  ├─ WithOnHookSession → streamSession 생성 (placeholder)
  │                      OnMsg 수신 시작, seq header 캐싱
  │
  ├─ OnPubStart        → appName 주입 + start()
  │                      Redis SetNX로 active 경합
  │                      경합 승리 시 connectWowza()
  │                        └─ Push() 블로킹 → onStatus 확인 완료
  │                        └─ seq header 재전송
  │                        └─ FSM: Standby → Active
  │
  ├─ OnMsg (Active)    → pushSession.WriteMsg() → Wowza
  │
  └─ OnStop            → EventStreamClose → cleanup()
                         Redis active/heartbeat 정리
                         pushSession.Dispose()
```
