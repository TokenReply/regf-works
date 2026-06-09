package grok

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/grok-fireworks-reg/internal/common"
	"github.com/grok-fireworks-reg/pkg/grpcweb"
)

// EnableNSFW 设置 TOS + 生日 + NSFW + Unhinged + PayUrl
// 优先 Go 原生（标准 HTTP），失败时自动降级到 Python curl_cffi
func EnableNSFW(ctx context.Context, ssoToken, email string, proxy *common.ProxyEntry, logf func(string, ...interface{})) (bool, string) {
	ok, checkoutURL := enableNSFWGo(ctx, ssoToken, email, proxy, logf)
	if ok {
		return true, checkoutURL
	}
	logf("[!] Go 原生配置失败，降级到备用方案...")
	return enableNSFWViaPython(ctx, ssoToken, email, proxy, logf)
}

// enableNSFWGo 纯 Go 实现 NSFW 配置（不依赖 Python / curl_cffi）
// 流程: 预热 grok.com → TOS(accounts.x.ai) → 生日 → NSFW → Unhinged → PayUrl
func enableNSFWGo(ctx context.Context, ssoToken, email string, proxy *common.ProxyEntry, logf func(string, ...interface{})) (bool, string) {
	client := makeHTTPClient(proxy, 20*time.Second)
	profile := browserProfiles[rand.Intn(len(browserProfiles))]

	// 设置 SSO cookie 到 cookie jar
	for _, domain := range []string{"https://grok.com", "https://accounts.x.ai"} {
		domainURL, _ := url.Parse(domain)
		client.Jar.SetCookies(domainURL, []*http.Cookie{
			{Name: "sso", Value: ssoToken},
			{Name: "sso-rw", Value: ssoToken},
		})
	}

	// 预热 grok.com（获取 __cf_bm cookie）
	warmupReq, _ := http.NewRequestWithContext(ctx, "GET", "https://grok.com", nil)
	warmupReq.Header.Set("User-Agent", profile.UserAgent)
	if resp, err := client.Do(warmupReq); err == nil {
		resp.Body.Close()
		logf("[*] 预热: HTTP %d", resp.StatusCode)
		if resp.StatusCode == 403 {
			logf("[-] 预热被拦截，放弃")
			return false, ""
		}
	} else {
		logf("[-] 预热失败: %s", err)
		return false, ""
	}

	// Step 1: TOS (accounts.x.ai gRPC-web)
	tosPayload := grpcweb.EncodeTosAccepted()
	tosResp, err := doGRPCWeb(ctx, client, profile.UserAgent,
		"https://accounts.x.ai/auth_mgmt.AuthManagement/SetTosAcceptedVersion",
		"https://accounts.x.ai", "https://accounts.x.ai/accept-tos",
		tosPayload, nil)
	if err != nil {
		logf("[-] 协议请求失败: %s", err)
		return false, ""
	}
	tosResp.Body.Close()
	if tosResp.StatusCode != 200 {
		logf("[-] 协议响应异常: %d", tosResp.StatusCode)
		return false, ""
	}
	logf("[+] 协议已接受")

	// Step 2: 生日 (grok.com REST)
	bdPayload, _ := json.Marshal(map[string]string{"birthDate": common.RandomBirthDate()})
	bdReq, _ := http.NewRequestWithContext(ctx, "POST", "https://grok.com/rest/auth/set-birth-date", bytes.NewReader(bdPayload))
	bdReq.Header.Set("Content-Type", "application/json")
	bdReq.Header.Set("Origin", "https://grok.com")
	bdReq.Header.Set("Referer", "https://grok.com/")
	bdReq.Header.Set("User-Agent", profile.UserAgent)
	bdResp, err := client.Do(bdReq)
	if err != nil {
		logf("[-] 信息设置失败: %s", err)
		return false, ""
	}
	bdResp.Body.Close()
	if bdResp.StatusCode != 200 {
		logf("[-] 信息设置异常: %d", bdResp.StatusCode)
		return false, ""
	}
	logf("[+] 信息设置成功")

	// Step 3: NSFW (grok.com gRPC-web)
	nsfwPayload := grpcweb.EncodeNsfwSettings()
	nsfwResp, err := doGRPCWeb(ctx, client, profile.UserAgent,
		"https://grok.com/auth_mgmt.AuthManagement/UpdateUserFeatureControls",
		"https://grok.com", "https://grok.com/",
		nsfwPayload, nil)
	if err != nil {
		logf("[-] 偏好设置请求失败: %s", err)
		return false, ""
	}
	nsfwResp.Body.Close()
	if nsfwResp.StatusCode != 200 {
		logf("[-] 偏好设置异常: %d", nsfwResp.StatusCode)
		return false, ""
	}
	logf("[+] 偏好设置成功")

	// Step 4: Unhinged (grok.com gRPC-web, 不阻断主流程)
	unhingedPayload := grpcweb.EncodeUnhingedSettings()
	unhingedResp, err := doGRPCWeb(ctx, client, profile.UserAgent,
		"https://grok.com/auth_mgmt.AuthManagement/UpdateUserFeatureControls",
		"https://grok.com", "https://grok.com/",
		unhingedPayload, nil)
	if err == nil && unhingedResp != nil {
		unhingedResp.Body.Close()
		if unhingedResp.StatusCode == 200 {
			logf("[+] 高级模式已开启")
		} else {
			logf("[-] 高级模式设置异常: %d（不阻断）", unhingedResp.StatusCode)
		}
	} else {
		logf("[-] 高级模式请求失败（不阻断）: %v", err)
	}

	// Step 5: PayUrl 支付链接（不阻断主流程）
	checkoutURL := ""
	if email != "" {
		checkoutURL = createCheckoutURLGo(ctx, client, profile.UserAgent, email, logf)
	}

	return true, checkoutURL
}

