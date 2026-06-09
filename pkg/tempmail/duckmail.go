package tempmail

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// DuckMailProvider 基于 https://api.duckmail.sbs 的临时邮箱服务
//
// API（文档：https://raw.githubusercontent.com/MoonWeSif/DuckMail/main/public/llm-api-docs.txt）：
//
//	POST   /accounts         — 创建邮箱 { address, password } (需 dk_ API Key)
//	POST   /token            — 用 {address,password} 换 Bearer token
//	GET    /domains          — 可用域名（shared 或私有）
//	GET    /messages         — 列入站邮件（需 Bearer）
//	GET    /messages/{id}    — 详情，含 text/html 正文（需 Bearer）
//	DELETE /accounts/{id}    — 删邮箱（需 Bearer）
//
// meta 返回:
//
//	provider   = "duckmail"
//	token      = Bearer（用于 GET /messages 轮询）
//	inbox_id   = 邮箱 id，用于 DELETE
//	password   = 邮箱密码（换 token 用；Python 侧需要 fallback 换 token 时传）
//	domain     = 用的域名（便于排查）
type DuckMailProvider struct {
	baseURL       string
	apiKey        string
	defaultDomain string
	httpClient    *http.Client
	mu            sync.Mutex
	lastCall      time.Time
	minDelay      time.Duration
}

const defaultDuckMailBaseURL = "https://api.duckmail.sbs"
const defaultDuckMailDomain = "duckmail.sbs"

// NewDuckMailProvider 创建 DuckMail provider
func NewDuckMailProvider(baseURL, apiKey, defaultDomain string) *DuckMailProvider {
	if strings.TrimSpace(baseURL) == "" {
		baseURL = defaultDuckMailBaseURL
	}
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if strings.TrimSpace(defaultDomain) == "" {
		defaultDomain = defaultDuckMailDomain
	}
	return &DuckMailProvider{
		baseURL:       baseURL,
		apiKey:        strings.TrimSpace(apiKey),
		defaultDomain: strings.TrimSpace(defaultDomain),
		httpClient:    &http.Client{Timeout: 15 * time.Second},
		minDelay:      250 * time.Millisecond,
	}
}

func (p *DuckMailProvider) Name() string { return "duckmail" }

func (p *DuckMailProvider) throttle() {
	p.mu.Lock()
	defer p.mu.Unlock()
	elapsed := time.Since(p.lastCall)
	if elapsed < p.minDelay {
		time.Sleep(p.minDelay - elapsed)
	}
	p.lastCall = time.Now()
}

// doRequest 发送 HTTP 请求。authToken 非空则用 Bearer authToken；否则用 apiKey。
func (p *DuckMailProvider) doRequest(ctx context.Context, method, rawURL, authToken string,
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
		tok := authToken
		if tok == "" {
			tok = p.apiKey
		}
		if tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
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
			return nil, fmt.Errorf("%w: duckmail HTTP %d: %s", ErrAuthFailed, resp.StatusCode, truncateDuckErr(raw))
		}
		if resp.StatusCode == 429 {
			return nil, fmt.Errorf("%w: duckmail 速率限制", ErrRateLimited)
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			lastErr = fmt.Errorf("duckmail HTTP %d: %s", resp.StatusCode, truncateDuckErr(raw))
			continue
		}
		return raw, nil
	}
	return nil, fmt.Errorf("duckmail 请求失败 (重试 %d 次): %w", maxRetries, lastErr)
}

