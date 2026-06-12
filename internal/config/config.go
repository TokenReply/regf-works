package config

import (
	"fmt"
	"os"
	"strings"

	"github.com/rs/zerolog/log"
	"github.com/spf13/viper"

	"github.com/grok-fireworks-reg/internal/common"
)

// Config 全局配置
type Config struct {
	Server     ServerConfig     `mapstructure:"server"`
	Auth       AuthConfig       `mapstructure:"auth"`
	Proxy      ProxyConfig      `mapstructure:"proxy"`
	Mail       MailConfig       `mapstructure:"mail"`
	Turnstile  TurnstileConfig  `mapstructure:"turnstile"`
	Grok       GrokConfig       `mapstructure:"grok"`
	Fireworks  FireworksConfig  `mapstructure:"fireworks"`
	OpenRouter OpenRouterConfig `mapstructure:"openrouter"`
	
	configFile string // 内部使用，记录配置文件路径
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
	MoeMail          MoeMailConfig  `mapstructure:"moemail"`
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

type MoeMailConfig struct {
	BaseURL    string `mapstructure:"base_url"`
	APIKey     string `mapstructure:"api_key"`
	Domains    string `mapstructure:"domains"`
	ExpiryTime int64  `mapstructure:"expiry_time"` // 毫秒，默认 3600000 (1小时)
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

type OpenRouterConfig struct {
	ServiceURL    string `mapstructure:"service_url"`
	MaxConcurrent int    `mapstructure:"max_concurrent"`
	SolverType    string `mapstructure:"solver_type"`
	SolverAPI     string `mapstructure:"solver_api"`
	YesCaptchaKey string `mapstructure:"yescaptcha_key"`
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
	viper.SetDefault("openrouter.service_url", "http://127.0.0.1:5001")
	viper.SetDefault("openrouter.max_concurrent", 10)
	viper.SetDefault("openrouter.solver_type", "selfhost")
	viper.SetDefault("openrouter.solver_api", "http://127.0.0.1:5072")
	viper.SetDefault("turnstile.solver_urls", []string{"http://127.0.0.1:5072"})
	viper.SetDefault("grok.site_key", "0x4AAAAAAAhr9JGVDZbrZOo0")

	if err := viper.ReadInConfig(); err != nil {
		log.Warn().Err(err).Msg("config file not found, using defaults")
	}

	var cfg Config
	if err := viper.Unmarshal(&cfg); err != nil {
		log.Fatal().Err(err).Msg("failed to parse config")
	}

	// 环境变量覆盖（直接读取，优先级最高）
	applyEnvOverrides(&cfg)

	// 记住配置文件路径，用于后续持久化
	cfg.configFile = cfgFile

	return &cfg
}

// applyEnvOverrides 从环境变量覆盖配置（支持 Docker -e 传参）
func applyEnvOverrides(cfg *Config) {
	if v := os.Getenv("AUTH_USERNAME"); v != "" {
		cfg.Auth.Username = v
	}
	if v := os.Getenv("AUTH_PASSWORD"); v != "" {
		cfg.Auth.Password = v
	}
	if v := os.Getenv("AUTH_JWT_SECRET"); v != "" {
		cfg.Auth.JWTSecret = v
	}
	if v := os.Getenv("PROXY_DEFAULT"); v != "" {
		cfg.Proxy.Default = v
	}
	if v := os.Getenv("AHEM_BASE_URL"); v != "" {
		cfg.Mail.Ahem.BaseURL = v
	}
	if v := os.Getenv("AHEM_DOMAINS"); v != "" {
		cfg.Mail.Ahem.Domains = v
	}
	if v := os.Getenv("YYDS_BASE_URL"); v != "" {
		cfg.Mail.YYDS.BaseURL = v
	}
	if v := os.Getenv("YYDS_API_KEY"); v != "" {
		cfg.Mail.YYDS.APIKey = v
	}
	if v := os.Getenv("GPTMAIL_BASE_URL"); v != "" {
		cfg.Mail.GPTMail.BaseURL = v
	}
	if v := os.Getenv("GPTMAIL_API_KEY"); v != "" {
		cfg.Mail.GPTMail.APIKey = v
	}
	if v := os.Getenv("MOEMAIL_BASE_URL"); v != "" {
		cfg.Mail.MoeMail.BaseURL = v
	}
	if v := os.Getenv("MOEMAIL_API_KEY"); v != "" {
		cfg.Mail.MoeMail.APIKey = v
	}
	if v := os.Getenv("MOEMAIL_DOMAINS"); v != "" {
		cfg.Mail.MoeMail.Domains = v
	}
	if v := os.Getenv("MAIL_PROVIDER_PRIORITY"); v != "" {
		cfg.Mail.ProviderPriority = v
	}
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
		"moemail_base_url":      c.Mail.MoeMail.BaseURL,
		"moemail_api_key":       c.Mail.MoeMail.APIKey,
		"moemail_domains":       c.Mail.MoeMail.Domains,
		"moemail_expiry_time":   fmt.Sprintf("%d", c.Mail.MoeMail.ExpiryTime),
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
		"moemail_base_url":       c.Mail.MoeMail.BaseURL,
		"moemail_api_key":        c.Mail.MoeMail.APIKey,
		"moemail_domains":        c.Mail.MoeMail.Domains,
		"moemail_expiry_time":    fmt.Sprintf("%d", c.Mail.MoeMail.ExpiryTime),
		"email_provider_priority": c.Mail.ProviderPriority,
	}
}

// ToOpenRouterConfig 转换为 openrouter worker 使用的 common.Config
func (c *Config) ToOpenRouterConfig() common.Config {
	return common.Config{
		"openrouter_reg_url":     c.OpenRouter.ServiceURL,
		"openrouter_solver_type": c.OpenRouter.SolverType,
		"openrouter_solver_api":  c.OpenRouter.SolverAPI,
		"yescaptcha_key":         c.OpenRouter.YesCaptchaKey,
		"yydsmail_base_url":      c.Mail.YYDS.BaseURL,
		"yydsmail_api_key":       c.Mail.YYDS.APIKey,
		"ahem_base_url":          c.Mail.Ahem.BaseURL,
		"ahem_domains":           c.Mail.Ahem.Domains,
		"gptmail_base_url":       c.Mail.GPTMail.BaseURL,
		"gptmail_api_key":        c.Mail.GPTMail.APIKey,
		"moemail_base_url":       c.Mail.MoeMail.BaseURL,
		"moemail_api_key":        c.Mail.MoeMail.APIKey,
		"moemail_domains":        c.Mail.MoeMail.Domains,
		"moemail_expiry_time":    fmt.Sprintf("%d", c.Mail.MoeMail.ExpiryTime),
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

// Save 将当前配置持久化到 config.yaml
func (c *Config) Save() error {
	path := c.configFile
	if path == "" {
		path = "configs/config.yaml"
	}

	viper.Set("server.port", c.Server.Port)
	viper.Set("auth.username", c.Auth.Username)
	viper.Set("auth.password", c.Auth.Password)
	viper.Set("auth.jwt_secret", c.Auth.JWTSecret)
	viper.Set("proxy.default", c.Proxy.Default)
	viper.Set("proxy.pool", c.Proxy.Pool)
	viper.Set("mail.provider_priority", c.Mail.ProviderPriority)
	viper.Set("mail.yydsmail.base_url", c.Mail.YYDS.BaseURL)
	viper.Set("mail.yydsmail.api_key", c.Mail.YYDS.APIKey)
	viper.Set("mail.ahem.base_url", c.Mail.Ahem.BaseURL)
	viper.Set("mail.ahem.domains", c.Mail.Ahem.Domains)
	viper.Set("mail.gptmail.base_url", c.Mail.GPTMail.BaseURL)
	viper.Set("mail.gptmail.api_key", c.Mail.GPTMail.APIKey)
	viper.Set("mail.moemail.base_url", c.Mail.MoeMail.BaseURL)
	viper.Set("mail.moemail.api_key", c.Mail.MoeMail.APIKey)
	viper.Set("mail.moemail.domains", c.Mail.MoeMail.Domains)
	viper.Set("mail.moemail.expiry_time", c.Mail.MoeMail.ExpiryTime)
	viper.Set("turnstile.solver_urls", c.Turnstile.SolverURLs)
	viper.Set("turnstile.solver_proxy", c.Turnstile.SolverProxy)
	viper.Set("turnstile.capsolver_key", c.Turnstile.CapSolverKey)
	viper.Set("turnstile.yescaptcha_key", c.Turnstile.YesCaptchaKey)
	viper.Set("grok.site_key", c.Grok.SiteKey)
	viper.Set("grok.action_id", c.Grok.ActionID)
	viper.Set("grok.state_tree", c.Grok.StateTree)
	viper.Set("grok.cf_bypass_solver_url", c.Grok.CFBypassSolverURL)
	viper.Set("fireworks.service_url", c.Fireworks.ServiceURL)
	viper.Set("fireworks.max_concurrent", c.Fireworks.MaxConcurrent)
	viper.Set("openrouter.service_url", c.OpenRouter.ServiceURL)
	viper.Set("openrouter.max_concurrent", c.OpenRouter.MaxConcurrent)
	viper.Set("openrouter.solver_type", c.OpenRouter.SolverType)
	viper.Set("openrouter.solver_api", c.OpenRouter.SolverAPI)
	viper.Set("openrouter.yescaptcha_key", c.OpenRouter.YesCaptchaKey)

	return viper.WriteConfigAs(path)
}
