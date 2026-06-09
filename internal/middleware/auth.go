package middleware

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
)

var jwtSecret []byte

// SetJWTSecret 设置 JWT 签名密钥
func SetJWTSecret(secret string) {
	jwtSecret = []byte(secret)
}

// RotateSecret 更换 JWT Secret，使所有现有 token 立即失效
func RotateSecret() {
	buf := make([]byte, 32)
	rand.Read(buf)
	jwtSecret = []byte(hex.EncodeToString(buf))
}

// GenerateToken 为用户生成 JWT token
func GenerateToken(username string) (string, error) {
	claims := jwt.MapClaims{
		"sub": username,
		"exp": time.Now().Add(72 * time.Hour).Unix(),
		"iat": time.Now().Unix(),
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return token.SignedString(jwtSecret)
}

// AuthRequired 验证 JWT token 的 Gin 中间件
func AuthRequired() gin.HandlerFunc {
	return func(c *gin.Context) {
		tokenStr := ""

		// 先检查 Authorization header
		auth := c.GetHeader("Authorization")
		if strings.HasPrefix(auth, "Bearer ") {
			tokenStr = strings.TrimPrefix(auth, "Bearer ")
		}

		// Cookie 作为 fallback
		if tokenStr == "" {
			tokenStr, _ = c.Cookie("token")
		}

		if tokenStr == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "未登录"})
			c.Abort()
			return
		}

		token, err := jwt.Parse(tokenStr, func(t *jwt.Token) (interface{}, error) {
			return jwtSecret, nil
		})
		if err != nil || !token.Valid {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "登录已过期"})
			c.Abort()
			return
		}

		if claims, ok := token.Claims.(jwt.MapClaims); ok {
			c.Set("username", claims["sub"])
		}
		c.Next()
	}
}
