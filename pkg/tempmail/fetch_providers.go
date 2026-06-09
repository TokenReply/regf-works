package tempmail

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

var errFetchOnlyProvider = fmt.Errorf("provider 仅支持验证码读取，不支持创建邮箱")

// FetchVerificationCodeByProvider 按 provider 名称直接拉取验证码。
// 这条路径用于结果页"获取 OTP"按钮，不依赖 MultiProvider 的创建逻辑。
func FetchVerificationCodeByProvider(ctx context.Context, providerName string, cfg map[string]string, addr string, meta map[string]string) (string, error) {
	provider, err := newFetchProvider(providerName, cfg, meta)
	if err != nil {
		return "", err
	}
	return provider.FetchVerificationCode(ctx, addr, meta, 1, 0)
}

// NormalizeProviderNameForAPI 对外暴露 provider 规范化结果，供 handler 做轻量判断。
func NormalizeProviderNameForAPI(name string) string {
	return normalizeProviderName(name)
}

func newFetchProvider(name string, cfg map[string]string, meta map[string]string) (EmailProvider, error) {
	switch normalizeProviderName(name) {
	case "yydsmail":
		baseURL := strings.TrimRight(cfg["yydsmail_base_url"], "/")
		if baseURL == "" {
			baseURL = ""
		}
		return NewYYDSMailProvider(baseURL, cfg["yydsmail_api_key"]), nil
	case "mailtm":
		return &mailTMProvider{baseURL: "https://api.mail.tm", name: "mailtm"}, nil
	case "mailgw":
		return &mailTMProvider{baseURL: "https://api.mail.gw", name: "mailgw"}, nil
	case "guerrillamail":
		return &guerrillaMailProvider{baseURL: "https://api.guerrillamail.com/ajax.php"}, nil
	case "templol":
		return &tempMailLOLProvider{baseURL: "https://api.tempmail.lol"}, nil
	case "tempmailio":
		return &tempMailIOProvider{baseURL: "https://api.internal.temp-mail.io/api/v3"}, nil
	case "mailfree":
		baseURL := strings.TrimRight(meta["base_url"], "/")
		adminToken := meta["admin_token"]
		if baseURL == "" || adminToken == "" {
			return nil, fmt.Errorf("mailfree: 缺少 base_url 或 admin_token")
		}
		return &mailfreeProvider{
			baseURL:    baseURL,
			adminToken: adminToken,
		}, nil
	default:
		return nil, fmt.Errorf("未支持的邮箱 provider: %s", name)
	}
}

func normalizeProviderName(name string) string {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "", "yydsmail":
		return "yydsmail"
	case "mailtm", "mail.tm":
		return "mailtm"
	case "mailgw", "mail.gw":
		return "mailgw"
	case "guerrillamail", "guerrilla", "guerrilla_mail":
		return "guerrillamail"
	case "templol", "tempmail.lol", "tempmail_lol":
		return "templol"
	case "tempmailio", "temp-mail.io", "temp_mail_io":
		return "tempmailio"
	case "mailfree":
		return "mailfree"
	default:
		return strings.ToLower(strings.TrimSpace(name))
	}
}

type fetchOnlyProvider struct{}

func (fetchOnlyProvider) GenerateEmail(context.Context) (string, map[string]string, error) {
	return "", nil, errFetchOnlyProvider
}

func (fetchOnlyProvider) DeleteEmail(context.Context, string, map[string]string) error {
	return nil
}

type mailTMProvider struct {
	fetchOnlyProvider
	baseURL string
	name    string
}

func (p *mailTMProvider) Name() string { return p.name }

