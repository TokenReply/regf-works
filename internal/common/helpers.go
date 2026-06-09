package common

import (
	"context"
	"crypto/rand"
	"fmt"
	"math/big"
	"net/url"
	"strings"
	"time"
)

// CtxSleep context-aware sleep
func CtxSleep(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}

// TruncStr truncate string for logging
func TruncStr(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

// RandomString generate random string from charset
func RandomString(length int, charset string) string {
	b := make([]byte, length)
	for i := range b {
		n, _ := rand.Int(rand.Reader, big.NewInt(int64(len(charset))))
		b[i] = charset[n.Int64()]
	}
	return string(b)
}

// RandomName generate a random name (first letter uppercase)
func RandomName(minLen, maxLen int) string {
	n, _ := rand.Int(rand.Reader, big.NewInt(int64(maxLen-minLen+1)))
	length := minLen + int(n.Int64())
	name := RandomString(length, "abcdefghijklmnopqrstuvwxyz")
	return strings.ToUpper(name[:1]) + name[1:]
}

// RandomUUID generate UUID v4
func RandomUUID() string {
	var buf [16]byte
	rand.Read(buf[:])
	buf[6] = (buf[6] & 0x0f) | 0x40
	buf[8] = (buf[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", buf[0:4], buf[4:6], buf[6:8], buf[8:10], buf[10:16])
}

// RandomBirthDate generate random birth date (20-40 years old)
func RandomBirthDate() string {
	now := time.Now()
	n, _ := rand.Int(rand.Reader, big.NewInt(21))
	age := 20 + int(n.Int64())
	month, _ := rand.Int(rand.Reader, big.NewInt(12))
	day, _ := rand.Int(rand.Reader, big.NewInt(28))
	return fmt.Sprintf("%d-%02d-%02dT16:00:00.000Z", now.Year()-age, 1+int(month.Int64()), 1+int(day.Int64()))
}

// SanitizeHTTPError remove sensitive proxy info from HTTP errors
func SanitizeHTTPError(err error) string {
	if err == nil {
		return ""
	}
	s := err.Error()
	if u, e := url.Parse(s); e == nil && u.User != nil {
		u.User = url.UserPassword("***", "***")
		return u.String()
	}
	return s
}

// LogSend non-blocking send to log channel
func LogSend(ch chan<- string, msg string) {
	defer func() { recover() }()
	select {
	case ch <- msg:
	default:
	}
}

// SettingOrDefault get config value or default
func SettingOrDefault(cfg Config, key, fallback string) string {
	if v, ok := cfg[key]; ok && v != "" {
		return v
	}
	return fallback
}

// FilterLocalURLs filter out localhost URLs from comma-separated list
func FilterLocalURLs(urls string) string {
	if urls == "" {
		return ""
	}
	parts := strings.Split(urls, ",")
	remote := make([]string, 0, len(parts))
	for _, u := range parts {
		u = strings.TrimSpace(u)
		if u == "" {
			continue
		}
		if strings.Contains(u, "127.0.0.1") || strings.Contains(u, "localhost") || strings.Contains(u, "host.docker.internal") {
			continue
		}
		remote = append(remote, u)
	}
	return strings.Join(remote, ",")
}
