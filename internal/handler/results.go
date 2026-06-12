package handler

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/grok-fireworks-reg/internal/common"
)

// ResultsHandler 结果管理处理器
type ResultsHandler struct {
	storage *common.ResultStorage
}

// NewResultsHandler 创建 ResultsHandler
func NewResultsHandler(storage *common.ResultStorage) *ResultsHandler {
	return &ResultsHandler{storage: storage}
}

// GetResults GET /api/results — 获取所有历史结果
func (h *ResultsHandler) GetResults(c *gin.Context) {
	results := h.storage.GetAll()
	c.JSON(http.StatusOK, results)
}

// ClearResults DELETE /api/results — 清空所有结果
func (h *ResultsHandler) ClearResults(c *gin.Context) {
	if err := h.storage.Clear(); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true, "message": "所有结果已清空"})
}

// DeleteBatch DELETE /api/results/batch — 批量删除指定索引的结果
// Body: {"indices": [0, 2, 5]}
func (h *ResultsHandler) DeleteBatch(c *gin.Context) {
	var req struct {
		Indices []int `json:"indices"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
		return
	}

	if err := h.storage.DeleteByIndices(req.Indices); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"ok":      true,
		"message": "已删除选中项",
		"count":   len(req.Indices),
	})
}

// DeleteByFilter DELETE /api/results/filter — 删除符合筛选条件的结果
// Body: {"platform": "grok", "status": "failed"}
func (h *ResultsHandler) DeleteByFilter(c *gin.Context) {
	var req struct {
		Platform string `json:"platform"`
		Status   string `json:"status"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
		return
	}

	if err := h.storage.DeleteByFilter(req.Platform, req.Status); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"ok":      true,
		"message": "已清空筛选结果",
	})
}
