package main

import (
	"context"
	"flag"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gin-contrib/cors"
	"github.com/gin-gonic/gin"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"

	"github.com/grok-fireworks-reg/internal/config"
	"github.com/grok-fireworks-reg/internal/handler"
	"github.com/grok-fireworks-reg/internal/middleware"
	"github.com/grok-fireworks-reg/web"
)

func main() {
	// 命令行参数
	cfgFile := flag.String("config", "", "配置文件路径")
	flag.Parse()

	// 初始化 zerolog
	zerolog.TimeFieldFormat = zerolog.TimeFormatUnix
	log.Logger = zerolog.New(zerolog.ConsoleWriter{Out: os.Stdout, TimeFormat: "15:04:05"}).
		With().Timestamp().Caller().Logger()

	// 加载配置
	cfg := config.Load(*cfgFile)
	log.Info().Int("port", cfg.Server.Port).Msg("配置加载完成")

	// 初始化 JWT
	middleware.SetJWTSecret(cfg.Auth.JWTSecret)

	// 初始化 gin
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(gin.Recovery())

	// CORS 中间件
	r.Use(cors.New(cors.Config{
		AllowOrigins:     []string{"*"},
		AllowMethods:     []string{"GET", "POST", "OPTIONS"},
		AllowHeaders:     []string{"Origin", "Content-Type", "Authorization"},
		ExposeHeaders:    []string{"Content-Length"},
		AllowCredentials: true,
		MaxAge:           12 * time.Hour,
	}))

	// 注册处理器
	authHandler := handler.NewAuthHandler(cfg)
	grokHandler := handler.NewGrokHandler(cfg)
	fireworksHandler := handler.NewFireworksHandler(cfg)
	settingsHandler := handler.NewSettingsHandler(cfg)

	// API 路由
	api := r.Group("/api")
	{
		// 无需认证的路由
		api.GET("/health", func(c *gin.Context) {
			c.JSON(http.StatusOK, gin.H{
				"status":    "ok",
				"grok":      "ready",
				"fireworks": "ready",
			})
		})
		api.POST("/auth/login", authHandler.Login)

		// 需要认证的路由
		protected := api.Group("")
		protected.Use(middleware.AuthRequired())
		{
			protected.GET("/auth/me", authHandler.Me)
			protected.POST("/auth/credentials", authHandler.ChangeCredentials)
			protected.POST("/grok/register", grokHandler.Register)
			protected.POST("/fireworks/register", fireworksHandler.Register)

			settings := protected.Group("/settings")
			{
				settings.GET("/mail", settingsHandler.GetMailSettings)
				settings.POST("/mail", settingsHandler.UpdateMailSettings)
				settings.GET("/proxy", settingsHandler.GetProxySettings)
				settings.POST("/proxy", settingsHandler.UpdateProxySettings)
			}
		}
	}

	// 嵌入式前端静态文件
	staticFS, _ := fs.Sub(web.StaticFS, ".")
	r.GET("/", func(c *gin.Context) {
		c.FileFromFS("index.html", http.FS(staticFS))
	})
	// SPA fallback：非 API 路径都返回 index.html
	r.NoRoute(func(c *gin.Context) {
		c.FileFromFS("index.html", http.FS(staticFS))
	})

	// 启动 HTTP 服务
	addr := fmt.Sprintf(":%d", cfg.Server.Port)
	srv := &http.Server{
		Addr:         addr,
		Handler:      r,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 0, // SSE 长连接不设超时
		IdleTimeout:  120 * time.Second,
	}

	go func() {
		log.Info().Str("addr", addr).Msg("服务启动")
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatal().Err(err).Msg("服务异常退出")
		}
	}()

	// 优雅关闭
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	log.Info().Msg("正在关闭服务...")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Fatal().Err(err).Msg("服务关闭失败")
	}
	log.Info().Msg("服务已停止")
}
