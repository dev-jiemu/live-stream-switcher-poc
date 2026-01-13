package handler

import (
	"bytes"
	"errors"
	"io"
	"log"

	"github.com/dev-jiemu/live-stream-switcher-poc/config"
	"github.com/yutopp/go-rtmp"
	rtmpmsg "github.com/yutopp/go-rtmp/message"
)

var _ rtmp.Handler = (*Handler)(nil)

type Handler struct {
	rtmp.DefaultHandler
	ConnectionId int64
	wowzaConn    *rtmp.ClientConn
	wowzaStream  *rtmp.Stream
	wowzaApp     string
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
	log.Printf("stream start : %s", cmd.PublishingName)

	streamKey := cmd.PublishingName

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
	log.Println("연결 종료 - Wowza 연결 정리")

	if v.wowzaStream != nil {
		v.wowzaStream.Close()
	}
	if v.wowzaConn != nil {
		v.wowzaConn.Close()
	}
}
