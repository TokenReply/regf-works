package tempmail

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog/log"
)

// DomainBlacklist 平台级邮箱域名黑名单管理器
// 当某个邮箱域名导致注册失败（HTTP 400 等）时，将其拉黑避免重复使用
// 特性：平台隔离、自动过期、持久化到 JSON 文件
type DomainBlacklist struct {
	platform string                 // "grok" 或 "fireworks"
	domains  map[string]time.Time   // domain → banned time
	mu       sync.RWMutex
	ttl      time.Duration          // 黑名单条目过期时间（默认 2 小时）
	filePath string                 // 持久化文件路径
}

// allBlacklists 全局黑名单持久化数据结构
type allBlacklists struct {
	Grok      map[string]time.Time `json:"grok"`
	Fireworks map[string]time.Time `json:"fireworks"`
}

// NewDomainBlacklist 创建域名黑名单实例
// platform: "grok" 或 "fireworks"
// ttl: 黑名单条目过期时间（推荐 2 小时）
// filePath: 持久化文件路径（如 "data/blacklist.json"）
func NewDomainBlacklist(platform string, ttl time.Duration, filePath string) *DomainBlacklist {
	b := &DomainBlacklist{
		platform: strings.ToLower(platform),
		domains:  make(map[string]time.Time),
		ttl:      ttl,
		filePath: filePath,
	}

	// 启动时加载持久化数据
	if err := b.load(); err != nil {
		log.Warn().Err(err).Str("platform", platform).Msg("黑名单加载失败，使用空黑名单")
	}

	// 启动定期清理和保存任务
	go b.autoCleanAndSave()

	return b
}

// Ban 将域名加入黑名单
func (b *DomainBlacklist) Ban(domain string) {
	domain = normalizeDomain(domain)
	if domain == "" {
		return
	}

	b.mu.Lock()
	b.domains[domain] = time.Now()
	b.mu.Unlock()

	log.Info().Str("platform", b.platform).Str("domain", domain).Msg("域名已拉黑")

	// 异步保存到文件
	go b.save()
}

// IsBanned 检查域名是否在黑名单中（自动清理过期条目）
func (b *DomainBlacklist) IsBanned(domain string) bool {
	domain = normalizeDomain(domain)
	if domain == "" {
		return false
	}

	b.mu.RLock()
	bannedAt, ok := b.domains[domain]
	b.mu.RUnlock()

	if !ok {
		return false
	}

	// 检查是否过期
	if time.Since(bannedAt) > b.ttl {
		b.mu.Lock()
		delete(b.domains, domain)
		b.mu.Unlock()
		log.Debug().Str("platform", b.platform).Str("domain", domain).Msg("黑名单条目已过期并清除")
		return false
	}

	return true
}

// GetAll 获取所有黑名单条目（供 API 查询）
// 返回 map[domain]bannedTime
func (b *DomainBlacklist) GetAll() map[string]time.Time {
	b.mu.RLock()
	defer b.mu.RUnlock()

	result := make(map[string]time.Time, len(b.domains))
	for domain, bannedAt := range b.domains {
		// 只返回未过期的条目
		if time.Since(bannedAt) <= b.ttl {
			result[domain] = bannedAt
		}
	}
	return result
}

// Clear 清空黑名单
func (b *DomainBlacklist) Clear() {
	b.mu.Lock()
	b.domains = make(map[string]time.Time)
	b.mu.Unlock()

	log.Info().Str("platform", b.platform).Msg("黑名单已清空")

	// 保存到文件
	go b.save()
}

// CleanExpired 清理所有过期条目
func (b *DomainBlacklist) CleanExpired() {
	b.mu.Lock()
	defer b.mu.Unlock()

	now := time.Now()
	cleaned := 0
	for domain, bannedAt := range b.domains {
		if now.Sub(bannedAt) > b.ttl {
			delete(b.domains, domain)
			cleaned++
		}
	}

	if cleaned > 0 {
		log.Debug().Str("platform", b.platform).Int("count", cleaned).Msg("已清理过期黑名单条目")
	}
}

// save 保存黑名单到 JSON 文件（异步调用，内部处理错误）
func (b *DomainBlacklist) save() {
	if b.filePath == "" {
		return
	}

	// 确保目录存在
	dir := filepath.Dir(b.filePath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		log.Error().Err(err).Str("dir", dir).Msg("创建黑名单目录失败")
		return
	}

	// 读取现有文件（合并所有平台的数据）
	all := allBlacklists{
		Grok:      make(map[string]time.Time),
		Fireworks: make(map[string]time.Time),
	}

	if data, err := os.ReadFile(b.filePath); err == nil {
		_ = json.Unmarshal(data, &all)
	}

	// 更新当前平台的数据
	b.mu.RLock()
	currentData := make(map[string]time.Time, len(b.domains))
	for k, v := range b.domains {
		currentData[k] = v
	}
	b.mu.RUnlock()

	switch b.platform {
	case "grok":
		all.Grok = currentData
	case "fireworks":
		all.Fireworks = currentData
	}

	// 写入文件
	data, err := json.MarshalIndent(all, "", "  ")
	if err != nil {
		log.Error().Err(err).Msg("序列化黑名单失败")
		return
	}

	if err := os.WriteFile(b.filePath, data, 0644); err != nil {
		log.Error().Err(err).Str("file", b.filePath).Msg("保存黑名单失败")
		return
	}

	log.Debug().Str("platform", b.platform).Str("file", b.filePath).Msg("黑名单已保存")
}

// load 从 JSON 文件加载黑名单
func (b *DomainBlacklist) load() error {
	if b.filePath == "" {
		return nil
	}

	data, err := os.ReadFile(b.filePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // 文件不存在是正常情况
		}
		return err
	}

	var all allBlacklists
	if err := json.Unmarshal(data, &all); err != nil {
		return err
	}

	// 加载当前平台的数据
	var platformData map[string]time.Time
	switch b.platform {
	case "grok":
		platformData = all.Grok
	case "fireworks":
		platformData = all.Fireworks
	}

	if platformData == nil {
		return nil
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	// 只加载未过期的条目
	now := time.Now()
	loaded := 0
	for domain, bannedAt := range platformData {
		if now.Sub(bannedAt) <= b.ttl {
			b.domains[domain] = bannedAt
			loaded++
		}
	}

	if loaded > 0 {
		log.Info().Str("platform", b.platform).Int("count", loaded).Msg("黑名单已加载")
	}

	return nil
}

// autoCleanAndSave 定期清理过期条目并保存（后台任务）
func (b *DomainBlacklist) autoCleanAndSave() {
	ticker := time.NewTicker(10 * time.Minute)
	defer ticker.Stop()

	for range ticker.C {
		b.CleanExpired()
		b.save()
	}
}

// normalizeDomain 标准化域名（小写、去空格）
func normalizeDomain(domain string) string {
	domain = strings.ToLower(strings.TrimSpace(domain))
	// 去除可能的协议前缀
	domain = strings.TrimPrefix(domain, "http://")
	domain = strings.TrimPrefix(domain, "https://")
	return domain
}

// ExtractDomain 从邮箱地址提取域名部分
func ExtractDomain(email string) string {
	parts := strings.SplitN(email, "@", 2)
	if len(parts) != 2 {
		return ""
	}
	return normalizeDomain(parts[1])
}
