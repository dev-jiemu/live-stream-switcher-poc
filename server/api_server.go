package server

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/dev-jiemu/live-stream-switcher-poc/models"
	"github.com/dev-jiemu/live-stream-switcher-poc/store"
	"github.com/gin-gonic/gin"
)

func APIStart() {
	router := gin.Default()

	router.POST("/api/stream-keys", func(c *gin.Context) {
		var req models.IssueStreamKeyRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, models.ErrorResponse{
				Error:   "invalid_request",
				Message: err.Error(),
			})
			return
		}

		// duration 기본값 설정 (24시간)
		duration := 24 * 60 // 분 단위
		if req.Duration != nil && *req.Duration > 0 {
			duration = *req.Duration
		}

		pair, err := store.KeyStore.GetOrCreate(req.Cpk, time.Duration(duration)*time.Minute)
		if err != nil {
			c.JSON(http.StatusInternalServerError, models.ErrorResponse{
				Error:   "generation_failed",
				Message: err.Error(),
			})
			return
		}

		// 새로 생성되었는지 확인 (생성 시간이 1초 이내면 새로 생성된 것)
		isNew := time.Since(pair.CreatedAt) < 1*time.Second

		c.JSON(http.StatusOK, models.IssueStreamKeyResponse{
			Cpk:       pair.Cpk,
			Main:      pair.Main,
			Backup:    pair.Backup,
			IsNew:     isNew,
			ExpiresAt: pair.ExpiresAt,
		})
	})

	router.DELETE("/api/stream-keys/:cpk", func(c *gin.Context) {
		cpk := c.Param("cpk")

		store.KeyStore.Delete(cpk)

		c.JSON(http.StatusOK, gin.H{
			"message": "Stream key deleted successfully",
			"cpk":     cpk,
		})
	})

	// 전체 키 목록 : 디버깅용
	router.GET("/api/stream-keys", func(c *gin.Context) {
		result := store.KeyStore.GetAll()

		c.JSON(http.StatusOK, gin.H{
			"total": len(result),
			"keys":  result,
		})
	})

	router.GET("/health", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{
			"status": "ok",
			"time":   time.Now(),
		})
	})

	slog.Debug("🚀 Server starting on :8080")
	router.Run(":8080")
}
