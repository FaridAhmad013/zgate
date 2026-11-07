package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/apissaj/zcode-gateway/internal/transport"
)

func TestSessionKeyStabilityAndPrivacyAcrossTurns(t *testing.T) {
	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)

	turn1 := map[string]any{
		"model": "glm-5.3-flash",
		"messages": []any{
			map[string]any{"role": "system", "content": "You are a coding assistant."},
			map[string]any{"role": "user", "content": "How do I reverse a linked list in Go?"},
		},
	}
	turn2 := map[string]any{
		"model": "glm-5.3-flash",
		"messages": []any{
			map[string]any{"role": "system", "content": "You are a coding assistant."},
			map[string]any{"role": "user", "content": "How do I reverse a linked list in Go?"},
			map[string]any{"role": "assistant", "content": "Iterate with prev, curr, next pointers."},
			map[string]any{"role": "user", "content": "Can you write a unit test for it?"},
		},
	}
	turn3 := map[string]any{
		"model": "glm-5.3-flash",
		"messages": []any{
			map[string]any{"role": "system", "content": "You are a coding assistant."},
			map[string]any{"role": "user", "content": "How do I reverse a linked list in Go?"},
			map[string]any{"role": "assistant", "content": "Iterate with prev, curr, next pointers."},
			map[string]any{"role": "user", "content": "Can you write a unit test for it?"},
			map[string]any{"role": "assistant", "content": "Here is TestReverse..."},
			map[string]any{"role": "user", "content": "Now benchmark it."},
		},
	}
	otherConv := map[string]any{
		"model": "glm-5.3-flash",
		"messages": []any{
			map[string]any{"role": "system", "content": "You are a coding assistant."},
			map[string]any{"role": "user", "content": "Explain PostgreSQL MVCC."},
		},
	}

	k1 := SessionKeyFor(req, turn1)
	k2 := SessionKeyFor(req, turn2)
	k3 := SessionKeyFor(req, turn3)
	kOther := SessionKeyFor(req, otherConv)

	if k1 == "" || k1 != k2 || k2 != k3 {
		t.Fatalf("session key must stay identical across turns: turn1=%q turn2=%q turn3=%q", k1, k2, k3)
	}
	if k1 == kOther {
		t.Fatalf("distinct conversations must get distinct keys: %q vs %q", k1, kOther)
	}
	if strings.Contains(k1, "linked list") || strings.Contains(k1, "coding assistant") {
		t.Fatalf("session key must not leak raw conversation text: %q", k1)
	}
}

