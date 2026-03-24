package handler

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"net"
	"sync/atomic"
	"time"

	"github.com/dev-jiemu/live-stream-switcher-poc/config"
	"github.com/yutopp/go-rtmp"
	rtmpmsg "github.com/yutopp/go-rtmp/message"
)

var _ rtmp.Handler = (*Handler)(nil)

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
}

// OnServe : 초기 설정
func (v *Handler) OnServe(conn *rtmp.Conn) {
	v.conn = conn
	v.lastSeen.Store(time.Now())

	ctx, cancel := context.WithCancel(context.Background())
	v.cancel = cancel

	// 별도 goroutine에서 타임아웃 감시
	go v.watchdog(ctx)
}

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
				_ = v.NetConn.Close() // → readChunk 에러 → OnClose 호출
				return
			}
		}
	}
}

// TODO : 해야할 일
// main 방송 연결중이다가 main 이 끊어지면 backup 으로 연결

func (v *Handler) OnConnect(timestamp uint32, cmd *rtmpmsg.NetConnectionConnect) error {
	appName := cmd.Command.App
	if len(appName) == 0 {
		return errors.New("app name is empty")
	}

	v.wowzaApp = appName

	return nil
}

// OnPublish : stream start
func (v *Handler) OnPublish(ctx *rtmp.StreamContext, timestamp uint32, cmd *rtmpmsg.NetStreamPublish) error {
	v.lastSeen.Store(time.Now())
	streamKey := cmd.PublishingName
	log.Printf("stream start : %s", streamKey)

	// watchdog start
	watchCtx, cancel := context.WithCancel(context.Background())
	v.cancel = cancel
	go v.watchdog(watchCtx)

	// TODO : 변경요망
	// 기존 : wowza 정보 그대로 받아다 forward
	// 변경 : 인입된 stream key 기준으로 main 이고 특정 cpk 를 바라보는 키가 있는지 조회 -> 있으면 연결

	wowzaConn, err := rtmp.Dial("rtmp", config.EnvConfig.Wowza.WowzaHost, &rtmp.ConnConfig{})
	if err != nil {
		log.Printf("Wowza 연결 실패: %v", err)
		return err
	}
	v.wowzaConn = wowzaConn

	connectCmd := &rtmpmsg.NetConnectionConnect{
		Command: rtmpmsg.NetConnectionConnectCommand{
			App: v.wowzaApp,
		},
	}
	if err := wowzaConn.Connect(connectCmd); err != nil {
		log.Printf("Wowza Connect 실패: %v", err)
		return err
	}

	createStreamBody := &rtmpmsg.NetConnectionCreateStream{}
	chunkSize := uint32(128)

	stream, err := wowzaConn.CreateStream(createStreamBody, chunkSize)
	if err != nil {
		log.Printf("Wowza 스트림 생성 실패: %v", err)
		return err
	}
	v.wowzaStream = stream

	publishBody := &rtmpmsg.NetStreamPublish{
		CommandObject:  nil,
		PublishingName: streamKey,
		PublishingType: "live", // 이거 뺐더니 EOF 나버림...
	}
	if err := stream.Publish(publishBody); err != nil {
		log.Printf("Wowza publish 실패: %v", err)
		return err
	}

	log.Printf("✅ Wowza로 포워딩 시작: %s/%s", v.wowzaApp, streamKey)
	return nil
}

func (v *Handler) OnAudio(timestamp uint32, payload io.Reader) error {
	v.lastSeen.Store(time.Now())

	buf := new(bytes.Buffer)
	if _, err := io.Copy(buf, payload); err != nil {
		return err
	}

	if v.wowzaStream != nil {
		// Audio 메시지 생성
		audioMsg := &rtmpmsg.AudioMessage{
			Payload: buf, // bytes.Buffer는 io.Reader 구현
		}

		if err := v.wowzaStream.Write(4, timestamp, audioMsg); err != nil {
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
		// Video 메시지 생성
		videoMsg := &rtmpmsg.VideoMessage{
			Payload: buf, // bytes.Buffer는 io.Reader 구현
		}

		if err := v.wowzaStream.Write(6, timestamp, videoMsg); err != nil {
			log.Printf("Wowza 비디오 전송 실패: %v", err)
		}
	}

	return nil
}

func (v *Handler) OnClose() {
	log.Println("Connection Close :P")

	// watchdog goroutine 종료
	if v.cancel != nil {
		v.cancel()
	}

	// v.conn 은 라이브러리가 OnClose 호출 후에 종료할거임
	// v.NetConn 도 라이브러리가 conn.Close() 내부에서 종료함 : rwc.Close()

	if v.wowzaStream != nil {
		v.wowzaStream.Close()
	}
	if v.wowzaConn != nil {
		v.wowzaConn.Close()
	}
}