func (p *mailTMProvider) FetchVerificationCode(ctx context.Context, addr string, meta map[string]string, maxAttempts int, interval time.Duration) (string, error) {
	token := strings.TrimSpace(meta["token"])
	if token == "" {
		return "", fmt.Errorf("%s: 缺少 token", p.name)
	}

	client := &http.Client{Timeout: 10 * time.Second}
	for i := 0; i < maxAttempts; i++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.baseURL+"/messages", nil)
		if err != nil {
			return "", err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Accept", "application/json")

		resp, err := client.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return "", ctx.Err()
			}
			time.Sleep(interval)
			continue
		}

		var payload interface{}
		err = json.NewDecoder(resp.Body).Decode(&payload)
		resp.Body.Close()
		if err != nil || resp.StatusCode < 200 || resp.StatusCode >= 300 {
			time.Sleep(interval)
			continue
		}

		messages := sortMessageMapsNewestFirst(collectMailTMMessageMaps(payload))
		for _, msg := range messages {
			subject := stringValue(msg["subject"])
			if code := ExtractVerificationCode(subject, ""); code != "" {
				return code, nil
			}

			msgID := stringValue(msg["id"])
			if msgID == "" {
				continue
			}
			detailReq, err := http.NewRequestWithContext(ctx, http.MethodGet, p.baseURL+"/messages/"+url.PathEscape(msgID), nil)
			if err != nil {
				return "", err
			}
			detailReq.Header.Set("Authorization", "Bearer "+token)
			detailReq.Header.Set("Accept", "application/json")
			detailResp, err := client.Do(detailReq)
			if err != nil {
				continue
			}
			var detail map[string]interface{}
			err = json.NewDecoder(detailResp.Body).Decode(&detail)
			detailResp.Body.Close()
			if err != nil || detailResp.StatusCode < 200 || detailResp.StatusCode >= 300 {
				continue
			}
			for _, field := range []string{"text", "html", "intro"} {
				for _, text := range appendContentValue(nil, detail[field]) {
					if code := ExtractVerificationCode("", text); code != "" {
						return code, nil
					}
				}
			}
		}

		time.Sleep(interval)
	}

	return "", fmt.Errorf("%s 获取验证码超时", p.name)
}

type guerrillaMailProvider struct {
	fetchOnlyProvider
	baseURL string
}

func (p *guerrillaMailProvider) Name() string { return "guerrillamail" }

func (p *guerrillaMailProvider) FetchVerificationCode(ctx context.Context, addr string, meta map[string]string, maxAttempts int, interval time.Duration) (string, error) {
	sidToken := strings.TrimSpace(meta["sid_token"])
	if sidToken == "" {
		return "", fmt.Errorf("guerrillamail: 缺少 sid_token")
	}

	client := &http.Client{Timeout: 10 * time.Second}
	for i := 0; i < maxAttempts; i++ {
		apiURL := fmt.Sprintf("%s?f=check_email&seq=0&sid_token=%s", p.baseURL, url.QueryEscape(sidToken))
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
		if err != nil {
			return "", err
		}
		resp, err := client.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return "", ctx.Err()
			}
			time.Sleep(interval)
			continue
		}

		var payload struct {
			List []map[string]interface{} `json:"list"`
		}
		err = json.NewDecoder(resp.Body).Decode(&payload)
		resp.Body.Close()
		if err != nil || resp.StatusCode < 200 || resp.StatusCode >= 300 {
			time.Sleep(interval)
			continue
		}

		for _, msg := range sortMessageMapsNewestFirst(payload.List) {
			subject := stringValue(msg["mail_subject"])
			if strings.Contains(subject, "Welcome to Guerrilla Mail") {
				continue
			}
			body := stringValue(msg["mail_body"])
			if code := ExtractVerificationCode(subject, body); code != "" {
				return code, nil
			}
		}

		time.Sleep(interval)
	}

	return "", fmt.Errorf("guerrillamail 获取验证码超时")
}

type tempMailLOLProvider struct {
	fetchOnlyProvider
	baseURL string
}

func (p *tempMailLOLProvider) Name() string { return "templol" }

