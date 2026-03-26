# live-stream-switcher-poc
live stream 전환 서버 practice

---

### Think 🤔
```text 
Client (main) → stream_key_main → Proxy Server → Wowza
Client (backup) → stream_key_backup → Proxy Server → (standby)
```

- 클라이언트는 main 용, backup 용 방송을 동시에 송출할 것임 
- 백업이 되는 송출건은 통신만 유지한 채 대기중 
- 이후 메인이 끊어졌다 판단되면 백업 방송이 Wowza 서버로 포워딩 됨 
- 백업방송으로 스트리밍 전환된 이후 메인 방송이 재부활해도 백업으로 쭉 이어감. 이후 백업이 죽을 경우 메인으로 변경 -> 스위칭 개념으로 이해됨
- 처음에는 main / backup 서버를 물리적으로 나누고 그 중간에 `redis` 를 둬서 둘 서버간의 통신 체크와 상태체크가 필요하다 생각했으나, 포워딩 서버도 n중화 될 수 있는 상황이라 해당 구조는 구현 난이도가 매우 높아지게 됨 ㅠ -> 분산 환경에서의 관리가 필요해짐
  - 따라서 해당 프로젝트는 main/backup 구분을 `stream key` 로만 진행하고, 방송 또는 서버가 살아있거나 종료됐는지를 체크하고 다음 명령을 내려주는 `controller` 서버를 별도로 두는 방향으로 생각하게 됨
  - (해당 부분은 확인이 필요하지만) stream key 생성 주체가 본인이거나, 다른 곳이여도 클라이언트의 메인과 백업용 스트림키가 다르게 생성된다면 프록시 입장으로써 초기 방송 송출 시점에 main / backup 판단 가능
  - 해당 키값은 초기 방송송출을 누구 먼저 할것인지에 대한 판단에 이용될 뿐, 이후 switch 가 필요할땐 사실상 방송이 누구껀지에 대한 판단 외엔 쓰이지 않을 것 같음 (active / inactive 구분 별도로 내려줌)
---

### 아키텍처 흐름(초안)
```
 Shooter
    │
    ├─ main stream key   ─┐
    └─ backup stream key ─┤
                          ▼
                       nginx
                     (RTMP 수신)
          (근데 이것도 확인 필요함: nginx의 역할)
                          │
              ┌───────────┴───────────┐
              ▼                       ▼
        Forwarder 1             Forwarder 2
        Forwarder 3             Forwarder 4   ← 동일 코드, 풀 운영
              │                       │
              └───────────┬───────────┘
                          │ heartbeat / 전환 신호
                          ▼
                      Controller (HA)
                          │
                  ┌───────┴───────┐
                  ▼               ▼
               Redis           Wowza
            (상태 저장)      (스트림 송출)
```

#### Forwarder
- RTMP 스트림을 수신하고 Wowza로 포워딩하는 핵심 모듈
- main / backup을 구별하지 않음. active / inactive 상태만 가짐
  - inactive 상태: 스트림 수신 + 링버퍼 유지 + Wowza TCP pre-warm
  - active 상태: 링버퍼 flush 후 Wowza로 RTMP push 시작
- Controller로부터 activate/deactivate 신호를 받아 동작 
- 주기적으로 heartbeat를 Controller에 전송

#### Controller
- 전환을 결정할 역할 주체
- HA 구성 할 수 있게 생각중(그래서 중간에 `redis` 생각)
- Forwarder Heartbeat 기반으로 스트림 상태 감지
- 전환 결정 후 신규 active Forwarder의 Wowza 연결 완료 콜백을 확인한 뒤 기존 active Forwarder를 deactivate (동시 연결 방지)
- Redis TTL 활용한 장애 감지 흐름 (*하단참고)
- 만약, console api 와의 연동이 필요한다면 해당 서버와 연동하면 될듯

---

### 시나리오 생각해보기
##### 방송 시작
```text
1. main, backup stream 동시 송출 시작
2. forwarder 가 각각 스트림 수신 함
3. controller 에게 각 스트림이 수신되었음을 알림
4. controller 메인에 해당하는 스트림키를 판단해서 해당 스트림키를 받은 forwarder 에게 active를 지정
5. active 를 수신받은 forwarder 는 wowza 에 publish 시작
6. backup stream key를 받은 Forwarder → inactive (링버퍼 + pre-warm 대기)
```

