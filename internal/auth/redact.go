package auth

import (
	"regexp"
	"strings"
)

var (
	jwtPattern      = regexp.MustCompile(`eyJ[A-Za-z0-9_-]{5,}\.[A-Za-z0-9_-]{5,}\.[A-Za-z0-9_-]*`)
	bearerPattern   = regexp.MustCompile(`(?i)(bearer\s+)[^\s"',;)\]]+`)
	encCredPattern  = regexp.MustCompile(`enc:v1:[A-Za-z0-9_.-]+`)
	jsonSecretField = regexp.MustCompile(`(?i)("(?:jwt|token|access_token|refresh_token|refreshToken|accessToken|poll_token|api_key|apiKey|x-api-key|authorization|bridge_token|bridgeToken)"\s*:\s*)"[^"]*"`)
	kvSecretField   = regexp.MustCompile(`(?i)\b((?:x-api-key|api[_-]?key|refresh[_-]?token|access[_-]?token|poll[_-]?token|bridge[_-]?token)\s*[:=]\s*)[^\s"',;&)\]]+`)
	skKeyPattern    = regexp.MustCompile(`\b(?:sk|zai)-[A-Za-z0-9_-]{12,}\b`)
)

func containsFoldASCII(s, sub string) bool {
	if len(sub) == 0 {
		return true
	}
	if len(s) < len(sub) {
		return false
	}
	limit := len(s) - len(sub)
	for i := 0; i <= limit; i++ {
		match := true
		for j := 0; j < len(sub); j++ {
			c1 := s[i+j]
			if c1 >= 'A' && c1 <= 'Z' {
				c1 += 'a' - 'A'
			}
			if c1 != sub[j] {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

// Redact scrubs JWTs, Bearer tokens, encrypted desktop credentials, API keys,
// and secret JSON/key-value fields from s before logging or returning errors.
func Redact(s string) string {
	if s == "" {
		return ""
	}
	out := s
	if strings.Contains(out, "eyJ") {
		out = jwtPattern.ReplaceAllString(out, "[REDACTED_JWT]")
	}
	if strings.Contains(out, "enc:v1:") {
		out = encCredPattern.ReplaceAllString(out, "enc:v1:[REDACTED]")
	}
	if strings.Contains(out, `"`) && strings.Contains(out, ":") {
		out = jsonSecretField.ReplaceAllString(out, `$1"[REDACTED]"`)
	}
	if containsFoldASCII(out, "bearer") {
		out = bearerPattern.ReplaceAllString(out, "${1}[REDACTED]")
	}
	if strings.ContainsAny(out, ":=") && (containsFoldASCII(out, "key") || containsFoldASCII(out, "token")) {
		out = kvSecretField.ReplaceAllString(out, "${1}[REDACTED]")
	}
	if strings.Contains(out, "sk-") || strings.Contains(out, "zai-") {
		out = skKeyPattern.ReplaceAllString(out, "[REDACTED_KEY]")
	}
	return out
}

// RedactSecrets scrubs known runtime secret values in addition to pattern-based
// credential redaction.
func RedactSecrets(s string, secrets ...string) string {
	out := Redact(s)
	for _, sec := range secrets {
		if trimmed := strings.TrimSpace(sec); len(trimmed) >= 4 && strings.Contains(out, trimmed) {
			out = strings.ReplaceAll(out, trimmed, "[REDACTED]")
		}
	}
	return out
}
