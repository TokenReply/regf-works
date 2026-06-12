package tempmail

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/rs/zerolog/log"
)

// GPTMailPublicKeyFetcher 自动获取 GPTMail 公共密钥
// 每天北京时间 8:01 自动刷新，缓存 20 小时
type GPTMailPublicKeyFetcher struct {
	mu         sync.RWMutex
	publicKey  string
	fetchedAt  time.Time
	httpClient *http.Client
}

var (
	globalGPTMailKeyFetcher     *GPTMailPublicKeyFetcher
	globalGPTMailKeyFetcherOnce sync.Once
)

// GetGPTMailPublicKeyFetcher 获取全局公共密钥获取器单例
func GetGPTMailPublicKeyFetcher() *GPTMailPublicKeyFetcher {
	globalGPTMailKeyFetcherOnce.Do(func() {
		globalGPTMailKeyFetcher = &GPTMailPublicKeyFetcher{
			httpClient: &http.Client{Timeout: 15 * time.Second},
		}
		// 启动后台定时任务
		go globalGPTMailKeyFetcher.start()
	})
	return globalGPTMailKeyFetcher
}

// GetPublicKey 获取当前缓存的公共密钥（如果没有或过期则立即获取）
func (f *GPTMailPublicKeyFetcher) GetPublicKey() string {
	f.mu.RLock()
	key := f.publicKey
	fetchedAt := f.fetchedAt
	f.mu.RUnlock()

	// 检查是否需要刷新（缓存时间 20 小时）
	if key == "" || time.Since(fetchedAt) > 20*time.Hour {
		log.Info().Msg("GPTMail 公共密钥过期或未初始化，立即获取")
		f.fetch()
		f.mu.RLock()
		key = f.publicKey
		f.mu.RUnlock()
	}

	return key
}

// start 启动定时任务：每天北京时间 8:01 自动获取公共密钥
func (f *GPTMailPublicKeyFetcher) start() {
	// 首次启动立即获取
	f.fetch()

	// 定时任务：每隔 1 小时检查一次是否到了 8:00-8:59
	ticker := time.NewTicker(1 * time.Hour)
	defer ticker.Stop()

	for range ticker.C {
		now := time.Now()
		// 转换为北京时间（UTC+8）
		beijing := now.In(time.FixedZone("CST", 8*3600))
		hour, min, _ := beijing.Clock()

		// 北京时间 8:00 ~ 8:59 之间，且距离上次获取超过 20 小时
		if hour == 8 && time.Since(f.fetchedAt) > 20*time.Hour {
			log.Info().Int("hour", hour).Int("min", min).Msg("定时触发 GPTMail 公共密钥获取")
			f.fetch()
		}
	}
}

// fetch 从 GPTMail API 获取公共密钥
func (f *GPTMailPublicKeyFetcher) fetch() {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// 调用 GPTMail 公共密钥 API
	apiURL := "https://mail.chatgpt.org.uk/api/public-key-status?reveal=1"
	req, err := http.NewRequestWithContext(ctx, "GET", apiURL, nil)
	if err != nil {
		log.Error().Err(err).Msg("GPTMail: 创建请求失败")
		return
	}
	req.Header.Set("X-Public-Key-Reveal", "click")

	resp, err := f.httpClient.Do(req)
	if err != nil {
		log.Error().Err(err).Msg("GPTMail: 获取公共密钥失败")
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		log.Error().Int("status", resp.StatusCode).Msg("GPTMail: API 返回非 200")
		return
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Error().Err(err).Msg("GPTMail: 读取响应失败")
		return
	}

	// 解析 JSON 响应
	var apiResp struct {
		Success bool `json:"success"`
		Data    struct {
			Key              string `json:"key"`
			RemainingToday   int    `json:"remaining_today"`
			SecondsUntilReset int    `json:"seconds_until_reset"`
		} `json:"data"`
		Error string `json:"error"`
	}

	if err := json.Unmarshal(body, &apiResp); err != nil {
		log.Error().Err(err).Msg("GPTMail: 解析响应失败")
		return
	}

	if !apiResp.Success || apiResp.Data.Key == "" {
		log.Warn().Str("error", apiResp.Error).Msg("GPTMail: API 返回失败")
		return
	}

	f.mu.Lock()
	f.publicKey = apiResp.Data.Key
	f.fetchedAt = time.Now()
	f.mu.Unlock()

	log.Info().
		Str("key", maskGPTMailKey(apiResp.Data.Key)).
		Int("remaining", apiResp.Data.RemainingToday).
		Int("reset_in", apiResp.Data.SecondsUntilReset).
		Msg("GPTMail 公共密钥已更新")
}

// maskGPTMailKey 隐藏密钥中间部分（日志输出用）
func maskGPTMailKey(key string) string {
	if len(key) <= 8 {
		return "***"
	}
	return key[:6] + "***" + key[len(key)-4:]
}

// ─── 集成到 MultiProvider ───

// GetGPTMailAPIKey 获取 GPTMail API Key（优先用户配置，否则使用公共密钥）
func GetGPTMailAPIKey(configuredKey string) string {
	if configuredKey != "" {
		return configuredKey
	}
	// 使用自动获取的公共密钥
	fetcher := GetGPTMailPublicKeyFetcher()
	publicKey := fetcher.GetPublicKey()
	if publicKey != "" {
		log.Debug().Str("key", maskGPTMailKey(publicKey)).Msg("GPTMail: 使用自动获取的公共密钥")
	}
	return publicKey
}

