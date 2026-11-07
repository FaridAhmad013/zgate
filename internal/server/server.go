// Package server is the HTTP front for the zcode start-plan lane.
//
// Routes:
//
//	POST /v1/messages           Anthropic wire format (also /v1/messages?beta=true)
//	POST /v1/chat/completions   OpenAI wire format
//	GET  /v1/models             live plan entitlements (fallback: static list)
//	GET  /v1/quota, /quota      billing readout
//	GET  /health                lane + credential state
//
// Auth for local clients is optional: set ZCODE_BRIDGE_TOKEN to require
// `Authorization: Bearer <token>` (or x-api-key), which keeps the bridge
// from being an open relay on a shared machine.
package server

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/apissaj/zcode-gateway/internal/accounts"
	"github.com/apissaj/zcode-gateway/internal/auth"
	"github.com/apissaj/zcode-gateway/internal/models"
	"github.com/apissaj/zcode-gateway/internal/quota"
	"github.com/apissaj/zcode-gateway/internal/transport"
)

const maxBodyBytes = 32 << 20

// Config is the server's runtime configuration.
type Config struct {
	Port        int
	Host        string
	BridgeToken string
	ModelsCache time.Duration
}

var (
	cfgMu sync.RWMutex
	cfg   = Config{Host: "127.0.0.1", Port: 20131, ModelsCache: 30 * time.Second}
)

var hostnamePattern = regexp.MustCompile(`^[a-zA-Z0-9]([a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?(\.[a-zA-Z0-9]([a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?)*$`)

// ResolveBindHost resolves the network bind address from environment variables.
// Priority: BIND_HOST -> HOST -> "127.0.0.1".
func ResolveBindHost() string {
	if v := strings.TrimSpace(os.Getenv("BIND_HOST")); v != "" {
		return v
	}
	if v := strings.TrimSpace(os.Getenv("HOST")); v != "" {
		return v
	}
	return "127.0.0.1"
}

// ValidateHost validates that host is a syntactically valid IP address or hostname
// without scheme, path, or port components.
func ValidateHost(host string) error {
	h := strings.TrimSpace(host)
	if h == "" {
		return errors.New("bind host must not be empty")
	}
	if strings.HasPrefix(h, "[") && strings.HasSuffix(h, "]") {
		h = h[1 : len(h)-1]
	}
	if strings.ContainsAny(h, " \t\r\n/\\?#@") {
		return fmt.Errorf("invalid bind host %q", host)
	}
	if ip := net.ParseIP(h); ip != nil {
		return nil
	}
	if strings.Contains(h, ":") {
		return fmt.Errorf("invalid bind host %q (must not include port or malformed IPv6)", host)
	}
	if looksNumericIPv4(h) {
		return fmt.Errorf("invalid IPv4 bind host %q", host)
	}
	if len(h) > 253 || !hostnamePattern.MatchString(h) {
		return fmt.Errorf("invalid bind host %q", host)
	}
	return nil
}

func looksNumericIPv4(h string) bool {
	parts := strings.Split(h, ".")
	for _, p := range parts {
		if p == "" {
			return true
		}
		for i := 0; i < len(p); i++ {
			if p[i] < '0' || p[i] > '9' {
				return false
			}
		}
	}
	return true
}

