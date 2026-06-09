package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/rs/zerolog/log"

	"github.com/grok-fireworks-reg/internal/common"
	"github.com/grok-fireworks-reg/internal/config"
	"github.com/grok-fireworks-reg/internal/grok"
)

// GrokRegisterRequest Grok 注册请求体
type GrokRegisterRequest struct {
	Proxy         string `json:"proxy,omitempty"`
	Count         int    `json:"count,omitempty"`
	EmailProvider string `json:"email_provider,omitempty"`
}

// GrokHandler Grok 注册处理器
type GrokHandler struct {
	cfg *config.Config
}

// NewGrokHandler 创建 GrokHandler
func NewGrokHandler(cfg *config.Config) *GrokHandler {
	return &GrokHandler{cfg: cfg}
}

// Register POST /api/grok/register
// SSE 流式返回日志和最终结果
func (h *GrokHandler) Register(c *gin.Context) {
	var req GrokRegisterRequest
	if err := c.ShouldBindJSON(&req); err != nil && err != io.EOF {
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("invalid request body: %s", err)})
		return
	}

	// 默认注册 1 个
	count := req.Count
	if count <= 0 {
		count = 1
	}

	// 构建 config：基础配置 + 请求覆盖
	workerCfg := h.cfg.ToGrokConfig()
	if req.EmailProvider != "" {
		workerCfg["email_provider_priority"] = req.EmailProvider
	}

	// 确定代理
	proxy := h.cfg.GetDefaultProxy()
	if req.Proxy != "" {
		proxy = &common.ProxyEntry{HTTP: req.Proxy, HTTPS: req.Proxy}
	}

	// 如果没有 action_id，先扫描获取
	if workerCfg["action_id"] == "" {
		log.Info().Msg("action_id 为空，执行 ScanConfig...")
		scanned, err := grok.ScanConfig(c.Request.Context(), proxy, workerCfg)
		if err != nil {
			log.Warn().Err(err).Msg("ScanConfig 失败")
		} else {
			for k, v := range scanned {
				if v != "" {
					workerCfg[k] = v
				}
			}
		}
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

			opts := grok.RegisterOpts{
				Proxy:  taskProxy,
				Config: workerCfg,
				LogCh:  logCh,
			}
			result := grok.Register(ctx, opts)
			resultCh <- result
		}
	}()

	// 消费日志和结果，流式输出
	for {
		select {
		case msg, ok := <-logCh:
			if !ok {
				// 日志通道关闭，输出剩余结果
				for result := range resultCh {
					data, _ := json.Marshal(result)
					writeSSE("result", string(data))
				}
				return
			}
			writeSSE("log", msg)

		case result, ok := <-resultCh:
			if !ok {
				// 结果通道关闭，输出剩余日志
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
