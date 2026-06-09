package tempmail

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// TempMailLolProvider 基于 https://api.tempmail.lol 的免费临时邮箱
//
// API（v2）：
//
//	POST   /v2/inbox/create  — 可选 {} 或 {community: bool, domain: ""}，返回 {address, token}
//	GET    /v2/inbox?token=… — 拉邮件，返回 {address, expires, emails:[{from,to,subject,body,html,date}]}
//
// 注意：tempmail.lol 在多个 SLD 下随机分配子域（accesswiki.net / tohal.org / for4u.net /
// shopsprint.org / 26ai.org / ...），DeepSeek 风控对每个 SLD 单独裁决，
// 实测大约 12% 的随机域能通过 DeepSeek 域名过滤。仅作为非 DeepSeek 平台的备选。
type TempMailLolProvider struct {
	baseURL    string
	apiKey     string
	httpClient *http.Client
	mu         sync.Mutex
	lastCall   time.Time
	minDelay   time.Duration
}

const defaultTempMailLolBaseURL = "https://api.tempmail.lol"

// NewTempMailLolProvider 创建 tempmail.lol provider。apiKey 可空（公开 API 也能用）。
func NewTempMailLolProvider(baseURL, apiKey string) *TempMailLolProvider {
	if strings.TrimSpace(baseURL) == "" {
		baseURL = defaultTempMailLolBaseURL
	}
	return &TempMailLolProvider{
		baseURL:    strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		apiKey:     strings.TrimSpace(apiKey),
		httpClient: &http.Client{Timeout: 15 * time.Second},
		minDelay:   250 * time.Millisecond,
	}
}

func (p *TempMailLolProvider) Name() string { return "tempmaillol" }

func (p *TempMailLolProvider) throttle() {
	p.mu.Lock()
	defer p.mu.Unlock()
	elapsed := time.Since(p.lastCall)
	if elapsed < p.minDelay {
		time.Sleep(p.minDelay - elapsed)
	}
	p.lastCall = time.Now()
}

func (p *TempMailLolProvider) doRequest(ctx context.Context, method, rawURL string,
	body []byte, maxRetries int) ([]byte, error) {
	var lastErr error
	for i := 0; i < maxRetries; i++ {
		p.throttle()

		var reader io.Reader
		if body != nil {
			reader = bytes.NewReader(body)
		}
		req, err := http.NewRequestWithContext(ctx, method, rawURL, reader)
		if err != nil {
			return nil, err
		}
		if p.apiKey != "" {
			req.Header.Set("Authorization", "Bearer "+p.apiKey)
		}
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		req.Header.Set("Accept", "application/json")
		req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; RegPlatform/1.0)")

		resp, err := p.httpClient.Do(req)
		if err != nil {
			lastErr = err
			time.Sleep(time.Duration(i+1) * 2 * time.Second)
			continue
		}
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if resp.StatusCode == 401 || resp.StatusCode == 403 {
			return nil, fmt.Errorf("%w: tempmail.lol HTTP %d: %s", ErrAuthFailed, resp.StatusCode, truncateTMLErr(raw))
		}
		if resp.StatusCode == 429 {
			return nil, fmt.Errorf("%w: tempmail.lol 速率限制", ErrRateLimited)
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			lastErr = fmt.Errorf("tempmail.lol HTTP %d: %s", resp.StatusCode, truncateTMLErr(raw))
			continue
		}
		return raw, nil
	}
	return nil, fmt.Errorf("tempmail.lol 请求失败 (重试 %d 次): %w", maxRetries, lastErr)
}

// GenerateEmail 创建一个 tempmail.lol 邮箱
func (p *TempMailLolProvider) GenerateEmail(ctx context.Context) (string, map[string]string, error) {
	createBody := []byte("{}")
	raw, err := p.doRequest(ctx, "POST", p.baseURL+"/v2/inbox/create", createBody, 3)
	if err != nil {
		return "", nil, fmt.Errorf("tempmail.lol 创建邮箱失败: %w", err)
	}
	var resp struct {
		Address string `json:"address"`
		Token   string `json:"token"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return "", nil, fmt.Errorf("tempmail.lol 创建响应解析失败: %w raw=%s", err, truncateTMLErr(raw))
	}
	if resp.Address == "" || resp.Token == "" {
		return "", nil, fmt.Errorf("tempmail.lol 创建返回缺字段: %s", truncateTMLErr(raw))
	}
	domain := ""
	if at := strings.LastIndex(resp.Address, "@"); at > 0 && at < len(resp.Address)-1 {
		domain = resp.Address[at+1:]
	}
	meta := map[string]string{
		"provider": "tempmaillol",
		"token":    resp.Token,
		"domain":   domain,
		"base_url": p.baseURL,
	}
	return resp.Address, meta, nil
}

// FetchVerificationCode 轮询 /auth/{token}（v1 接口，v2 token 也能用，且实测 /v2/inbox 经常超时）
func (p *TempMailLolProvider) FetchVerificationCode(ctx context.Context, addr string,
	meta map[string]string, maxAttempts int, interval time.Duration) (string, error) {
	token := meta["token"]
	if token == "" {
		return "", fmt.Errorf("tempmail.lol: meta 缺 token")
	}
	listURL := p.baseURL + "/auth/" + token

	for i := 0; i < maxAttempts; i++ {
		raw, err := p.doRequest(ctx, "GET", listURL, nil, 1)
		if err != nil {
			time.Sleep(interval)
			continue
		}
		var resp struct {
			Email []struct {
				From    string `json:"from"`
				Subject string `json:"subject"`
				Body    string `json:"body"`
				HTML    string `json:"html"`
			} `json:"email"`
		}
		if err := json.Unmarshal(raw, &resp); err != nil {
			time.Sleep(interval)
			continue
		}
		for _, m := range resp.Email {
			if code := ExtractVerificationCode(m.Subject, ""); code != "" {
				return code, nil
			}
			if code := ExtractVerificationCode("", m.Body); code != "" {
				return code, nil
			}
			if code := ExtractVerificationCode("", m.HTML); code != "" {
				return code, nil
			}
		}
		time.Sleep(interval)
	}
	return "", fmt.Errorf("tempmail.lol 获取验证码超时 (%d 次轮询)", maxAttempts)
}

// DeleteEmail tempmail.lol 没有删除接口，邮箱 10min 自动过期
func (p *TempMailLolProvider) DeleteEmail(ctx context.Context, addr string, meta map[string]string) error {
	return nil
}

func truncateTMLErr(raw []byte) string {
	s := string(raw)
	if len(s) > 200 {
		return s[:200] + "..."
	}
	return s
}