func TestSSECRLFMultilineMessageIDAndErrorStreamHandling(t *testing.T) {
	setupBridge(t)
	Configure(Config{Port: 0, Host: "127.0.0.1", BridgeToken: "", ModelsCache: 100 * time.Millisecond})

	var streamMode string
	customUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch streamMode {
		case "crlf-multiline":
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			// CRLF delimiters + multiline JSON data payload + custom message.id.
			_, _ = fmt.Fprintf(w, "event: message_start\r\ndata: {\"type\":\"message_start\",\r\ndata: \"message\":{\"id\":\"msg_custom_777\",\"model\":\"glm-5.3-flash\"}}\r\n\r\n")
			_, _ = fmt.Fprintf(w, "event: content_block_delta\r\ndata: {\"type\":\"content_block_delta\",\"index\":0,\r\ndata: \"delta\":{\"type\":\"text_delta\",\"text\":\"CRLF-\"}}\r\n\r\n")
			_, _ = fmt.Fprintf(w, "event: content_block_delta\r\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"OK\"}}\r\n\r\n")
			_, _ = fmt.Fprintf(w, "event: message_delta\r\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"input_tokens\":5,\"output_tokens\":2}}\r\n\r\n")
			_, _ = fmt.Fprintf(w, "event: message_stop\r\ndata: {\"type\":\"message_stop\"}\r\n\r\n")
		case "midstream-error":
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			_, _ = fmt.Fprintf(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_err_1\"}}\n\n")
			_, _ = fmt.Fprintf(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"PARTIAL\"}}\n\n")
			_, _ = fmt.Fprintf(w, "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"Overloaded token Bearer secret-jwt-12345\"}}\n\n")
		case "upstream-401":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = fmt.Fprintf(w, `{"code":401,"msg":"jwt revoked"}`)
		}
	}))
	defer customUpstream.Close()

	origInf := transport.InferenceOrigin
	transport.InferenceOrigin = customUpstream.URL
	defer func() { transport.InferenceOrigin = origInf }()

	chatStreamReq := map[string]any{
		"model":    "glm-5.3-flash",
		"stream":   true,
		"messages": []any{map[string]any{"role": "user", "content": "test stream"}},
	}

	// 1. CRLF + multiline SSE events + upstream message ID preservation.
	streamMode = "crlf-multiline"
	status, text := rawRequest(t, "POST", "/v1/chat/completions", "application/json", chatStreamReq, nil)
	if status != http.StatusOK {
		t.Fatalf("CRLF stream status = %d: %s", status, text)
	}
	if !strings.Contains(text, "CRLF-") || !strings.Contains(text, `"OK"`) {
		t.Fatalf("CRLF stream missing text deltas: %s", text)
	}
	if !strings.Contains(text, `"id":"msg_custom_777"`) {
		t.Fatalf("OpenAI stream chunks did not preserve upstream message.id msg_custom_777: %s", text)
	}
	if strings.Contains(text, "chatcmpl-stream") {
		t.Fatalf("OpenAI stream fell back to chatcmpl-stream despite message_start id: %s", text)
	}
	if !strings.Contains(text, "data: [DONE]\n\n") {
		t.Fatalf("CRLF stream missing [DONE]: %s", text)
	}

	// 2. Mid-stream upstream error event translated to OpenAI error frame, secrets redacted, no fake stop, terminated with [DONE].
	streamMode = "midstream-error"
	status, text = rawRequest(t, "POST", "/v1/chat/completions", "application/json", chatStreamReq, nil)
	if status != http.StatusOK {
		t.Fatalf("midstream error status = %d", status)
	}
	if !strings.Contains(text, `"error"`) || !strings.Contains(text, `"overloaded_error"`) {
		t.Fatalf("expected mid-stream error event in OpenAI output, got: %s", text)
	}
	if strings.Contains(text, "secret-jwt-12345") {
		t.Fatalf("mid-stream error leaked secret token: %s", text)
	}
	if strings.Contains(text, `"finish_reason":"stop"`) {
		t.Fatalf("mid-stream error must not emit fake finish_reason:stop: %s", text)
	}
	if !strings.Contains(text, "data: [DONE]\n\n") {
		t.Fatalf("mid-stream error must terminate with [DONE]: %s", text)
	}

	// 3. Upstream 401 (provider credential rejected) maps to 502 Bad Gateway (not 401 bridge auth error).
	streamMode = "upstream-401"
	status, text = rawRequest(t, "POST", "/v1/chat/completions", "application/json", map[string]any{
		"model":    "glm-5.3-flash",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}, nil)
	if status != http.StatusBadGateway {
		t.Fatalf("upstream 401 should map to 502 Bad Gateway so clients don't mistake it for bridge auth error, got %d: %s", status, text)
	}
	var errBody map[string]any
	_ = json.Unmarshal([]byte(text), &errBody)
	errMap, _ := errBody["error"].(map[string]any)
	if errMap == nil || errMap["type"] != "upstream_authentication_error" {
		t.Fatalf("expected OpenAI error shape with type=upstream_authentication_error, got: %s", text)
	}
}

func TestRequestValidationAndDialectErrorFormatOnRoutes(t *testing.T) {
	setupBridge(t)
	Configure(Config{Port: 0, Host: "127.0.0.1", BridgeToken: "", ModelsCache: 100 * time.Millisecond})

	// 1. Invalid messages on OpenAI endpoint -> 400 in OpenAI error format.
	status, text := rawRequest(t, "POST", "/v1/chat/completions", "application/json", map[string]any{
		"model":    "glm-5.3-flash",
		"messages": "not-an-array",
	}, nil)
	if status != http.StatusBadRequest {
		t.Fatalf("invalid messages on OpenAI route -> %d, want 400", status)
	}
	var oaiErr map[string]any
	_ = json.Unmarshal([]byte(text), &oaiErr)
	if oaiErr["type"] != nil || oaiErr["error"] == nil {
		t.Fatalf("expected OpenAI error envelope on /v1/chat/completions, got: %s", text)
	}

	// 2. Invalid messages on Anthropic endpoint -> 400 in Anthropic error format.
	status, text = rawRequest(t, "POST", "/v1/messages", "application/json", map[string]any{
		"model":    "glm-5.3-flash",
		"messages": []any{},
	}, nil)
	if status != http.StatusBadRequest {
		t.Fatalf("empty messages on Anthropic route -> %d, want 400", status)
	}
	var anthErr map[string]any
	_ = json.Unmarshal([]byte(text), &anthErr)
	if anthErr["type"] != "error" || anthErr["error"] == nil {
		t.Fatalf("expected Anthropic error envelope on /v1/messages, got: %s", text)
	}
}
