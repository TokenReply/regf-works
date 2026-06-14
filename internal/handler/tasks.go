package handler

import (
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/grok-fireworks-reg/internal/task"
)

// TasksHandler 任务管理 API
type TasksHandler struct {
	mgr *task.Manager
}

func NewTasksHandler(mgr *task.Manager) *TasksHandler {
	return &TasksHandler{mgr: mgr}
}

// SubmitTask POST /api/tasks — 提交新任务
func (h *TasksHandler) SubmitTask(c *gin.Context) {
	var req struct {
		Platform      string `json:"platform"`
		Count         int    `json:"count"`
		Concurrency   int    `json:"concurrency"`
		Delay         int    `json:"delay"`
		EmailProvider string `json:"email_provider"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
		return
	}
	if req.Platform == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "platform required"})
		return
	}
	if req.Count <= 0 {
		req.Count = 1
	}
	if req.Concurrency <= 0 {
		req.Concurrency = 1
	}

	config := map[string]interface{}{
		"email_provider": req.EmailProvider,
	}

	t, err := h.mgr.Submit(req.Platform, req.Count, req.Concurrency, req.Delay, config)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"id":       t.ID,
		"status":   t.Status,
		"platform": t.Platform,
	})
}

// ListTasks GET /api/tasks — 列出所有任务
func (h *TasksHandler) ListTasks(c *gin.Context) {
	tasks := h.mgr.List()

	type taskView struct {
		ID         string        `json:"id"`
		Platform   string        `json:"platform"`
		Status     string        `json:"status"`
		Count      int           `json:"count"`
		Progress   task.Progress `json:"progress"`
		CreatedAt  string        `json:"created_at"`
		FinishedAt string        `json:"finished_at,omitempty"`
	}

	views := make([]taskView, 0, len(tasks))
	for _, t := range tasks {
		v := taskView{
			ID:        t.ID,
			Platform:  t.Platform,
			Status:    string(t.GetStatus()),
			Count:     t.Count,
			Progress:  t.GetProgress(),
			CreatedAt: t.CreatedAt.Format("2006-01-02 15:04:05"),
		}
		if t.FinishedAt != nil {
			v.FinishedAt = t.FinishedAt.Format("2006-01-02 15:04:05")
		}
		views = append(views, v)
	}

	c.JSON(http.StatusOK, views)
}

// GetTaskLogs GET /api/tasks/:id/logs — SSE 推送日志（支持断点续传）
func (h *TasksHandler) GetTaskLogs(c *gin.Context) {
	id := c.Param("id")
	t := h.mgr.Get(id)
	if t == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "task not found"})
		return
	}

	// offset 参数：从第 N 条日志开始
	offset, _ := strconv.Atoi(c.DefaultQuery("offset", "0"))

	c.Writer.Header().Set("Content-Type", "text/event-stream")
	c.Writer.Header().Set("Cache-Control", "no-cache")
	c.Writer.Header().Set("Connection", "keep-alive")
	c.Writer.Header().Set("X-Accel-Buffering", "no")
	c.Writer.WriteHeaderNow()

	writeSSE := func(event, data string) {
		fmt.Fprintf(c.Writer, "event: %s\ndata: %s\n\n", event, data)
		c.Writer.Flush()
	}

	// 1. 先发送历史日志
	history, nextOffset := t.Logs().ReadFrom(offset)
	for _, msg := range history {
		writeSSE("log", msg)
	}
	_ = nextOffset

	// 发送当前进度
	p := t.GetProgress()
	writeSSE("progress", fmt.Sprintf(`{"total":%d,"completed":%d,"success":%d,"failed":%d}`,
		p.Total, p.Completed, p.Success, p.Failed))

	// 2. 如果任务已结束，发 done 并关闭
	if t.GetStatus() != task.StatusRunning {
		writeSSE("done", fmt.Sprintf(`{"status":"%s"}`, t.GetStatus()))
		return
	}

	// 3. 订阅实时日志
	ch, unsub := t.Subscribe()
	defer unsub()

	ctx := c.Request.Context()
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case msg, ok := <-ch:
			if !ok {
				// channel 关闭，任务结束
				writeSSE("done", fmt.Sprintf(`{"status":"%s"}`, t.GetStatus()))
				return
			}
			writeSSE("log", msg)

		case <-ticker.C:
			// 定期发送进度
			p := t.GetProgress()
			writeSSE("progress", fmt.Sprintf(`{"total":%d,"completed":%d,"success":%d,"failed":%d}`,
				p.Total, p.Completed, p.Success, p.Failed))

			// 检查任务是否结束
			if t.GetStatus() != task.StatusRunning {
				writeSSE("done", fmt.Sprintf(`{"status":"%s"}`, t.GetStatus()))
				return
			}

		case <-ctx.Done():
			// 浏览器断开 — 任务不取消，只是停止推送
			return
		}
	}
}

// CancelTask DELETE /api/tasks/:id — 取消任务
func (h *TasksHandler) CancelTask(c *gin.Context) {
	id := c.Param("id")
	if err := h.mgr.Cancel(id); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true, "message": "任务已取消"})
}

// DeleteTask DELETE /api/tasks/:id/record — 删除任务记录
func (h *TasksHandler) DeleteTask(c *gin.Context) {
	id := c.Param("id")
	if err := h.mgr.Delete(id); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}
