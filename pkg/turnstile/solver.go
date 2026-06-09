package turnstile

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// ── 健康追踪 ──

// solverHealth 单个 solver 节点的健康状态
type solverHealth struct {
	mu              sync.Mutex
	consecutiveFail int
	cooldownUntil   time.Time
}

// record 记录一次求解结果
func (h *solverHealth) record(ok bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if ok {
		h.consecutiveFail = 0
		h.cooldownUntil = time.Time{}
	} else {
		h.consecutiveFail++
		// 连续 3 次失败 → 冷却 2 分钟
		if h.consecutiveFail >= 3 {
			h.cooldownUntil = time.Now().Add(2 * time.Minute)
		}
	}
}

// isHealthy 是否可用（未在冷却中）
func (h *solverHealth) isHealthy() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.cooldownUntil.IsZero() || time.Now().After(h.cooldownUntil)
}

// ── Solver ──

// Solver Cloudflare Turnstile 验证码求解器（多后端并行竞速）
type Solver struct {
	solverURLs    []string // 本地 solver URL 列表
	capSolverKey  string   // CapSolver API key
	yesCaptchaKey string   // YesCaptcha API key
	proxyURL      string   // 代理地址（传给本地 solver 浏览器）
	httpClient    *http.Client
	// 健康追踪（按 solver URL / 服务名 索引）
	healthMu sync.RWMutex
	health   map[string]*solverHealth
}

// NewSolver 创建求解器
func NewSolver(solverURLs []string, capSolverKey, yesCaptchaKey, proxyURL string) *Solver {
	cleaned := make([]string, 0, len(solverURLs))
	for _, u := range solverURLs {
		u = strings.TrimSpace(u)
		u = strings.TrimRight(u, "/")
		if u != "" {
			cleaned = append(cleaned, u)
		}
	}
	return &Solver{
		solverURLs:    cleaned,
		capSolverKey:  capSolverKey,
		yesCaptchaKey: yesCaptchaKey,
		proxyURL:      proxyURL,
		httpClient:    &http.Client{Timeout: 30 * time.Second},
		health:        make(map[string]*solverHealth),
	}
}

// getHealth 获取指定 solver 的健康状态（懒初始化）
func (s *Solver) getHealth(key string) *solverHealth {
	s.healthMu.RLock()
	h, ok := s.health[key]
	s.healthMu.RUnlock()
	if ok {
		return h
	}
	s.healthMu.Lock()
	defer s.healthMu.Unlock()
	if h, ok = s.health[key]; !ok {
		h = &solverHealth{}
		s.health[key] = h
	}
	return h
}

// ── 主入口 ──

// Solve 求解 Turnstile，返回 token
// 级联顺序：本地 solver 并行竞速 → 云端 solver 并行竞速 → 内置脚本
func (s *Solver) Solve(ctx context.Context, siteURL, siteKey string, maxAttempts int, logf func(string, ...interface{})) (string, error) {
	if logf == nil {
		logf = func(string, ...interface{}) {}
	}
	var lastErr error

	for i := 0; i < maxAttempts; i++ {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}

		// 1) 本地 solver 并行竞速
		if len(s.solverURLs) > 0 {
			token, err := s.raceLocalSolvers(ctx, siteURL, siteKey, logf)
			if err == nil && token != "" {
				return token, nil
			}
			lastErr = err
		}

		// 2) 云端 solver 顺序 fallback（CapSolver 先，失败再 YesCaptcha）
		if s.capSolverKey != "" || s.yesCaptchaKey != "" {
			token, err := s.cascadeCloudSolvers(ctx, siteURL, siteKey, logf)
			if err == nil && token != "" {
				return token, nil
			}
			lastErr = err
		}

		// 3) 内置脚本（最后手段）
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		logf("[*] 切换到内置脚本...")
		if token, err := s.solveBuiltinScript(ctx, siteURL, siteKey); err == nil && token != "" {
			logf("[+] 卧槽Σ(°ロ°)成功了（内置脚本）")
			return token, nil
		}

		if i < maxAttempts-1 {
			logf("[!] 本轮全部失败，等待重试 (%d/%d)...", i+1, maxAttempts)
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(3 * time.Second):
			}
		}
	}
	return "", fmt.Errorf("验证求解失败 (%d 次): %v", maxAttempts, lastErr)
}

// ── 并行竞速 ──

