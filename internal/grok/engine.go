package grok

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/grok-fireworks-reg/internal/common"
	"github.com/grok-fireworks-reg/pkg/grpcweb"
	"github.com/grok-fireworks-reg/pkg/tempmail"
	"github.com/grok-fireworks-reg/pkg/turnstile"
)

// ─── 常量 ───

// defaultStateTree Next.js 路由状态树默认值（sign-up 页面）
const defaultStateTree = `%5B%22%22%2C%7B%22children%22%3A%5B%22(app)%22%2C%7B%22children%22%3A%5B%22(auth)%22%2C%7B%22children%22%3A%5B%22sign-up%22%2C%7B%22children%22%3A%5B%22__PAGE__%22%2C%7B%7D%2C%22%2Fsign-up%22%2C%22refresh%22%5D%7D%5D%7D%2Cnull%2Cnull%5D%7D%2Cnull%2Cnull%5D%7D%2Cnull%2Cnull%2Ctrue%5D`

// ─── 正则表达式 ───

var (
	// 参考 grokzhuce: 用 \d+: 锚定 RSC 段号分隔符，防止 URL 末尾吃掉多余数字
	reCookieURLAnchored = regexp.MustCompile(`(https://[^"\s]+set-cookie\?q=[^:"\s]+)\d+:`)
	// 降级: JSON 引号内的 URL（引号自然截止）
	reCookieURLQuoted = regexp.MustCompile(`(https://[^"\s]+set-cookie\?q=[^:"\s]+)`)
)

// ─── 邮箱域名黑名单 ───

var grokBlacklist = tempmail.NewDomainBlacklist("grok", 2*time.Hour, "data/blacklist.json")

// GetBlacklist 返回 Grok 黑名单实例（供 API handler 访问）
func GetBlacklist() *tempmail.DomainBlacklist {
	return grokBlacklist
}

// ─── 主注册流程 ───

