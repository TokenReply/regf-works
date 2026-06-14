package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/rs/zerolog/log"

	"github.com/grok-fireworks-reg/internal/common"
	"github.com/grok-fireworks-reg/internal/config"
	"github.com/grok-fireworks-reg/internal/fireworks"
	"github.com/grok-fireworks-reg/internal/grok"
	"github.com/grok-fireworks-reg/internal/novita"
	"github.com/grok-fireworks-reg/internal/openrouter"
	"github.com/grok-fireworks-reg/internal/task"
)

// RegisterWorkers 注册所有平台的 worker 到 TaskManager
func RegisterWorkers(mgr *task.Manager, cfg *config.Config, storage *common.ResultStorage) {
	mgr.RegisterWorker("grok", makeGrokWorker(cfg, storage))
	mgr.RegisterWorker("fireworks", makeFireworksWorker(cfg, storage))
	mgr.RegisterWorker("openrouter", makeOpenRouterWorker(cfg, storage))
	mgr.RegisterWorker("novita", makeNovitaWorker(cfg, storage))
}

func makeGrokWorker(cfg *config.Config, storage *common.ResultStorage) task.WorkerFunc {
	return func(ctx context.Context, t *task.Task, params map[string]interface{}) {
		workerCfg := cfg.ToGrokConfig()
		if ep, ok := params["email_provider"].(string); ok && ep != "" {
			workerCfg["email_provider_priority"] = ep
		}

		// ScanConfig
		t.LogWrite("[*] 扫描注册页配置...")
		scanned, err := grok.ScanConfig(ctx, cfg.GetDefaultProxy(), workerCfg)
		if err != nil {
			t.LogWrite("[-] ScanConfig 失败: " + err.Error())
		} else {
			for k, v := range scanned {
				workerCfg[k] = v
			}
		}

		runSequential(ctx, t, cfg, storage, "grok", workerCfg, func(ctx context.Context, proxy *common.ProxyEntry, logCh chan<- string) *common.RegisterResult {
			return grok.Register(ctx, grok.RegisterOpts{
				Proxy: proxy, Config: workerCfg, LogCh: logCh,
			})
		})
	}
}

func makeFireworksWorker(cfg *config.Config, storage *common.ResultStorage) task.WorkerFunc {
	return func(ctx context.Context, t *task.Task, params map[string]interface{}) {
		workerCfg := cfg.ToFireworksConfig()
		if ep, ok := params["email_provider"].(string); ok && ep != "" {
			workerCfg["email_provider_priority"] = ep
		}
		runSequential(ctx, t, cfg, storage, "fireworks", workerCfg, func(ctx context.Context, proxy *common.ProxyEntry, logCh chan<- string) *common.RegisterResult {
			return fireworks.Register(ctx, fireworks.RegisterOpts{
				Proxy: proxy, Config: workerCfg, LogCh: logCh,
			})
		})
	}
}

func makeOpenRouterWorker(cfg *config.Config, storage *common.ResultStorage) task.WorkerFunc {
	return func(ctx context.Context, t *task.Task, params map[string]interface{}) {
		workerCfg := cfg.ToOpenRouterConfig()
		if ep, ok := params["email_provider"].(string); ok && ep != "" {
			workerCfg["email_provider_priority"] = ep
		}
		runSequential(ctx, t, cfg, storage, "openrouter", workerCfg, func(ctx context.Context, proxy *common.ProxyEntry, logCh chan<- string) *common.RegisterResult {
			return openrouter.Register(ctx, openrouter.RegisterOpts{
				Proxy: proxy, Config: workerCfg, LogCh: logCh,
			})
		})
	}
}

func makeNovitaWorker(cfg *config.Config, storage *common.ResultStorage) task.WorkerFunc {
	return func(ctx context.Context, t *task.Task, params map[string]interface{}) {
		workerCfg := cfg.ToNovitaConfig()
		if ep, ok := params["email_provider"].(string); ok && ep != "" {
			workerCfg["email_provider_priority"] = ep
		}
		runSequential(ctx, t, cfg, storage, "novita", workerCfg, func(ctx context.Context, proxy *common.ProxyEntry, logCh chan<- string) *common.RegisterResult {
			return novita.Register(ctx, novita.RegisterOpts{
				Proxy: proxy, Config: workerCfg, LogCh: logCh,
			})
		})
	}
}

// registerFunc 注册单个账号的函数签名
type registerFunc func(ctx context.Context, proxy *common.ProxyEntry, logCh chan<- string) *common.RegisterResult

// runSequential 顺序执行注册任务（支持并发和延迟）
func runSequential(ctx context.Context, t *task.Task, cfg *config.Config, storage *common.ResultStorage,
	platform string, workerCfg common.Config, regFn registerFunc) {

	semaphore := make(chan struct{}, t.Concurrency)

	for i := 0; i < t.Count; i++ {
		select {
		case <-ctx.Done():
			return
		default:
		}

		// 延迟
		if i > 0 && t.Delay > 0 {
			t.LogWrite(fmt.Sprintf("[*] 等待 %d 秒后注册下一个...", t.Delay))
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Duration(t.Delay) * time.Second):
			}
		}

		semaphore <- struct{}{}

		func(idx int) {
			defer func() { <-semaphore }()

			// 选择代理
			proxy := cfg.GetDefaultProxy()
			if pool := cfg.GetProxyPool(); len(pool) > 0 {
				proxy = pool[idx%len(pool)]
			}

			// 日志桥接：engine 写 logCh → Task 写 RingBuffer + 通知订阅者
			logCh := make(chan string, 50)
			done := make(chan struct{})
			go func() {
				defer close(done)
				for msg := range logCh {
					t.LogWrite(msg)
				}
			}()

			result := regFn(ctx, proxy, logCh)
			close(logCh)
			<-done // 等日志全部转发完

			// 更新进度
			t.UpdateProgress(result.OK)

			// 持久化结果
			result.Platform = platform
			if result.OK {
				result.Status = "success"
			} else {
				result.Status = "failed"
			}
			result.Time = time.Now().Format("2006-01-02 15:04:05")
			if err := storage.Append(*result); err != nil {
				log.Error().Err(err).Msg("保存结果失败")
			}

			// 发送结果事件
			data, _ := json.Marshal(result)
			t.LogWrite("RESULT:" + string(data))
		}(i)
	}

	// 等待所有并发完成
	for i := 0; i < t.Concurrency && i < t.Count; i++ {
		semaphore <- struct{}{}
	}
}
