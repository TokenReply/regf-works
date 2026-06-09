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

// AhemProvider 基于 AHEM (Ad-Hoc Email Server) 的临时邮箱服务
// 特点：无需认证、无需创建邮箱、任意前缀@支持域名即可收信
// API 文档: https://github.com/o4oren/Ad-Hoc-Email-Server
//
// API 接口：
//   GET  /api/properties                       → 获取支持域名列表
//   GET  /api/mailbox/{prefix}/email           → 邮件列表
//   GET  /api/mailbox/{prefix}/email/{emailId} → 邮件详情
//   DELETE /api/mailbox/{prefix}/email/{emailId} → 删除邮件
//   DELETE /api/mailbox/{prefix}               → 删除整个邮箱
type AhemProvider struct {
	baseURL    string   // API 基础地址（如 https://mail.example.com/api）
	domains    []string // 用户配置的可用域名列表
	httpClient *http.Client
}

// ahemEmailListItem 邮件列表项
type ahemEmailListItem struct {
	EmailID   string `json:"emailId"`
	Subject   string `json:"subject"`
	Timestamp int64  `json:"timestamp"`
	Sender    struct {
		Address string `json:"address"`
		Name    string `json:"name"`
	} `json:"sender"`
}

// ahemEmailDetail 邮件详情
type ahemEmailDetail struct {
	ID        string `json:"_id"`
	Subject   string `json:"subject"`
	Text      string `json:"text"`
	HTML      any    `json:"html"` // string 或 false
	Timestamp int64  `json:"timestamp"`
	From      struct {
		Text string `json:"text"`
	} `json:"from"`
}

// NewAhemProvider 创建 AHEM 邮箱 provider
// baseURL: API 地址（如 https://mail.example.com/api 或 https://mail.example.com）
// domains: 逗号分隔的可用域名列表（如 "example.com,mail.example.com"）
func NewAhemProvider(baseURL, domains string) *AhemProvider {
	normalizedURL := normalizeAhemBaseURL(baseURL)

	// 解析域名列表
	var domainList []string
	for _, d := range strings.Split(domains, ",") {
		d = strings.TrimSpace(d)
		if d != "" {
			domainList = append(domainList, d)
		}
	}

	p := &AhemProvider{
		baseURL:    normalizedURL,
		domains:    domainList,
		httpClient: &http.Client{Timeout: 15 * time.Second},
	}

	// 如果没有配置域名，尝试从 API 获取
	if len(p.domains) == 0 && normalizedURL != "" {
		if fetched := p.fetchDomains(); len(fetched) > 0 {
			p.domains = fetched
		}
	}

	return p
}

func (p *AhemProvider) Name() string { return "ahem" }

// GenerateEmail 生成随机邮箱地址（AHEM 无需预先创建）
func (p *AhemProvider) GenerateEmail(ctx context.Context) (string, map[string]string, error) {
	if p.baseURL == "" {
		return "", nil, fmt.Errorf("ahem: 未配置 API 地址 (ahem_base_url)")
	}
	if len(p.domains) == 0 {
		return "", nil, fmt.Errorf("ahem: 未配置可用域名 (ahem_domains)")
	}

	// 随机选择域名
	domainIdx, _ := rand.Int(rand.Reader, big.NewInt(int64(len(p.domains))))
	domain := p.domains[domainIdx.Int64()]

	// 生成随机前缀（10位小写字母）
	prefix := randomAhemPrefix(10)
	addr := prefix + "@" + domain

	meta := map[string]string{
		"provider": "ahem",
		"prefix":   prefix,
		"domain":   domain,
		"base_url": p.baseURL,
	}

	log.Debug().Str("email", addr).Str("api", p.baseURL).Msg("ahem: 邮箱已生成（无需创建）")
	return addr, meta, nil
}