// Register 执行一次完整的 Grok 协议注册流程（gRPC-web + Server Actions）
// 返回 RegisterResult 包含注册结果
func Register(ctx context.Context, opts RegisterOpts) *common.RegisterResult {
	// 随机延迟分散启动
	jitter := time.Duration(rand.Intn(3000)) * time.Millisecond
	select {
	case <-ctx.Done():
		return &common.RegisterResult{OK: false, Error: "context cancelled"}
	case <-time.After(jitter):
	}

	logf := func(format string, args ...interface{}) {
		if opts.LogCh != nil {
			common.LogSend(opts.LogCh, fmt.Sprintf(format, args...))
		}
	}

	// 选择浏览器指纹
	profile := browserProfiles[rand.Intn(len(browserProfiles))]

	logf("[*] 任务开始")

	// 获取配置（gRPC-web 模式不再需要 action_id / state_tree）
	siteKey := opts.Config["site_key"]

	// ─── 创建 HTTP 会话 ───
	client := makeHTTPClient(opts.Proxy, 30*time.Second)

	// 会话预热（获取 __cf_bm cookie）
	warmupReq, _ := http.NewRequestWithContext(ctx, "GET", "https://accounts.x.ai", nil)
	warmupReq.Header.Set("User-Agent", profile.UserAgent)
	if resp, err := client.Do(warmupReq); err == nil {
		resp.Body.Close()
	}

	// ─── Phase 1: 创建临时邮箱（多 provider 自动切换 + 黑名单过滤）───
	// 平台专属邮箱 provider 覆盖全局优先级
	if platformProviders := opts.Config["grok_email_providers"]; platformProviders != "" {
		opts.Config["email_provider_priority"] = platformProviders
	}
	mailProvider := tempmail.NewMultiProvider(opts.Config)
	
	// 生成邮箱并检查黑名单，最多重试 3 次
	var email string
	var mailMeta map[string]string
	var err error
	maxRetries := 3
	for attempt := 0; attempt < maxRetries; attempt++ {
		email, mailMeta, err = mailProvider.GenerateEmail(ctx)
		if err != nil {
			logf("[-] 创建邮箱失败: %s", err)
			if opts.OnFail != nil {
				opts.OnFail()
			}
			return &common.RegisterResult{OK: false, Error: fmt.Sprintf("创建邮箱失败: %s", err)}
		}
		
		// 检查域名是否在黑名单中
		domain := tempmail.ExtractDomain(email)
		if grokBlacklist.IsBanned(domain) {
			logf("[!] 域名 %s 已被拉黑，重新生成邮箱 (attempt %d/%d)", domain, attempt+1, maxRetries)
			// 清理当前邮箱
			go mailProvider.DeleteEmail(context.Background(), email, mailMeta)
			continue
		}
		
		// 找到了非黑名单域名，跳出循环
		break
	}
	
	// 如果 3 次重试后仍然是黑名单域名
	domain := tempmail.ExtractDomain(email)
	if grokBlacklist.IsBanned(domain) {
		logf("[-] 无法获取非黑名单邮箱，已重试 %d 次", maxRetries)
		if opts.OnFail != nil {
			opts.OnFail()
		}
		return &common.RegisterResult{OK: false, Email: email, Error: "无法获取非黑名单邮箱"}
	}
	
	password := common.RandomString(15, "abcdefghijklmnopqrstuvwxyz0123456789")
	logf("[*] 邮箱: %s (via %s)", email, mailMeta["provider"])

	// 注册失败时清理邮箱
	defer func() {
		go mailProvider.DeleteEmail(context.Background(), email, mailMeta)
	}()

	// ─── Phase 2: 发送验证码 (gRPC-web) ───
	logf("[*] 发送验证码...")
	grpcBody := grpcweb.EncodeEmailCode(email)
	sendResp, err := doGRPCWeb(ctx, client, profile.UserAgent,
		"https://accounts.x.ai/auth_mgmt.AuthManagement/CreateEmailValidationCode",
		"https://accounts.x.ai", "https://accounts.x.ai/sign-up?redirect=grok-com",
		grpcBody, nil)
	if err != nil || sendResp.StatusCode != 200 {
		status := 0
		if sendResp != nil {
			status = sendResp.StatusCode
			sendResp.Body.Close()
		}
		logf("[-] 发送验证码失败: HTTP %d, %v", status, err)
		// HTTP 400/403 可能是邮箱域名被拒绝，拉黑该域名
		if status == 400 || status == 403 {
			d := tempmail.ExtractDomain(email)
			grokBlacklist.Ban(d)
			logf("[!] 域名 %s 已拉黑 (Grok 拒绝，HTTP %d)", d, status)
		}
		if opts.OnFail != nil {
			opts.OnFail()
		}
		return &common.RegisterResult{OK: false, Email: email, Error: fmt.Sprintf("发送验证码失败: HTTP %d", status)}
	}
	sendResp.Body.Close()
	logf("[+] 验证码已发送")

	// ─── Phase 3: 获取验证码 ───
	logf("[*] 等待验证码（最长 30s）...")
	var code string
	for attempt := 1; attempt <= 30; attempt++ {
		select {
		case <-ctx.Done():
			logf("[-] 等待验证码时任务被取消")
			if opts.OnFail != nil {
				opts.OnFail()
			}
			return &common.RegisterResult{OK: false, Email: email, Error: "等待验证码时任务被取消"}
		case <-time.After(1 * time.Second):
		}
		c, err := mailProvider.FetchVerificationCode(ctx, email, mailMeta, 1, 0)
		if err == nil && c != "" {
			code = c
			break
		}
		if attempt%5 == 0 {
			logf("[*] 等待验证码中... 已等待 %ds", attempt)
		}
	}
	if code == "" {
		logf("[-] 获取验证码超时")
		tempmail.RecordFailure(mailMeta["provider"])
		if opts.OnFail != nil {
			opts.OnFail()
		}
		return &common.RegisterResult{OK: false, Email: email, Error: "获取验证码超时"}
	}
	logf("[+] 验证码: %s", code)
	tempmail.RecordSuccess(mailMeta["provider"])

	// ─── Phase 4: 验证邮箱 (gRPC-web) ───
	verifyBody := grpcweb.EncodeVerifyCode(email, code)
	verifyResp, err := doGRPCWeb(ctx, client, profile.UserAgent,
		"https://accounts.x.ai/auth_mgmt.AuthManagement/VerifyEmailValidationCode",
		"https://accounts.x.ai", "https://accounts.x.ai/sign-up?redirect=grok-com",
		verifyBody, nil)
	if err != nil || verifyResp.StatusCode != 200 {
		if verifyResp != nil {
			verifyResp.Body.Close()
		}
		logf("[-] 验证邮箱失败")
		if opts.OnFail != nil {
			opts.OnFail()
		}
		return &common.RegisterResult{OK: false, Email: email, Error: "验证邮箱失败"}
	}
	verifyResp.Body.Close()
	logf("[+] 邮箱验证成功")

	// ─── Phase 5+6: Server Actions 注册 ───
	firstName := common.RandomName(4, 6)
	lastName := common.RandomName(4, 6)

	var solverURLs []string
	if u := opts.Config["turnstile_solver_url"]; u != "" {
		solverURLs = append(solverURLs, u)
	}
	if u := opts.Config["cf_bypass_solver_url"]; u != "" {
		solverURLs = append(solverURLs, u)
	}
	// 从代理池提取代理地址，传给 Turnstile solver 浏览器使用
	var solverProxyURL string
	if opts.Proxy != nil {
		if opts.Proxy.HTTPS != "" {
			solverProxyURL = opts.Proxy.HTTPS
		} else if opts.Proxy.HTTP != "" {
			solverProxyURL = opts.Proxy.HTTP
		}
	}
	// grok 注册强制走本地 solver + builtin 脚本兜底，跳过 capsolver/yescaptcha
	solver := turnstile.NewSolver(solverURLs, "", "", solverProxyURL)

	actionID := opts.Config["action_id"]
	stateTree := common.SettingOrDefault(opts.Config, "state_tree", defaultStateTree)

	if actionID == "" {
		logf("[-] 缺少注册配置，无法注册")
		if opts.OnFail != nil {
			opts.OnFail()
		}
		return &common.RegisterResult{OK: false, Email: email, Error: "缺少 action_id 配置"}
	}

	var ssoToken string
	registered := false

	for attempt := 0; attempt < 3; attempt++ {
		select {
		case <-ctx.Done():
			if opts.OnFail != nil {
				opts.OnFail()
			}
			return &common.RegisterResult{OK: false, Email: email, Error: "context cancelled"}
		default:
		}

		logf("[*] 注册尝试 %d/3...", attempt+1)

		logf("[*] 验证远程服务接口...")
		turnstileToken, err := solver.Solve(ctx, "https://accounts.x.ai/sign-up", siteKey, 3, logf)
		if err != nil {
			logf("[-] 验证失败: %s", err)
			common.CtxSleep(ctx, 3*time.Second)
			continue
		}
		logf("[+] 卧槽Σ(°ロ°)验证通过 (%d chars)", len(turnstileToken))

		regPayload := []map[string]interface{}{{
			"emailValidationCode": code,
			"createUserAndSessionRequest": map[string]interface{}{
				"email":              email,
				"givenName":          firstName,
				"familyName":         lastName,
				"clearTextPassword":  password,
				"tosAcceptedVersion": "$undefined",
			},
			"turnstileToken":         turnstileToken,
			"promptOnDuplicateEmail": true,
		}}
		jsonBody, _ := json.Marshal(regPayload)

		regReq, _ := http.NewRequestWithContext(ctx, "POST", "https://accounts.x.ai/sign-up", bytes.NewReader(jsonBody))
		regReq.Header.Set("Content-Type", "text/plain;charset=UTF-8")
		regReq.Header.Set("Accept", "text/x-component")
		regReq.Header.Set("Next-Action", actionID)
		regReq.Header.Set("Next-Router-State-Tree", stateTree)
		regReq.Header.Set("Origin", "https://accounts.x.ai")
		regReq.Header.Set("Referer", "https://accounts.x.ai/sign-up")
		regReq.Header.Set("User-Agent", profile.UserAgent)

		regResp, err := client.Do(regReq)
		if err != nil {
			logf("[-] 注册请求失败: %s", err)
			common.CtxSleep(ctx, 3*time.Second)
			continue
		}
		respBody, _ := io.ReadAll(regResp.Body)
		regResp.Body.Close()
		respText := string(respBody)

		if regResp.StatusCode != 200 {
			logf("[-] 注册响应: HTTP %d, body=%s", regResp.StatusCode, common.TruncStr(respText, 200))
			common.CtxSleep(ctx, 3*time.Second)
			continue
		}

		var cookieURLMatch string
		if sm := reCookieURLAnchored.FindStringSubmatch(respText); len(sm) > 1 {
			cookieURLMatch = sm[1]
			logf("[*] 会话处理中...")
		} else if sm := reCookieURLQuoted.FindStringSubmatch(respText); len(sm) > 1 {
			cookieURLMatch = sm[1]
			logf("[*] 会话处理中（备用）...")
		}
		if cookieURLMatch == "" {
			logf("[-] 会话处理失败，响应异常")
			common.CtxSleep(ctx, 3*time.Second)
			continue
		}

		cookieReq, _ := http.NewRequestWithContext(ctx, "GET", cookieURLMatch, nil)
		cookieReq.Header.Set("User-Agent", profile.UserAgent)
		cookieResp, err := client.Do(cookieReq)
		if err != nil {
			logf("[-] 会话回调失败: %s", err)
			break
		}

		for _, sc := range cookieResp.Header["Set-Cookie"] {
			if strings.HasPrefix(sc, "sso=") && !strings.HasPrefix(sc, "sso-rw=") {
				parts := strings.SplitN(sc, ";", 2)
				kv := strings.SplitN(parts[0], "=", 2)
				if len(kv) == 2 && kv[1] != "" {
					ssoToken = kv[1]
				}
			}
		}
		cookieResp.Body.Close()
		logf("[*] 会话回调完成: HTTP %d", cookieResp.StatusCode)

		if ssoToken == "" {
			for _, domain := range []string{
				cookieResp.Request.URL.String(),
				"https://accounts.x.ai",
				"https://grok.com",
				"https://x.ai",
			} {
				domainURL, err := url.Parse(domain)
				if err != nil {
					continue
				}
				for _, c := range client.Jar.Cookies(domainURL) {
					if c.Name == "sso" && ssoToken == "" {
						ssoToken = c.Value
					}
				}
			}
		}

		if ssoToken == "" {
			logf("[-] 未获取到登录凭证")
			break
		}

		logf("[+] 注册成功!")
		registered = true
		break
	}

	if !registered {
		if opts.OnFail != nil {
			opts.OnFail()
		}
		return &common.RegisterResult{OK: false, Email: email, Error: "注册失败: 未获取到 SSO token"}
	}

	// ─── Phase 7+8: TOS + 生日 + NSFW + Unhinged + PayUrl ───
	logf("[*] 马上处理..设置中...")
	nsfwEnabled, checkoutURL := EnableNSFW(ctx, ssoToken, email, opts.Proxy, logf)
	if nsfwEnabled {
		logf("[+] 马上处理..成")
	} else {
		logf("[!] 马上处理..失败（账号已创建，继续保存）")
	}

	result := map[string]interface{}{
		"auth_token":      ssoToken,
		"feature_enabled": nsfwEnabled,
		"password":        password,
	}
	if checkoutURL != "" {
		result["redirect_url"] = checkoutURL
	}
	// 保存邮箱 provider 信息，供后续 FetchOTP 使用
	result["provider"] = mailMeta["provider"]
	emailMetaCopy := make(map[string]interface{}, len(mailMeta))
	for k, v := range mailMeta {
		emailMetaCopy[k] = v
	}
	result["email_meta"] = emailMetaCopy
	logf("[OK] 任务完成: %s", email)

	if opts.OnSuccess != nil {
		opts.OnSuccess(email, result)
	}

	return &common.RegisterResult{
		OK:       true,
		Email:    email,
		Platform: "grok",
		Data:     result,
	}
}

