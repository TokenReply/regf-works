package task

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
)

// ─── RingBuffer ───

// RingBuffer 固定大小的环形日志缓冲区
type RingBuffer struct {
	buf   []string
	size  int
	head  int
	count int
	mu    sync.RWMutex
}

func NewRingBuffer(size int) *RingBuffer {
	return &RingBuffer{
		buf:  make([]string, size),
		size: size,
	}
}

// Write 写入一条日志
func (r *RingBuffer) Write(msg string) {
	r.mu.Lock()
	r.buf[r.head] = msg
	r.head = (r.head + 1) % r.size
	if r.count < r.size {
		r.count++
	}
	r.mu.Unlock()
}

// ReadFrom 从 offset 开始读取所有日志，返回日志和下一个 offset
func (r *RingBuffer) ReadFrom(offset int) ([]string, int) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	if offset >= r.count {
		return nil, r.count
	}

	var result []string
	start := 0
	if r.count == r.size {
		start = r.head // 环形缓冲区满了，最旧的在 head 位置
	}

	for i := offset; i < r.count; i++ {
		idx := (start + i) % r.size
		result = append(result, r.buf[idx])
	}
	return result, r.count
}

// Count 返回当前日志数
func (r *RingBuffer) Count() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.count
}

// ─── Task ───

type TaskStatus string

const (
	StatusPending   TaskStatus = "pending"
	StatusRunning   TaskStatus = "running"
	StatusCompleted TaskStatus = "completed"
	StatusCancelled TaskStatus = "cancelled"
)

type Progress struct {
	Total     int `json:"total"`
	Completed int `json:"completed"`
	Success   int `json:"success"`
	Failed    int `json:"failed"`
}

type Task struct {
	ID          string     `json:"id"`
	Platform    string     `json:"platform"`
	Status      TaskStatus `json:"status"`
	Count       int        `json:"count"`
	Concurrency int        `json:"concurrency"`
	Delay       int        `json:"delay"`
	Progress    Progress   `json:"progress"`
	CreatedAt   time.Time  `json:"created_at"`
	FinishedAt  *time.Time `json:"finished_at,omitempty"`

	// 内部
	cancel      context.CancelFunc
	logs        *RingBuffer
	subscribers []chan string
	subMu       sync.Mutex
	mu          sync.RWMutex
}

// LogWrite 写入日志并通知所有订阅者
func (t *Task) LogWrite(msg string) {
	t.logs.Write(msg)

	t.subMu.Lock()
	defer t.subMu.Unlock()
	for _, ch := range t.subscribers {
		select {
		case ch <- msg:
		default:
			// 订阅者处理不过来，跳过
		}
	}
}

// Subscribe 订阅实时日志，返回 channel 和取消函数
func (t *Task) Subscribe() (chan string, func()) {
	ch := make(chan string, 100)

	t.subMu.Lock()
	t.subscribers = append(t.subscribers, ch)
	t.subMu.Unlock()

	unsubscribe := func() {
		t.subMu.Lock()
		defer t.subMu.Unlock()
		for i, c := range t.subscribers {
			if c == ch {
				t.subscribers = append(t.subscribers[:i], t.subscribers[i+1:]...)
				close(ch)
				break
			}
		}
	}
	return ch, unsubscribe
}

// UpdateProgress 更新进度
func (t *Task) UpdateProgress(success bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.Progress.Completed++
	if success {
		t.Progress.Success++
	} else {
		t.Progress.Failed++
	}
}

// Finish 标记任务完成
func (t *Task) Finish(status TaskStatus) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.Status = status
	now := time.Now()
	t.FinishedAt = &now
}

// Logs 获取日志缓冲区
func (t *Task) Logs() *RingBuffer {
	return t.logs
}

// GetStatus 获取状态快照
func (t *Task) GetStatus() TaskStatus {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.Status
}

// GetProgress 获取进度快照
func (t *Task) GetProgress() Progress {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.Progress
}