// IsLoopbackHost reports whether host refers exclusively to the local loopback interface.
func IsLoopbackHost(host string) bool {
	h := strings.TrimSpace(host)
	if strings.HasPrefix(h, "[") && strings.HasSuffix(h, "]") {
		h = h[1 : len(h)-1]
	}
	if strings.EqualFold(h, "localhost") {
		return true
	}
	if ip := net.ParseIP(h); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

// ValidateConfig enforces network binding and authentication invariants before startup.
func ValidateConfig(c Config) error {
	if err := ValidateHost(c.Host); err != nil {
		return err
	}
	if c.Port < 0 || c.Port > 65535 {
		return fmt.Errorf("invalid port %d", c.Port)
	}
	if !IsLoopbackHost(c.Host) && strings.TrimSpace(c.BridgeToken) == "" {
		return fmt.Errorf("refusing to bind non-loopback host %q without ZCODE_BRIDGE_TOKEN", c.Host)
	}
	return nil
}

// Configure sets the package-level config before ListenAndServe.
func Configure(c Config) {
	if strings.TrimSpace(c.Host) == "" {
		c.Host = "127.0.0.1"
	}
	if c.ModelsCache <= 0 {
		c.ModelsCache = 30 * time.Second
	}
	cfgMu.Lock()
	cfg = c
	cfgMu.Unlock()
	modelsMu.Lock()
	modelsCache = nil
	modelsMu.Unlock()
}

func getConfig() Config {
	cfgMu.RLock()
	defer cfgMu.RUnlock()
	return cfg
}

func redactMsg(msg string) string {
	return auth.RedactSecrets(msg, getConfig().BridgeToken)
}

func sendJSON(w http.ResponseWriter, status int, payload any) {
	body, _ := json.Marshal(payload)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Length", fmt.Sprint(len(body)))
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func anthropicError(w http.ResponseWriter, status int, message, typ string) {
	if typ == "" {
		typ = "api_error"
	}
	sendJSON(w, status, map[string]any{"type": "error", "error": map[string]any{"type": typ, "message": redactMsg(message)}})
}

func openAIError(w http.ResponseWriter, status int, message string, code any) {
	sendJSON(w, status, map[string]any{"error": map[string]any{"message": redactMsg(message), "type": "invalid_request_error", "code": code}})
}

func constantTimeTokenEqual(presented, expected string) bool {
	if expected == "" || presented == "" {
		return false
	}
	ph := sha256.Sum256([]byte(presented))
	eh := sha256.Sum256([]byte(expected))
	hashEq := subtle.ConstantTimeCompare(ph[:], eh[:])
	lenEq := subtle.ConstantTimeEq(int32(len(presented)), int32(len(expected)))
	return (hashEq & lenEq) == 1
}

func authorized(r *http.Request) bool {
	expected := strings.TrimSpace(getConfig().BridgeToken)
	if expected == "" {
		return IsLoopbackHost(getConfig().Host)
	}
	header := r.Header.Get("Authorization")
	presented := ""
	if len(header) >= 7 && strings.EqualFold(header[:7], "bearer ") {
		presented = strings.TrimSpace(header[7:])
	}
	if presented == "" {
		presented = strings.TrimSpace(r.Header.Get("X-Api-Key"))
	}
	return constantTimeTokenEqual(presented, expected)
}

func readBody(r *http.Request) ([]byte, error) {
	return io.ReadAll(io.LimitReader(r.Body, maxBodyBytes+1))
}

func parseJSON(raw []byte) (map[string]any, bool) {
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil || m == nil {
		return nil, false
	}
	return m, true
}

// SessionKeyFor derives the conversation affinity key. The gateway meters
// plan usage per session and flags brand-new session churn, so one client
// conversation must keep one derived session_<32hex> (see
// systemprompt.SessionIDFor). Sources, in order: OpenAI `user`, Anthropic
// metadata.user_id (the desktop shape carries session_id inside), a caller
// x-session-id header, else the first user turn + conversation size.
func SessionKeyFor(r *http.Request, body map[string]any) string {
	if u, ok := body["user"].(string); ok && strings.TrimSpace(u) != "" {
		return strings.TrimSpace(u)
	}
	if meta, ok := body["metadata"].(map[string]any); ok {
		if uid, ok := meta["user_id"].(string); ok && strings.TrimSpace(uid) != "" {
			// The desktop shape embeds JSON: {"device_id":...,"session_id":"..."}.
			if embedded := extractJSONField(uid, "session_id"); embedded != "" {
				return embedded
			}
			return strings.TrimSpace(uid)
		}
	}
	if inbound := r.Header.Get("X-Session-Id"); strings.TrimSpace(inbound) != "" {
		return strings.TrimSpace(inbound)
	}
	msgs, _ := body["messages"].([]any)
	first := ""
	for _, m := range msgs {
		if mm, ok := m.(map[string]any); ok && mm["role"] == "user" {
			if c, ok := mm["content"].(string); ok {
				first = c
			} else {
				raw, _ := json.Marshal(mm["content"])
				first = string(raw)
			}
			break
		}
	}
	if len(first) > 384 {
		first = first[:384]
	}
	return fmt.Sprintf("anon:%s:%d", first, len(msgs))
}

// extractJSONField pulls one top-level string field out of an embedded JSON
// string without a full unmarshal (best-effort).
func extractJSONField(s, field string) string {
	key := `"` + field + `"`
	i := strings.Index(s, key)
	if i < 0 {
		return ""
	}
	rest := s[i+len(key):]
	j := strings.Index(rest, `"`)
	if j < 0 {
		return ""
	}
	rest = rest[j+1:]
	k := strings.Index(rest, `"`)
	if k < 0 {
		return ""
	}
	return rest[:k]
}

// relayUpstreamError copies an upstream error into the caller's dialect.
func relayUpstreamError(w http.ResponseWriter, err error, dialect string) {
	message := err.Error()
	status := 502
	var code any
	if ue, ok := err.(*transport.UpstreamError); ok {
		if ue.Status > 0 {
			status = ue.Status
		}
		code = ue.Code
	}
	if dialect == "openai" {
		openAIError(w, status, message, code)
		return
	}
	typ := "api_error"
	if f, ok := code.(float64); ok && f == 3012 {
		typ = "rate_limit_error"
	}
	anthropicError(w, status, message, typ)
}

// handleMessages serves POST /v1/messages (Anthropic dialect).
func handleMessages(w http.ResponseWriter, r *http.Request, body map[string]any, accountID string) {
	stream, _ := body["stream"].(bool)
	payload := body
	rawModel := stringOf(body["model"])
	baseModel, effort := models.ExtractModelAndEffort(rawModel, body["reasoning_effort"])
	payload["model"] = models.NormalizeModel(baseModel)
	if payload["thinking"] == nil {
		payload["thinking"] = map[string]any{"type": "enabled", "effort": effort}
	} else if th, ok := payload["thinking"].(map[string]any); ok {
		if th["effort"] == nil && th["budget_tokens"] == nil {
			th["effort"] = effort
		}
	}
	upstream, err := transport.SendAnthropic(transport.SendOpts{
		Body:            payload,
		AccountID:       accountID,
		ClientSessionID: SessionKeyFor(r, body),
		Context:         r.Context(),
	})
	if err != nil {
		relayUpstreamError(w, err, "anthropic")
		return
	}
	defer upstream.Body.Close()
	if stream {
		pipeStream(w, upstream, "anthropic", stringOf(payload["model"]))
		return
	}
	raw, err := io.ReadAll(io.LimitReader(upstream.Body, 32<<20))
	if err != nil || !isJSON(raw) {
		anthropicError(w, 502, "Upstream returned a non-JSON response", "")
		return
	}
	sendJSON(w, 200, json.RawMessage(raw))
}

// handleChatCompletions serves POST /v1/chat/completions (OpenAI dialect).
func handleChatCompletions(w http.ResponseWriter, r *http.Request, body map[string]any, accountID string) {
	stream, _ := body["stream"].(bool)
	translated := models.OpenAIToAnthropic(body)
	model := stringOf(translated["model"])
	upstream, err := transport.SendAnthropic(transport.SendOpts{
		Body:            translated,
		AccountID:       accountID,
		ClientSessionID: SessionKeyFor(r, body),
		Context:         r.Context(),
	})
	if err != nil {
		relayUpstreamError(w, err, "openai")
		return
	}
	defer upstream.Body.Close()
	if stream {
		pipeStream(w, upstream, "openai", model)
		return
	}
	raw, err := io.ReadAll(io.LimitReader(upstream.Body, 32<<20))
	if err != nil {
		openAIError(w, 502, "Upstream returned a non-JSON response", nil)
		return
	}
	var result map[string]any
	if json.Unmarshal(raw, &result) != nil || result == nil {
		openAIError(w, 502, "Upstream returned a non-JSON response", nil)
		return
	}
	sendJSON(w, 200, models.AnthropicToOpenAI(result, model))
}

func stringOf(v any) string {
	s, _ := v.(string)
	return s
}

func isJSON(raw []byte) bool {
	var m any
	return json.Unmarshal(raw, &m) == nil
}

// pipeStream streams upstream SSE frames to the caller. Anthropic frames
// pass through untouched; OpenAI callers get frames rewritten into
// chat.completion.chunk.
func pipeStream(w http.ResponseWriter, upstream *http.Response, dialect, model string) {
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(200)
	flusher, _ := w.(http.Flusher)

	reader := upstream.Body
	buf := make([]byte, 16<<10)
	var carry string
	streamState := &chunkState{toolByBlock: map[float64]int{}, nextToolIndex: 0}

	writeSSE := func(frame string) bool {
		if _, err := io.WriteString(w, frame); err != nil {
			return false
		}
		if flusher != nil {
			flusher.Flush()
		}
		return true
	}

	for {
		n, err := reader.Read(buf)
		if n > 0 {
			carry += string(buf[:n])
			if dialect != "openai" {
				if !writeSSE(string(buf[:n])) {
					return
				}
				carry = ""
			} else {
				for {
					idx := strings.Index(carry, "\n\n")
					if idx < 0 {
						break
					}
					frame := carry[:idx]
					carry = carry[idx+2:]
					var dataLine string
					for _, line := range strings.Split(frame, "\n") {
						if strings.HasPrefix(line, "data:") {
							dataLine = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
							break
						}
					}
					if dataLine == "" || dataLine == "[DONE]" {
						continue
					}
					var event map[string]any
					if json.Unmarshal([]byte(dataLine), &event) != nil {
						continue
					}
					if chunk := toOpenAIChunk(event, model, streamState); chunk != nil {
						encoded, _ := json.Marshal(chunk)
						if !writeSSE("data: " + string(encoded) + "\n\n") {
							return
						}
					}
					if event["type"] == "message_stop" {
						if !writeSSE("data: [DONE]\n\n") {
							return
						}
					}
				}
			}
		}
		if err != nil {
			// Drain any buffered frame before the EOF/error.
			if dialect == "openai" {
				if leftover := flushOpenAIFrame(carry, model, streamState); leftover != "" {
					if !writeSSE(leftover) {
						return
					}
				}
			} else if carry != "" {
				if !writeSSE(carry) {
					return
				}
			}
			if err != io.EOF {
				// Stream aborted mid-way: report in-band.
				safeErr := redactMsg(err.Error())
				if dialect == "openai" {
					frame, _ := json.Marshal(map[string]any{"error": map[string]any{"message": safeErr, "type": "upstream_error"}})
					_ = writeSSE("data: " + string(frame) + "\n\n")
				} else {
					frame, _ := json.Marshal(map[string]any{"type": "error", "error": map[string]any{"type": "api_error", "message": safeErr}})
					_ = writeSSE("data: " + string(frame) + "\n\n")
				}
			}
			return
		}
	}
}

// flushOpenAIFrame converts one buffered SSE frame (possibly without its
// trailing blank line) into an OpenAI chunk write; returns the bytes to
// write or "" when nothing translatable remains.
func flushOpenAIFrame(frame, model string, state *chunkState) string {
	var dataLine string
	for _, line := range strings.Split(frame, "\n") {
		if strings.HasPrefix(line, "data:") {
			dataLine = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			break
		}
	}
	if dataLine == "" || dataLine == "[DONE]" {
		return ""
	}
	var event map[string]any
	if json.Unmarshal([]byte(dataLine), &event) != nil {
		return ""
	}
	chunk := toOpenAIChunk(event, model, state)
	if chunk == nil {
		if event["type"] == "message_stop" {
			return "data: [DONE]\n\n"
		}
		return ""
	}
	encoded, _ := json.Marshal(chunk)
	out := "data: " + string(encoded) + "\n\n"
	if event["type"] == "message_stop" {
		out += "data: [DONE]\n\n"
	}
	return out
}

// chunkState tracks Anthropic block indexes -> OpenAI tool_call indexes.
// Anthropic numbers content blocks globally (thinking=0, tool_use=1...);
// OpenAI expects tool_calls numbered from 0 within the message.
type chunkState struct {
	toolByBlock   map[float64]int
	nextToolIndex int
}

// toOpenAIChunk maps one Anthropic stream event onto an OpenAI
// chat.completion.chunk. nil = drop the event.
func toOpenAIChunk(event map[string]any, model string, state *chunkState) map[string]any {
	id := stringOf(event["id"])
	if id == "" {
		id = "chatcmpl-stream"
	}
	base := map[string]any{
		"id":      id,
		"object":  "chat.completion.chunk",
		"created": time.Now().Unix(),
		"model":   model,
	}
	typ := stringOf(event["type"])
	switch typ {
	case "message_start":
		base["choices"] = []any{map[string]any{"index": 0, "delta": map[string]any{"role": "assistant", "content": ""}, "finish_reason": nil}}
		return base
	case "content_block_delta":
		delta, _ := event["delta"].(map[string]any)
		if delta == nil {
			return nil
		}
		switch delta["type"] {
		case "text_delta":
			t, _ := delta["text"].(string)
			base["choices"] = []any{map[string]any{"index": 0, "delta": map[string]any{"content": t}, "finish_reason": nil}}
			return base
		case "thinking_delta":
			t, _ := delta["thinking"].(string)
			base["choices"] = []any{map[string]any{"index": 0, "delta": map[string]any{"reasoning_content": t}, "finish_reason": nil}}
			return base
		case "input_json_delta":
			blockIdx, _ := event["index"].(float64)
			toolIndex, ok := state.toolByBlock[blockIdx]
			if !ok {
				return nil // text/thinking block, not a tool
			}
			pj, _ := delta["partial_json"].(string)
			base["choices"] = []any{map[string]any{"index": 0, "delta": map[string]any{
				"tool_calls": []any{map[string]any{"index": toolIndex, "function": map[string]any{"arguments": pj}}},
			}, "finish_reason": nil}}
			return base
		}
		return nil
	case "content_block_start":
		block, _ := event["content_block"].(map[string]any)
		if block == nil || block["type"] != "tool_use" {
			return nil
		}
		toolIndex := state.nextToolIndex
		state.nextToolIndex++
		blockIdx, _ := event["index"].(float64)
		state.toolByBlock[blockIdx] = toolIndex
		name, _ := block["name"].(string)
		bid, _ := block["id"].(string)
		base["choices"] = []any{map[string]any{"index": 0, "delta": map[string]any{
			"tool_calls": []any{map[string]any{
				"index": toolIndex, "id": bid, "type": "function",
				"function": map[string]any{"name": name, "arguments": ""},
			}},
		}, "finish_reason": nil}}
		return base
	case "message_delta":
		delta, _ := event["delta"].(map[string]any)
		if delta == nil {
			return nil
		}
		stop, _ := delta["stop_reason"].(string)
		finish := "stop"
		switch stop {
		case "tool_use":
			finish = "tool_calls"
		case "max_tokens":
			finish = "length"
		}
		chunk := map[string]any{
			"id": id, "object": "chat.completion.chunk", "created": time.Now().Unix(), "model": model,
			"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": finish}},
		}
		if usage, ok := event["usage"].(map[string]any); ok {
			chunk["usage"] = models.ToOpenAIUsage(usage)
		}
		return chunk
	}
	return nil
}

