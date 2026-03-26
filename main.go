package main

import (
	"flag"
	"fmt"
	"log"

	"github.com/dev-jiemu/live-stream-switcher-poc/config"
	goRtmp "github.com/dev-jiemu/live-stream-switcher-poc/go-rtmp"
	"github.com/dev-jiemu/live-stream-switcher-poc/logger"
	"github.com/dev-jiemu/live-stream-switcher-poc/store"
)

func main() {
	serverID := flag.String("id", "", "서버 식별자 (예: server-a, server-b)")
	port := flag.String("port", "", "RTMP 리슨 포트 (예: 1935, 1936)")
	flag.Parse()

	var err error
	if err = config.InitConfig(); err != nil {
		log.Fatalf("fail to load config : %v", err)
	}

	if err = logger.SlogInit(); err != nil {
		log.Fatalf("fail to init slog logger : %v", err)
	}

	// 커맨드라인 인자가 있으면 config 덮어쓰기
	if *serverID != "" {
		config.EnvConfig.ServerID = *serverID
	}
	if *port != "" {
		config.EnvConfig.Address = fmt.Sprintf("localhost:%s", *port)
	}

	log.Printf("wowza_host : %v\n", config.EnvConfig.Wowza.WowzaHost)
	log.Printf("server id  : %v\n", config.EnvConfig.ServerID)
	log.Printf("address    : %v\n", config.EnvConfig.Address)

	store.NewRedisClient(fmt.Sprintf("%s:%s", config.EnvConfig.Redis.Address, config.EnvConfig.Redis.Port))

	// TODO : lal 구현시 해당 코드 주석처리
	goRtmp.RTMPStart()
}
