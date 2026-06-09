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

	"golang.org/x/time/rate"
)

// 包级 mail.tm 共享状态：每个 worker 注册时会 NewMultiProvider → NewMailTMProvider，
// 如果限流/域缓存放在 instance 上，N 个并发 worker 就会瞬间打 N×3 个请求触发 429。
// 这里把限流器和域缓存提到包级别，所有 instance 共享。
//
// mail.tm 免费层公开 8 req/s。我们留余量按 5 req/s + 突发 4 控（足够支撑 ~50 个/s 注册稳态）。
var (
	mailtmLimiter = rate.NewLimiter(rate.Every(200*time.Millisecond), 4)

	mailtmDomainsMu     sync.Mutex
	mailtmCachedDomains []string
	mailtmDomainsAt     time.Time
)

// MailTMProvider 基于 https://api.mail.tm 的免费临时邮箱
//
// API（无需 API Key）：
//
//	GET    /domains          — 列出当前活跃域名（mail.tm 经常只开 1 个）
//	POST   /accounts         — 创建邮箱 { address, password }，返回 {id,address}
//	POST   /token            — { address, password } -> { token }
//	GET    /messages         — 列入站邮件（Bearer）
//	GET    /messages/{id}    — 详情含 text/html 正文（Bearer）
//	DELETE /accounts/{id}    — 删邮箱（Bearer）
//
// 注意：mail.tm 的活跃域是它自家管辖的（如 deltajohnsons.com），
// DeepSeek 风控经常会把它整域拉黑——所以仅作为非 DeepSeek 平台备选。
type MailTMProvider struct {
	baseURL    string
	httpClient *http.Client
}

const defaultMailTMBaseURL = "https://api.mail.tm"

// NewMailTMProvider 创建 mail.tm provider
func NewMailTMProvider(baseURL string) *MailTMProvider {
	if strings.TrimSpace(baseURL) == "" {
		baseURL = defaultMailTMBaseURL
	}
	return &MailTMProvider{
		baseURL:    strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		httpClient: &http.Client{Timeout: 15 * time.Second},
	}
}

func (p *MailTMProvider) Name() string { return "mailtm" }

func (p *MailTMProvider) doRequest(ctx context.Context, method, rawURL, bearer string,
	body []byte, maxRetries int) ([]byte, error) {
	var lastErr error
	for i := 0; i < maxRetries; i++ {
		// 包级限流：所有并发 worker 共享，避免 N 个 instance 各自 throttle 把 mail.tm 打 429
		if err := mailtmLimiter.Wait(ctx); err != nil {
			return nil, fmt.Errorf("mail.tm 限流等待被取消: %w", err)
		}

		var reader io.Reader
		if body != nil {
			reader = bytes.NewReader(body)
		}
		req, err := http.NewRequestWithContext(ctx, method, rawURL, reader)
		if err != nil {
			return nil, err
		}
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
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
			return nil, fmt.Errorf("%w: mail.tm HTTP %d: %s", ErrAuthFailed, resp.StatusCode, truncateMailErr(raw))
		}
		// mail.tm 的 429 是每秒限流（8 req/s），不是日额耗尽。
		// 不向 MultiProvider 抛 ErrRateLimited（那会触发 6h 冷却把整个 provider 当天废掉），
		// 改为内部退避重试，让限流器自然吸收突发。
		if resp.StatusCode == 429 {
			lastErr = fmt.Errorf("mail.tm 429 transient")
			backoff := time.Duration(1<<i) * time.Second
			if backoff > 8*time.Second {
				backoff = 8 * time.Second
			}
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(backoff):
			}
			continue
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			lastErr = fmt.Errorf("mail.tm HTTP %d: %s", resp.StatusCode, truncateMailErr(raw))
			continue
		}
		return raw, nil
	}
	return nil, fmt.Errorf("mail.tm 请求失败 (重试 %d 次): %w", maxRetries, lastErr)
}

// listDomains 从 /domains 拉活跃域名，30 分钟包级缓存（所有 instance 共享）
func (p *MailTMProvider) listDomains(ctx context.Context) ([]string, error) {
	mailtmDomainsMu.Lock()
	defer mailtmDomainsMu.Unlock()
	if len(mailtmCachedDomains) > 0 && time.Since(mailtmDomainsAt) < 30*time.Minute {
		return mailtmCachedDomains, nil
	}
	raw, err := p.doRequest(ctx, "GET", p.baseURL+"/domains?page=1", "", nil, 3)
	if err != nil {
		return nil, err
	}
	// mail.tm 同时支持 Hydra 包裹（hydra:member）和裸数组
	var domains []string
	var arr []struct {
		Domain   string `json:"domain"`
		IsActive bool   `json:"isActive"`
	}
	if err := json.Unmarshal(raw, &arr); err != nil {
		var hydra struct {
			Member []struct {
				Domain   string `json:"domain"`
				IsActive bool   `json:"isActive"`
			} `json:"hydra:member"`
		}
		if err2 := json.Unmarshal(raw, &hydra); err2 != nil {
			return nil, fmt.Errorf("mail.tm /domains 解析失败: %w; %v; raw=%s", err, err2, truncateMailErr(raw))
		}
		arr = hydra.Member
	}
	for _, d := range arr {
		if d.IsActive && d.Domain != "" {
			domains = append(domains, d.Domain)
		}
	}
	if len(domains) == 0 {
		return nil, fmt.Errorf("mail.tm 当前没有活跃域名")
	}
	mailtmCachedDomains = domains
	mailtmDomainsAt = time.Now()
	return domains, nil
}

