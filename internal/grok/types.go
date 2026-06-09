package grok

import (
	"github.com/grok-fireworks-reg/internal/common"
)

// RegisterOpts options for a single Grok registration
type RegisterOpts struct {
	Proxy     *common.ProxyEntry
	Config    common.Config
	LogCh     chan<- string
	OnSuccess func(email string, credential map[string]interface{})
	OnFail    func()
}

// browserProfile 浏览器指纹配置
type browserProfile struct {
	Name      string
	UserAgent string
}

var browserProfiles = []browserProfile{
	{Name: "chrome120", UserAgent: "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36"},
	{Name: "chrome119", UserAgent: "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/119.0.0.0 Safari/537.36"},
	{Name: "chrome110", UserAgent: "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/110.0.0.0 Safari/537.36"},
	{Name: "edge99", UserAgent: "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/99.0.4844.74 Safari/537.36 Edg/99.0.1150.46"},
	{Name: "edge101", UserAgent: "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/101.0.4951.64 Safari/537.36 Edg/101.0.1210.53"},
}
