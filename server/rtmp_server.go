package server

import (
	"io"
	"log"
	"net"
	"sync/atomic"

	"github.com/dev-jiemu/live-stream-switcher-poc/config"
	handler2 "github.com/dev-jiemu/live-stream-switcher-poc/handler"
	"github.com/yutopp/go-rtmp"
)

var totalConnection int64 = 0
var activeConnection int64 = 0

func RTMPStart() {
	var err error

	log.Println("====================")
	log.Println("RTMP Forward Server")
	log.Println("====================")

	tcpAddr, err := net.ResolveTCPAddr("tcp", config.EnvConfig.Address)
	if err != nil {
		log.Fatalf("failed tcp resolve: %s", err)
	}
	log.Printf("tcp address: %s\n", tcpAddr.String())

	listener, err := net.ListenTCP("tcp", tcpAddr)
	if err != nil {
		log.Fatalf("failed tcp listen: %s", err)
	}
	log.Printf("tcp listen: %s\n", listener.Addr().String())

	server := rtmp.NewServer(&rtmp.ServerConfig{
		OnConnect: func(conn net.Conn) (io.ReadWriteCloser, *rtmp.ConnConfig) {
			connectID := atomic.AddInt64(&totalConnection, 1)
			atomic.AddInt64(&activeConnection, 1)
			defer atomic.AddInt64(&activeConnection, -1)

			handler := &handler2.Handler{
				ConnectionId: connectID,
			}

			return conn, &rtmp.ConnConfig{
				Handler: handler,
				ControlState: rtmp.StreamControlStateConfig{
					DefaultBandwidthWindowSize: 6 * 1024 * 1024 / 8,
				},
			}
		},
	})

	if err = server.Serve(listener); err != nil {
		log.Fatalf("failed tcp serve: %s", err)
	}
}