// --- /v1/models ---

type cacheEntry struct {
	at   time.Time
	list any
}

var (
	modelsMu    sync.Mutex
	modelsCache *cacheEntry
)

// liveModels builds /v1/models from live entitlements (per stored account,
// summed); static catalogue when billing is down.
func liveModels(ctx context.Context) any {
	modelsMu.Lock()
	defer modelsMu.Unlock()
	now := time.Now()
	cacheTTL := getConfig().ModelsCache
	if cacheTTL <= 0 {
		cacheTTL = 30 * time.Second
	}
	if modelsCache != nil && now.Sub(modelsCache.at) < cacheTTL {
		return modelsCache.list
	}
	var list any = staticModelList()
	cands, err := transport.Candidates("", "")
	if err == nil {
		grouped := map[string]*liveModel{}
		var order []string
		serverTime := now.Unix()
		for _, acct := range cands {
			q, err := quota.ReadQuota(ctx, acct.JWT, acct.DeviceID)
			if err != nil {
				continue // one dead account must not blank the list
			}
			if q.ServerTime > 0 {
				serverTime = q.ServerTime
			}
			for id, g := range quota.ByModel(q.Rows) {
				m := grouped[id]
				if m == nil {
					m = &liveModel{ID: id, Object: "model", Created: serverTime, OwnedBy: "zcode-plan", DisplayName: g.Model}
					grouped[id] = m
					order = append(order, id)
				}
				m.RemainingTokens += g.RemainingUnits
				label := firstNonEmpty(acct.Label, acct.ID, "default")
				for _, p := range g.Plans {
					m.Plans = append(m.Plans, planView{
						Plan: p.Plan, RemainingUnits: p.RemainingUnits,
						Period: p.Period, PeriodEnd: p.PeriodEnd, EndsAt: p.EndsAt,
						Account: label,
					})
				}
			}
		}
		if grouped["glm-5.3"] == nil {
			grouped["glm-5.3"] = &liveModel{
				ID: "glm-5.3", Object: "model", Created: serverTime, OwnedBy: "zcode-plan", DisplayName: "GLM-5.3",
				RemainingTokens: 500000000,
			}
			order = append(order, "glm-5.3")
		}
		if len(order) > 0 {
			out := make([]liveModel, 0, len(order)*4)
			for _, id := range order {
				base := *grouped[id]
				out = append(out, base)
				for _, eff := range []string{"low", "high", "max"} {
					variant := base
					variant.ID = id + ":" + eff
					variant.DisplayName = base.DisplayName + " (" + strings.ToUpper(eff) + ")"
					out = append(out, variant)
				}
			}
			list = out
		}
	}
	modelsCache = &cacheEntry{at: now, list: list}
	return list
}

