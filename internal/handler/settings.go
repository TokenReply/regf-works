package handler

import (
	"net/http"
	"sync"

	"github.com/gin-gonic/gin"
	"github.com/rs/zerolog/log"

	"github.com/grok-fireworks-reg/internal/config"
)

// SettingsHandler 系统设置处理器
// 提供前端接口查询/更新邮箱服务配置（运行时热更新，不需要重启服务）
type SettingsHandler struct {
	cfg *config.Config
	mu  sync.RWMutex
}

// NewSettingsHandler 创建 SettingsHandler
func NewSettingsHandler(cfg *config.Config) *SettingsHandler {
	return &SettingsHandler{cfg: cfg}
}

// MailSettings 邮箱配置（前端可读写）
type MailSettings struct {
	ProviderPriority string `json:"provider_priority"` // 如 "ahem,yydsmail,gptmail,moemail"
	YYDS             struct {
		BaseURL string `json:"base_url"`
		APIKey  string `json:"api_key"`
	} `json:"yydsmail"`
	Ahem struct {
		BaseURL string `json:"base_url"` // AHEM 服务地址（如 https://mail.example.com）
		Domains string `json:"domains"`  // 可用域名列表（逗号分隔，留空自动获取）
	} `json:"ahem"`
	GPTMail struct {
		BaseURL string `json:"base_url"` // GPTMail 服务地址（默认 https://mail.chatgpt.org.uk）
		APIKey  string `json:"api_key"`  // GPTMail API Key
	} `json:"gptmail"`
	MoeMail struct {
		BaseURL    string `json:"base_url"`    // MoeMail 自建服务地址
		APIKey     string `json:"api_key"`     // MoeMail API Key
		Domains    string `json:"domains"`     // 可用域名列表（逗号分隔，留空自动获取）
		ExpiryTime int64  `json:"expiry_time"` // 邮箱有效期（毫秒），默认 3600000 (1小时)
	} `json:"moemail"`
	DuckMail struct {
		BaseURL       string `json:"base_url"`       // 默认 https://api.duckmail.sbs
		APIKey        string `json:"api_key"`        // dk_ 开头的 API Key
		DefaultDomain string `json:"default_domain"` // 默认 duckmail.sbs
	} `json:"duckmail"`
}

// GetMailSettings GET /api/settings/mail — 获取当前邮箱配置
func (h *SettingsHandler) GetMailSettings(c *gin.Context) {
	h.mu.RLock()
	defer h.mu.RUnlock()

	resp := MailSettings{}
	resp.ProviderPriority = h.cfg.Mail.ProviderPriority
	resp.YYDS.BaseURL = h.cfg.Mail.YYDS.BaseURL
	resp.YYDS.APIKey = maskKey(h.cfg.Mail.YYDS.APIKey)
	resp.Ahem.BaseURL = h.cfg.Mail.Ahem.BaseURL
	resp.Ahem.Domains = h.cfg.Mail.Ahem.Domains
	resp.GPTMail.BaseURL = h.cfg.Mail.GPTMail.BaseURL
	resp.GPTMail.APIKey = maskKey(h.cfg.Mail.GPTMail.APIKey)
	resp.MoeMail.BaseURL = h.cfg.Mail.MoeMail.BaseURL
	resp.MoeMail.APIKey = maskKey(h.cfg.Mail.MoeMail.APIKey)
	resp.MoeMail.Domains = h.cfg.Mail.MoeMail.Domains
	resp.MoeMail.ExpiryTime = h.cfg.Mail.MoeMail.ExpiryTime
	resp.DuckMail.BaseURL = h.cfg.Mail.DuckMail.BaseURL
	resp.DuckMail.APIKey = maskKey(h.cfg.Mail.DuckMail.APIKey)
	resp.DuckMail.DefaultDomain = h.cfg.Mail.DuckMail.DefaultDomain

	c.JSON(http.StatusOK, resp)
}

