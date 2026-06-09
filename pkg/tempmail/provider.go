package tempmail

import (
	"context"
	"errors"
	"time"
)

// ErrRateLimited 表示邮箱服务返回 429（速率限制）
var ErrRateLimited = errors.New("邮箱服务速率限制 (429)")

// ErrAuthFailed 表示邮箱服务认证失败（401/403，API Key 无效）
var ErrAuthFailed = errors.New("邮箱服务认证失败 (401/403)")

// EmailProvider 临时邮箱服务提供者接口
// meta 用于携带 provider 特有的状态（如 Mail.tm 的 JWT token、Guerrilla Mail 的 sid_token、tempmail.lol 的 token）
type EmailProvider interface {
	// Name 返回 provider 唯一标识（如 "yydsmail"）
	Name() string

	// GenerateEmail 创建一个临时邮箱，返回邮箱地址和 provider 元数据
	GenerateEmail(ctx context.Context) (addr string, meta map[string]string, err error)

	// FetchVerificationCode 从邮箱中提取验证码
	FetchVerificationCode(ctx context.Context, addr string, meta map[string]string, maxAttempts int, interval time.Duration) (string, error)

	// DeleteEmail 删除/清理临时邮箱（非关键步骤，失败不影响主流程）
	DeleteEmail(ctx context.Context, addr string, meta map[string]string) error
}
