package tempmail

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog/log"
)

const (
	cooldownRateLimit = 6 * time.Hour    // 429 每日限额耗尽，长冷却
	cooldownAuthFail  = 15 * time.Minute // 401/403 认证失败/额度耗尽，冷却 15 分钟
	cooldownError     = 30 * time.Second // 其他临时错误冷却时间
)

// MultiProvider 多邮箱服务商自动切换
// 当主 provider 返回 429 或错误时，自动尝试下一个可用 provider
type MultiProvider struct {
	providers []EmailProvider
	cooldowns map[string]time.Time // provider name → 冷却截止时间
	mu        sync.RWMutex
}

// NewMultiProvider 根据 worker config 构建多 provider 实例
// 读取配置：
//   - email_provider_priority: 逗号分隔的优先级列表（如 "yydsmail"）
//   - yydsmail_base_url: YYDS Mail API 地址（默认 ""）
//   - yydsmail_api_key: YYDS Mail API Key（支持多行、逗号或分号分隔多个 key）
func NewMultiProvider(cfg map[string]string) *MultiProvider {
	priority := cfg["email_provider_priority"]
	if priority == "" {
		log.Warn().Msg("未配置 email_provider_priority，不会启用任何邮箱 provider")
	}

	// 构建可用 provider 映射（仅构建在 priority 列表中出现的 provider）
	available := make(map[string]EmailProvider)
	prioritySet := make(map[string]bool)
	for _, name := range strings.Split(priority, ",") {
		prioritySet[strings.TrimSpace(name)] = true
	}

	// YYDS Mail（自建临时邮箱服务）
	if prioritySet["yydsmail"] {
		yydsURL := cfg["yydsmail_base_url"]
		if yydsURL == "" {
			yydsURL = ""
		}
		yydsKey := cfg["yydsmail_api_key"]
		if len(parseYYDSMailAPIKeys(yydsKey)) > 0 {
			available["yydsmail"] = NewYYDSMailProvider(yydsURL, yydsKey)
		} else {
			log.Warn().Msg("yydsmail 在优先级列表中但未配置 yydsmail_api_key")
		}
	}

	// DuckMail（api.duckmail.sbs，dk_ key 授权）—— fireworks 验证通过，能领 $6
	if prioritySet["duckmail"] {
		duckURL := cfg["duckmail_base_url"]
		duckKey := cfg["duckmail_api_key"]
		duckDomain := cfg["duckmail_default_domain"]
		if duckKey != "" {
			available["duckmail"] = NewDuckMailProvider(duckURL, duckKey, duckDomain)
		} else {
			log.Warn().Msg("duckmail 在优先级列表中但未配置 duckmail_api_key")
		}
	}

	// Mail.tm（api.mail.tm，免费无 key，活跃域少且可能被风控整域拉黑）
	if prioritySet["mailtm"] {
		mailtmURL := cfg["mailtm_base_url"]
		available["mailtm"] = NewMailTMProvider(mailtmURL)
	}

	// TempMail.lol（api.tempmail.lol，免费无 key，多 SLD 随机分配）
	if prioritySet["tempmaillol"] {
		tmlURL := cfg["tempmaillol_base_url"]
		tmlKey := cfg["tempmaillol_api_key"]
		available["tempmaillol"] = NewTempMailLolProvider(tmlURL, tmlKey)
	}

	// AHEM（Ad-Hoc Email Server，自建服务，无需认证，任意前缀可收信）
	if prioritySet["ahem"] {
		ahemURL := cfg["ahem_base_url"]
		ahemDomains := cfg["ahem_domains"]
		if ahemURL != "" {
			available["ahem"] = NewAhemProvider(ahemURL, ahemDomains)
		} else {
			log.Warn().Msg("ahem 在优先级列表中但未配置 ahem_base_url")
		}
	}

	// GPTMail（mail.chatgpt.org.uk，API Key 授权，自动生成随机邮箱）
	if prioritySet["gptmail"] {
		gptURL := cfg["gptmail_base_url"]
		gptKey := cfg["gptmail_api_key"]
		if gptKey != "" {
			available["gptmail"] = NewGPTMailProvider(gptURL, gptKey)
		} else {
			log.Warn().Msg("gptmail 在优先级列表中但未配置 gptmail_api_key")
		}
	}

	// 按优先级排列（严格模式：仅使用优先级列表中的 provider）
	var providers []EmailProvider
	for _, name := range strings.Split(priority, ",") {
		name = strings.TrimSpace(name)
		if p, ok := available[name]; ok {
			providers = append(providers, p)
		}
	}

	// 按成功率加权排序（优先用亲和度高的 provider，加随机抖动防止锁死）
	weightedShuffle(providers)

	if len(providers) == 0 {
		log.Warn().Msg("未配置任何邮箱 provider，请检查系统设置")
	}

	return &MultiProvider{
		providers: providers,
		cooldowns: make(map[string]time.Time),
	}
}

