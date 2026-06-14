package tempmail

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"time"

	"github.com/rs/zerolog/log"
)

// GPTMailProvider 基于 GPTMail (mail.chatgpt.org.uk) 的临时邮箱服务
// API 文档: https://mail.chatgpt.org.uk/zh/api
//
// API 接口：
//   GET/POST /api/generate-email        → 生成临时邮箱
//   GET  /api/emails?email=...           → 获取邮件列表
//   GET  /api/email/{id}                 → 获取单封邮件详情
//   DELETE /api/email/{id}               → 删除单封邮件
//   DELETE /api/emails/clear?email=...   → 清空邮箱
type GPTMailProvider struct {
	baseURL    string // API 基础地址（默认 https://mail.chatgpt.org.uk）
	apiKey     string // API Key（必需）
	httpClient *http.Client
}

// gptMailGenerateResp 生成邮箱响应
type gptMailGenerateResp struct {
	Success bool `json:"success"`
	Data    struct {
		Email  string `json:"email"`
		Domain string `json:"domain"`
		Prefix string `json:"prefix"`
	} `json:"data"`
	Error string `json:"error,omitempty"`
}

// gptMailEmailsResp 邮件列表响应
type gptMailEmailsResp struct {
	Success bool `json:"success"`
	Data    struct {
		Emails []struct {
			ID           string `json:"id"`
			EmailAddress string `json:"email_address"`
			FromAddress  string `json:"from_address"`
			Subject      string `json:"subject"`
			Timestamp    int64  `json:"timestamp"`
		} `json:"emails"`
		Count int `json:"count"`
	} `json:"data"`
	Error string `json:"error,omitempty"`
}

// gptMailEmailDetail 邮件详情响应
type gptMailEmailDetail struct {
	Success bool `json:"success"`
	Data    struct {
		ID           string `json:"id"`
		EmailAddress string `json:"email_address"`
		FromAddress  string `json:"from_address"`
		Subject      string `json:"subject"`
		Content      string `json:"content"`
		HTMLContent  string `json:"html_content"`
		HasHTML      bool   `json:"has_html"`
		Timestamp    int64  `json:"timestamp"`
	} `json:"data"`
	Error string `json:"error,omitempty"`
}

// NewGPTMailProvider 创建 GPTMail provider
// baseURL: API 地址（默认 https://mail.chatgpt.org.uk）
// apiKey: API Key（必需，可从 https://shop.chatgpt.org.uk 获取）
func NewGPTMailProvider(baseURL, apiKey string) *GPTMailProvider {
	if baseURL == "" {
		baseURL = "https://mail.chatgpt.org.uk"
	}
	baseURL = strings.TrimRight(baseURL, "/")

	return &GPTMailProvider{
		baseURL:    baseURL,
		apiKey:     apiKey,
		httpClient: &http.Client{Timeout: 15 * time.Second},
	}
}

func (p *GPTMailProvider) Name() string { return "gptmail" }

// GenerateEmail 生成随机邮箱地址
func (p *GPTMailProvider) GenerateEmail(ctx context.Context) (string, map[string]string, error) {
	if p.apiKey == "" {
		return "", nil, fmt.Errorf("gptmail: 未配置 API Key (gptmail_api_key)")
	}

	// 调用 GET /api/generate-email 随机生成
	genURL := p.baseURL + "/api/generate-email"
	req, err := http.NewRequestWithContext(ctx, "GET", genURL, nil)
	if err != nil {
		return "", nil, fmt.Errorf("gptmail: 构建请求失败: %w", err)
	}
	req.Header.Set("X-API-Key", p.apiKey)

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return "", nil, fmt.Errorf("gptmail: 请求失败: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return "", nil, fmt.Errorf("gptmail: HTTP %d: %s", resp.StatusCode, string(body))
	}

	var result gptMailGenerateResp
	if err := json.Unmarshal(body, &result); err != nil {
		return "", nil, fmt.Errorf("gptmail: 解析响应失败: %w", err)
	}

	if !result.Success || result.Data.Email == "" {
		return "", nil, fmt.Errorf("gptmail: 生成邮箱失败: %s", result.Error)
	}

	meta := map[string]string{
		"provider": "gptmail",
		"prefix":   result.Data.Prefix,
		"domain":   result.Data.Domain,
		"base_url": p.baseURL,
		"api_key":  p.apiKey,
	}

	log.Debug().Str("email", result.Data.Email).Msg("gptmail: 邮箱已生成")
	return result.Data.Email, meta, nil
}