// createCheckoutURLGo 纯 Go 生成 Grok Pro Stripe 订阅支付链接
func createCheckoutURLGo(ctx context.Context, client *http.Client, ua, email string, logf func(string, ...interface{})) string {
	// Step 1: 创建 Stripe 客户
	custPayload, _ := json.Marshal(map[string]interface{}{
		"billingInfo": map[string]string{
			"name":  common.RandomName(5, 8) + " " + common.RandomName(5, 8),
			"email": email,
		},
	})
	custReq, _ := http.NewRequestWithContext(ctx, "POST",
		"https://grok.com/rest/subscriptions/customer/new", bytes.NewReader(custPayload))
	custReq.Header.Set("Content-Type", "application/json")
	custReq.Header.Set("Origin", "https://grok.com")
	custReq.Header.Set("Referer", "https://grok.com/")
	custReq.Header.Set("User-Agent", ua)
	custReq.Header.Set("X-Xai-Request-Id", common.RandomUUID())
	custResp, err := client.Do(custReq)
	if err != nil {
		logf("[-] 支付初始化失败: %s", err)
		return ""
	}
	custResp.Body.Close()
	if custResp.StatusCode != 200 && custResp.StatusCode != 201 && custResp.StatusCode != 204 {
		logf("[-] 支付初始化异常: %d", custResp.StatusCode)
		return ""
	}

	// Step 2: 创建订阅获取 checkout URL
	subPayload, _ := json.Marshal(map[string]interface{}{
		"stripeHosted": map[string]string{
			"successUrl": "https://grok.com/?checkout=success&tier=SUBSCRIPTION_TIER_GROK_PRO&interval=monthly#subscribe",
		},
		"priceId":                          "price_1R6nQ9HJohyvID2ck7FNrVdw",
		"campaignId":                       "subcamp_HeAxW",
		"ignoreExistingActiveSubscriptions": false,
		"subscriptionType":                 "MONTHLY",
		"requestedTier":                    "REQUESTED_TIER_GROK_PRO",
	})
	subReq, _ := http.NewRequestWithContext(ctx, "POST",
		"https://grok.com/rest/subscriptions/subscribe/new", bytes.NewReader(subPayload))
	subReq.Header.Set("Content-Type", "application/json")
	subReq.Header.Set("Origin", "https://grok.com")
	subReq.Header.Set("Referer", "https://grok.com/")
	subReq.Header.Set("User-Agent", ua)
	subReq.Header.Set("X-Xai-Request-Id", common.RandomUUID())
	subResp, err := client.Do(subReq)
	if err != nil {
		logf("[-] 订阅创建失败: %s", err)
		return ""
	}
	defer subResp.Body.Close()
	if subResp.StatusCode != 200 {
		logf("[-] 订阅创建异常: %d", subResp.StatusCode)
		return ""
	}

	var subResult map[string]interface{}
	if err := json.NewDecoder(io.LimitReader(subResp.Body, 64*1024)).Decode(&subResult); err != nil {
		logf("[-] 订阅响应解析失败: %s", err)
		return ""
	}
	if u, ok := subResult["url"].(string); ok && u != "" {
		logf("[+] 支付链接获取成功")
		return u
	}
	if u, ok := subResult["checkoutUrl"].(string); ok && u != "" {
		logf("[+] 支付链接获取成功")
		return u
	}
	logf("[-] 支付链接未找到")
	return ""
}