func (p *tempMailLOLProvider) FetchVerificationCode(ctx context.Context, addr string, meta map[string]string, maxAttempts int, interval time.Duration) (string, error) {
	token := strings.TrimSpace(meta["token"])
	if token == "" {
		return "", fmt.Errorf("tempmail.lol: 缺少 token")
	}

	client := &http.Client{Timeout: 10 * time.Second}
	for i := 0; i < maxAttempts; i++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.baseURL+"/auth/"+url.PathEscape(token), nil)
		if err != nil {
			return "", err
		}
		resp, err := client.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return "", ctx.Err()
			}
			time.Sleep(interval)
			continue
		}

		var payload struct {
			Email []map[string]interface{} `json:"email"`
		}
		err = json.NewDecoder(resp.Body).Decode(&payload)
		resp.Body.Close()
		if err != nil || resp.StatusCode < 200 || resp.StatusCode >= 300 {
			time.Sleep(interval)
			continue
		}

		for _, msg := range sortMessageMapsNewestFirst(payload.Email) {
			subject := stringValue(msg["subject"])
			body := firstNonEmptyString(msg["body"], msg["html"])
			if code := ExtractVerificationCode(subject, body); code != "" {
				return code, nil
			}
		}

		time.Sleep(interval)
	}

	return "", fmt.Errorf("tempmail.lol 获取验证码超时")
}

type tempMailIOProvider struct {
	fetchOnlyProvider
	baseURL string
}

func (p *tempMailIOProvider) Name() string { return "tempmailio" }

func (p *tempMailIOProvider) FetchVerificationCode(ctx context.Context, addr string, meta map[string]string, maxAttempts int, interval time.Duration) (string, error) {
	token := strings.TrimSpace(meta["token"])
	if token == "" {
		return "", fmt.Errorf("temp-mail.io: 缺少 token")
	}

	client := &http.Client{Timeout: 10 * time.Second}
	for i := 0; i < maxAttempts; i++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.baseURL+"/email/"+url.PathEscape(addr)+"/messages", nil)
		if err != nil {
			return "", err
		}
		req.Header.Set("Authorization", "Bearer "+token)

		resp, err := client.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return "", ctx.Err()
			}
			time.Sleep(interval)
			continue
		}

		var payload []map[string]interface{}
		err = json.NewDecoder(resp.Body).Decode(&payload)
		resp.Body.Close()
		if err != nil || resp.StatusCode < 200 || resp.StatusCode >= 300 {
			time.Sleep(interval)
			continue
		}

		for _, msg := range sortMessageMapsNewestFirst(payload) {
			subject := stringValue(msg["subject"])
			body := firstNonEmptyString(msg["body_text"], msg["body_html"])
			if code := ExtractVerificationCode(subject, body); code != "" {
				return code, nil
			}
		}

		time.Sleep(interval)
	}

	return "", fmt.Errorf("temp-mail.io 获取验证码超时")
}

type mailfreeProvider struct {
	fetchOnlyProvider
	baseURL    string
	adminToken string
}

func (p *mailfreeProvider) Name() string { return "mailfree" }

