package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/grok-fireworks-reg/internal/common"
	"github.com/grok-fireworks-reg/internal/config"
	"github.com/grok-fireworks-reg/internal/fireworks"
)

// FireworksRegisterRequest Fireworks 注册请求体
type FireworksRegisterRequest struct {
	Proxy         string `json:"proxy,omitempty"`
	Count         int    `json:"count,omitempty"`
	EmailProvider string `json:"email_provider,omitempty"`
}

// FireworksHandler Fireworks 注册处理器
type FireworksHandler struct {
	cfg *config.Config
}

// NewFireworksHandler 创建 FireworksHandler
func NewFireworksHandler(cfg *config.Config) *FireworksHandler {
	return &FireworksHandler{cfg: cfg}
}

// Register POST /api/fireworks/register
// SSE 流式返回日志和最终结果
func (h *FireworksHandler) Register(c *gin.Context) {
	var req FireworksRegisterRequest
	if err := c.ShouldBindJSON(&req); err != nil && err != io.EOF {
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("invalid request body: %s", err)})
		return
	}

	// 默认注册 1 个
	count := req.Count
	if count <= 0 {
		count = 1
	}

	// 并发上限
	maxConcurrent := h.cfg.Fireworks.MaxConcurrent
	if maxConcurrent <= 0 {
		maxConcurrent = 10
	}
	if count > maxConcurrent {
		count = maxConcurrent
	}

	// 构建 config：基础配置 + 请求覆盖
	workerCfg := h.cfg.ToFireworksConfig()
	if req.EmailProvider != "" {
		workerCfg["email_provider_priority"] = req.EmailProvider
	}

	// 确定代理
	proxy := h.cfg.GetDefaultProxy()
	if req.Proxy != "" {
		proxy = &common.ProxyEntry{HTTP: req.Proxy, HTTPS: req.Proxy}
	}

	// 设置 SSE 响应头
	c.Writer.Header().Set("Content-Type", "text/event-stream")
	c.Writer.Header().Set("Cache-Control", "no-cache")
	c.Writer.Header().Set("Connection", "keep-alive")
	c.Writer.Header().Set("X-Accel-Buffering", "no")
	c.Writer.WriteHeaderNow()

	// 创建 context，客户端断开时取消
	ctx, cancel := context.WithCancel(c.Request.Context())
	defer cancel()

	// 监听客户端断开
	go func() {
		<-c.Writer.CloseNotify()
		cancel()
	}()

	// 日志通道
	logCh := make(chan string, 100)

	// SSE 写入辅助函数
	writeSSE := func(event, data string) {
		fmt.Fprintf(c.Writer, "event: %s\ndata: %s\n\n", event, data)
		c.Writer.Flush()
	}

	// 启动注册 goroutine
	resultCh := make(chan *common.RegisterResult, count)
	go func() {
		defer close(logCh)
		defer close(resultCh)
		for i := 0; i < count; i++ {
			select {
			case <-ctx.Done():
				return
			default:
			}

			// 从代理池轮询
			taskProxy := proxy
			if pool := h.cfg.GetProxyPool(); len(pool) > 0 {
				taskProxy = pool[i%len(pool)]
			}

			opts := fireworks.RegisterOpts{
				Proxy:  taskProxy,
				Config: workerCfg,
				LogCh:  logCh,
			}
			result := fireworks.Register(ctx, opts)
			resultCh <- result
		}
	}()

	// 消费日志和结果，流式输出
	for {
		select {
		case msg, ok := <-logCh:
			if !ok {
				for result := range resultCh {
					data, _ := json.Marshal(result)
					writeSSE("result", string(data))
				}
				return
			}
			writeSSE("log", msg)

		case result, ok := <-resultCh:
			if !ok {
				for msg := range logCh {
					writeSSE("log", msg)
				}
				return
			}
			data, _ := json.Marshal(result)
			writeSSE("result", string(data))

		case <-ctx.Done():
			writeSSE("log", "任务已取消")
			return
		}
	}
}