// GenerateEmail 创建一个随机前缀的 duckmail.sbs 邮箱，并换取 Bearer token
func (p *DuckMailProvider) GenerateEmail(ctx context.Context) (string, map[string]string, error) {
	// 生成随机前缀：前缀≥3 字符
	var buf [6]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", nil, fmt.Errorf("duckmail 随机前缀失败: %w", err)
	}
	prefix := "fw" + hex.EncodeToString(buf[:])
	address := prefix + "@" + p.defaultDomain

	// 生成一个邮箱密码（用于 POST /token 换 Bearer）
	var pbuf [9]byte
	if _, err := rand.Read(pbuf[:]); err != nil {
		return "", nil, fmt.Errorf("duckmail 随机密码失败: %w", err)
	}
	password := "Dk" + hex.EncodeToString(pbuf[:]) + "!A"

	// 1) POST /accounts — apiKey 认证
	createBody, _ := json.Marshal(map[string]string{
		"address":  address,
		"password": password,
	})
	raw, err := p.doRequest(ctx, "POST", p.baseURL+"/accounts", "", createBody, 3)
	if err != nil {
		return "", nil, fmt.Errorf("duckmail 创建邮箱失败: %w", err)
	}
	var createResp struct {
		ID      string `json:"id"`
		Address string `json:"address"`
	}
	if err := json.Unmarshal(raw, &createResp); err != nil {
		return "", nil, fmt.Errorf("duckmail 创建响应解析失败: %w raw=%s", err, truncateDuckErr(raw))
	}
	if createResp.Address == "" || createResp.ID == "" {
		return "", nil, fmt.Errorf("duckmail 创建返回缺字段: %s", truncateDuckErr(raw))
	}

	// 2) POST /token — 用 {address,password} 换 Bearer
	tokBody, _ := json.Marshal(map[string]string{
		"address":  createResp.Address,
		"password": password,
	})
	raw, err = p.doRequest(ctx, "POST", p.baseURL+"/token", "", tokBody, 3)
	if err != nil {
		return "", nil, fmt.Errorf("duckmail 换 token 失败: %w", err)
	}
	var tokResp struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(raw, &tokResp); err != nil {
		return "", nil, fmt.Errorf("duckmail token 响应解析失败: %w", err)
	}
	if tokResp.Token == "" {
		return "", nil, fmt.Errorf("duckmail token 为空: %s", truncateDuckErr(raw))
	}

	meta := map[string]string{
		"provider": "duckmail",
		"token":    tokResp.Token,
		"inbox_id": createResp.ID,
		"password": password,
		"domain":   p.defaultDomain,
		"base_url": p.baseURL,
	}
	return createResp.Address, meta, nil
}

// FetchVerificationCode 对 DuckMail 来说，fireworks 场景下 Python 侧用自定义 extractor 抓确认链接，
// 这里保留兜底实现——拉 /messages 最新一封，尝试从 subject/body 提取 6 位 OTP。
func (p *DuckMailProvider) FetchVerificationCode(ctx context.Context, addr string,
	meta map[string]string, maxAttempts int, interval time.Duration) (string, error) {
	token := meta["token"]
	if token == "" {
		return "", fmt.Errorf("duckmail: meta 缺 token")
	}
	listURL := p.baseURL + "/messages"

	for i := 0; i < maxAttempts; i++ {
		raw, err := p.doRequest(ctx, "GET", listURL, token, nil, 1)
		if err != nil {
			time.Sleep(interval)
			continue
		}
		var resp struct {
			Member []struct {
				ID      string `json:"id"`
				Subject string `json:"subject"`
			} `json:"hydra:member"`
		}
		if err := json.Unmarshal(raw, &resp); err != nil {
			time.Sleep(interval)
			continue
		}
		for _, msg := range resp.Member {
			if code := ExtractVerificationCode(msg.Subject, ""); code != "" {
				return code, nil
			}
			// 拉详情
			detailURL := fmt.Sprintf("%s/messages/%s", p.baseURL, msg.ID)
			detailRaw, err := p.doRequest(ctx, "GET", detailURL, token, nil, 1)
			if err != nil {
				continue
			}
			var detail map[string]interface{}
			if err := json.Unmarshal(detailRaw, &detail); err != nil {
				continue
			}
			for _, text := range collectDetailContents(detail) {
				if code := ExtractVerificationCode("", text); code != "" {
					return code, nil
				}
			}
		}
		time.Sleep(interval)
	}
	return "", fmt.Errorf("duckmail 获取验证码超时 (%d 次轮询)", maxAttempts)
}

// DeleteEmail 删邮箱（非关键）
func (p *DuckMailProvider) DeleteEmail(ctx context.Context, addr string, meta map[string]string) error {
	inboxID := meta["inbox_id"]
	token := meta["token"]
	if inboxID == "" || token == "" {
		return nil
	}
	deleteURL := fmt.Sprintf("%s/accounts/%s", p.baseURL, inboxID)
	_, err := p.doRequest(ctx, "DELETE", deleteURL, token, nil, 1)
	return err
}

func truncateDuckErr(raw []byte) string {
	s := string(raw)
	if len(s) > 200 {
		return s[:200] + "..."
	}
	return s
}