##### main 스트림 장애 -> backup 전환
```text
1. active forwarder 에서 스트림 끊어진걸 감지함
    - RTMP 연결 끊김
    - silent stream
2. wowza push 중단 후 controller 한테 장애 이벤트 전송함
3. controller 는 backup stream key 를 받은 forwarder 를 찾아서 active 신호 내려줌
    - 근데 이 경우에 backup 못찾을수도 있는데... backup 도 장애날 정도로 문제가 있는 경우라서 방송 종료처리 해야할듯
4. backup forwarder 가 wowza 에게 연결 후 controller 에게 연결완료 callback 전달
5. controller 는 콜백을 받으면 기존 main forwarder 에게 inactive 전달
6. 이후 main stream 이 RTMP 가 다시 연결되더라도 inactive 상태이므로 링버퍼 + pre-warm 대기 상태가 됨
```

##### 방송종료
```text
FCUnpublish 받은 경우:
  1. Forwarder FCUnpublish 수신 → normalClose = true
  2. Forwarder: OnClose 발생 → Controller에 "정상종료" 이벤트
  3. Controller: 백업 전환 없이 방송 종료 처리
  4. Wowza 연결 해제, 상태 초기화

FCUnpublish 없이 끊긴 경우 (정상종료 확인 불가):
  1. Forwarder OnClose 발생 → Controller에 "장애" 이벤트 (정상이여도 안오면 장애로 간주함)
  2. Controller 백업 전환 시도
  3. backup Forwarder도 없음 → 방송 종료 처리
  4. Wowza 연결 해제, 상태 초기화
```

###### 참고
RTMP 프로토콜에서 정상 종료와 비정상 종료를 구별하는 방법
```text
정상 종료 (OBS에서 방송 중지):
RTMP FCUnpublish 커맨드 전송
- RTMP DeleteStream 커맨드 전송
- TCP 연결 종료
- OnClose 호출

비정상 종료 (네트워크 끊김 등):
FCUnpublish, DeleteStream 없이
- TCP 연결 그냥 끊김
- OnClose 호출
```
근데 이것도 OBS 기준이고 뭐로 방송하느냐에 따라 FCUnpublish 커멘드 안올 수도 있다고 함;;


---

### 2026.03.23 변경안
🧐 Url Prefix 형태로 분리해서 처리 -> 물리서버 구축 환경 자체가 다를 예정이라고 함

##### 방송 시작
```text
1. main/backup 동시 인입
2. main → Redis active = "main" 등록, wowza 포워딩 시작, heartbeat 갱신
3. backup → Redis active 확인 → main이 active → standby
```

##### main stream 장애
```text
1. main heartbeat TTL 만료
2. backup 감지 → Redis active = "backup" 로 변경
3. backup → wowza 포워딩 시작, heartbeat 갱신
4. main 복구되어 재인입
   → Redis active = "backup" 확인
   → standby (포워딩 안 함)
   → heartbeat만 갱신 (언제든 투입 가능하도록)
```

##### backup stream 장애 (main active)
```text
1. backup heartbeat TTL 만료
2. main 감지 → Redis active = "main" 으로 변경
3. main → 링버퍼 flush → wowza 포워딩 재시작
```

##### 방송종료
```text
case 1: main active, 둘 다 살아있음
  backup FCUnpublish → standby였으니까 그냥 종료
  main FCUnpublish   → active 종료 = 방송 종료 → wowza 연결 해제

case 2: backup active, main 없음
  backup FCUnpublish → active 종료 = 방송 종료 → wowza 연결 해제

case 3: backup active, main 재복구로 standby 중
  backup FCUnpublish → active 종료 = 방송 종료 → wowza 연결 해제
  main FCUnpublish   → standby였으니까 그냥 종료
```

---

### 2026.03.26 Memo

EOF Issue 가 생각 이상으로 많이 발생하는데, `onStatus` 이벤트의 중요성...? 을 생각하다보니 찾게된 패키지
- 현재 사용중이였던 라이브러리: github.com/yutopp/go-rtmp
- 찾은 라이브러리: github.com/q191201771/lal

현재 사용한 `go-rtmp` 라이브러리는 연결과 별개로 스트림 전송 fail 을 잡아낼수 없는 구조라나.... <br/>
따라서 `lal` 라이브러리로도 구현 해봄