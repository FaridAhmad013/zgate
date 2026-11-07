package models

import (
	"testing"
)

func TestExtractModelAndEffort(t *testing.T) {
	tests := []struct {
		inputModel  string
		inputEffort any
		wantModel   string
		wantEffort  string
	}{
		{"glm-5.3-flash:low", nil, "glm-5.3-flash", "low"},
		{"glm-5.3-flash:high", nil, "glm-5.3-flash", "high"},
		{"glm-5.3-flash:max", nil, "glm-5.3-flash", "max"},
		{"glm-5.3:medium", nil, "glm-5.3", "high"},
		{"glm-5.3-flash", "high", "glm-5.3-flash", "high"},
		{"glm-5.3-flash", "low", "glm-5.3-flash", "low"},
		{"glm-5.3-flash", nil, "glm-5.3-flash", "low"},
	}

	for _, tc := range tests {
		m, e := ExtractModelAndEffort(tc.inputModel, tc.inputEffort)
		if m != tc.wantModel || e != tc.wantEffort {
			t.Errorf("ExtractModelAndEffort(%q, %v) = (%q, %q), want (%q, %q)",
				tc.inputModel, tc.inputEffort, m, e, tc.wantModel, tc.wantEffort)
		}
	}
}

func TestOpenAIToAnthropicEffort(t *testing.T) {
	req := map[string]any{
		"model": "glm-5.3-flash:high",
		"messages": []any{
			map[string]any{"role": "user", "content": "hello"},
		},
	}
	out := OpenAIToAnthropic(req)
	if out["model"] != "glm-5.3-flash" {
		t.Fatalf("model = %v, want glm-5.3-flash", out["model"])
	}
	th, ok := out["thinking"].(map[string]any)
	if !ok {
		t.Fatalf("thinking not a map: %v", out["thinking"])
	}
	if th["effort"] != "high" {
		t.Fatalf("effort = %v, want high", th["effort"])
	}
}

func TestImageURLValidationAndDataURLPreservation(t *testing.T) {
	validDataURL := "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII="
	validHTTPS := "https://cdn.example.com/images/diagram.png"

	content := ToAnthropicContent([]any{
		map[string]any{"type": "text", "text": "describe these"},
		map[string]any{"type": "image_url", "image_url": map[string]any{"url": validDataURL}},
		map[string]any{"type": "image_url", "image_url": map[string]any{"url": validHTTPS}},
		// Dangerous / unsupported URLs that must be rejected:
		map[string]any{"type": "image_url", "image_url": map[string]any{"url": "http://cdn.example.com/insecure.png"}},
		map[string]any{"type": "image_url", "image_url": map[string]any{"url": "file:///etc/passwd"}},
		map[string]any{"type": "image_url", "image_url": map[string]any{"url": "javascript:alert(1)"}},
		map[string]any{"type": "image_url", "image_url": map[string]any{"url": "https://127.0.0.1:8080/internal.png"}},
		map[string]any{"type": "image_url", "image_url": map[string]any{"url": "https://localhost/internal.png"}},
		map[string]any{"type": "image_url", "image_url": map[string]any{"url": "https://169.254.169.254/latest/meta-data/"}},
		map[string]any{"type": "image_url", "image_url": map[string]any{"url": "https://10.0.0.12/private.png"}},
		map[string]any{"type": "image_url", "image_url": map[string]any{"url": "https://192.168.1.1/router.png"}},
		map[string]any{"type": "image_url", "image_url": map[string]any{"url": "https://172.16.5.4/intra.png"}},
		map[string]any{"type": "image_url", "image_url": map[string]any{"url": "https://[::1]/loopback.png"}},
		map[string]any{"type": "image_url", "image_url": map[string]any{"url": "https://service.internal/secret.png"}},
		map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:text/html;base64,PHNjcmlwdD4="}},
	})

	blocks, ok := content.([]map[string]any)
	if !ok {
		t.Fatalf("expected []map[string]any, got %T", content)
	}
	if len(blocks) != 3 {
		t.Fatalf("expected exactly 3 safe blocks (1 text, 1 data image, 1 https image), got %d: %+v", len(blocks), blocks)
	}
	src1 := blocks[1]["source"].(map[string]any)
	if src1["type"] != "base64" || src1["media_type"] != "image/png" {
		t.Fatalf("unexpected data URL block: %+v", src1)
	}
	src2 := blocks[2]["source"].(map[string]any)
	if src2["type"] != "url" || src2["url"] != validHTTPS {
		t.Fatalf("unexpected https URL block: %+v", src2)
	}
}
