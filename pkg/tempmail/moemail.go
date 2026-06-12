package tempmail

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"strings"
	"time"

	"github.com/rs/zerolog/log"
)

// MoeMailProvider 基于 MoeMail 自建邮箱服务
// API 文档: https://docs.moemail.app/api.html
//
// API 接口：
//   POST /api/emails/generate           → 创建临时邮箱
//   GET  /api/emails/{emailId}          → 获取邮件列表
//   GET  /api/emails/{emailId}/{messageId} → 获取单封邮件详情
//   DELETE /api/emails/{emailId}        → 删除邮箱
type MoeMailProvider struct {
	baseURL     string   // API 基础地址（自建服务地址）
	apiKey      string   // API Key（必需）
	domains     []string // 可用域名列表
	expiryTime  int64    // 邮箱有效期（毫秒），默认 3600000 (1小时)
	httpClient  *http.Client
}

// moeMailGenerateResp 创建邮箱响应
type moeMailGenerateResp struct {
	ID    string `json:"id"`
	Email string `json:"email"`
}

// moeMailEmailListResp 邮件列表响应
type moeMailEmailListResp struct {
	Messages []struct {
		ID           string `json:"id"`
		FromAddress  string `json:"from_address"`
		Subject      string `json:"subject"`
		ReceivedAt   int64  `json:"received_at"`
	} `json:"messages"`
	NextCursor string `json:"nextCursor"`
	Total      int    `json:"total"`
}

// moeMailMessageDetail 单封邮件详情响应
type moeMailMessageDetail struct {
	Message struct {
		ID          string `json:"id"`
		FromAddress string `json:"from_address"`
		Subject     string `json:"subject"`
		Content     string `json:"content"`
		HTML        string `json:"html"`
		ReceivedAt  int64  `json:"received_at"`
	} `json:"message"`
}

// moeMailConfigResp 系统配置响应
type moeMailConfigResp struct {
	EmailDomains string `json:"emailDomains"` // 逗号分隔的域名列表
	MaxEmails    string `json:"maxEmails"`
}

// NewMoeMailProvider 创建 MoeMail provider
// baseURL: 自建服务地址（如 https://moemail.example.com）
// apiKey: API Key（必需，从个人中心创建）
// domains: 逗号分隔的域名列表（留空则从服务端获取）
// expiryTime: 邮箱有效期（毫秒），默认 3600000 (1小时)
func NewMoeMailProvider(baseURL, apiKey, domains string, expiryTime int64) *MoeMailProvider {
	if baseURL == "" {
		log.Warn().Msg("moemail: 未配置 base_url，provider 将不可用")
		return nil
	}
	if apiKey == "" {
		log.Warn().Msg("moemail: 未配置 api_key，provider 将不可用")
		return nil
	}

	baseURL = strings.TrimRight(baseURL, "/")

	// 解析域名列表
	var domainList []string
	if domains != "" {
		for _, d := range strings.Split(domains, ",") {
			d = strings.TrimSpace(d)
			if d != "" {
				domainList = append(domainList, d)
			}
		}
	}

	// 默认邮箱有效期为 1 小时
	if expiryTime <= 0 {
		expiryTime = 3600000 // 1小时（毫秒）
	}

	p := &MoeMailProvider{
		baseURL:    baseURL,
		apiKey:     apiKey,
		domains:    domainList,
		expiryTime: expiryTime,
		httpClient: &http.Client{Timeout: 15 * time.Second},
	}

	// 如果没有配置域名，尝试从 API 获取
	if len(p.domains) == 0 {
		if fetched := p.fetchDomains(); len(fetched) > 0 {
			p.domains = fetched
			log.Info().Strs("domains", fetched).Msg("moemail: 已从服务端获取域名列表")
		}
	}

	return p
}

func (p *MoeMailProvider) Name() string { return "moemail" }