// raceLocalSolvers 并行提交到所有本地 solver，第一个成功的 token 胜出
func (s *Solver) raceLocalSolvers(ctx context.Context, siteURL, siteKey string, logf func(string, ...interface{})) (string, error) {
	// 单 solver 直接串行（省 goroutine 开销）
	if len(s.solverURLs) == 1 {
		u := s.solverURLs[0]
		h := s.getHealth(u)
		logf("[*] 冲鸭冲鸭冲鸭...")
		token, err := s.solveLocal(ctx, u, siteURL, siteKey, logf)
		if err == nil && token != "" {
			logf("[+] 卧槽Σ(°ロ°)成功了 #1")
			h.record(true)
			return token, nil
		}
		h.record(false)
		logf("[-] 解密中 #1 失败: %v", err)
		return "", err
	}

	// 多 solver 并行竞速
	raceCtx, raceCancel := context.WithCancel(ctx)
	defer raceCancel()

	type raceResult struct {
		token string
		err   error
		idx   int
	}
	ch := make(chan raceResult, len(s.solverURLs))

	for idx, solverURL := range s.solverURLs {
		idx, solverURL := idx, solverURL
		h := s.getHealth(solverURL)
		go func() {
			// 冷却中的 solver 延迟启动（仍参与竞速，只是让健康的先跑）
			if !h.isHealthy() {
				logf("[*] 解密中 #%d 冷却中，延迟启动...", idx+1)
				select {
				case <-raceCtx.Done():
					ch <- raceResult{err: raceCtx.Err(), idx: idx}
					return
				case <-time.After(5 * time.Second):
				}
			}
			logf("[*] 冲鸭冲鸭冲鸭... #%d", idx+1)
			token, err := s.solveLocal(raceCtx, solverURL, siteURL, siteKey, logf)
			ch <- raceResult{token: token, err: err, idx: idx}
		}()
	}

	var lastErr error
	for range s.solverURLs {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case r := <-ch:
			h := s.getHealth(s.solverURLs[r.idx])
			if r.err == nil && r.token != "" {
				logf("[+] 卧槽Σ(°ロ°)成功了 #%d", r.idx+1)
				raceCancel() // 取消其他 solver
				h.record(true)
				return r.token, nil
			}
			// 被取消的不算失败
			if r.err != nil && r.err != context.Canceled {
				logf("[-] 解密中 #%d 失败: %v", r.idx+1, r.err)
				lastErr = r.err
				h.record(false)
			}
		}
	}
	return "", lastErr
}

// cascadeCloudSolvers 顺序 fallback：CapSolver 先打，失败再 YesCaptcha。
// 之前是并行竞速，但每个云端 createTask 立刻计费，并行 = 双倍烧钱；
// 改成串行后只有 CapSolver 真正失败时才会请求 YesCaptcha。
func (s *Solver) cascadeCloudSolvers(ctx context.Context, siteURL, siteKey string, logf func(string, ...interface{})) (string, error) {
	var lastErr error
	if s.capSolverKey != "" {
		logf("[*] 冲鸭冲鸭冲鸭... 云端A")
		token, err := s.solveCapSolver(ctx, siteURL, siteKey, logf)
		if err == nil && token != "" {
			logf("[+] 卧槽Σ(°ロ°)成功了（云端A）")
			return token, nil
		}
		if err != nil && err != context.Canceled {
			logf("[-] 云端A 失败: %v", err)
			lastErr = err
		}
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
	}
	if s.yesCaptchaKey != "" {
		logf("[*] 冲鸭冲鸭冲鸭... 云端B")
		token, err := s.solveYesCaptcha(ctx, siteURL, siteKey, logf)
		if err == nil && token != "" {
			logf("[+] 卧槽Σ(°ロ°)成功了（云端B）")
			return token, nil
		}
		if err != nil && err != context.Canceled {
			logf("[-] 云端B 失败: %v", err)
			lastErr = err
		}
	}
	if lastErr == nil {
		return "", fmt.Errorf("无可用云端服务")
	}
	return "", lastErr
}

// ── 远程端 ──

