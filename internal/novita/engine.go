package novita

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/rs/zerolog/log"

	"github.com/grok-fireworks-reg/internal/common"
	"github.com/grok-fireworks-reg/pkg/tempmail"
)

// ─── 邮箱域名黑名单 ───

var novitaBlacklist = tempmail.NewDomainBlacklist("novita", 0, "data/blacklist.json")

// GetBlacklist 返回 Novita 黑名单实例
func GetBlacklist() *tempmail.DomainBlacklist {
	return novitaBlacklist
}

// Register 执行一次完整的 Novita AI 注册流程
func Register(ctx context.Context, opts RegisterOpts) *common.RegisterResult {
	logf := func(format string, args ...interface{}) {
		msg := fmt.Sprintf(format, args...)
		common.LogSend(opts.LogCh, msg)
		log.Info().Str("platform", "novita").Msg(msg)
	}

	serviceURL := common.SettingOrDefault(opts.Config, "novita_reg_url", "http://127.0.0.1:5002")

	// 1. 创建临时邮箱
	logf("[*] 任务开始")
	mailProvider := tempmail.NewMultiProvider(opts.Config)
	if mailProvider.ProviderCount() == 0 {
		return &common.RegisterResult{OK: false, Error: "无可用邮箱 provider", Platform: "novita"}
	}

	var email string
	var meta map[string]string
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		email, meta, err = mailProvider.GenerateEmail(ctx)
		if err != nil {
			return &common.RegisterResult{OK: false, Error: fmt.Sprintf("创建邮箱失败: %s", err), Platform: "novita"}
		}

		domain := tempmail.ExtractDomain(email)
		if novitaBlacklist.IsBanned(domain) {
			logf("[!] 域名 %s 已被拉黑，重新生成邮箱 (%d/3)", domain, attempt+1)
			go mailProvider.DeleteEmail(context.Background(), email, meta)
			continue
		}
		break
	}

	domain := tempmail.ExtractDomain(email)
	if novitaBlacklist.IsBanned(domain) {
		return &common.RegisterResult{OK: false, Email: email, Error: "无法获取非黑名单邮箱", Platform: "novita"}
	}

	logf("[*] 邮箱: %s (via %s)", email, meta["provider"])

	defer func() {
		_ = mailProvider.DeleteEmail(context.Background(), email, meta)
	}()

	// 2. 构造请求
	proxyStr := ""
	if opts.Proxy != nil {
		proxyStr = opts.Proxy.HTTPS
		if proxyStr == "" {
			proxyStr = opts.Proxy.HTTP
		}
	}

	reqBody := ServiceRequest{
		Email:          email,
		Proxy:          proxyStr,
		SolverAPI:      common.SettingOrDefault(opts.Config, "novita_solver_api", "http://127.0.0.1:5072"),
		MailProvider:   meta["provider"],
		MailMeta:       meta,
		AhemBaseURL:    common.SettingOrDefault(opts.Config, "ahem_base_url", ""),
		YYDSMailURL:    common.SettingOrDefault(opts.Config, "yydsmail_base_url", ""),
		YYDSMailKey:    common.SettingOrDefault(opts.Config, "yydsmail_api_key", ""),
		GPTMailBaseURL: common.SettingOrDefault(opts.Config, "gptmail_base_url", "https://mail.chatgpt.org.uk"),
		GPTMailAPIKey:  common.SettingOrDefault(opts.Config, "gptmail_api_key", ""),
	}

	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return &common.RegisterResult{OK: false, Email: email, Error: fmt.Sprintf("序列化请求失败: %s", err), Platform: "novita"}
	}

	// 3. 调用 Python 服务
	logf("[*] 调用注册服务...")
	result, err := callNovitaService(ctx, serviceURL+"/novita/process", bodyBytes, opts.LogCh)
	if err != nil {
		return &common.RegisterResult{OK: false, Email: email, Error: fmt.Sprintf("注册服务调用失败: %s", err), Platform: "novita"}
	}

	// 4. 转换结果
	data := map[string]interface{}{
		"api_key":  result.APIKey,
		"password": result.Password,
	}

	if result.OK {
		logf("[OK] 任务完成: %s", email)
	} else {
		logf("[-] 注册失败: %s", result.Error)
		errLower := strings.ToLower(result.Error)
		if strings.Contains(errLower, "illegal") || strings.Contains(errLower, "blocked") {
			d := tempmail.ExtractDomain(email)
			// 窗口内累计达阈值才拉黑（好域偶发一次不误杀），拉黑 2h 后自动恢复
			if novitaBlacklist.RecordIllegal(d) {
				logf("[!] 域名 %s 多次 EMAIL_ILLEGAL，已拉黑 2h", d)
			}
		}
	}

	return &common.RegisterResult{
		OK:       result.OK,
		Email:    email,
		Error:    result.Error,
		Platform: "novita",
		Data:     data,
	}
}

func callNovitaService(ctx context.Context, url string, body []byte, logCh chan<- string) (*ServiceResult, error) {
	transport := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}
	client := &http.Client{
		Transport: transport,
		Timeout:   5 * time.Minute,
	}

	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("创建请求失败: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("请求失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(respBody)[:300])
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64*1024), 256*1024)

	var lastLine string
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "LOG:") {
			logMsg := strings.TrimPrefix(line, "LOG:")
			common.LogSend(logCh, logMsg)
		} else {
			lastLine = line
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("读取响应流失败: %w", err)
	}

	if lastLine == "" {
		return nil, fmt.Errorf("注册服务未返回结果")
	}

	var result ServiceResult
	if err := json.Unmarshal([]byte(lastLine), &result); err != nil {
		return nil, fmt.Errorf("解析结果失败: %w (raw=%s)", err, common.TruncStr(lastLine, 200))
	}

	return &result, nil
}
