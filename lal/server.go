package lal

import (
	"log"

	"github.com/dev-jiemu/live-stream-switcher-poc/config"
	"github.com/q191201771/lal/pkg/logic"
)

// log level : 1=Debug, 2=Info, 3=Warn, 4=Error
func buildConfRaw(addr string) []byte {
	return []byte(`{
		"conf_version": "v0.4.1",
		"rtmp": {
			"enable": true,
			"addr": "` + addr + `"
		},
		"log": {
            "level": 2
        }
	}`)
}

func RTMPStart() {
	log.Println("====================")
	log.Println("RTMP Forward Server")
	log.Println("package : github.com/q191201771/lal")
	log.Println("====================")

	registry := newSessionRegistry()

	server := logic.NewLalServer(
		func(option *logic.Option) {
			option.ConfRawContent = buildConfRaw(config.EnvConfig.Address)
			option.NotifyHandler = &notifyHandler{registry: registry}
		},
	)

	// WithOnHookSession : 스트림 시작 시 placeholder streamSession 생성
	// appName은 이 시점에 없으므로 OnPubStart에서 주입 (notify_handler.go 참고)
	server.WithOnHookSession(newHookSessionFn(registry))

	log.Printf("[lal] RTMP 서버 시작: %s", config.EnvConfig.Address)
	if err := server.RunLoop(); err != nil {
		log.Fatalf("[lal] 서버 실행 실패: %v", err)
	}
}