// UpdateMailSettings POST /api/settings/mail — 更新邮箱配置（运行时生效）
func (h *SettingsHandler) UpdateMailSettings(c *gin.Context) {
	var req MailSettings
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	// 更新 provider 优先级
	if req.ProviderPriority != "" {
		h.cfg.Mail.ProviderPriority = req.ProviderPriority
	}

	// 更新 YYDS Mail 配置
	if req.YYDS.BaseURL != "" {
		h.cfg.Mail.YYDS.BaseURL = req.YYDS.BaseURL
	}
	if req.YYDS.APIKey != "" && req.YYDS.APIKey != "***" {
		h.cfg.Mail.YYDS.APIKey = req.YYDS.APIKey
	}

	// 更新 AHEM 配置
	if req.Ahem.BaseURL != "" {
		h.cfg.Mail.Ahem.BaseURL = req.Ahem.BaseURL
	}
	// domains 允许设为空（表示自动从 API 获取）
	h.cfg.Mail.Ahem.Domains = req.Ahem.Domains

	// 更新 GPTMail 配置
	if req.GPTMail.BaseURL != "" {
		h.cfg.Mail.GPTMail.BaseURL = req.GPTMail.BaseURL
	}
	if req.GPTMail.APIKey != "" && req.GPTMail.APIKey != "***" {
		h.cfg.Mail.GPTMail.APIKey = req.GPTMail.APIKey
	}

	// 更新 MoeMail 配置
	if req.MoeMail.BaseURL != "" {
		h.cfg.Mail.MoeMail.BaseURL = req.MoeMail.BaseURL
	}
	if req.MoeMail.APIKey != "" && req.MoeMail.APIKey != "***" {
		h.cfg.Mail.MoeMail.APIKey = req.MoeMail.APIKey
	}
	h.cfg.Mail.MoeMail.Domains = req.MoeMail.Domains
	if req.MoeMail.ExpiryTime > 0 {
		h.cfg.Mail.MoeMail.ExpiryTime = req.MoeMail.ExpiryTime
	}

	// 更新 DuckMail 配置
	if req.DuckMail.BaseURL != "" {
		h.cfg.Mail.DuckMail.BaseURL = req.DuckMail.BaseURL
	}
	if req.DuckMail.APIKey != "" && req.DuckMail.APIKey != "***" {
		h.cfg.Mail.DuckMail.APIKey = req.DuckMail.APIKey
	}
	if req.DuckMail.DefaultDomain != "" {
		h.cfg.Mail.DuckMail.DefaultDomain = req.DuckMail.DefaultDomain
	}

	c.JSON(http.StatusOK, gin.H{
		"ok":      true,
		"message": "邮箱配置已更新（运行时生效）",
	})

	// 持久化到 config.yaml
	if err := h.cfg.Save(); err != nil {
		log.Error().Err(err).Msg("持久化邮箱配置失败")
	}
}

// ProxySettings 代理配置
type ProxySettings struct {
	Default string   `json:"default"`
	Pool    []string `json:"pool"`
}

// GetProxySettings GET /api/settings/proxy — 获取代理配置
func (h *SettingsHandler) GetProxySettings(c *gin.Context) {
	h.mu.RLock()
	defer h.mu.RUnlock()

	c.JSON(http.StatusOK, ProxySettings{
		Default: h.cfg.Proxy.Default,
		Pool:    h.cfg.Proxy.Pool,
	})
}

// UpdateProxySettings POST /api/settings/proxy — 更新代理配置
func (h *SettingsHandler) UpdateProxySettings(c *gin.Context) {
	var req ProxySettings
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	h.cfg.Proxy.Default = req.Default
	if req.Pool != nil {
		h.cfg.Proxy.Pool = req.Pool
	}

	c.JSON(http.StatusOK, gin.H{
		"ok":      true,
		"message": "代理配置已更新",
	})

	// 持久化到 config.yaml
	if err := h.cfg.Save(); err != nil {
		log.Error().Err(err).Msg("持久化代理配置失败")
	}
}

// maskKey 隐藏 API key 中间部分
func maskKey(key string) string {
	if key == "" {
		return ""
	}
	if len(key) <= 8 {
		return "***"
	}
	return key[:4] + "***" + key[len(key)-4:]
}
