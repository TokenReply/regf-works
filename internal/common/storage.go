package common

import (
	"encoding/json"
	"os"
	"sync"
)

// ResultStorage 结果持久化存储
type ResultStorage struct {
	filePath string
	mu       sync.RWMutex
	results  []RegisterResult
}

// NewResultStorage 创建结果存储
func NewResultStorage(filePath string) *ResultStorage {
	s := &ResultStorage{
		filePath: filePath,
		results:  []RegisterResult{},
	}
	s.Load()
	return s
}

// Load 从文件加载历史结果
func (s *ResultStorage) Load() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	data, err := os.ReadFile(s.filePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // 文件不存在，忽略
		}
		return err
	}

	return json.Unmarshal(data, &s.results)
}

// Save 保存当前结果到文件
func (s *ResultStorage) Save() error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.saveLocked()
}

// saveLocked 保存（调用方已持有锁）
func (s *ResultStorage) saveLocked() error {
	data, err := json.MarshalIndent(s.results, "", "  ")
	if err != nil {
		return err
	}

	dir := "data"
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}

	return os.WriteFile(s.filePath, data, 0644)
}

// Append 追加新结果并持久化
func (s *ResultStorage) Append(result RegisterResult) error {
	s.mu.Lock()
	s.results = append([]RegisterResult{result}, s.results...)
	err := s.saveLocked()
	s.mu.Unlock()
	return err
}

// GetAll 获取所有结果
func (s *ResultStorage) GetAll() []RegisterResult {
	s.mu.RLock()
	defer s.mu.RUnlock()

	cp := make([]RegisterResult, len(s.results))
	for i, r := range s.results {
		cp[i] = r
	}
	return cp
}

// Clear 清空所有结果
func (s *ResultStorage) Clear() error {
	s.mu.Lock()
	s.results = []RegisterResult{}
	err := s.saveLocked()
	s.mu.Unlock()
	return err
}

// DeleteByIndices 删除指定索引的结果
func (s *ResultStorage) DeleteByIndices(indices []int) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	toDelete := make(map[int]bool)
	for _, idx := range indices {
		if idx >= 0 && idx < len(s.results) {
			toDelete[idx] = true
		}
	}

	filtered := []RegisterResult{}
	for i, r := range s.results {
		if !toDelete[i] {
			filtered = append(filtered, r)
		}
	}
	s.results = filtered

	return s.saveLocked()
}

// DeleteByFilter 删除符合筛选条件的结果
func (s *ResultStorage) DeleteByFilter(platform, status string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	filtered := []RegisterResult{}
	for _, r := range s.results {
		match := true
		if platform != "" && r.Platform != platform {
			match = false
		}
		if status != "" && r.Status != status {
			match = false
		}
		if !match {
			filtered = append(filtered, r)
		}
	}
	s.results = filtered

	return s.saveLocked()
}
