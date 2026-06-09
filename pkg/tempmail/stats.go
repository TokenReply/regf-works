package tempmail

import (
	"math/rand"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog/log"
)

// providerStat 单个 provider 的成功/失败原子计数
type providerStat struct {
	success atomic.Int64
	fail    atomic.Int64
}

// statsTracker 全局 provider 成功率追踪器
// 跨所有 MultiProvider 实例共享，用于按亲和度加权选择 provider
type statsTracker struct {
	stats map[string]*providerStat
	mu    sync.RWMutex
}

// globalTracker 包级单例
var globalTracker = &statsTracker{
	stats: make(map[string]*providerStat),
}

func init() {
	// 每 10 分钟衰减一次计数器，让历史表现差的 provider 更快恢复
	go func() {
		ticker := time.NewTicker(10 * time.Minute)
		for range ticker.C {
			globalTracker.decay()
		}
	}()
}

// RecordSuccess 记录某 provider 一次成功（验证码到达 / 注册成功）
func RecordSuccess(providerName string) {
	if providerName == "" {
		return
	}
	s := globalTracker.getOrCreate(providerName)
	newSucc := s.success.Add(1)
	total := newSucc + s.fail.Load()
	log.Debug().Str("provider", providerName).
		Int64("success", newSucc).Int64("total", total).
		Msg("邮箱 provider 成功 +1")
}

// RecordFailure 记录某 provider 一次失败（验证码超时等明确邮箱问题）
func RecordFailure(providerName string) {
	if providerName == "" {
		return
	}
	s := globalTracker.getOrCreate(providerName)
	newFail := s.fail.Add(1)
	total := s.success.Load() + newFail
	log.Debug().Str("provider", providerName).
		Int64("fail", newFail).Int64("total", total).
		Msg("邮箱 provider 失败 +1")
}

// getOrCreate 获取或创建 provider 统计条目（双重检查锁）
func (t *statsTracker) getOrCreate(name string) *providerStat {
	t.mu.RLock()
	if s, ok := t.stats[name]; ok {
		t.mu.RUnlock()
		return s
	}
	t.mu.RUnlock()

	t.mu.Lock()
	defer t.mu.Unlock()
	if s, ok := t.stats[name]; ok {
		return s
	}
	s := &providerStat{}
	t.stats[name] = s
	return s
}

// getWeight 计算 provider 的选择权重（0.1 ~ 1.0）
//   - 无数据 → 0.5（给新 provider 公平机会）
//   - 样本 < 3 → 0.5（数据不足，不下结论）
//   - 正常 = success/(success+fail)，下限 0.1（不完全排除）
func (t *statsTracker) getWeight(name string) float64 {
	t.mu.RLock()
	s, ok := t.stats[name]
	t.mu.RUnlock()
	if !ok {
		return 0.5
	}
	succ := s.success.Load()
	fail := s.fail.Load()
	total := succ + fail
	if total < 3 {
		return 0.5
	}
	rate := float64(succ) / float64(total)
	if rate < 0.1 {
		return 0.1
	}
	return rate
}

// decay 衰减所有计数器（÷2），让 provider 有恢复窗口
// 使用 Add 负值代替 Load+Store，避免与并发 RecordSuccess/RecordFailure 竞争
func (t *statsTracker) decay() {
	t.mu.RLock()
	defer t.mu.RUnlock()
	for name, s := range t.stats {
		succ := s.success.Load()
		fail := s.fail.Load()
		if succ > 1 {
			s.success.Add(-(succ / 2))
		}
		if fail > 1 {
			s.fail.Add(-(fail / 2))
		}
		log.Debug().Str("provider", name).
			Int64("success", s.success.Load()).Int64("fail", s.fail.Load()).
			Msg("邮箱 provider 统计衰减")
	}
}

// weightedShuffle 按成功率加权排序 provider 列表
// 高成功率优先，加 0.15 随机抖动防止完全锁定到单一 provider
// 预计算权重再排序，确保比较函数满足传递性
func weightedShuffle(providers []EmailProvider) {
	weights := make([]float64, len(providers))
	for i, p := range providers {
		weights[i] = globalTracker.getWeight(p.Name()) + rand.Float64()*0.15
	}
	sort.SliceStable(providers, func(i, j int) bool {
		return weights[i] > weights[j]
	})
}

// GetProviderStats 返回所有 provider 的统计快照（调试/监控用）
func GetProviderStats() map[string][2]int64 {
	globalTracker.mu.RLock()
	defer globalTracker.mu.RUnlock()
	result := make(map[string][2]int64, len(globalTracker.stats))
	for name, s := range globalTracker.stats {
		result[name] = [2]int64{s.success.Load(), s.fail.Load()}
	}
	return result
}

// WeightedShuffleNames 按成功率加权排序 provider 名字列表（供远程服务模式使用）
// 高成功率优先，加 0.15 随机抖动防止完全锁定到单一 provider
func WeightedShuffleNames(names []string) {
	weights := make([]float64, len(names))
	for i, n := range names {
		weights[i] = globalTracker.getWeight(n) + rand.Float64()*0.15
	}
	sort.SliceStable(names, func(i, j int) bool {
		return weights[i] > weights[j]
	})
}