// GenerateEmail 创建临时邮箱
func (p *MoeMailProvider) GenerateEmail(ctx context.Context) (string, map[string]string, error) {
	if p.baseURL == "" || p.apiKey == "" {
		return "", nil, fmt.Errorf("moemail: 未配置 base_url 或 api_key")
	}

	// 如果域名列表为空，尝试获取
	if len(p.domains) == 0 {
		if fetched := p.fetchDomains(); len(fetched) > 0 {
			p.domains = fetched
		} else {
			return "", nil, fmt.Errorf("moemail: 无可用域名")
		}
	}

	// 随机选择域名
	domain := p.domains[rand.Intn(len(p.domains))]

	// 生成随机前缀（8-12位小写字母+数字）
	prefix := randomMoeMailPrefix(8 + rand.Intn(5))

	// 调用 API 创建邮箱
	genURL := p.baseURL + "/api/emails/generate"
	reqBody := map[string]interface{}{
		"name":       prefix,
		"expiryTime": p.expiryTime,
		"domain":     domain,
	}
	bodyBytes, _ := json.Marshal(reqBody)

	req, err := http.NewRequestWithContext(ctx, "POST", genURL, strings.NewReader(string(bodyBytes)))
	if err != nil {
		return "", nil, fmt.Errorf("moemail: 构建请求失败: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", p.apiKey)

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return "", nil, fmt.Errorf("moemail: 请求失败: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return "", nil, fmt.Errorf("moemail: HTTP %d: %s", resp.StatusCode, string(body))
	}

	var result moeMailGenerateResp
	if err := json.Unmarshal(body, &result); err != nil {
		return "", nil, fmt.Errorf("moemail: 解析响应失败: %w", err)
	}

	if result.ID == "" || result.Email == "" {
		return "", nil, fmt.Errorf("moemail: 创建邮箱失败，返回数据不完整")
	}

	meta := map[string]string{
		"provider":  "moemail",
		"email_id":  result.ID,
		"domain":    domain,
		"base_url":  p.baseURL,
		"api_key":   p.apiKey,
	}

	log.Debug().Str("email", result.Email).Str("email_id", result.ID).Msg("moemail: 邮箱已创建")
	return result.Email, meta, nil
}

// FetchVerificationCode 轮询获取验证码
func (p *MoeMailProvider) FetchVerificationCode(ctx context.Context, addr string, meta map[string]string, maxAttempts int, interval time.Duration) (string, error) {
	emailID := meta["email_id"]
	if emailID == "" {
		return "", fmt.Errorf("moemail: email_id 缺失")
	}

	apiKey := meta["api_key"]
	if apiKey == "" {
		apiKey = p.apiKey
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
		listURL := fmt.Sprintf("%s/api/emails/%s", baseURL, emailID)
		req, err := http.NewRequestWithContext(ctx, "GET", listURL, nil)
		if err != nil {
			return "", fmt.Errorf("moemail: 构建请求失败: %w", err)
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
			return "", fmt.Errorf("moemail: 请求失败: %w", err)
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
			return "", fmt.Errorf("moemail: HTTP %d", resp.StatusCode)
		}

		var listResp moeMailEmailListResp
		if err := json.Unmarshal(body, &listResp); err != nil || len(listResp.Messages) == 0 {
			if attempt < maxAttempts {
				select {
				case <-ctx.Done():
					return "", ctx.Err()
				case <-time.After(interval):
				}
				continue
			}
			return "", fmt.Errorf("moemail: 未收到邮件（%d 次轮询）", maxAttempts)
		}

		// 获取最新一封邮件详情
		latestID := listResp.Messages[0].ID
		detail, err := p.getMessageDetail(ctx, baseURL, apiKey, emailID, latestID)
		if err != nil {
			continue
		}

		// 从邮件正文中提取验证码
		code := ExtractVerificationCode(detail.Message.Subject, detail.Message.Content+" "+detail.Message.HTML)
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

	return "", fmt.Errorf("moemail: 验证码获取超时（%d 次轮询）", maxAttempts)
}

// DeleteEmail 删除邮箱
func (p *MoeMailProvider) DeleteEmail(ctx context.Context, addr string, meta map[string]string) error {
	emailID := meta["email_id"]
	if emailID == "" {
		return nil
	}

	apiKey := meta["api_key"]
	if apiKey == "" {
		apiKey = p.apiKey
	}

	baseURL := meta["base_url"]
	if baseURL == "" {
		baseURL = p.baseURL
	}

	deleteURL := fmt.Sprintf("%s/api/emails/%s", baseURL, emailID)
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

// getMessageDetail 获取单封邮件详情
func (p *MoeMailProvider) getMessageDetail(ctx context.Context, baseURL, apiKey, emailID, messageID string) (*moeMailMessageDetail, error) {
	detailURL := fmt.Sprintf("%s/api/emails/%s/%s", baseURL, emailID, messageID)
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

	var detail moeMailMessageDetail
	if err := json.NewDecoder(resp.Body).Decode(&detail); err != nil {
		return nil, err
	}

	return &detail, nil
}

// fetchDomains 从 /api/config 获取支持的域名列表
func (p *MoeMailProvider) fetchDomains() []string {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	configURL := p.baseURL + "/api/config"
	req, err := http.NewRequestWithContext(ctx, "GET", configURL, nil)
	if err != nil {
		return nil
	}
	req.Header.Set("X-API-Key", p.apiKey)

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil
	}

	var config moeMailConfigResp
	if err := json.NewDecoder(resp.Body).Decode(&config); err != nil {
		return nil
	}

	// 解析逗号分隔的域名列表
	if config.EmailDomains == "" {
		return nil
	}

	var domains []string
	for _, d := range strings.Split(config.EmailDomains, ",") {
		d = strings.TrimSpace(d)
		if d != "" {
			domains = append(domains, d)
		}
	}
	return domains
}

// randomMoeMailPrefix 生成随机邮箱前缀
func randomMoeMailPrefix(length int) string {
	const charset = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, length)
	for i := range b {
		b[i] = charset[rand.Intn(len(charset))]
	}
	return string(b)
}
