package tempmail

import (
	"bytes"
	"io"
	"mime/quotedprintable"
	"regexp"
	"strings"
)

// 验证码提取正则（所有 provider 共用）
var (
	// 策略 1a: subject 以验证码开头（Grok 格式 "ABC123-DEF456"）
	reSubjectCodeDash = regexp.MustCompile(`^([A-Z0-9]+-[A-Z0-9]+)`)
	// 策略 1b: subject 末尾的 6-8 位数字验证码（OpenAI 格式 "Your ChatGPT code is 798357"）
	reSubjectCodeTail = regexp.MustCompile(`\b(\d{6,8})\s*$`)
	// 策略 1c: 显式提示 verification code 的 6 位数字
	reKeywordDigits = regexp.MustCompile(`(?i)(?:verification code|verify|code)[^0-9]{0,30}(\d{6})`)
	// 策略 1d: 显式提示 verification code 的 6 位字母数字混合码
	reKeywordAlphaNum = regexp.MustCompile(`(?i)(?:verification code|verify|code)\s*[:：\s]*\b([A-Z0-9]{6})\b`)
	// 策略 1e: HTML 中 verification-code 样式块
	reHTMLSpanCode = regexp.MustCompile(`(?i)<span[^>]*class="[^"]*verification-code[^"]*"[^>]*>\s*([A-Z0-9]{6,8})\s*</span>`)
	// 策略 2: 任意位置的 6-8 位大写字母+数字兜底
	reInlineCode = regexp.MustCompile(`\b([A-Z0-9]{6,8})\b`)
)

var verificationCodeBlacklist = map[string]struct{}{
	"GOOGLE": {},
	"GEMINI": {},
	"VERIFY": {},
	"SIGNIN": {},
	"PLEASE": {},
	"CHANGE": {},
	"ACCEPT": {},
	"CANCEL": {},
	"SUBMIT": {},
}

// ExtractVerificationCode 从邮件 subject/body 中提取验证码。
// 先走显式模式，再走 HTML 和 quoted-printable 解码，最后才用通用兜底规则。
func ExtractVerificationCode(subject, body string) string {
	if subject != "" {
		// Grok 的 subject 常见为 "ABC123-DEF456" 这种前缀格式。
		if m := reSubjectCodeDash.FindStringSubmatch(strings.ToUpper(subject)); len(m) > 1 {
			return strings.ReplaceAll(m[1], "-", "")
		}
		// OpenAI 常见 "Your ChatGPT code is 798357"。
		if m := reSubjectCodeTail.FindStringSubmatch(subject); len(m) > 1 {
			return m[1]
		}
		if code := extractCodeFromText(subject, true); code != "" {
			return code
		}
	}
	if body != "" {
		if code := extractCodeFromText(body, true); code != "" {
			return code
		}
	}
	return ""
}

func extractCodeFromText(text string, allowPlainDigits bool) string {
	if text == "" {
		return ""
	}

	upperText := strings.ToUpper(text)
	if m := reHTMLSpanCode.FindStringSubmatch(upperText); len(m) > 1 && isPlausibleVerificationCode(m[1], false) {
		return m[1]
	}
	if m := reKeywordDigits.FindStringSubmatch(text); len(m) > 1 && isPlausibleVerificationCode(m[1], true) {
		return m[1]
	}
	if m := reKeywordAlphaNum.FindStringSubmatch(upperText); len(m) > 1 && isPlausibleVerificationCode(m[1], false) {
		return m[1]
	}

	if decoded := decodeQuotedPrintable(text); decoded != "" && decoded != text {
		if code := extractCodeFromText(decoded, allowPlainDigits); code != "" {
			return code
		}
	}

	for _, candidate := range reInlineCode.FindAllString(upperText, -1) {
		if isPlausibleVerificationCode(candidate, allowPlainDigits) {
			return candidate
		}
	}
	return ""
}

func isPlausibleVerificationCode(code string, allowPlainDigits bool) bool {
	if code == "" {
		return false
	}

	code = strings.ToUpper(strings.TrimSpace(code))
	if _, blocked := verificationCodeBlacklist[code]; blocked {
		return false
	}

	if isAllDigits(code) {
		if !allowPlainDigits {
			return false
		}
		if strings.Trim(code, "0") == "" {
			return false
		}
		if len(code) == 6 && (strings.HasPrefix(code, "19") || strings.HasPrefix(code, "20")) {
			return false
		}
		return true
	}

	return containsDigit(code)
}

func decodeQuotedPrintable(text string) string {
	reader := quotedprintable.NewReader(strings.NewReader(text))
	decoded, err := io.ReadAll(reader)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(bytes.TrimSpace(decoded)))
}

func isAllDigits(text string) bool {
	if text == "" {
		return false
	}
	for _, ch := range text {
		if ch < '0' || ch > '9' {
			return false
		}
	}
	return true
}

func containsDigit(text string) bool {
	for _, ch := range text {
		if ch >= '0' && ch <= '9' {
			return true
		}
	}
	return false
}
