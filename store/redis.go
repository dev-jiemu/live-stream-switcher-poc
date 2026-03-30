package store

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"strings"
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
	client *redis.ClusterClient
}

var Client *Redis

func NewRedisClient(addrs []string) {
	ctx := context.Background()

	rdb := redis.NewClusterClient(&redis.ClusterOptions{
		Addrs:      addrs,
		MaxRetries: 3,
		// 클러스터 노드가 MOVED 응답으로 내부 IP(172.28.0.x)를 알려줄 때
		// 로컬 개발 환경에서는 해당 IP에 직접 접근 불가 → localhost:포트로 변환
		// 물론 이건 docker-compose local 환경에서 이야기고, 실제 prod 올라갈땐 확인 필수임
		NewClient: func(opt *redis.Options) *redis.Client {
			// 172.28.0.x:PORT → 127.0.0.1:PORT 로 재매핑
			host, port, err := net.SplitHostPort(opt.Addr)
			if err == nil && strings.HasPrefix(host, "172.28.") {
				opt.Addr = net.JoinHostPort("127.0.0.1", port)
			}
			return redis.NewClient(opt)
		},
	})

	_, err := rdb.Ping(ctx).Result()
	if err != nil {
		log.Fatal("redis connect error: ", err)
	}

	Client = &Redis{
		client: rdb,
	}

	// 서버 가동 시점에 이 서버 ID로 남아있는 잔여 키 정리
	// 클러스터 환경에서 Keys()는 접속한 노드 하나만 조회하므로 ForEachMaster로 전체 노드 순회
	heartbeatPattern := fmt.Sprintf("rtmp:heartbeat:*:%s", config.EnvConfig.ServerID)
	activePattern := "rtmp:active:*"
	var totalHeartbeat, totalActive int

	_ = rdb.ForEachMaster(ctx, func(ctx context.Context, node *redis.Client) error {
		hKeys, _ := node.Keys(ctx, heartbeatPattern).Result()
		if len(hKeys) > 0 {
			node.Del(ctx, hKeys...)
			totalHeartbeat += len(hKeys)
		}

		aKeys, _ := node.Keys(ctx, activePattern).Result()
		for _, k := range aKeys {
			val, _ := node.Get(ctx, k).Result()
			if val == config.EnvConfig.ServerID {
				node.Del(ctx, k)
				totalActive++
				log.Printf("잔여 active 키 정리: %s", k)
			}
		}
		return nil
	})

	if totalHeartbeat > 0 {
		log.Printf("잔여 heartbeat 키 %d개 정리", totalHeartbeat)
	}
}

// heartbeatKey : rtmp:heartbeat:{app:streamKey}:serverName
// {app:streamKey} hash tag → activeKey와 항상 같은 슬롯에 배정됨 (Lua 멀티키 보장)
func heartbeatKey(app, streamKey, serverName string) string {
	return fmt.Sprintf("rtmp:heartbeat:{%s:%s}:%s", app, streamKey, serverName)
}

// activeKey : rtmp:active:{app:streamKey}
// {app:streamKey} hash tag → heartbeatKey와 항상 같은 슬롯에 배정됨 (Lua 멀티키 보장)
func activeKey(app, streamKey string) string {
	return fmt.Sprintf("rtmp:active:{%s:%s}", app, streamKey)
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

// setActiveWithHeartbeatScript : active 등록과 heartbeat 키 세팅을 원자적으로 수행하는 Lua 스크립트
// active 등록(SetNX)에 성공한 경우에만 heartbeat 키도 함께 세팅
// → active 등록 ~ 첫 heartbeat tick 사이 공백에 Standby 서버가 오탐하는 문제 방지
var setActiveWithHeartbeatScript = redis.NewScript(`
	local ok = redis.call('SET', KEYS[1], ARGV[1], 'NX')
	if ok then
		redis.call('SET', KEYS[2], ARGV[2], 'EX', ARGV[3])
		return 1
	end
	return 0
`)

// SetActiveWithHeartbeat : active 등록 + heartbeat 키 세팅을 원자적으로 수행
// SetActive 대신 이 함수를 사용하면 등록 직후부터 Standby 서버가 "살아있음"으로 판단함
func (v *Redis) SetActiveWithHeartbeat(ctx context.Context, app, streamKey, serverName string) (bool, error) {
	aKey := activeKey(app, streamKey)
	hKey := heartbeatKey(app, streamKey, serverName)
	ttlSeconds := int(HeartbeatTTL.Seconds())

	log.Printf("[redis] SetActiveWithHeartbeat aKey=%s hKey=%s", aKey, hKey)

	result, err := setActiveWithHeartbeatScript.Run(ctx, v.client,
		[]string{aKey, hKey},
		serverName, time.Now().Unix(), ttlSeconds,
	).Int()
	if err != nil {
		return false, err
	}

	// true, false 가 아니라 0, 1 이렇게 올 수도 있다고 ㅇㅂㅇ?
	return result == 1, nil
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