// FetchVerificationCode 轮询 AHEM 邮箱获取验证码
func (p *AhemProvider) FetchVerificationCode(ctx context.Context, addr string, meta map[string]string, maxAttempts int, interval time.Duration) (string, error) {
	prefix := meta["prefix"]
	if prefix == "" {
		// 从地址中提取
		parts := strings.SplitN(addr, "@", 2)
		if len(parts) != 2 {
			return "", fmt.Errorf("ahem: 无效的邮箱地址: %s", addr)
		}
		prefix = parts[0]
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
		listURL := fmt.Sprintf("%s/mailbox/%s/email", baseURL, prefix)
		req, err := http.NewRequestWithContext(ctx, "GET", listURL, nil)
		if err != nil {
			return "", fmt.Errorf("ahem: 构建请求失败: %w", err)
		}

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
			return "", fmt.Errorf("ahem: 请求失败: %w", err)
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
			return "", fmt.Errorf("ahem: HTTP %d", resp.StatusCode)
		}

		var emails []ahemEmailListItem
		if err := json.Unmarshal(body, &emails); err != nil || len(emails) == 0 {
			if attempt < maxAttempts {
				select {
				case <-ctx.Done():
					return "", ctx.Err()
				case <-time.After(interval):
				}
				continue
			}
			return "", fmt.Errorf("ahem: 未收到邮件（%d 次轮询）", maxAttempts)
		}

		// 获取最新一封邮件详情
		latestID := emails[0].EmailID
		detail, err := p.getEmailDetail(ctx, baseURL, prefix, latestID)
		if err != nil {
			continue
		}

		// 从邮件正文中提取验证码
		mailText := detail.Text
		if mailText == "" {
			if htmlStr, ok := detail.HTML.(string); ok {
				mailText = htmlStr
			}
		}

		// 合并 subject + text 用于提取
		code := ExtractVerificationCode(detail.Subject, mailText)
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

	return "", fmt.Errorf("ahem: 验证码获取超时（%d 次轮询）", maxAttempts)
}

// DeleteEmail 删除整个邮箱
func (p *AhemProvider) DeleteEmail(ctx context.Context, addr string, meta map[string]string) error {
	prefix := meta["prefix"]
	if prefix == "" {
		parts := strings.SplitN(addr, "@", 2)
		if len(parts) != 2 {
			return nil
		}
		prefix = parts[0]
	}

	baseURL := meta["base_url"]
	if baseURL == "" {
		baseURL = p.baseURL
	}

	deleteURL := fmt.Sprintf("%s/mailbox/%s", baseURL, prefix)
	req, err := http.NewRequestWithContext(ctx, "DELETE", deleteURL, nil)
	if err != nil {
		return err
	}
	resp, err := p.httpClient.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// ─── 内部方法 ───

// getEmailDetail 获取单封邮件详情
func (p *AhemProvider) getEmailDetail(ctx context.Context, baseURL, prefix, emailID string) (*ahemEmailDetail, error) {
	detailURL := fmt.Sprintf("%s/mailbox/%s/email/%s", baseURL, prefix, emailID)
	req, err := http.NewRequestWithContext(ctx, "GET", detailURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := p.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	var detail ahemEmailDetail
	if err := json.NewDecoder(resp.Body).Decode(&detail); err != nil {
		return nil, err
	}
	return &detail, nil
}

// fetchDomains 从 /api/properties 获取支持的域名列表
func (p *AhemProvider) fetchDomains() []string {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	propURL := p.baseURL + "/properties"
	req, err := http.NewRequestWithContext(ctx, "GET", propURL, nil)
	if err != nil {
		return nil
	}
	resp, err := p.httpClient.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()

	var props struct {
		AllowedDomains []string `json:"allowedDomains"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&props); err != nil {
		return nil
	}
	return props.AllowedDomains
}

// normalizeAhemBaseURL 统一 AHEM API 基址
// 支持输入: https://mail.example.com 或 https://mail.example.com/api
func normalizeAhemBaseURL(raw string) string {
	baseURL := strings.TrimRight(strings.TrimSpace(raw), "/")
	if baseURL == "" {
		return ""
	}
	// 确保以 /api 结尾
	if !strings.HasSuffix(baseURL, "/api") {
		baseURL += "/api"
	}
	return baseURL
}

// randomAhemPrefix 生成随机邮箱前缀
func randomAhemPrefix(length int) string {
	const charset = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, length)
	for i := range b {
		n, _ := rand.Int(rand.Reader, big.NewInt(int64(len(charset))))
		b[i] = charset[n.Int64()]
	}
	return string(b)
}