// ─── 内部方法 ───

// doGRPCWeb 发送 gRPC-web 请求
func doGRPCWeb(ctx context.Context, client *http.Client, ua, endpoint, origin, referer string, body []byte, extraCookies map[string]string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, "POST", endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/grpc-web+proto")
	req.Header.Set("X-Grpc-Web", "1")
	req.Header.Set("X-User-Agent", "connect-es/2.1.1")
	req.Header.Set("Origin", origin)
	req.Header.Set("Referer", referer)
	req.Header.Set("User-Agent", ua)

	// 将额外 cookie 添加到 jar（保留 jar 已有的 __cf_bm 等 Cloudflare cookie）
	if len(extraCookies) > 0 {
		var cookies []*http.Cookie
		for k, v := range extraCookies {
			cookies = append(cookies, &http.Cookie{Name: k, Value: v})
		}
		client.Jar.SetCookies(req.URL, cookies)
	}

	return client.Do(req)
}

// makeHTTPClient 创建带代理和 cookie jar 的 HTTP 客户端
func makeHTTPClient(proxy *common.ProxyEntry, timeout time.Duration) *http.Client {
	jar, _ := cookiejar.New(nil)
	transport := &http.Transport{
		MaxIdleConns:        10,
		IdleConnTimeout:     30 * time.Second,
		DisableCompression:  false,
		TLSHandshakeTimeout: 10 * time.Second,
	}
	common.ApplyProxy(transport, proxy)

	return &http.Client{
		Transport: transport,
		Jar:       jar,
		Timeout:   timeout,
	}
}
