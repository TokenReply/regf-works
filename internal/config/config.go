package config

import (
	"strings"

	"github.com/rs/zerolog/log"
	"github.com/spf13/viper"

	"github.com/grok-fireworks-reg/internal/common"
)

// Config 全局配置
type Config struct {
	Server    ServerConfig    `mapstructure:"server"`
	Auth      AuthConfig      `mapstructure:"auth"`
	Proxy     ProxyConfig     `mapstructure:"proxy"`
	Mail      MailConfig      `mapstructure:"mail"`
	Turnstile TurnstileConfig `mapstructure:"turnstile"`
	Grok      GrokConfig      `mapstructure:"grok"`
	Fireworks FireworksConfig `mapstructure:"fireworks"`
}

// AuthConfig 认证配置
type AuthConfig struct {
	Username  string `mapstructure:"username"`
	Password  string `mapstructure:"password"`
	JWTSecret string `mapstructure:"jwt_secret"`
}

type ServerConfig struct {
	Port int `mapstructure:"port"`
}

type ProxyConfig struct {
	Default string   `mapstructure:"default"`
	Pool    []string `mapstructure:"pool"`
}

type MailConfig struct {
	ProviderPriority string         `mapstructure:"provider_priority"`
	YYDS             YYDSMailConfig `mapstructure:"yydsmail"`
	Ahem             AhemConfig     `mapstructure:"ahem"`
	GPTMail          GPTMailConfig  `mapstructure:"gptmail"`
}

type YYDSMailConfig struct {
	BaseURL string `mapstructure:"base_url"`
	APIKey  string `mapstructure:"api_key"`
}

type AhemConfig struct {
	BaseURL string `mapstructure:"base_url"`
	Domains string `mapstructure:"domains"`
}

type GPTMailConfig struct {
	BaseURL string `mapstructure:"base_url"`
	APIKey  string `mapstructure:"api_key"`
}

type TurnstileConfig struct {
	SolverURLs    []string `mapstructure:"solver_urls"`
	SolverProxy   string   `mapstructure:"solver_proxy"`
	CapSolverKey  string   `mapstructure:"capsolver_key"`
	YesCaptchaKey string   `mapstructure:"yescaptcha_key"`
}

type GrokConfig struct {
	SiteKey           string `mapstructure:"site_key"`
	ActionID          string `mapstructure:"action_id"`
	StateTree         string `mapstructure:"state_tree"`
	CFBypassSolverURL string `mapstructure:"cf_bypass_solver_url"`
}

type FireworksConfig struct {
	ServiceURL    string `mapstructure:"service_url"`
	MaxConcurrent int    `mapstructure:"max_concurrent"`
}

// Load 加载配置文件
func Load(cfgFile string) *Config {
	if cfgFile != "" {
		viper.SetConfigFile(cfgFile)
	} else {
		viper.SetConfigName("config")
		viper.SetConfigType("yaml")
		viper.AddConfigPath("./configs")
		viper.AddConfigPath(".")
	}

	// 默认值
	viper.SetDefault("server.port", 8080)
	viper.SetDefault("auth.username", "admin")
	viper.SetDefault("auth.password", "admin123")
	viper.SetDefault("auth.jwt_secret", "change-me-in-production")
	viper.SetDefault("fireworks.service_url", "http://127.0.0.1:5000")
	viper.SetDefault("fireworks.max_concurrent", 10)
	viper.SetDefault("grok.site_key", "0x4AAAAAAAhr9JGVDZbrZOo0")

	// 环境变量覆盖
	viper.AutomaticEnv()
	viper.SetEnvPrefix("REG")

	if err := viper.ReadInConfig(); err != nil {
		log.Warn().Err(err).Msg("config file not found, using defaults")
	}

	var cfg Config
	if err := viper.Unmarshal(&cfg); err != nil {
		log.Fatal().Err(err).Msg("failed to parse config")
	}
	return &cfg
}

// ToGrokConfig 转换为 grok worker 使用的 common.Config
func (c *Config) ToGrokConfig() common.Config {
	return common.Config{
		"site_key":               c.Grok.SiteKey,
		"action_id":             c.Grok.ActionID,
		"state_tree":            c.Grok.StateTree,
		"cf_bypass_solver_url":  c.Grok.CFBypassSolverURL,
		"turnstile_solver_url":  joinStrings(c.Turnstile.SolverURLs),
		"capsolver_key":         c.Turnstile.CapSolverKey,
		"yescaptcha_key":        c.Turnstile.YesCaptchaKey,
		"yydsmail_base_url":     c.Mail.YYDS.BaseURL,
		"yydsmail_api_key":      c.Mail.YYDS.APIKey,
		"ahem_base_url":         c.Mail.Ahem.BaseURL,
		"ahem_domains":          c.Mail.Ahem.Domains,
		"gptmail_base_url":      c.Mail.GPTMail.BaseURL,
		"gptmail_api_key":       c.Mail.GPTMail.APIKey,
		"email_provider_priority": c.Mail.ProviderPriority,
	}
}

// ToFireworksConfig 转换为 fireworks worker 使用的 common.Config
func (c *Config) ToFireworksConfig() common.Config {
	return common.Config{
		"fireworks_reg_url":      c.Fireworks.ServiceURL,
		"yydsmail_base_url":      c.Mail.YYDS.BaseURL,
		"yydsmail_api_key":       c.Mail.YYDS.APIKey,
		"ahem_base_url":          c.Mail.Ahem.BaseURL,
		"ahem_domains":           c.Mail.Ahem.Domains,
		"gptmail_base_url":       c.Mail.GPTMail.BaseURL,
		"gptmail_api_key":        c.Mail.GPTMail.APIKey,
		"email_provider_priority": c.Mail.ProviderPriority,
	}
}

// GetDefaultProxy 从配置中获取默认代理
func (c *Config) GetDefaultProxy() *common.ProxyEntry {
	if c.Proxy.Default == "" {
		return nil
	}
	return &common.ProxyEntry{
		HTTP:  c.Proxy.Default,
		HTTPS: c.Proxy.Default,
	}
}

// GetProxyPool 返回代理池列表
func (c *Config) GetProxyPool() []*common.ProxyEntry {
	if len(c.Proxy.Pool) == 0 {
		return nil
	}
	entries := make([]*common.ProxyEntry, 0, len(c.Proxy.Pool))
	for _, p := range c.Proxy.Pool {
		if p != "" {
			entries = append(entries, &common.ProxyEntry{HTTP: p, HTTPS: p})
		}
	}
	return entries
}

func joinStrings(ss []string) string {
	return strings.Join(ss, ",")
}
