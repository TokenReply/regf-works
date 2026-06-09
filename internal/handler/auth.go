package handler

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/grok-fireworks-reg/internal/config"
	"github.com/grok-fireworks-reg/internal/middleware"
)

// AuthHandler 认证处理器
type AuthHandler struct {
	cfg *config.Config
}

// NewAuthHandler 创建 AuthHandler
func NewAuthHandler(cfg *config.Config) *AuthHandler {
	return &AuthHandler{cfg: cfg}
}

// LoginRequest 登录请求体
type LoginRequest struct {
	Username string `json:"username" binding:"required"`
	Password string `json:"password" binding:"required"`
}

// Login POST /api/auth/login — 用户登录
func (h *AuthHandler) Login(c *gin.Context) {
	var req LoginRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请输入用户名和密码"})
		return
	}

	if req.Username != h.cfg.Auth.Username || req.Password != h.cfg.Auth.Password {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "用户名或密码错误"})
		return
	}

	token, err := middleware.GenerateToken(req.Username)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "生成 Token 失败"})
		return
	}

	// 设置 cookie 方便浏览器直接访问
	c.SetCookie("token", token, 72*3600, "/", "", false, true)
	c.JSON(http.StatusOK, gin.H{"ok": true, "token": token})
}

// Me GET /api/auth/me — 获取当前用户信息
func (h *AuthHandler) Me(c *gin.Context) {
	username, _ := c.Get("username")
	c.JSON(http.StatusOK, gin.H{"username": username})
}

// ChangeCredentialsRequest 修改账号密码请求体
type ChangeCredentialsRequest struct {
	NewUsername string `json:"new_username"`
	NewPassword string `json:"new_password"`
}

// ChangeCredentials POST /api/auth/credentials — 修改用户名和密码
// 修改成功后当前 token 失效，前端需要重新登录
func (h *AuthHandler) ChangeCredentials(c *gin.Context) {
	var req ChangeCredentialsRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请求格式错误"})
		return
	}

	if req.NewUsername == "" && req.NewPassword == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请提供新用户名或新密码"})
		return
	}

	// 更新凭据
	if req.NewUsername != "" {
		h.cfg.Auth.Username = req.NewUsername
	}
	if req.NewPassword != "" {
		if len(req.NewPassword) < 6 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "密码至少 6 个字符"})
			return
		}
		h.cfg.Auth.Password = req.NewPassword
	}

	// 更换 JWT Secret 使所有现有 token 失效
	middleware.RotateSecret()

	// 清除 cookie
	c.SetCookie("token", "", -1, "/", "", false, true)

	c.JSON(http.StatusOK, gin.H{
		"ok":      true,
		"message": "账号信息已更新，请重新登录",
		"logout":  true,
	})
}