// ─── TaskManager ───

// WorkerFunc 注册 worker 函数签名
// ctx: 任务 context（取消时 ctx.Done()）
// task: 任务对象（写日志、更新进度）
// config: 平台配置参数
type WorkerFunc func(ctx context.Context, task *Task, config map[string]interface{})

type Manager struct {
	tasks   map[string]*Task
	workers map[string]WorkerFunc // platform → worker
	mu      sync.RWMutex
}

func NewManager() *Manager {
	m := &Manager{
		tasks:   make(map[string]*Task),
		workers: make(map[string]WorkerFunc),
	}
	// 启动自动清理
	go m.autoCleanup()
	return m
}

// RegisterWorker 注册平台的 worker 函数
func (m *Manager) RegisterWorker(platform string, fn WorkerFunc) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.workers[platform] = fn
}

// Submit 提交新任务
func (m *Manager) Submit(platform string, count, concurrency, delay int, config map[string]interface{}) (*Task, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	worker, ok := m.workers[platform]
	if !ok {
		return nil, fmt.Errorf("unknown platform: %s", platform)
	}

	ctx, cancel := context.WithCancel(context.Background())

	task := &Task{
		ID:          uuid.New().String(),
		Platform:    platform,
		Status:      StatusRunning,
		Count:       count,
		Concurrency: concurrency,
		Delay:       delay,
		Progress:    Progress{Total: count},
		CreatedAt:   time.Now(),
		cancel:      cancel,
		logs:        NewRingBuffer(1000),
	}

	m.tasks[task.ID] = task

	// 启动后台 worker
	go func() {
		defer func() {
			if task.GetStatus() == StatusRunning {
				task.Finish(StatusCompleted)
			}
			task.LogWrite(fmt.Sprintf("[*] 任务结束: %d 成功, %d 失败",
				task.GetProgress().Success, task.GetProgress().Failed))
		}()

		worker(ctx, task, config)
	}()

	return task, nil
}

// Get 获取单个任务
func (m *Manager) Get(id string) *Task {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.tasks[id]
}

// List 获取所有任务（按时间倒序）
func (m *Manager) List() []*Task {
	m.mu.RLock()
	defer m.mu.RUnlock()

	list := make([]*Task, 0, len(m.tasks))
	for _, t := range m.tasks {
		list = append(list, t)
	}
	// 按创建时间倒序
	for i := 0; i < len(list)-1; i++ {
		for j := i + 1; j < len(list); j++ {
			if list[j].CreatedAt.After(list[i].CreatedAt) {
				list[i], list[j] = list[j], list[i]
			}
		}
	}
	return list
}

// Cancel 取消任务
func (m *Manager) Cancel(id string) error {
	m.mu.RLock()
	task, ok := m.tasks[id]
	m.mu.RUnlock()
	if !ok {
		return fmt.Errorf("task not found: %s", id)
	}
	if task.GetStatus() != StatusRunning {
		return fmt.Errorf("task not running: %s", task.GetStatus())
	}
	task.cancel()
	task.Finish(StatusCancelled)
	task.LogWrite("[*] 任务已取消")
	return nil
}

// Delete 删除任务记录
func (m *Manager) Delete(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	task, ok := m.tasks[id]
	if !ok {
		return fmt.Errorf("task not found: %s", id)
	}
	if task.GetStatus() == StatusRunning {
		task.cancel()
		task.Finish(StatusCancelled)
	}
	delete(m.tasks, id)
	return nil
}

// autoCleanup 定期清理已完成超过 2 小时的任务
func (m *Manager) autoCleanup() {
	ticker := time.NewTicker(30 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		m.mu.Lock()
		now := time.Now()
		for id, t := range m.tasks {
			if t.FinishedAt != nil && now.Sub(*t.FinishedAt) > 2*time.Hour {
				delete(m.tasks, id)
			}
		}
		m.mu.Unlock()
	}
}