// isAvailable 检查 provider 是否可用（未在冷却中）
func (mp *MultiProvider) isAvailable(name string) bool {
	mp.mu.RLock()
	defer mp.mu.RUnlock()
	if until, ok := mp.cooldowns[name]; ok {
		return time.Now().After(until)
	}
	return true
}

// markCooldown 标记 provider 进入冷却期（duration 区分 429 和普通错误）
func (mp *MultiProvider) markCooldown(name string, duration time.Duration) {
	mp.mu.Lock()
	defer mp.mu.Unlock()
	mp.cooldowns[name] = time.Now().Add(duration)
	log.Warn().Str("provider", name).Dur("cooldown", duration).
		Msg("邮箱服务进入冷却期，切换到下一个 provider")
}

// GenerateEmail 尝试所有可用 provider 创建邮箱
func (mp *MultiProvider) GenerateEmail(ctx context.Context) (string, map[string]string, error) {
	var errs []string
	for _, p := range mp.providers {
		if !mp.isAvailable(p.Name()) {
			continue
		}

		addr, meta, err := p.GenerateEmail(ctx)
		if err == nil {
			log.Debug().Str("provider", p.Name()).Str("email", addr).Msg("邮箱创建成功")
			return addr, meta, nil
		}

		// 429 → 长冷却
		if errors.Is(err, ErrRateLimited) {
			mp.markCooldown(p.Name(), cooldownRateLimit)
			errs = append(errs, fmt.Sprintf("%s: 429 限速", p.Name()))
			continue
		}

		// 401/403 → 中冷却，key 无效不会自愈
		if errors.Is(err, ErrAuthFailed) {
			mp.markCooldown(p.Name(), cooldownAuthFail)
			errs = append(errs, fmt.Sprintf("%s: 认证失败", p.Name()))
			log.Warn().Str("provider", p.Name()).Err(err).Msg("邮箱服务认证失败，冷却 15 分钟")
			continue
		}

		// 其他临时错误 → 短冷却（30 秒）
		mp.markCooldown(p.Name(), cooldownError)
		errs = append(errs, fmt.Sprintf("%s: %s", p.Name(), err.Error()))
		log.Warn().Str("provider", p.Name()).Err(err).Msg("邮箱创建失败，进入冷却并尝试下一个")
	}

	return "", nil, fmt.Errorf("所有邮箱服务均不可用: %s", strings.Join(errs, "; "))
}

// FetchVerificationCode 使用创建邮箱时的同一 provider 获取验证码
func (mp *MultiProvider) FetchVerificationCode(ctx context.Context, addr string, meta map[string]string, maxAttempts int, interval time.Duration) (string, error) {
	providerName := meta["provider"]
	for _, p := range mp.providers {
		if p.Name() == providerName {
			return p.FetchVerificationCode(ctx, addr, meta, maxAttempts, interval)
		}
	}
	return "", fmt.Errorf("未找到邮箱 provider: %s", providerName)
}

// DeleteEmail 使用创建邮箱时的同一 provider 删除邮箱
func (mp *MultiProvider) DeleteEmail(ctx context.Context, addr string, meta map[string]string) error {
	providerName := meta["provider"]
	for _, p := range mp.providers {
		if p.Name() == providerName {
			return p.DeleteEmail(ctx, addr, meta)
		}
	}
	return nil // provider 找不到，静默返回
}

// ProviderCount 返回可用 provider 数量
func (mp *MultiProvider) ProviderCount() int {
	return len(mp.providers)
}

// ProviderNames 返回所有 provider 名称（按优先级排列）
func (mp *MultiProvider) ProviderNames() []string {
	names := make([]string, len(mp.providers))
	for i, p := range mp.providers {
		names[i] = p.Name()
	}
	return names
}
