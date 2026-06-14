# 后台任务队列系统 Spec

## 目标

关闭浏览器窗口后注册任务继续运行，重新打开浏览器能看到进度和日志。

## 当前问题

```
浏览器 ──SSE──→ Go handler ──→ worker goroutine
  关闭浏览器 → SSE 断开 → c.Request.Context() 取消 → 所有任务中止
```

## 新架构

```
浏览器 ──POST /api/tasks──→ TaskManager.Submit() ──→ 返回 task_id
                                    │
                                    ▼
                            后台 goroutine（独立 context）
                            ├── worker 1 → 注册账号 1
                            ├── worker 2 → 注册账号 2
                            └── ...
                                    │
                                    ▼ 日志写入 ring buffer
浏览器 ──GET /api/tasks/:id/logs──→ SSE 推送日志（可断开重连）
浏览器 ──GET /api/tasks──→ 查看所有任务状态
浏览器 ──DELETE /api/tasks/:id──→ 取消任务
```

## 数据结构

### Task

```go
type Task struct {
    ID          string         `json:"id"`          // UUID
    Platform    string         `json:"platform"`    // grok/fireworks/openrouter/novita
    Status      string         `json:"status"`      // pending/running/completed/cancelled
    Count       int            `json:"count"`       // 注册总数
    Concurrency int            `json:"concurrency"` // 并发数
    Delay       int            `json:"delay"`       // 间隔秒数（novita）
    Progress    TaskProgress   `json:"progress"`    // 进度
    CreatedAt   time.Time      `json:"created_at"`
    
    // 内部使用
    cancel      context.CancelFunc
    logs        *RingBuffer        // 日志环形缓冲区（最近 1000 条）
    subscribers []chan string       // SSE 订阅者
    mu          sync.RWMutex
}

type TaskProgress struct {
    Total     int `json:"total"`
    Completed int `json:"completed"`
    Success   int `json:"success"`
    Failed    int `json:"failed"`
}
```

### RingBuffer（日志缓冲区）

```go
type RingBuffer struct {
    buf   []string
    size  int
    head  int
    count int
    mu    sync.RWMutex
}
```

- 固定大小 1000 条日志
- 新日志覆盖最旧的
- 支持从指定 offset 读取（SSE 重连时续传）

### TaskManager

```go
type TaskManager struct {
    tasks map[string]*Task   // task_id → Task
    mu    sync.RWMutex
}
```

## API 设计

### 1. 提交任务

```
POST /api/tasks
Content-Type: application/json
Authorization: Bearer <token>

{
    "platform": "novita",
    "count": 10,
    "concurrency": 1,
    "delay": 60,
    "email_provider": "ahem"
}

→ 200 OK
{
    "id": "550e8400-e29b-41d4-a716-446655440000",
    "status": "running"
}
```

### 2. 查看所有任务

```
GET /api/tasks
Authorization: Bearer <token>

→ 200 OK
[
    {
        "id": "550e8400...",
        "platform": "novita",
        "status": "running",
        "count": 10,
        "progress": {"total": 10, "completed": 3, "success": 2, "failed": 1},
        "created_at": "2026-06-14T10:00:00Z"
    }
]
```

### 3. 订阅任务日志（SSE）

```
GET /api/tasks/:id/logs?offset=0
Authorization: Bearer <token>

→ SSE stream
event: log
data: [*] 任务开始

event: log
data: [*] 邮箱: xxx@xxx.bond (via ahem)

event: result
data: {"ok":true,"email":"xxx","api_key":"sk-xxx"}

event: progress
data: {"total":10,"completed":1,"success":1,"failed":0}

event: done
data: {"status":"completed"}
```

- `offset` 参数：从第 N 条日志开始（重连续传）
- 断开重连时带上 `offset=已收到条数` 即可续传
- 任务完成后发 `done` 事件

### 4. 取消任务

```
DELETE /api/tasks/:id
Authorization: Bearer <token>

→ 200 OK
{"ok": true, "message": "任务已取消"}
```

### 5. 删除任务记录

```
DELETE /api/tasks/:id/record
Authorization: Bearer <token>

→ 200 OK
{"ok": true}
```

## 前端改造

### 提交流程

```
1. 用户点"开始注册"
2. POST /api/tasks → 拿到 task_id
3. 打开 SSE: GET /api/tasks/{task_id}/logs?offset=0
4. 实时显示日志和进度
5. 用户关闭浏览器 → SSE 断开，任务继续跑
6. 用户重新打开浏览器 → GET /api/tasks 查看活跃任务
7. 点击任务 → 重新 SSE 订阅日志（带 offset 续传）
```

### UI 变化

- 终端日志窗口增加"任务列表"视图
- 显示所有活跃/完成的任务
- 每个任务显示进度条 (3/10)
- 可以同时运行多个平台的任务

### 停止按钮

- "停止" → DELETE /api/tasks/:id（服务端取消）
- 不再依赖 AbortController 断开 SSE

## 兼容性

### 保留旧 API（过渡期）

```
POST /api/grok/register      → 内部转为 POST /api/tasks + SSE 订阅
POST /api/fireworks/register  → 同上
POST /api/openrouter/register → 同上
POST /api/novita/register     → 同上
```

旧 API 改为：
1. 创建 Task
2. 在当前 HTTP 连接上做 SSE 推送（和之前行为一致）
3. 但任务本身用独立 context，SSE 断开不取消任务

这样前端可以逐步迁移到新 API，旧 API 也能用。

## 文件变动清单

| 文件 | 操作 | 说明 |
|------|------|------|
| `internal/task/manager.go` | 新增 | TaskManager + Task + RingBuffer |
| `internal/handler/tasks.go` | 新增 | 任务 API handler |
| `internal/handler/grok.go` | 修改 | Register 改用 TaskManager |
| `internal/handler/fireworks.go` | 修改 | 同上 |
| `internal/handler/openrouter.go` | 修改 | 同上 |
| `internal/handler/novita.go` | 修改 | 同上 |
| `cmd/server/main.go` | 修改 | 初始化 TaskManager + 注册新路由 |
| `web/index.html` | 修改 | 前端改造 |

## 实施顺序

1. **Phase 1**：实现 TaskManager + 新 API（不改旧 API）
2. **Phase 2**：改造旧 handler 的 context（SSE 断开不取消任务）
3. **Phase 3**：前端迁移到新 API
4. **Phase 4**：删除旧 SSE 逻辑（可选）

## 资源考虑

- 日志 RingBuffer 1000 条 × 每条约 200 字节 = 200KB/任务
- 同时 10 个任务 = 2MB 内存，可接受
- 任务完成 1 小时后自动清理日志缓冲区