// GenerateEmail 创建一个 mail.tm 邮箱并换 Bearer
func (p *MailTMProvider) GenerateEmail(ctx context.Context) (string, map[string]string, error) {
	domains, err := p.listDomains(ctx)
	if err != nil {
		return "", nil, err
	}
	domain := domains[0] // 通常只有 1 个活跃域

	var ubuf [6]byte
	if _, err := rand.Read(ubuf[:]); err != nil {
		return "", nil, fmt.Errorf("mail.tm 随机前缀失败: %w", err)
	}
	prefix := "rp" + hex.EncodeToString(ubuf[:])
	address := prefix + "@" + domain

	var pbuf [9]byte
	if _, err := rand.Read(pbuf[:]); err != nil {
		return "", nil, fmt.Errorf("mail.tm 随机密码失败: %w", err)
	}
	password := "Mt" + hex.EncodeToString(pbuf[:]) + "!A"

	createBody, _ := json.Marshal(map[string]string{
		"address":  address,
		"password": password,
	})
	raw, err := p.doRequest(ctx, "POST", p.baseURL+"/accounts", "", createBody, 3)
	if err != nil {
		return "", nil, fmt.Errorf("mail.tm 创建邮箱失败: %w", err)
	}
	var createResp struct {
		ID      string `json:"id"`
		Address string `json:"address"`
	}
	if err := json.Unmarshal(raw, &createResp); err != nil {
		return "", nil, fmt.Errorf("mail.tm 创建响应解析失败: %w raw=%s", err, truncateMailErr(raw))
	}
	if createResp.Address == "" || createResp.ID == "" {
		return "", nil, fmt.Errorf("mail.tm 创建返回缺字段: %s", truncateMailErr(raw))
	}

	tokBody, _ := json.Marshal(map[string]string{
		"address":  createResp.Address,
		"password": password,
	})
	raw, err = p.doRequest(ctx, "POST", p.baseURL+"/token", "", tokBody, 3)
	if err != nil {
		return "", nil, fmt.Errorf("mail.tm 换 token 失败: %w", err)
	}
	var tokResp struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(raw, &tokResp); err != nil {
		return "", nil, fmt.Errorf("mail.tm token 响应解析失败: %w", err)
	}
	if tokResp.Token == "" {
		return "", nil, fmt.Errorf("mail.tm token 为空: %s", truncateMailErr(raw))
	}

	meta := map[string]string{
		"provider": "mailtm",
		"token":    tokResp.Token,
		"inbox_id": createResp.ID,
		"password": password,
		"domain":   domain,
		"base_url": p.baseURL,
	}
	return createResp.Address, meta, nil
}

// FetchVerificationCode 兜底实现：拉 /messages 找最新邮件，subject + body 提取 OTP
func (p *MailTMProvider) FetchVerificationCode(ctx context.Context, addr string,
	meta map[string]string, maxAttempts int, interval time.Duration) (string, error) {
	token := meta["token"]
	if token == "" {
		return "", fmt.Errorf("mail.tm: meta 缺 token")
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
			// fallback: 裸数组
			var arr []struct {
				ID      string `json:"id"`
				Subject string `json:"subject"`
			}
			if err2 := json.Unmarshal(raw, &arr); err2 != nil {
				time.Sleep(interval)
				continue
			}
			for _, m := range arr {
				resp.Member = append(resp.Member, struct {
					ID      string `json:"id"`
					Subject string `json:"subject"`
				}{ID: m.ID, Subject: m.Subject})
			}
		}
		for _, msg := range resp.Member {
			if code := ExtractVerificationCode(msg.Subject, ""); code != "" {
				return code, nil
			}
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
	return "", fmt.Errorf("mail.tm 获取验证码超时 (%d 次轮询)", maxAttempts)
}

// DeleteEmail 删邮箱（非关键）
func (p *MailTMProvider) DeleteEmail(ctx context.Context, addr string, meta map[string]string) error {
	inboxID := meta["inbox_id"]
	token := meta["token"]
	if inboxID == "" || token == "" {
		return nil
	}
	deleteURL := fmt.Sprintf("%s/accounts/%s", p.baseURL, inboxID)
	_, err := p.doRequest(ctx, "DELETE", deleteURL, token, nil, 1)
	return err
}

func truncateMailErr(raw []byte) string {
	s := string(raw)
	if len(s) > 200 {
		return s[:200] + "..."
	}
	return s
}
