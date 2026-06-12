package fireworks

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

var fireworksBlacklist = tempmail.NewDomainBlacklist("fireworks", 2*time.Hour, "data/blacklist.json")

// GetBlacklist 返回 Fireworks 黑名单实例（供 API handler 访问）
func GetBlacklist() *tempmail.DomainBlacklist {
	return fireworksBlacklist
}

// Register 执行一次完整的 Fireworks 注册流程
// 1. 通过 MultiProvider 创建临时邮箱
// 2. 调用 Python 注册服务（POST /fireworks/process）
// 3. 流式读取日志 + 最终 JSON 结果
// 4. 转换为通用 RegisterResult 返回
func Register(ctx context.Context, opts RegisterOpts) *common.RegisterResult {
	logf := func(format string, args ...interface{}) {
		msg := fmt.Sprintf(format, args...)
		common.LogSend(opts.LogCh, msg)
		log.Info().Str("platform", "fireworks").Msg(msg)
	}

	// 获取服务地址
	serviceURL := common.SettingOrDefault(opts.Config, "fireworks_reg_url", "http://127.0.0.1:5000")

	// 1. 创建临时邮箱（含黑名单过滤 + 重试）
	logf("创建临时邮箱...")
	mailProvider := tempmail.NewMultiProvider(opts.Config)
	if mailProvider.ProviderCount() == 0 {
		return &common.RegisterResult{OK: false, Error: "无可用邮箱 provider", Platform: "fireworks"}
	}

	// 生成邮箱并检查黑名单，最多重试 3 次
	var email string
	var meta map[string]string
	var err error
	maxRetries := 3
	for attempt := 0; attempt < maxRetries; attempt++ {
		email, meta, err = mailProvider.GenerateEmail(ctx)
		if err != nil {
			return &common.RegisterResult{OK: false, Error: fmt.Sprintf("创建邮箱失败: %s", err), Platform: "fireworks"}
		}
		
		// 检查域名是否在黑名单中
		domain := tempmail.ExtractDomain(email)
		if fireworksBlacklist.IsBanned(domain) {
			logf("[!] 域名 %s 已被拉黑，重新生成邮箱 (attempt %d/%d)", domain, attempt+1, maxRetries)
			// 清理当前邮箱
			go mailProvider.DeleteEmail(context.Background(), email, meta)
			continue
		}
		
		// 找到了非黑名单域名，跳出循环
		break
	}
	
	// 如果 3 次重试后仍然是黑名单域名
	domain := tempmail.ExtractDomain(email)
	if fireworksBlacklist.IsBanned(domain) {
		logf("[-] 无法获取非黑名单邮箱，已重试 %d 次", maxRetries)
		return &common.RegisterResult{OK: false, Email: email, Error: "无法获取非黑名单邮箱", Platform: "fireworks"}
	}
	
	logf("邮箱已创建: %s (provider=%s)", email, meta["provider"])

	// 清理邮箱（注册完成后）
	defer func() {
		_ = mailProvider.DeleteEmail(context.Background(), email, meta)
	}()

	// 2. 构造请求体
	proxyStr := ""
	if opts.Proxy != nil {
		proxyStr = opts.Proxy.HTTPS
		if proxyStr == "" {
			proxyStr = opts.Proxy.HTTP
		}
	}

	reqBody := ServiceRequest{
		Email:        email,
		Proxy:        proxyStr,
		YYDSMailURL:  common.SettingOrDefault(opts.Config, "yydsmail_base_url", ""),
		YYDSMailKey:  common.SettingOrDefault(opts.Config, "yydsmail_api_key", ""),
		MailProvider: meta["provider"],
		MailMeta:     meta,
	}

	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return &common.RegisterResult{OK: false, Email: email, Error: fmt.Sprintf("序列化请求失败: %s", err), Platform: "fireworks"}
	}

	// 3. 调用 Python 服务
	logf("调用注册服务: %s", serviceURL)
	result, err := callRegService(ctx, serviceURL+"/fireworks/process", bodyBytes, opts.LogCh)
	if err != nil {
		return &common.RegisterResult{OK: false, Email: email, Error: fmt.Sprintf("注册服务调用失败: %s", err), Platform: "fireworks"}
	}

	// 4. 转换结果
	data := map[string]interface{}{
		"apikey":                 result.APIKey,
		"account_id":            result.AccountID,
		"requested_account_id":  result.RequestedAccountID,
		"key_id":                result.KeyID,
		"user_sub":              result.UserSub,
		"password":              result.Password,
		"first_name":            result.FirstName,
		"last_name":             result.LastName,
		"suspend_state":         result.SuspendState,
		"account_status_code":   result.AccountStatusCode,
		"account_status_message": result.AccountStatusMessage,
		"models_check":          result.ModelsCheck,
		"warning":               result.Warning,
		"retriable":             result.Retriable,
	}

	if result.OK {
		logf("注册成功: email=%s account_id=%s key=%s...", email, result.AccountID, common.TruncStr(result.APIKey, 10))
	} else {
		logf("注册失败: %s (retriable=%v)", result.Error, result.Retriable)
		// 如果错误信息包含邮箱域名拒绝的关键词，拉黑该域名
		errLower := strings.ToLower(result.Error)
		if strings.Contains(errLower, "email") && (strings.Contains(errLower, "domain") || 
			strings.Contains(errLower, "not allowed") || strings.Contains(errLower, "reject") ||
			strings.Contains(errLower, "invalid") || strings.Contains(errLower, "blocked")) {
			d := tempmail.ExtractDomain(email)
			fireworksBlacklist.Ban(d)
			logf("[!] 域名 %s 已拉黑 (Fireworks 拒绝: %s)", d, result.Error)
		}
	}

	return &common.RegisterResult{
		OK:       result.OK,
		Email:    email,
		Error:    result.Error,
		Platform: "fireworks",
		Data:     data,
	}
}

// callRegService 调用 Python 注册服务，流式读取 LOG: 行并解析最终 JSON 结果
func callRegService(ctx context.Context, url string, body []byte, logCh chan<- string) (*ServiceResult, error) {
	transport := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}
	client := &http.Client{
		Transport: transport,
		Timeout:   5 * time.Minute, // 注册流程可能较长（等邮件）
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

	// 流式读取响应
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
			log.Debug().Str("platform", "fireworks").Msg(logMsg)
		} else {
			// 非 LOG 行视为最终 JSON 结果
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
		return nil, fmt.Errorf("解析结果 JSON 失败: %w (raw=%s)", err, common.TruncStr(lastLine, 200))
	}

	return &result, nil
}
