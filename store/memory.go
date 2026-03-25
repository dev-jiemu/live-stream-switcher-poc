package store

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync"
	"time"
)

type StreamKeyType string

const (
	StreamKeyMain   StreamKeyType = "main"
	StreamKeyBackup StreamKeyType = "backup"
)

type StreamKey struct {
	Key       string        `json:"key"`
	Type      StreamKeyType `json:"type"`
	CreatedAt time.Time     `json:"created_at"`
	ExpiresAt time.Time     `json:"expires_at"`
}

type StreamKeyPair struct {
	Cpk       string     `json:"cpk"`
	Main      *StreamKey `json:"main"`
	Backup    *StreamKey `json:"backup"`
	CreatedAt time.Time  `json:"created_at"`
	ExpiresAt time.Time  `json:"expires_at"`
}

type StreamKeyStore struct {
	mutex sync.RWMutex
	store map[string]*StreamKeyPair // key : cpk
}

var KeyStore *StreamKeyStore

func NewStreamKeyStore() *StreamKeyStore {
	return &StreamKeyStore{
		store: make(map[string]*StreamKeyPair),
	}
}

func generateKey() (string, error) {
	bytes := make([]byte, 16) // 32 hex chars
	if _, err := rand.Read(bytes); err != nil {
		return "", fmt.Errorf("failed to generate random bytes: %w", err)
	}
	return hex.EncodeToString(bytes), nil
}

func (v *StreamKeyStore) GetOrCreate(cpk string, duration time.Duration) (*StreamKeyPair, error) {
	var err error

	v.mutex.Lock()
	defer v.mutex.Unlock()

	if pair, exists := v.store[cpk]; exists {
		if time.Now().Before(pair.ExpiresAt) {
			return pair, nil
		}
		delete(v.store, cpk) // 만료된 경우 삭제
	}

	mainKey, err := generateKey()
	if err != nil {
		return nil, fmt.Errorf("failed to generate main key: %w", err)
	}

	backupKey, err := generateKey()
	if err != nil {
		return nil, fmt.Errorf("failed to generate backup key: %w", err)
	}

	now := time.Now()
	expiresAt := now.Add(duration)

	pair := &StreamKeyPair{
		Cpk:       cpk,
		Main:      &StreamKey{mainKey, StreamKeyMain, now, expiresAt},
		Backup:    &StreamKey{backupKey, StreamKeyBackup, now, expiresAt},
		CreatedAt: now,
		ExpiresAt: expiresAt,
	}

	v.store[cpk] = pair
	return pair, err
}

func (v *StreamKeyStore) Delete(cpk string) {
	v.mutex.Lock()
	defer v.mutex.Unlock()
	delete(v.store, cpk)
}

// CleanupExpired : 만료된 key count
func (v *StreamKeyStore) CleanupExpired() int {
	v.mutex.Lock()
	defer v.mutex.Unlock()

	var count int
	now := time.Now()

	for cpk, pair := range v.store {
		if now.After(pair.ExpiresAt) {
			delete(v.store, cpk)
			count++
		}
	}

	return count
}

func (v *StreamKeyStore) GetAll() map[string]*StreamKeyPair {
	v.mutex.RLock()
	defer v.mutex.RUnlock()

	result := make(map[string]*StreamKeyPair, len(v.store))
	for k, v := range v.store {
		result[k] = v
	}

	return result
}
