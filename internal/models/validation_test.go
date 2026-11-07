package models

import (
	"testing"
)

func TestValidateRequestOpenAIAndAnthropic(t *testing.T) {
	validOpenAI := map[string]any{
		"model":            "glm-5.3-flash:high",
		"max_tokens":       float64(2048),
		"reasoning_effort": "high",
		"messages": []any{
			map[string]any{"role": "system", "content": "You are helpful."},
			map[string]any{"role": "user", "content": "Hello"},
		},
		"tools": []any{
			map[string]any{
				"type": "function",
				"function": map[string]any{
					"name":       "get_weather",
					"parameters": map[string]any{"type": "object"},
				},
			},
		},
	}
	if err := ValidateRequest(validOpenAI, "openai"); err != nil {
		t.Fatalf("expected valid OpenAI request to pass, got: %v", err)
	}

	validAnthropic := map[string]any{
		"model":      "glm-5.3",
		"max_tokens": float64(4096),
		"system":     "Be concise.",
		"thinking":   map[string]any{"type": "enabled", "effort": "low", "budget_tokens": float64(1024)},
		"messages": []any{
			map[string]any{
				"role": "user",
				"content": []any{
					map[string]any{"type": "text", "text": "Look at this"},
					map[string]any{
						"type":   "image",
						"source": map[string]any{"type": "url", "url": "https://cdn.example.com/pic.png"},
					},
				},
			},
		},
		"tools": []any{
			map[string]any{
				"name":         "read_file",
				"input_schema": map[string]any{"type": "object"},
			},
		},
	}
	if err := ValidateRequest(validAnthropic, "anthropic"); err != nil {
		t.Fatalf("expected valid Anthropic request to pass, got: %v", err)
	}

	invalidCases := []struct {
		name    string
		dialect string
		body    map[string]any
	}{
		{"nil body", "openai", nil},
		{"missing messages", "openai", map[string]any{"model": "glm-5.3-flash"}},
		{"non-array messages", "openai", map[string]any{"model": "glm-5.3-flash", "messages": "hello"}},
		{"empty messages", "openai", map[string]any{"model": "glm-5.3-flash", "messages": []any{}}},
		{"system-only messages", "openai", map[string]any{"model": "glm-5.3-flash", "messages": []any{map[string]any{"role": "system", "content": "sys"}}}},
		{"invalid role openai", "openai", map[string]any{"model": "glm-5.3-flash", "messages": []any{map[string]any{"role": "admin", "content": "hi"}}}},
		{"invalid role anthropic", "anthropic", map[string]any{"model": "glm-5.3-flash", "messages": []any{map[string]any{"role": "system", "content": "hi"}}}},
		{"missing content", "openai", map[string]any{"model": "glm-5.3-flash", "messages": []any{map[string]any{"role": "user"}}}},
		{"invalid model path traversal", "openai", map[string]any{"model": "../../etc/passwd", "messages": []any{map[string]any{"role": "user", "content": "hi"}}}},
		{"non-string model", "openai", map[string]any{"model": 123, "messages": []any{map[string]any{"role": "user", "content": "hi"}}}},
		{"empty string model", "openai", map[string]any{"model": "   ", "messages": []any{map[string]any{"role": "user", "content": "hi"}}}},
		{"zero max_tokens", "openai", map[string]any{"model": "glm-5.3-flash", "max_tokens": float64(0), "messages": []any{map[string]any{"role": "user", "content": "hi"}}}},
		{"excessive max_tokens", "openai", map[string]any{"model": "glm-5.3-flash", "max_tokens": float64(999999), "messages": []any{map[string]any{"role": "user", "content": "hi"}}}},
		{"fractional max_tokens", "openai", map[string]any{"model": "glm-5.3-flash", "max_tokens": float64(12.5), "messages": []any{map[string]any{"role": "user", "content": "hi"}}}},
		{"invalid reasoning_effort", "openai", map[string]any{"model": "glm-5.3-flash", "reasoning_effort": "ultra", "messages": []any{map[string]any{"role": "user", "content": "hi"}}}},
		{"invalid thinking type", "anthropic", map[string]any{"model": "glm-5.3-flash", "thinking": map[string]any{"type": "auto"}, "messages": []any{map[string]any{"role": "user", "content": "hi"}}}},
		{"non-object thinking", "anthropic", map[string]any{"model": "glm-5.3-flash", "thinking": true, "messages": []any{map[string]any{"role": "user", "content": "hi"}}}},
		{"invalid tool missing name", "openai", map[string]any{"model": "glm-5.3-flash", "tools": []any{map[string]any{"type": "function", "function": map[string]any{}}}, "messages": []any{map[string]any{"role": "user", "content": "hi"}}}},
		{"unsafe image_url ssrf", "openai", map[string]any{
			"model": "glm-5.3-flash",
			"messages": []any{map[string]any{
				"role": "user",
				"content": []any{
					map[string]any{"type": "image_url", "image_url": map[string]any{"url": "https://127.0.0.1:8080/internal.png"}},
				},
			}},
		}},
		{"unsafe anthropic image url", "anthropic", map[string]any{
			"model": "glm-5.3-flash",
			"messages": []any{map[string]any{
				"role": "user",
				"content": []any{
					map[string]any{"type": "image", "source": map[string]any{"type": "url", "url": "http://example.com/plain.png"}},
				},
			}},
		}},
	}

	for _, tc := range invalidCases {
		if err := ValidateRequest(tc.body, tc.dialect); err == nil {
			t.Fatalf("case %q (%s) succeeded, want validation error", tc.name, tc.dialect)
		}
	}
}