// solveLocal 单节点本地求解（提交 + 轮询）
func (s *Solver) solveLocal(ctx context.Context, solverURL, siteURL, siteKey string, logf func(string, ...interface{})) (string, error) {
	// 提交请求 — 使用 10s 超时快速检测不可达节点
	submitCtx, submitCancel := context.WithTimeout(ctx, 10*time.Second)
	defer submitCancel()

	reqURL := fmt.Sprintf("%s/turnstile?url=%s&sitekey=%s", solverURL, url.QueryEscape(siteURL), url.QueryEscape(siteKey))
	if s.proxyURL != "" {
		reqURL += "&proxy=" + url.QueryEscape(s.proxyURL)
	}
	req, err := http.NewRequestWithContext(submitCtx, "GET", reqURL, nil)
	if err != nil {
		return "", fmt.Errorf("远程端构建请求失败: %w", err)
	}
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("远程端连接失败: %w", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	var createResp map[string]interface{}
	if err := json.Unmarshal(body, &createResp); err != nil {
		return "", fmt.Errorf("远程端响应解析失败: %w", err)
	}

	taskID, _ := createResp["taskId"].(string)
	if taskID == "" {
		taskID, _ = createResp["task_id"].(string)
	}
	if taskID == "" {
		if token, ok := createResp["token"].(string); ok && token != "" {
			return token, nil
		}
		return "", fmt.Errorf("远程端无 taskId")
	}

	logf("[*] 任务提交啦，等等我马上就好...")

	// 首次轮询等待缩短到 2s（原 5s 太慢，浏览器启动 + 页面加载通常 2-3s）
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case <-time.After(2 * time.Second):
	}

	resultURL := fmt.Sprintf("%s/result?id=%s", solverURL, taskID)
	for j := 0; j < 30; j++ {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		pollReq, _ := http.NewRequestWithContext(ctx, "GET", resultURL, nil)
		resp, err := s.httpClient.Do(pollReq)
		if err != nil {
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(2 * time.Second):
			}
			continue
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		var result map[string]interface{}
		if err := json.Unmarshal(body, &result); err != nil {
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(2 * time.Second):
			}
			continue
		}

		if errID, ok := result["errorId"].(float64); ok && errID != 0 {
			desc, _ := result["errorDescription"].(string)
			code, _ := result["errorCode"].(string)
			// "Task not found" = poll 打到了负载均衡的错误节点，继续轮询等正确节点
			if strings.Contains(strings.ToLower(desc), "not found") || strings.Contains(strings.ToLower(code), "not_found") {
				select {
				case <-ctx.Done():
					return "", ctx.Err()
				case <-time.After(2 * time.Second):
				}
				continue
			}
			return "", fmt.Errorf("远程端失败: %v (%s)", code, desc)
		}
		status, _ := result["status"].(string)
		if status == "CAPTCHA_FAIL" {
			return "", fmt.Errorf("远程端失败")
		}
		if j%3 == 0 {
			logf("[*] 等等我马上就好... (%d/30)", j+1)
		}
		if solution, ok := result["solution"].(map[string]interface{}); ok {
			if token, ok := solution["token"].(string); ok && token != "" {
				return token, nil
			}
		}
		if token, ok := result["token"].(string); ok && token != "" {
			return token, nil
		}
		if value, ok := result["value"].(string); ok && value != "" {
			return value, nil
		}

		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	return "", fmt.Errorf("远程端轮询超时")
}

// ── 云端求解器 ──

// solveCapSolver CapSolver 云服务求解 Turnstile
func (s *Solver) solveCapSolver(ctx context.Context, siteURL, siteKey string, logf func(string, ...interface{})) (string, error) {
	// 创建任务
	payload, _ := json.Marshal(map[string]interface{}{
		"clientKey": s.capSolverKey,
		"task": map[string]interface{}{
			"type":       "AntiTurnstileTaskProxyLess",
			"websiteURL": siteURL,
			"websiteKey": siteKey,
		},
	})

	req, err := http.NewRequestWithContext(ctx, "POST", "https://api.capsolver.com/createTask", strings.NewReader(string(payload)))
	if err != nil {
		return "", fmt.Errorf("CapSolver 构建请求失败: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("CapSolver 连接失败: %w", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	var createResp struct {
		ErrorID          int    `json:"errorId"`
		ErrorCode        string `json:"errorCode"`
		ErrorDescription string `json:"errorDescription"`
		TaskID           string `json:"taskId"`
	}
	if err := json.Unmarshal(body, &createResp); err != nil {
		return "", fmt.Errorf("CapSolver 响应解析失败: %w", err)
	}
	if createResp.ErrorID != 0 {
		return "", fmt.Errorf("CapSolver 创建任务失败: %s (%s)", createResp.ErrorCode, createResp.ErrorDescription)
	}
	if createResp.TaskID == "" {
		return "", fmt.Errorf("CapSolver 无 taskId: %s", string(body))
	}

	logf("[*] 云端A 任务提交啦，等等我...")

	// 轮询结果（最多 120 秒）
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case <-time.After(5 * time.Second):
	}
	for j := 0; j < 40; j++ {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		pollPayload, _ := json.Marshal(map[string]interface{}{
			"clientKey": s.capSolverKey,
			"taskId":    createResp.TaskID,
		})
		pollReq, _ := http.NewRequestWithContext(ctx, "POST", "https://api.capsolver.com/getTaskResult", strings.NewReader(string(pollPayload)))
		pollReq.Header.Set("Content-Type", "application/json")
		resp, err := s.httpClient.Do(pollReq)
		if err != nil {
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(3 * time.Second):
			}
			continue
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		var result struct {
			ErrorID  int    `json:"errorId"`
			Status   string `json:"status"`
			Solution struct {
				Token     string `json:"token"`
				UserAgent string `json:"userAgent"`
			} `json:"solution"`
		}
		if err := json.Unmarshal(body, &result); err != nil {
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(3 * time.Second):
			}
			continue
		}

		if result.Status == "ready" && result.Solution.Token != "" {
			return result.Solution.Token, nil
		}
		if result.ErrorID != 0 {
			return "", fmt.Errorf("CapSolver 任务失败: errorId=%d", result.ErrorID)
		}
		if result.Status == "failed" {
			return "", fmt.Errorf("CapSolver 任务失败")
		}
		if j%5 == 0 {
			logf("[*] 云端A 等等我马上就好... (%d/40)", j+1)
		}

		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
	return "", fmt.Errorf("CapSolver 轮询超时")
}

