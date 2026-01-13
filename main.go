package main

import (
	"log"
	"log/slog"
	"time"

	"github.com/dev-jiemu/live-stream-switcher-poc/config"
	"github.com/dev-jiemu/live-stream-switcher-poc/logger"
	"github.com/dev-jiemu/live-stream-switcher-poc/server"
	"github.com/dev-jiemu/live-stream-switcher-poc/store"
)

func main() {
	var err error
	if err = config.InitConfig(); err != nil {
		log.Fatalf("fail to load config : %v", err)
	}

	if err = logger.SlogInit(); err != nil {
		log.Fatalf("fail to init slog logger : %v", err)
	}

	store.KeyStore = store.NewStreamKeyStore()

	// expired stream key
	go func() {
		ticker := time.NewTicker(1 * time.Minute)
		defer ticker.Stop()

		for range ticker.C {
			count := store.KeyStore.CleanupExpired()
			if count > 0 {
				slog.Debug("[CleanUp] Removed expired stream key", "count", count)
			}
		}
	}()

	// RTMP Server start
	server.RTMPStart()

	// Api Server start
	server.APIStart()
}
