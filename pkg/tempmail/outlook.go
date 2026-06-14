package tempmail

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// OutlookProvider 预购 @outlook.com 账号池 provider。
//
// 账号文件每行：email----password----client_id----refresh_token
//（后三段顺序自适应：UUID=client_id，M. 开头=refresh_token，其余=password）。
// GenerateEmail 从池中领取一个未使用账号（used 文件去重，append-only）。
// 收件统一交给 Python outlook_mail 服务（IMAP XOAUTH2，:5003 /outlook/poll），
// 避免在 Go 侧重复实现 IMAP；Go 仅在 grok 这类 Go-native 平台 FetchVerificationCode 时用到。
type OutlookProvider struct {
	accountsFile string
	usedFile     string
	pollURL      string
	mode         string // code / novita_token / link
	httpClient   *http.Client
}

const (
	defaultOutlookAccountsFile = "data/outlook_accounts.txt"
	defaultOutlookUsedFile     = "data/outlook_used.txt"
	defaultOutlookPollURL      = "http://127.0.0.1:5003/outlook/poll"
)

// outlookPoolMu 跨 provider 实例串行化账号领取（MultiProvider 每任务新建实例）。
var outlookPoolMu sync.Mutex

type outlookAccount struct {
	Email        string
	Password     string
	ClientID     string
	RefreshToken string
}

// NewOutlookProvider 创建 Outlook 账号池 provider。空参用默认值。
func NewOutlookProvider(accountsFile, usedFile, pollURL, mode string) *OutlookProvider {
	if strings.TrimSpace(accountsFile) == "" {
		accountsFile = defaultOutlookAccountsFile
	}
	if strings.TrimSpace(usedFile) == "" {
		usedFile = defaultOutlookUsedFile
	}
	if strings.TrimSpace(pollURL) == "" {
		pollURL = defaultOutlookPollURL
	}
	if strings.TrimSpace(mode) == "" {
		mode = "code"
	}
	return &OutlookProvider{
		accountsFile: accountsFile,
		usedFile:     usedFile,
		pollURL:      pollURL,
		mode:         mode,
		httpClient:   &http.Client{Timeout: 200 * time.Second},
	}
}

func (p *OutlookProvider) Name() string { return "outlook" }

// looksLikeUUID 粗判 client_id
func looksLikeUUID(s string) bool {
	return len(s) >= 32 && strings.Count(s, "-") >= 4
}

// parseOutlookLine 解析一行账号，后三段顺序自适应。
func parseOutlookLine(line string) (outlookAccount, bool) {
	parts := strings.Split(strings.TrimSpace(line), "----")
	if len(parts) != 4 {
		return outlookAccount{}, false
	}
	acc := outlookAccount{Email: strings.TrimSpace(parts[0])}
	rest := []string{strings.TrimSpace(parts[1]), strings.TrimSpace(parts[2]), strings.TrimSpace(parts[3])}
	for _, v := range rest {
		switch {
		case looksLikeUUID(v) && acc.ClientID == "":
			acc.ClientID = v
		case strings.HasPrefix(v, "M.") && acc.RefreshToken == "":
			acc.RefreshToken = v
		default:
			if acc.Password == "" {
				acc.Password = v
			}
		}
	}
	// 兜底：若自适应没识别全，退回固定顺序 email----password----client_id----refresh_token
	if acc.ClientID == "" || acc.RefreshToken == "" {
		acc.Password = rest[0]
		acc.ClientID = rest[1]
		acc.RefreshToken = rest[2]
	}
	if acc.Email == "" || acc.ClientID == "" || acc.RefreshToken == "" {
		return outlookAccount{}, false
	}
	return acc, true
}

func (p *OutlookProvider) loadUsed() map[string]struct{} {
	used := make(map[string]struct{})
	data, err := os.ReadFile(p.usedFile)
	if err != nil {
		return used
	}
	for _, ln := range strings.Split(string(data), "\n") {
		ln = strings.TrimSpace(ln)
		if ln != "" {
			used[strings.ToLower(ln)] = struct{}{}
		}
	}
	return used
}

