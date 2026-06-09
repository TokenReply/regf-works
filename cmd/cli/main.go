package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/spf13/cobra"

	"github.com/grok-fireworks-reg/internal/common"
	"github.com/grok-fireworks-reg/internal/config"
	"github.com/grok-fireworks-reg/internal/fireworks"
	"github.com/grok-fireworks-reg/internal/grok"
)

var (
	cfgFile  string
	proxy    string
	count    int
	provider string
	output   string
)

func main() {
	log.Logger = log.Output(zerolog.ConsoleWriter{Out: os.Stderr})

	rootCmd := &cobra.Command{
		Use:   "reg-cli",
		Short: "Grok + Fireworks 账号注册 CLI 工具",
	}

	rootCmd.PersistentFlags().StringVar(&cfgFile, "config", "", "配置文件路径")
	rootCmd.PersistentFlags().StringVar(&proxy, "proxy", "", "代理地址 (覆盖配置)")
	rootCmd.PersistentFlags().IntVar(&count, "count", 1, "注册数量")
	rootCmd.PersistentFlags().StringVar(&provider, "provider", "", "邮箱 provider (覆盖配置)")
	rootCmd.PersistentFlags().StringVar(&output, "output", "", "结果输出文件 (JSON Lines)")

	rootCmd.AddCommand(grokCmd())
	rootCmd.AddCommand(fireworksCmd())

	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

func grokCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "grok",
		Short: "注册 Grok 账号",
		Run: func(cmd *cobra.Command, args []string) {
			cfg := config.Load(cfgFile)
			runGrokRegistration(cfg)
		},
	}
}

func fireworksCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "fireworks",
		Short: "注册 Fireworks 账号",
		Run: func(cmd *cobra.Command, args []string) {
			cfg := config.Load(cfgFile)
			runFireworksRegistration(cfg)
		},
	}
}

func runGrokRegistration(cfg *config.Config) {
	ctx := context.Background()

	// 构建 worker 配置
	workerCfg := common.Config(cfg.ToGrokConfig())
	if provider != "" {
		workerCfg["email_provider_priority"] = provider
	}

	// 代理
	var proxyEntry *common.ProxyEntry
	if proxy != "" {
		proxyEntry = &common.ProxyEntry{HTTPS: proxy, HTTP: proxy}
	} else {
		proxyEntry = cfg.GetDefaultProxy()
	}

	// 先扫描配置
	if workerCfg["action_id"] == "" {
		fmt.Println("[*] 扫描 Grok 配置 (action_id)...")
		scanned, err := grok.ScanConfig(ctx, proxyEntry, workerCfg)
		if err != nil {
			fmt.Printf("[!] 扫描失败: %v (继续使用已有配置)\n", err)
		} else {
			for k, v := range scanned {
				if v != "" {
					workerCfg[k] = v
				}
			}
			fmt.Printf("[+] action_id: %s\n", workerCfg["action_id"])
		}
	}

	var results []*common.RegisterResult

	for i := 0; i < count; i++ {
		if count > 1 {
			fmt.Printf("\n━━━ 第 %d/%d 个 ━━━\n", i+1, count)
		}

		logCh := make(chan string, 100)

		// 打印日志
		done := make(chan struct{})
		go func() {
			for msg := range logCh {
				fmt.Println(msg)
			}
			close(done)
		}()

		opts := grok.RegisterOpts{
			Proxy:  proxyEntry,
			Config: workerCfg,
			LogCh:  logCh,
		}

		result := grok.Register(ctx, opts)
		close(logCh)
		<-done

		results = append(results, result)

		if result.OK {
			fmt.Printf("\n[OK] 注册成功: %s\n", result.Email)
			if data, err := json.MarshalIndent(result.Data, "  ", "  "); err == nil {
				fmt.Printf("  凭据: %s\n", string(data))
			}
		} else {
			fmt.Printf("\n[FAIL] 注册失败: %s\n", result.Error)
		}
	}

	// 输出到文件
	if output != "" {
		writeResults(output, results)
	}

	// 统计
	success := 0
	for _, r := range results {
		if r.OK {
			success++
		}
	}
	fmt.Printf("\n━━━ 完成: %d/%d 成功 ━━━\n", success, count)
}

func runFireworksRegistration(cfg *config.Config) {
	ctx := context.Background()

	workerCfg := common.Config(cfg.ToFireworksConfig())
	if provider != "" {
		workerCfg["email_provider_priority"] = provider
	}

	var proxyEntry *common.ProxyEntry
	if proxy != "" {
		proxyEntry = &common.ProxyEntry{HTTPS: proxy, HTTP: proxy}
	} else {
		proxyEntry = cfg.GetDefaultProxy()
	}

	var results []*common.RegisterResult

	for i := 0; i < count; i++ {
		if count > 1 {
			fmt.Printf("\n━━━ 第 %d/%d 个 ━━━\n", i+1, count)
		}

		logCh := make(chan string, 100)

		done := make(chan struct{})
		go func() {
			for msg := range logCh {
				fmt.Println(msg)
			}
			close(done)
		}()

		opts := fireworks.RegisterOpts{
			Proxy:  proxyEntry,
			Config: workerCfg,
			LogCh:  logCh,
		}

		result := fireworks.Register(ctx, opts)
		close(logCh)
		<-done

		results = append(results, result)

		if result.OK {
			fmt.Printf("\n[OK] 注册成功: %s\n", result.Email)
			if data, err := json.MarshalIndent(result.Data, "  ", "  "); err == nil {
				fmt.Printf("  凭据: %s\n", string(data))
			}
		} else {
			fmt.Printf("\n[FAIL] 注册失败: %s\n", result.Error)
		}
	}

	if output != "" {
		writeResults(output, results)
	}

	success := 0
	for _, r := range results {
		if r.OK {
			success++
		}
	}
	fmt.Printf("\n━━━ 完成: %d/%d 成功 ━━━\n", success, count)
}

func writeResults(path string, results []*common.RegisterResult) {
	f, err := os.Create(path)
	if err != nil {
		fmt.Printf("[!] 无法创建输出文件: %v\n", err)
		return
	}
	defer f.Close()

	for _, r := range results {
		line, _ := json.Marshal(r)
		f.Write(line)
		f.WriteString("\n")
	}
	fmt.Printf("[+] 结果已写入: %s\n", path)
}

// suppress unused import
var _ = strings.Join
