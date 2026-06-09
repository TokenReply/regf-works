package grok

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/grok-fireworks-reg/internal/common"
	"github.com/rs/zerolog/log"
)

// ─── 扫描用正则 ───

var (
	reSiteKey   = regexp.MustCompile(`sitekey":"(0x4[a-zA-Z0-9_-]+)"`)
	reStateTree = regexp.MustCompile(`next-router-state-tree":"([^"]+)"`)
	reScriptSrc = regexp.MustCompile(`<script[^>]+src="([^"]*_next/static[^"]*)"`)
	reActionID  = regexp.MustCompile(`7f[a-fA-F0-9]{40}`)
)

// ScanConfig 扫描 Grok 注册页面获取 site_key、action_id、state_tree
// Server Actions 注册模式必须获取 action_id
func ScanConfig(ctx context.Context, proxy *common.ProxyEntry, cfg common.Config) (common.Config, error) {
	// 优先读 DB 已持久化的值作为 fallback，避免 X.AI 更新 site_key 后硬编码过期
	fallbackSiteKey := cfg["grok_site_key"]
	if fallbackSiteKey == "" {
		fallbackSiteKey = "0x4AAAAAAAhr9JGVDZbrZOo0"
	}
	result := common.Config{
		"site_key":   fallbackSiteKey,
		"state_tree": defaultStateTree,
	}

	// 远程服务模式：VPS 无需直连 x.ai，HF worker 会自行获取 action_id
	if cfg["grok_reg_url"] != "" {
		// 尝试用 DB 里缓存的 action_id，没有也不报错
		if aid := cfg["grok_action_id"]; aid != "" {
			result["action_id"] = aid
		}
		log.Info().Msg("remote mode, skip local scan")
		return result, nil
	}

	// 1) 直连扫描
	proxies := []*common.ProxyEntry{proxy}
	if proxy != nil {
		proxies = append(proxies, nil)
	}
	for _, p := range proxies {
		scanned, err := doScan(ctx, p)
		if err == nil {
			for _, k := range []string{"site_key", "action_id", "state_tree"} {
				if v := scanned[k]; v != "" {
					result[k] = v
				}
			}
			if result["action_id"] != "" {
				log.Info().Msg("config scan ok")
				return result, nil
			}
		}
	}

	// 2) CF-Bypass 降级扫描
	if cfBypassURL := cfg["cf_bypass_solver_url"]; cfBypassURL != "" {
		scanned, err := doScanViaCFBypass(ctx, cfBypassURL)
		if err == nil {
			for _, k := range []string{"site_key", "action_id", "state_tree"} {
				if v := scanned[k]; v != "" {
					result[k] = v
				}
			}
			if result["action_id"] != "" {
				log.Info().Msg("bypass scan ok")
				return result, nil
			}
		}
	}

	if result["action_id"] == "" {
		return result, fmt.Errorf("扫描失败: 未获取到 action_id（Server Actions 注册需要此配置）")
	}
	return result, nil
}

// doScan 实际扫描逻辑
func doScan(ctx context.Context, proxy *common.ProxyEntry) (common.Config, error) {
	cfg := common.Config{
		"site_key":   "0x4AAAAAAAhr9JGVDZbrZOo0",
		"action_id":  "",
		"state_tree": "%5B%22%22%2C%7B%22children%22%3A%5B%22(app)%22%2C%7B%22children%22%3A%5B%22(auth)%22%2C%7B%22children%22%3A%5B%22sign-up%22%2C%7B%22children%22%3A%5B%22__PAGE__%22%2C%7B%7D%2C%22%2Fsign-up%22%2C%22refresh%22%5D%7D%5D%7D%2Cnull%2Cnull%5D%7D%2Cnull%2Cnull%5D%7D%2Cnull%2Cnull%2Ctrue%5D",
	}

	client := makeHTTPClient(proxy, 30*time.Second)
	profile := browserProfiles[rand.Intn(len(browserProfiles))]

	// GET 注册页面
	req, err := http.NewRequestWithContext(ctx, "GET", "https://accounts.x.ai/sign-up", nil)
	if err != nil {
		return cfg, err
	}
	req.Header.Set("User-Agent", profile.UserAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")

	resp, err := client.Do(req)
	if err != nil {
		return cfg, fmt.Errorf("扫描注册页失败: %w", err)
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024*1024)) // 1MB 上限（HTML 页面）
	resp.Body.Close()
	html := string(body)

	// 提取 site_key
	if m := reSiteKey.FindStringSubmatch(html); len(m) > 1 {
		cfg["site_key"] = m[1]
	}

	// 提取 state_tree
	if m := reStateTree.FindStringSubmatch(html); len(m) > 1 {
		cfg["state_tree"] = m[1]
	}

	// 提取 JS bundle URL，搜索 action_id
	scriptMatches := reScriptSrc.FindAllStringSubmatch(html, -1)
	for _, sm := range scriptMatches {
		if len(sm) < 2 {
			continue
		}
		jsURL := sm[1]
		if !strings.HasPrefix(jsURL, "http") {
			jsURL = "https://accounts.x.ai" + jsURL
		}

		jsReq, _ := http.NewRequestWithContext(ctx, "GET", jsURL, nil)
		jsReq.Header.Set("User-Agent", profile.UserAgent)
		jsResp, err := client.Do(jsReq)
		if err != nil {
			continue
		}
		jsBody, _ := io.ReadAll(jsResp.Body)
		jsResp.Body.Close()

		if m := reActionID.FindString(string(jsBody)); m != "" {
			cfg["action_id"] = m
			break
		}
	}

	if cfg["action_id"] == "" {
		return cfg, fmt.Errorf("未找到 action_id")
	}

	log.Info().Msg("config scan ok")
	return cfg, nil
}

