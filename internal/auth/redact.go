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

// Redact scrubs JWTs, Bearer tokens, encrypted desktop credentials, API keys,
// and secret JSON/key-value fields from s before logging or returning errors.
func Redact(s string) string {
	if s == "" {
		return ""
	}
	out := jwtPattern.ReplaceAllString(s, "[REDACTED_JWT]")
	out = encCredPattern.ReplaceAllString(out, "enc:v1:[REDACTED]")
	out = jsonSecretField.ReplaceAllString(out, `$1"[REDACTED]"`)
	out = bearerPattern.ReplaceAllString(out, "${1}[REDACTED]")
	out = kvSecretField.ReplaceAllString(out, "${1}[REDACTED]")
	out = skKeyPattern.ReplaceAllString(out, "[REDACTED_KEY]")
	return out
}

// RedactSecrets scrubs known runtime secret values in addition to pattern-based
// credential redaction.
func RedactSecrets(s string, secrets ...string) string {
	out := Redact(s)
	for _, sec := range secrets {
		if trimmed := strings.TrimSpace(sec); len(trimmed) >= 4 {
			out = strings.ReplaceAll(out, trimmed, "[REDACTED]")
		}
	}
	return out
}