// solveYesCaptcha YesCaptcha 云服务求解
func (s *Solver) solveYesCaptcha(ctx context.Context, siteURL, siteKey string, logf func(string, ...interface{})) (string, error) {
	payload, _ := json.Marshal(map[string]interface{}{
		"clientKey": s.yesCaptchaKey,
		"task": map[string]interface{}{
			"type":       "TurnstileTaskProxyless",
			"websiteURL": siteURL,
			"websiteKey": siteKey,
		},
	})

	req, err := http.NewRequestWithContext(ctx, "POST", "https://api.yescaptcha.com/createTask", strings.NewReader(string(payload)))
	if err != nil {
		return "", fmt.Errorf("YesCaptcha 构建请求失败: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("YesCaptcha 连接失败: %w", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	var createResp map[string]interface{}
	if err := json.Unmarshal(body, &createResp); err != nil {
		return "", fmt.Errorf("YesCaptcha 响应解析失败: %w", err)
	}
	taskID, ok := createResp["taskId"].(string)
	if !ok || taskID == "" {
		return "", fmt.Errorf("YesCaptcha 无 taskId: %s", string(body))
	}

	logf("[*] 云端B 任务提交啦，等等我...")
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case <-time.After(5 * time.Second):
	}

	for j := 0; j < 60; j++ {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		pollPayload, _ := json.Marshal(map[string]interface{}{
			"clientKey": s.yesCaptchaKey,
			"taskId":    taskID,
		})
		pollReq, _ := http.NewRequestWithContext(ctx, "POST", "https://api.yescaptcha.com/getTaskResult", strings.NewReader(string(pollPayload)))
		pollReq.Header.Set("Content-Type", "application/json")
		resp, err := s.httpClient.Do(pollReq)
		if err != nil {
			logf("[*] 云端B 请求失败，重试中...")
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(3 * time.Second):
			}
			continue
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		var result map[string]interface{}
		if err := json.Unmarshal(body, &result); err != nil {
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(3 * time.Second):
			}
			continue
		}

		status, _ := result["status"].(string)
		if j%5 == 0 {
			logf("[*] 云端B 等等我马上就好... (%d/60)", j+1)
		}
		if status == "ready" {
			if solution, ok := result["solution"].(map[string]interface{}); ok {
				if token, ok := solution["token"].(string); ok && token != "" {
					return token, nil
				}
			}
		}
		if status == "failed" {
			return "", fmt.Errorf("YesCaptcha 任务失败")
		}

		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
	return "", fmt.Errorf("YesCaptcha 轮询超时")
}

// ── 内置脚本 ──

// solveBuiltinScript 内置浏览器脚本求解（最后手段）
func (s *Solver) solveBuiltinScript(ctx context.Context, siteURL, siteKey string) (string, error) {
	scriptPath := os.Getenv("TURNSTILE_SOLVER_SCRIPT")
	if scriptPath == "" {
		scriptPath = os.Getenv("CAMOUFOX_SOLVER_SCRIPT")
	}
	if scriptPath == "" {
		execDir := filepath.Dir(os.Args[0])
		candidates := []string{
			"scripts/solve_turnstile.py",
			filepath.Join(execDir, "scripts", "solve_turnstile.py"),
			filepath.Join(execDir, "..", "scripts", "solve_turnstile.py"),
		}
		for _, p := range candidates {
			if _, err := os.Stat(p); err == nil {
				scriptPath = p
				break
			}
		}
	}
	if scriptPath == "" {
		return "", fmt.Errorf("内置 solver 脚本未找到")
	}

	scriptCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()

	cmd := exec.CommandContext(scriptCtx, "python3", scriptPath, siteURL, siteKey, "45")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("内置 solver 失败: %w\n%s", err, string(output))
	}

	for _, line := range strings.Split(string(output), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "TOKEN:") {
			return strings.TrimPrefix(line, "TOKEN:"), nil
		}
		if strings.HasPrefix(line, "ERROR:") {
			return "", fmt.Errorf("内置 solver: %s", strings.TrimPrefix(line, "ERROR:"))
		}
	}

	return "", fmt.Errorf("内置 solver 无 token")
}