// FetchVerificationCode 轮询获取验证码
func (p *GPTMailProvider) FetchVerificationCode(ctx context.Context, addr string, meta map[string]string, maxAttempts int, interval time.Duration) (string, error) {
	apiKey := meta["api_key"]
	if apiKey == "" {
		apiKey = p.apiKey
	}
	if apiKey == "" {
		return "", fmt.Errorf("gptmail: 未配置 API Key")
	}

	baseURL := meta["base_url"]
	if baseURL == "" {
		baseURL = p.baseURL
	}

	if interval <= 0 {
		interval = 3 * time.Second
	}
	if maxAttempts <= 0 {
		maxAttempts = 30
	}

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		default:
		}

		// 获取邮件列表
		listURL := fmt.Sprintf("%s/api/emails?email=%s", baseURL, addr)
		req, err := http.NewRequestWithContext(ctx, "GET", listURL, nil)
		if err != nil {
			return "", fmt.Errorf("gptmail: 构建请求失败: %w", err)
		}
		req.Header.Set("X-API-Key", apiKey)

		resp, err := p.httpClient.Do(req)
		if err != nil {
			if attempt < maxAttempts {
				select {
				case <-ctx.Done():
					return "", ctx.Err()
				case <-time.After(interval):
				}
				continue
			}
			return "", fmt.Errorf("gptmail: 请求失败: %w", err)
		}

		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if resp.StatusCode != 200 {
			if attempt < maxAttempts {
				select {
				case <-ctx.Done():
					return "", ctx.Err()
				case <-time.After(interval):
				}
				continue
			}
			return "", fmt.Errorf("gptmail: HTTP %d", resp.StatusCode)
		}

		var listResp gptMailEmailsResp
		if err := json.Unmarshal(body, &listResp); err != nil || !listResp.Success || len(listResp.Data.Emails) == 0 {
			if attempt < maxAttempts {
				select {
				case <-ctx.Done():
					return "", ctx.Err()
				case <-time.After(interval):
				}
				continue
			}
			return "", fmt.Errorf("gptmail: 未收到邮件（%d 次轮询）", maxAttempts)
		}

		// 获取最新一封邮件详情
		latestID := listResp.Data.Emails[0].ID
		detail, err := p.getEmailDetail(ctx, baseURL, apiKey, latestID)
		if err != nil {
			continue
		}

		// 从邮件正文中提取验证码
		code := ExtractVerificationCode(detail.Data.Subject, detail.Data.Content+" "+detail.Data.HTMLContent)
		if code != "" {
			return code, nil
		}

		// 没提取到验证码，继续等待下一封
		if attempt < maxAttempts {
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(interval):
			}
		}
	}

	return "", fmt.Errorf("gptmail: 验证码获取超时（%d 次轮询）", maxAttempts)
}

// DeleteEmail 清空邮箱
func (p *GPTMailProvider) DeleteEmail(ctx context.Context, addr string, meta map[string]string) error {
	apiKey := meta["api_key"]
	if apiKey == "" {
		apiKey = p.apiKey
	}

	baseURL := meta["base_url"]
	if baseURL == "" {
		baseURL = p.baseURL
	}

	deleteURL := fmt.Sprintf("%s/api/emails/clear?email=%s", baseURL, addr)
	req, err := http.NewRequestWithContext(ctx, "DELETE", deleteURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("X-API-Key", apiKey)

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// ─── 内部方法 ───

// getEmailDetail 获取单封邮件详情
func (p *GPTMailProvider) getEmailDetail(ctx context.Context, baseURL, apiKey, emailID string) (*gptMailEmailDetail, error) {
	detailURL := fmt.Sprintf("%s/api/email/%s", baseURL, emailID)
	req, err := http.NewRequestWithContext(ctx, "GET", detailURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-API-Key", apiKey)

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	var detail gptMailEmailDetail
	if err := json.NewDecoder(resp.Body).Decode(&detail); err != nil {
		return nil, err
	}

	if !detail.Success {
		return nil, fmt.Errorf("gptmail: %s", detail.Error)
	}

	return &detail, nil
}

// randomGPTMailPrefix 生成随机邮箱前缀
func randomGPTMailPrefix(length int) string {
	const charset = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, length)
	for i := range b {
		n, _ := rand.Int(rand.Reader, big.NewInt(int64(len(charset))))
		b[i] = charset[n.Int64()]
	}
	return string(b)
}