func (p *mailfreeProvider) FetchVerificationCode(ctx context.Context, addr string, meta map[string]string, maxAttempts int, interval time.Duration) (string, error) {
	client := &http.Client{Timeout: 10 * time.Second}
	for i := 0; i < maxAttempts; i++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.baseURL+"/api/emails?mailbox="+url.QueryEscape(addr), nil)
		if err != nil {
			return "", err
		}
		req.Header.Set("Authorization", "Bearer "+p.adminToken)

		resp, err := client.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return "", ctx.Err()
			}
			time.Sleep(interval)
			continue
		}

		var payload []map[string]interface{}
		err = json.NewDecoder(resp.Body).Decode(&payload)
		resp.Body.Close()
		if err != nil || resp.StatusCode < 200 || resp.StatusCode >= 300 {
			time.Sleep(interval)
			continue
		}

		for _, msg := range sortMessageMapsNewestFirst(payload) {
			if verificationCode := stringValue(msg["verification_code"]); verificationCode != "" {
				return verificationCode, nil
			}

			emailID := stringValue(msg["id"])
			if emailID != "" {
				detailReq, err := http.NewRequestWithContext(ctx, http.MethodGet, p.baseURL+"/api/email/"+url.PathEscape(emailID), nil)
				if err == nil {
					detailReq.Header.Set("Authorization", "Bearer "+p.adminToken)
					detailResp, err := client.Do(detailReq)
					if err == nil {
						var detail map[string]interface{}
						if json.NewDecoder(detailResp.Body).Decode(&detail) == nil && detailResp.StatusCode >= 200 && detailResp.StatusCode < 300 {
							for k, v := range detail {
								if _, exists := msg[k]; !exists || stringValue(msg[k]) == "" {
									msg[k] = v
								}
							}
						}
						detailResp.Body.Close()
					}
				}
			}

			for _, field := range []string{"subject", "content", "html_content", "text_content", "body", "text"} {
				if code := ExtractVerificationCode("", firstNonEmptyString(msg[field])); code != "" {
					return code, nil
				}
			}
		}

		time.Sleep(interval)
	}

	return "", fmt.Errorf("mailfree 获取验证码超时")
}

func collectMailTMMessageMaps(payload interface{}) []map[string]interface{} {
	switch v := payload.(type) {
	case map[string]interface{}:
		if members, ok := v["hydra:member"]; ok {
			return interfaceSliceToMapSlice(members)
		}
		if items, ok := v["messages"]; ok {
			return interfaceSliceToMapSlice(items)
		}
	case []interface{}:
		return interfaceSliceToMapSlice(v)
	}
	return nil
}

func interfaceSliceToMapSlice(value interface{}) []map[string]interface{} {
	items, ok := value.([]interface{})
	if !ok {
		return nil
	}
	result := make([]map[string]interface{}, 0, len(items))
	for _, item := range items {
		if m, ok := item.(map[string]interface{}); ok {
			result = append(result, m)
		}
	}
	return result
}

func sortMessageMapsNewestFirst(messages []map[string]interface{}) []map[string]interface{} {
	sorted := append([]map[string]interface{}(nil), messages...)
	sort.SliceStable(sorted, func(i, j int) bool {
		ti, okI := extractMapTimestamp(sorted[i])
		tj, okJ := extractMapTimestamp(sorted[j])
		if !(okI && okJ) {
			return false
		}
		if ti.Equal(tj) {
			return false
		}
		return ti.After(tj)
	})
	return sorted
}

func extractMapTimestamp(msg map[string]interface{}) (time.Time, bool) {
	for _, key := range []string{"received_at", "receivedAt", "created_at", "createdAt", "updated_at", "date", "timestamp"} {
		if value, ok := msg[key]; ok {
			switch v := value.(type) {
			case string:
				if ts, ok := parseTimestampString(v); ok {
					return ts, true
				}
			case json.Number:
				if num, err := v.Float64(); err == nil {
					if ts, ok := parseTimestampNumber(num); ok {
						return ts, true
					}
				}
			case float64:
				if ts, ok := parseTimestampNumber(v); ok {
					return ts, true
				}
			}
		}
	}
	return time.Time{}, false
}

func stringValue(value interface{}) string {
	switch v := value.(type) {
	case string:
		return strings.TrimSpace(v)
	case json.Number:
		return strings.TrimSpace(v.String())
	case float64:
		return strings.TrimSpace(fmt.Sprintf("%.0f", v))
	default:
		return ""
	}
}

func firstNonEmptyString(values ...interface{}) string {
	for _, value := range values {
		switch v := value.(type) {
		case string:
			if strings.TrimSpace(v) != "" {
				return v
			}
		case []string:
			for _, item := range v {
				if strings.TrimSpace(item) != "" {
					return item
				}
			}
		case []interface{}:
			for _, item := range v {
				if s := firstNonEmptyString(item); s != "" {
					return s
				}
			}
		}
	}
	return ""
}