type liveModel struct {
	ID              string     `json:"id"`
	Object          string     `json:"object"`
	Created         int64      `json:"created"`
	OwnedBy         string     `json:"owned_by"`
	DisplayName     string     `json:"display_name"`
	RemainingTokens float64    `json:"plan_remaining_tokens"`
	Plans           []planView `json:"plans"`
}

type planView struct {
	Plan           string `json:"plan"`
	RemainingUnits any    `json:"remaining_units"`
	Period         any    `json:"period"`
	PeriodEnd      any    `json:"period_end"`
	EndsAt         any    `json:"ends_at"`
	Account        string `json:"account"`
}

type staticModel struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	OwnedBy string `json:"owned_by"`
}

func staticModelList() []staticModel {
	out := make([]staticModel, 0, 24)
	for _, m := range models.ListModels() {
		out = append(out, staticModel{ID: m.ID, Object: "model", OwnedBy: "zcode-plan"})
		for _, eff := range []string{"low", "high", "max"} {
			out = append(out, staticModel{ID: m.ID + ":" + eff, Object: "model", OwnedBy: "zcode-plan"})
		}
	}
	return out
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// accountIDOf resolves the ?account= / x-zcode-account pin.
func accountIDOf(r *http.Request) string {
	if v := r.URL.Query().Get("account"); v != "" {
		return v
	}
	return r.Header.Get("X-Zcode-Account")
}

// accountsSummary mirrors accounts.Summarize for /health.
func accountsSummary() []accounts.Summary { return accounts.Summarize() }
