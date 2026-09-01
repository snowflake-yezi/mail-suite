package identity

import (
	"net/url"
	"path"
	"strings"
	"unicode/utf8"
)

const maximumReturnPathBytes = 2048

// NormalizeReturnPath 校验并返回只能落在邮箱端或管理端的规范站内绝对路径。
func NormalizeReturnPath(candidate string) (string, error) {
	if len(candidate) == 0 || len(candidate) > maximumReturnPathBytes || !utf8.ValidString(candidate) {
		return "", NewError(ErrorCodeInvalidRequest)
	}
	if hasControlCharacter(candidate) || strings.Contains(candidate, "\\") {
		return "", NewError(ErrorCodeInvalidRequest)
	}
	lowerCandidate := strings.ToLower(candidate)
	if strings.Contains(lowerCandidate, "%25") {
		return "", NewError(ErrorCodeInvalidRequest)
	}
	parsed, err := url.ParseRequestURI(candidate)
	if err != nil || parsed.IsAbs() || parsed.Host != "" || parsed.Opaque != "" || parsed.Fragment != "" {
		return "", NewError(ErrorCodeInvalidRequest)
	}
	if !strings.HasPrefix(parsed.Path, "/") || strings.HasPrefix(parsed.Path, "//") {
		return "", NewError(ErrorCodeInvalidRequest)
	}
	lowerEscapedPath := strings.ToLower(parsed.EscapedPath())
	if strings.Contains(lowerEscapedPath, "%2f") || strings.Contains(lowerEscapedPath, "%5c") {
		return "", NewError(ErrorCodeInvalidRequest)
	}
	if hasControlCharacter(parsed.Path) || strings.Contains(parsed.Path, "\\") || path.Clean(parsed.Path) != parsed.Path {
		return "", NewError(ErrorCodeInvalidRequest)
	}
	if !isProtectedPortalPath(parsed.Path) {
		return "", NewError(ErrorCodeInvalidRequest)
	}
	if parsed.RawQuery != "" {
		decodedQuery, decodeErr := url.QueryUnescape(parsed.RawQuery)
		if decodeErr != nil || hasControlCharacter(decodedQuery) || strings.Contains(decodedQuery, "\\") {
			return "", NewError(ErrorCodeInvalidRequest)
		}
	}
	normalized := parsed.EscapedPath()
	if parsed.RawQuery != "" {
		normalized += "?" + parsed.RawQuery
	}
	return normalized, nil
}

// isProtectedPortalPath 只允许认证完成后有明确账号类型约束的两个受保护路由前缀。
func isProtectedPortalPath(candidate string) bool {
	return candidate == "/mail" ||
		strings.HasPrefix(candidate, "/mail/") ||
		candidate == "/admin" ||
		strings.HasPrefix(candidate, "/admin/")
}

// hasControlCharacter 拒绝 ASCII 与 Unicode 控制字符，防止日志、header 和路由解释歧义。
func hasControlCharacter(value string) bool {
	return strings.IndexFunc(value, func(character rune) bool {
		return character < 0x20 || (character >= 0x7f && character <= 0x9f)
	}) >= 0
}
