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