func (p *OutlookProvider) markUsed(email string) error {
	f, err := os.OpenFile(p.usedFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(email + "\n")
	return err
}

// claim 领取一个未使用账号（串行）。
func (p *OutlookProvider) claim() (outlookAccount, error) {
	outlookPoolMu.Lock()
	defer outlookPoolMu.Unlock()

	data, err := os.ReadFile(p.accountsFile)
	if err != nil {
		return outlookAccount{}, fmt.Errorf("读取 outlook 账号池失败 (%s): %w", p.accountsFile, err)
	}
	used := p.loadUsed()
	for _, ln := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(ln) == "" {
			continue
		}
		acc, ok := parseOutlookLine(ln)
		if !ok {
			continue
		}
		if _, seen := used[strings.ToLower(acc.Email)]; seen {
			continue
		}
		if err := p.markUsed(acc.Email); err != nil {
			return outlookAccount{}, fmt.Errorf("标记 outlook 账号已用失败: %w", err)
		}
		return acc, nil
	}
	return outlookAccount{}, fmt.Errorf("outlook 账号池无可用账号（已用尽）")
}

// GenerateEmail 领取一个 Outlook 账号。
func (p *OutlookProvider) GenerateEmail(ctx context.Context) (string, map[string]string, error) {
	acc, err := p.claim()
	if err != nil {
		return "", nil, err
	}
	meta := map[string]string{
		"provider":      "outlook",
		"email":         acc.Email,
		"password":      acc.Password,
		"client_id":     acc.ClientID,
		"refresh_token": acc.RefreshToken,
		"pool_id":       acc.Email,
	}
	return acc.Email, meta, nil
}

// FetchVerificationCode 调 Python outlook_mail 服务（IMAP XOAUTH2）取验证码。
// grok 等 Go-native 平台用；maxAttempts/interval 在此忽略（Python 端按内部 timeout 轮询）。
func (p *OutlookProvider) FetchVerificationCode(ctx context.Context, addr string,
	meta map[string]string, maxAttempts int, interval time.Duration) (string, error) {
	reqBody, _ := json.Marshal(map[string]interface{}{
		"email":         addr,
		"client_id":     meta["client_id"],
		"refresh_token": meta["refresh_token"],
		"mode":          p.mode,
		"timeout":       170,
	})
	req, err := http.NewRequestWithContext(ctx, "POST", p.pollURL, bytes.NewReader(reqBody))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := p.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("outlook 收件服务调用失败: %w", err)
	}
	defer resp.Body.Close()
	var out struct {
		OK    bool   `json:"ok"`
		Value string `json:"value"`
		Error string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("outlook 收件响应解析失败: %w", err)
	}
	if !out.OK || out.Value == "" {
		return "", fmt.Errorf("outlook 未取到验证码: %s", out.Error)
	}
	return out.Value, nil
}

// DeleteEmail 注册失败时回收账号（从 used 文件移除，可被再次领取）。
func (p *OutlookProvider) DeleteEmail(ctx context.Context, addr string, meta map[string]string) error {
	outlookPoolMu.Lock()
	defer outlookPoolMu.Unlock()
	data, err := os.ReadFile(p.usedFile)
	if err != nil {
		return nil
	}
	var kept []string
	target := strings.ToLower(strings.TrimSpace(addr))
	for _, ln := range strings.Split(string(data), "\n") {
		t := strings.TrimSpace(ln)
		if t == "" || strings.ToLower(t) == target {
			continue
		}
		kept = append(kept, t)
	}
	out := strings.Join(kept, "\n")
	if out != "" {
		out += "\n"
	}
	return os.WriteFile(p.usedFile, []byte(out), 0600)
}
