package models

import (
	"time"

	"github.com/dev-jiemu/live-stream-switcher-poc/store"
)

type IssueStreamKeyRequest struct {
	Cpk      string `json:"cpk" binding:"required"`
	Duration *int   `json:"duration"` // default 24h
}

type IssueStreamKeyResponse struct {
	Cpk       string           `json:"cpk" binding:"required"`
	Main      *store.StreamKey `json:"main"`
	Backup    *store.StreamKey `json:"backup"`
	IsNew     bool             `json:"isNew"`
	ExpiresAt time.Time        `json:"expiresAt"`
}

type ErrorResponse struct {
	Error   string `json:"error"`
	Message string `json:"message"`
}
