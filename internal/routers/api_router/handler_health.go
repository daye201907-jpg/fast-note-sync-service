// Package api_router provides HTTP API router handlers
// Package api_router 提供 HTTP API 路由处理器
package api_router

import (
	"time"

	"github.com/haierkeys/fast-note-sync-service/internal/app"
	"github.com/haierkeys/fast-note-sync-service/internal/service"
	pkgapp "github.com/haierkeys/fast-note-sync-service/pkg/app"
	"github.com/haierkeys/fast-note-sync-service/pkg/code"

	"github.com/gin-gonic/gin"
)

// HealthHandler health check handler
// HealthHandler 健康检查处理器
type HealthHandler struct {
	*Handler
}

// NewHealthHandler creates health check handler instance
// NewHealthHandler 创建健康检查处理器实例
func NewHealthHandler(a *app.App) *HealthHandler {
	return &HealthHandler{Handler: NewHandler(a)}
}

// HealthResponse health check response
// HealthResponse 健康检查响应
type HealthResponse struct {
	Status   string  `json:"status"`   // "healthy" or "unhealthy" // "healthy" 或 "unhealthy"
	Version  string  `json:"version"`  // Service version number // 服务版本号
	Uptime   float64 `json:"uptime"`   // Uptime (seconds) // 运行时间（秒）
	Database string  `json:"database"` // "connected" or "error" // "connected" 或 "error"
	// Capabilities is absent on binaries that predate conditional note writes.
	// Clients must see atomicNoteWrite.supported and enabled before sending atomic:true.
	// A later code 1 without data.atomicApplied is not an atomic commit.
	// Capabilities 在不支持条件写入的旧二进制上不存在。客户端必须先看到 supported 与 enabled。
	Capabilities HealthCapabilities `json:"capabilities"`
}

// HealthCapabilities reports non-sensitive server abilities.
// HealthCapabilities 只描述非敏感的服务端能力。
type HealthCapabilities struct {
	AtomicNoteWrite HealthAtomicNoteWrite `json:"atomicNoteWrite"`
}

// HealthAtomicNoteWrite is the conditional-write capability.
// Supported is true on this build. Enabled follows FNS_ATOMIC_NOTE_WRITE.
// HealthAtomicNoteWrite 是条件写入能力。Supported 在本构建上为 true。
// Enabled 跟随 FNS_ATOMIC_NOTE_WRITE。
type HealthAtomicNoteWrite struct {
	Supported bool `json:"supported"`
	Enabled   bool `json:"enabled"`
}

// Check health check interface
// @Summary Health check
// @Description Check service health status, including database connection
// @Tags System
// @Produce json
// @Success 200 {object} HealthResponse
// @Router /api/health [get]
func (h *HealthHandler) Check(c *gin.Context) {
	response := HealthResponse{
		Status:   "healthy",
		Version:  h.App.Version().Version,
		Uptime:   time.Since(h.App.StartTime).Seconds(),
		Database: "connected",
		Capabilities: HealthCapabilities{
			AtomicNoteWrite: HealthAtomicNoteWrite{
				Supported: true,
				Enabled:   service.AtomicNoteWriteEnabled(),
			},
		},
	}

	// Check database connection
	// 检查数据库连接
	if err := h.App.DB.Raw("SELECT 1").Error; err != nil {
		response.Status = "unhealthy"
		response.Database = "error"
		pkgapp.NewResponse(c).ToResponse(code.Failed.WithData(response))
		return
	}

	pkgapp.NewResponse(c).ToResponse(code.Success.WithData(response))
}