// doScanViaCFBypass 通过 CF-Bypass Solver 过 Cloudflare 后获取页面 HTML，再提取配置
func doScanViaCFBypass(ctx context.Context, cfBypassURL string) (common.Config, error) {
	cfg := common.Config{
		"site_key":   "0x4AAAAAAAhr9JGVDZbrZOo0",
		"action_id":  "",
		"state_tree": "%5B%22%22%2C%7B%22children%22%3A%5B%22(app)%22%2C%7B%22children%22%3A%5B%22(auth)%22%2C%7B%22children%22%3A%5B%22sign-up%22%2C%7B%22children%22%3A%5B%22__PAGE__%22%2C%7B%7D%2C%22%2Fsign-up%22%2C%22refresh%22%5D%7D%5D%7D%2Cnull%2Cnull%5D%7D%2Cnull%2Cnull%5D%7D%2Cnull%2Cnull%2Ctrue%5D",
	}

	// 调用 cf-bypass-solver 的 /fetch 接口
	fetchURL := strings.TrimRight(cfBypassURL, "/") + "/fetch?url=" + url.QueryEscape("https://accounts.x.ai/sign-up")
	client := &http.Client{Timeout: 120 * time.Second} // 过盾需要等浏览器，给足时间
	req, err := http.NewRequestWithContext(ctx, "GET", fetchURL, nil)
	if err != nil {
		return cfg, err
	}

	resp, err := client.Do(req)
	if err != nil {
		return cfg, fmt.Errorf("CF-Bypass fetch 请求失败: %w", err)
	}
	defer resp.Body.Close()

	var result struct {
		OK    bool   `json:"ok"`
		HTML  string `json:"html"`
		Error string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return cfg, fmt.Errorf("CF-Bypass 响应解析失败: %w", err)
	}
	if !result.OK || result.HTML == "" {
		return cfg, fmt.Errorf("CF-Bypass 获取页面失败: %s", result.Error)
	}

	html := result.HTML
	log.Info().Int("html_len", len(html)).Msg("bypass ok, extracting")

	// 提取 site_key
	if m := reSiteKey.FindStringSubmatch(html); len(m) > 1 {
		cfg["site_key"] = m[1]
	}

	// 提取 state_tree
	if m := reStateTree.FindStringSubmatch(html); len(m) > 1 {
		cfg["state_tree"] = m[1]
	}

	// 提取 JS bundle URL，搜索 action_id
	profile := browserProfiles[rand.Intn(len(browserProfiles))]
	scriptMatches := reScriptSrc.FindAllStringSubmatch(html, -1)
	for _, sm := range scriptMatches {
		if len(sm) < 2 {
			continue
		}
		jsURL := sm[1]
		if !strings.HasPrefix(jsURL, "http") {
			jsURL = "https://accounts.x.ai" + jsURL
		}

		jsReq, _ := http.NewRequestWithContext(ctx, "GET", jsURL, nil)
		jsReq.Header.Set("User-Agent", profile.UserAgent)
		jsResp, err := client.Do(jsReq)
		if err != nil {
			continue
		}
		jsBody, _ := io.ReadAll(jsResp.Body)
		jsResp.Body.Close()

		if m := reActionID.FindString(string(jsBody)); m != "" {
			cfg["action_id"] = m
			break
		}
	}

	if cfg["action_id"] == "" {
		return cfg, fmt.Errorf("CF-Bypass 过盾后仍未找到 action_id")
	}

	log.Info().Msg("bypass scan ok")
	return cfg, nil
}
