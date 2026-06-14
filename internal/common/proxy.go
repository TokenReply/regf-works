package common

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"time"
)

// envProxyURL 读取环境变量中的代理地址（HTTPS_PROXY 优先于 HTTP_PROXY）
func envProxyURL() *url.URL {
	raw := os.Getenv("HTTPS_PROXY")
	if raw == "" {
		raw = os.Getenv("HTTP_PROXY")
	}
	if raw == "" {
		raw = os.Getenv("https_proxy")
	}
	if raw == "" {
		raw = os.Getenv("http_proxy")
	}
	if raw == "" {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil
	}
	return u
}

// proxyChainDialer 创建通过 firstProxy（HTTP CONNECT）建立隧道的 DialContext，
// 用于实现代理链：App → firstProxy(Clash) → 目标地址(后端代理)
//
// 配合 transport.Proxy = http.ProxyURL(backendProxy) 使用时，链路为：
//
//	App → Clash(CONNECT) → backendProxy → 最终目标
func proxyChainDialer(firstProxy *url.URL) func(ctx context.Context, network, addr string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		// 1. TCP 连接到第一级代理（Clash）
		d := net.Dialer{Timeout: 15 * time.Second}
		conn, err := d.DialContext(ctx, "tcp", firstProxy.Host)
		if err != nil {
			return nil, fmt.Errorf("连接代理 %s 失败: %w", firstProxy.Host, err)
		}

		// 2. 发送 HTTP CONNECT 请求，建立到目标地址的隧道
		req := &http.Request{
			Method: "CONNECT",
			URL:    &url.URL{Opaque: addr},
			Host:   addr,
			Header: make(http.Header),
		}
		if err := req.Write(conn); err != nil {
			conn.Close()
			return nil, fmt.Errorf("发送 CONNECT 失败: %w", err)
		}

		// 3. 读取 CONNECT 响应
		resp, err := http.ReadResponse(bufio.NewReader(conn), req)
		if err != nil {
			conn.Close()
			return nil, fmt.Errorf("CONNECT 响应读取失败: %w", err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			conn.Close()
			return nil, fmt.Errorf("CONNECT 返回 %d", resp.StatusCode)
		}

		return conn, nil
	}
}

// ApplyProxy 为 http.Transport 设置代理策略，支持三层回退：
//  1. 有请求代理 + 有环境代理 → 代理链（环境代理 → 请求代理 → 目标）
//  2. 有请求代理，无环境代理 → 直连请求代理
//  3. 无请求代理 → 回退到环境变量代理
//  4. 都没有 → 直连
//
// 所有模式都强制 IPv4 连接，避免 IPv6 超时问题。
func ApplyProxy(transport *http.Transport, proxy *ProxyEntry) {
	envProxy := envProxyURL()

	// 解析请求级代理
	var reqProxyURL *url.URL
	if proxy != nil {
		proxyStr := proxy.HTTPS
		if proxyStr == "" {
			proxyStr = proxy.HTTP
		}
		if proxyStr != "" {
			if u, err := url.Parse(proxyStr); err == nil {
				reqProxyURL = u
			}
		}
	}

	switch {
	case reqProxyURL != nil && envProxy != nil:
		// 代理链：App → Clash(envProxy) → 后端代理(reqProxy) → 目标
		transport.DialContext = proxyChainDialer(envProxy)
		transport.Proxy = http.ProxyURL(reqProxyURL)

	case reqProxyURL != nil:
		// 仅后端代理（无 Clash）
		transport.Proxy = http.ProxyURL(reqProxyURL)

	default:
		// 无请求代理 → 环境变量回退（有 Clash 走 Clash，没有就直连）
		transport.Proxy = http.ProxyFromEnvironment
	}

	// 强制 IPv4：包装 DialContext，将 network 替换为 tcp4
	origDial := transport.DialContext
	if origDial == nil {
		origDial = (&net.Dialer{
			Timeout:   15 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext
	}
	transport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		if network == "tcp" {
			network = "tcp4"
		}
		return origDial(ctx, network, addr)
	}
}