// enableNSFWViaPython 通过 Python curl_cffi 子进程设置 TOS + 生日 + NSFW + Unhinged + PayUrl
// 返回 (nsfw成功, checkout_url)
func enableNSFWViaPython(ctx context.Context, ssoToken, email string, proxy *common.ProxyEntry, logf func(string, ...interface{})) (bool, string) {
	// 查找 enable_nsfw.py 脚本
	scriptPath := ""
	candidates := []string{
		"scripts/enable_nsfw.py",
	}
	// 也检查可执行文件相对路径
	if execPath, err := os.Executable(); err == nil {
		execDir := filepath.Dir(execPath)
		candidates = append(candidates,
			filepath.Join(execDir, "scripts", "enable_nsfw.py"),
			filepath.Join(execDir, "..", "scripts", "enable_nsfw.py"),
		)
	}
	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			scriptPath = p
			break
		}
	}
	if scriptPath == "" {
		logf("[-] 马上处理..组件未找到")
		return false, ""
	}

	// 调用: python3 enable_nsfw.py --sso <TOKEN> --email <EMAIL>
	cmdCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	cmd := exec.CommandContext(cmdCtx, "python3", scriptPath, "--sso", ssoToken)

	// 传递代理配置给 Python 脚本
	if proxy != nil {
		proxyStr := proxy.HTTPS
		if proxyStr == "" {
			proxyStr = proxy.HTTP
		}
		if proxyStr != "" {
			cmd.Args = append(cmd.Args, "--proxy", proxyStr)
		}
	}

	// 传递 email 以生成 PayUrl checkout 链接
	if email != "" {
		cmd.Args = append(cmd.Args, "--email", email)
	}

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	outStr := strings.TrimSpace(stdout.String())
	errStr := strings.TrimSpace(stderr.String())

	// 打印 Python 脚本调试日志（stderr 里有各步骤的 HTTP 状态码）
	if errStr != "" {
		for _, line := range strings.Split(errStr, "\n") {
			line = strings.TrimSpace(line)
			if line != "" {
				logf("[*] %s", line)
			}
		}
	}

	if err != nil {
		logf("[!] 马上处理..失败: %s", common.TruncStr(outStr, 200))
		return false, ""
	}

	// 解析输出：NSFW_OK:<msg>|<checkout_url> 或 NSFW_FAIL:<reason>
	checkoutURL := ""
	for _, line := range strings.Split(outStr, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "NSFW_OK:") {
			payload := strings.TrimPrefix(line, "NSFW_OK:")
			// 格式: <msg>|<checkout_url>（checkout_url 可为空）
			parts := strings.SplitN(payload, "|", 2)
			logf("[+] %s", parts[0])
			if len(parts) == 2 && parts[1] != "" {
				checkoutURL = parts[1]
				logf("[+] PayUrl: %s", common.TruncStr(checkoutURL, 80))
			}
			return true, checkoutURL
		}
		if strings.HasPrefix(line, "NSFW_FAIL:") {
			logf("[-] 马上处理..失败: %s", strings.TrimPrefix(line, "NSFW_FAIL:"))
			return false, ""
		}
	}

	logf("[!] 马上处理..异常: %s", common.TruncStr(outStr, 200))
	return false, ""
}
