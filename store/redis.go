package store

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/dev-jiemu/live-stream-switcher-poc/config"
	"github.com/redis/go-redis/v9"
)

const (
	HeartbeatTTL      = 5 * time.Second
	HeartbeatInterval = 2 * time.Second
	PollingInterval   = 2 * time.Second
)

type Redis struct {
	client *redis.Client
}

var Client *Redis

func NewRedisClient(addr string) {
	ctx := context.Background()
	// TODO : cluster 모드 고려하기
	rdb := redis.NewClient(&redis.Options{
		Addr: addr,
	})

	_, err := rdb.Ping(ctx).Result()
	if err != nil {
		log.Fatal("redis connect error: ", err)
	}

	Client = &Redis{
		client: rdb,
	}

	// server id 값으로 남아있는 기존 데이터 지우기 : 서버 가동 시점에 기존 데이터가 남아있으면 비정상으로 간주함
	pattern := fmt.Sprintf("rtmp:heartbeat:*:%s", config.EnvConfig.ServerID)
	keys, _ := Client.client.Keys(ctx, pattern).Result()
	if len(keys) > 0 {
		Client.client.Del(ctx, keys...)
		log.Printf("잔여 heartbeat 키 %d개 정리", len(keys))
	}

	activePattern := "rtmp:active:*"
	activeKeys, _ := Client.client.Keys(ctx, activePattern).Result()
	for _, k := range activeKeys {
		val, _ := Client.client.Get(ctx, k).Result()
		if val == config.EnvConfig.ServerID {
			Client.client.Del(ctx, k)
			log.Printf("잔여 active 키 정리: %s", k)
		}
	}
}

// heartbeatKey : rtmp:heartbeat:{app}:{streamKey}:{serverName}
func heartbeatKey(serverName, app, streamKey string) string {
	return fmt.Sprintf("rtmp:heartbeat:%s:%s:%s", app, streamKey, serverName)
}

// activeKey : rtmp:active:{app}:{streamKey}
func activeKey(app, streamKey string) string {
	return fmt.Sprintf("rtmp:active:%s:%s", app, streamKey)
}

// RefreshHeartbeat : heartbeat 갱신 (TTL 연장)
func (v *Redis) RefreshHeartbeat(ctx context.Context, app, streamKey, serverName string) error {
	key := heartbeatKey(app, streamKey, serverName)
	return v.client.Set(ctx, key, time.Now().Unix(), HeartbeatTTL).Err()
}

// IsAlive : heartbeat key 존재 여부로 생존 확인
func (v *Redis) IsAlive(ctx context.Context, app, streamKey, serverName string) (bool, error) {
	key := heartbeatKey(app, streamKey, serverName)
	exists, err := v.client.Exists(ctx, key).Result()
	if err != nil {
		return false, err
	}

	return exists > 0, nil
}

// SetActive : active 등록 (SetNX - 없을 때만 등록)
func (v *Redis) SetActive(ctx context.Context, app, streamKey, serverName string) (bool, error) {
	key := activeKey(app, streamKey)
	ok, err := v.client.SetNX(ctx, key, serverName, 0).Result()
	return ok, err
}

// GetActive : active 서버 ID 조회
func (v *Redis) GetActive(ctx context.Context, app, streamKey string) (string, error) {
	key := activeKey(app, streamKey)
	val, err := v.client.Get(ctx, key).Result()
	if errors.Is(err, redis.Nil) {
		return "", nil
	}

	return val, err
}

// DeleteActive : active key 삭제
func (v *Redis) DeleteActive(ctx context.Context, app, streamKey string) error {
	key := activeKey(app, streamKey)
	return v.client.Del(ctx, key).Err()
}

// DelHeartbeat : heartbeat key 삭제
func (v *Redis) DelHeartbeat(ctx context.Context, app, streamKey, serverName string) error {
	key := heartbeatKey(app, streamKey, serverName)
	return v.client.Del(ctx, key).Err()
}

// WatchHeartbeat : active 서버의 heartbeat 감시 → 만료 시 채널 신호
func (v *Redis) WatchHeartbeat(ctx context.Context, app, streamKey string) <-chan struct{} {
	ch := make(chan struct{}, 1)

	go func() {
		ticker := time.NewTicker(PollingInterval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				activeServer, err := v.GetActive(ctx, app, streamKey)
				if err != nil {
					continue
				}
				if activeServer == "" {
					// active key 자체가 없음 = 이미 정리됨
					ch <- struct{}{}
					return
				}

				alive, err := v.IsAlive(ctx, app, streamKey, activeServer)
				if err != nil {
					continue
				}
				if !alive {
					v.DeleteActive(ctx, app, streamKey) // 죽은 서버 흔적 정리
					ch <- struct{}{}
					return
				}
			}
		}
	}()

	return ch
}
