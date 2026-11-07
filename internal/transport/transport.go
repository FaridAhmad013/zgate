// Package transport is the one place that talks to the zcode start-plan
// gateway.
//
// The lane, verified live 2026-10-30 against a running account:
//
//	POST https://zcode.z.ai/api/v1/zcode-plan/anthropic/v1/messages
//	Authorization: Bearer <plan JWT>
//	+ the client's identity headers (identity LLM face)
//	+ the client's 3-block identity system prompt (systemprompt)
//	-> 200 text/event-stream, GLM-5.3 / GLM-5.3-Flash on plan quota.
//
// What this lane does NOT use (each tried, each wrong):
//   - the V4 signing handshake (X-Client-Sig/X-Client-Pow/X-App-Id). Signed
//     requests authenticate but land on the paid lane and answer
//     `429 1113 "Insufficient balance or no resource package"` even with a
//     plan balance.
//   - X-Zcode-Authorization: injected by the client only under
//     /api/v1/ultra*; on this path the bearer itself is the plan JWT.
//   - x-api-key: the desktop client mirrors the JWT into it, the gateway
//     ignores it — omitting it is fine.
//
// The gateway also content-inspects the `system` field: a request without
// the exact identity blocks is refused with `405 {"code":3012}` ("unusual
// activity") regardless of headers — the 3012 is a prompt-shape rejection,
// not only an abuse one.
package transport

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/apissaj/zcode-gateway/internal/accounts"
	"github.com/apissaj/zcode-gateway/internal/auth"
	"github.com/apissaj/zcode-gateway/internal/identity"
	"github.com/apissaj/zcode-gateway/internal/systemprompt"
)

// InferenceOrigin and PlanAnthropicPath locate the plan lane.
var (
	InferenceOrigin   = envOr("ZCODE_INFERENCE_ORIGIN", "https://zcode.z.ai")
	PlanAnthropicPath = "/api/v1/zcode-plan/anthropic/v1/messages"
)

// UpstreamError carries the gateway's status/code to the client dialect.
type UpstreamError struct {
	Msg           string
	Status        int
	Body          any
	Kind          string // upstream | config | protocol
	Code          any    // gateway envelope code (3012/1113/3008/3009/3001)
	AccountSwitch bool   // internal: rotation may continue with another account
}

func (e *UpstreamError) Error() string { return auth.Redact(e.Msg) }

func envOr(key, fallback string) string {
	if v := strings.TrimSpace(osGetenv(key)); v != "" {
		return v
	}
	return fallback
}

// errorFromResponse maps the gateway's {code,msg,logid} envelope onto an
// UpstreamError with a human message per known code.
func errorFromResponse(status int, body []byte) *UpstreamError {
	var env struct {
		Code  any    `json:"code"`
		Msg   string `json:"msg"`
		Error *struct {
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"error"`
		Raw string `json:"-"`
	}
	text := auth.Redact(string(body))
	if json.Unmarshal(body, &env) != nil {
		env.Raw = text
	}
	env.Msg = auth.Redact(env.Msg)
	if env.Error != nil {
		env.Error.Message = auth.Redact(env.Error.Message)
	}
	code := env.Code
	message := ""
	switch code {
	case float64(3012):
		message = "zcode gateway blocked this request (3012). The system prompt must carry the exact ZCode identity blocks and the session must look like a real client. If this account was just very active, wait a minute and retry."
	case float64(1113):
		message = "zcode paid lane answered \"insufficient balance\" (1113): the request was not recognized as start-plan traffic."
	case float64(3009):
		message = fmt.Sprintf("model concurrency limit (3009)%s - retry shortly", msgSuffix(env.Msg))
	case float64(3008):
		message = fmt.Sprintf("account concurrency limit (3008)%s - the desktop client is busy on this account; retry shortly", msgSuffix(env.Msg))
	case float64(3001):
		message = fmt.Sprintf("gateway parameter error (3001)%s", msgSuffix(env.Msg))
	default:
		if env.Error != nil && env.Error.Message != "" {
			message = env.Error.Message
		} else {
			message = fmt.Sprintf("upstream %d", status)
		}
	}
	return &UpstreamError{Msg: auth.Redact(message), Status: status, Body: env, Kind: "upstream", Code: code}
}

func msgSuffix(msg string) string {
	if strings.TrimSpace(msg) == "" {
		return ""
	}
	return ": " + msg
}

// isTransient reports gateway responses worth retrying in place: model
// concurrency (3009), account concurrency (3008) when no alternate account is
// available, generic 429 (excluding 1113 quota exhaustion), and 5xx.
func isTransient(err *UpstreamError) bool {
	if err == nil {
		return false
	}
	if codeIs(err.Code, 1113) {
		return false
	}
	if codeIs(err.Code, 3009) || codeIs(err.Code, 3008) {
		return true
	}
	if err.Status == http.StatusTooManyRequests || err.Status >= 500 {
		return true
	}
	return false
}

// isSwitchable reports errors that justify rotating to the next account:
// this credential's allowance is spent (1113), it is rejected (401/403), or
// its user slot is busy on the desktop client (3008). Global model concurrency
// (3009) and 5xx get retried in place instead.
func isSwitchable(err *UpstreamError, status int) bool {
	if err == nil {
		return false
	}
	if codeIs(err.Code, 1113) || codeIs(err.Code, 3008) {
		return true
	}
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		return true
	}
	if status == http.StatusTooManyRequests && !codeIs(err.Code, 3009) {
		return true
	}
	return false
}

func concurrencyCooldown() time.Duration {
	if v := strings.TrimSpace(osGetenv("ZCODE_CONCURRENCY_COOLDOWN_MS")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return time.Duration(n) * time.Millisecond
		}
	}
	return 5 * time.Second
}

func codeIs(code any, want float64) bool {
	f, ok := code.(float64)
	return ok && f == want
}

// candidate is one account attempt for a request.
type candidate struct {
	ID          string // "" for the legacy/env single account
	Label       string
	JWT         string
	DeviceID    string
	SessionSeed string
	UserID      string
}

// legacyUserID extracts the account uuid from a JWT's user_id claim.
func legacyUserID(jwt string) string {
	claims, ok := auth.DecodeJWT(jwt)
	if !ok {
		return ""
	}
	if v, _ := claims["user_id"].(string); v != "" {
		return v
	}
	v, _ := claims["sub"].(string)
	return v
}

func newUUID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// Candidates resolves the accounts one request may try, in order: an
// explicit JWT wins; then a pinned account; then the stored ring; then the
// single-token fallbacks. Empty result = no credential at all.
func Candidates(jwt, accountID string) ([]candidate, error) {
	if strings.TrimSpace(jwt) != "" {
		mid := identity.ResolveDeviceMid()
		if mid == "" {
			mid = newUUID()
		}
		return []candidate{{JWT: jwt, DeviceID: mid, UserID: legacyUserID(jwt)}}, nil
	}
	if accountID != "" {
		for _, a := range accounts.List() {
			if a.ID == accountID {
				return []candidate{{ID: a.ID, Label: a.Label, JWT: a.JWT, DeviceID: a.DeviceID, SessionSeed: a.SessionSeed, UserID: a.UserID}}, nil
			}
		}
	}
	if stored := accounts.Usable(nil); len(stored) > 0 {
		out := make([]candidate, 0, len(stored))
		for _, a := range stored {
			out = append(out, candidate{ID: a.ID, Label: a.Label, JWT: a.JWT, DeviceID: a.DeviceID, SessionSeed: a.SessionSeed, UserID: a.UserID})
		}
		return out, nil
	}
	legacy := auth.ResolvePlanJWT("")
	if legacy == "" {
		return nil, &UpstreamError{
			Msg:    "No zcode plan JWT available. Run: zcode-gateway login (or keep the desktop client signed in, or set ZCODE_PLAN_JWT)",
			Status: 500, Kind: "config",
		}
	}
	mid := identity.ResolveDeviceMid()
	if mid == "" {
		mid = newUUID()
	}
	return []candidate{{JWT: legacy, DeviceID: mid, SessionSeed: osGetenv("ZCODE_SESSION_SEED"), Label: "default", UserID: legacyUserID(legacy)}}, nil
}

// ShapePlanBody shapes one Anthropic-format body for the plan gateway:
// canonical identity system blocks with the client's own system prompt
// riding block 3, session metadata, and sane sampling defaults.
func ShapePlanBody(body map[string]any, sessionID, deviceID, accountUUID string) (map[string]any, error) {
	if body == nil {
		return nil, errors.New("nil request body")
	}
	out := make(map[string]any, len(body)+4)
	for k, v := range body {
		out[k] = v
	}
	out["system"] = systemprompt.BuildSystem(body["system"])

	var meta map[string]any
	if origMeta, ok := body["metadata"].(map[string]any); ok && len(origMeta) > 0 {
		meta = make(map[string]any, len(origMeta)+1)
		for k, v := range origMeta {
			meta[k] = v
		}
	} else {
		meta = make(map[string]any, 1)
	}
	meta["user_id"] = systemprompt.MetadataUserID(deviceID, sessionID, accountUUID)
	out["metadata"] = meta

	if out["max_tokens"] == nil {
		out["max_tokens"] = 8192
	}
	if out["thinking"] == nil && systemprompt.ForceThinking() {
		out["thinking"] = map[string]any{"type": "enabled"}
	}
	return out, nil
}

var (
	laneMu  sync.Mutex
	laneSem chan struct{}
)

func resolveLaneWidth() int {
	if v := strings.TrimSpace(osGetenv("ZCODE_LANE_WIDTH")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return 1
}

func laneSlot() chan struct{} {
	want := resolveLaneWidth()
	laneMu.Lock()
	defer laneMu.Unlock()
	if laneSem == nil || (cap(laneSem) != want && len(laneSem) == 0) {
		laneSem = make(chan struct{}, want)
	}
	return laneSem
}

func acquireLane(ctx context.Context) (func(), error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	sem := laneSlot()
	select {
	case sem <- struct{}{}:
		var once sync.Once
		return func() {
			once.Do(func() { <-sem })
		}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// WithLane serializes inference calls through the configured concurrency lane
// (ZCODE_LANE_WIDTH, default 1). The gateway enforces a per-account in-flight
// limit shared with the desktop client (429 3008 user / 3009 model), so
// concurrent bridge clients queue instead of collectively tripping the limit.
func WithLane(ctx context.Context, fn func() error) error {
	release, err := acquireLane(ctx)
	if err != nil {
		return err
	}
	defer release()
	return fn()
}

// ErrStreamIdleTimeout is returned when an active stream receives no data
// for longer than the configured idle timeout.
var ErrStreamIdleTimeout = errors.New("upstream stream idle timeout")

var sharedTransport = &http.Transport{
	Proxy:                 http.ProxyFromEnvironment,
	DialContext:           (&net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
	ForceAttemptHTTP2:     true,
	MaxIdleConns:          100,
	MaxIdleConnsPerHost:   20,
	IdleConnTimeout:       90 * time.Second,
	TLSHandshakeTimeout:   10 * time.Second,
	ExpectContinueTimeout: 1 * time.Second,
}

var sharedClient = &http.Client{
	Transport: sharedTransport,
	CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

// SendOpts carries one upstream call's parameters.
type SendOpts struct {
	Body            map[string]any // Anthropic-format request
	JWT             string         // explicit token override (skips accounts)
	AccountID       string         // pin to one stored account
	ClientSessionID string         // conversation affinity key
	Context         context.Context
	HTTP            *http.Client
	UpstreamTimeout time.Duration // response header deadline (0: 2m or ZCODE_HEADER_TIMEOUT_MS / ZCODE_UPSTREAM_TIMEOUT_MS)
	IdleTimeout     time.Duration // per-read stream inactivity deadline (0: 2m or ZCODE_STREAM_IDLE_TIMEOUT_MS / ZCODE_UPSTREAM_TIMEOUT_MS)
}

func resolveHeaderTimeout(o SendOpts) time.Duration {
	if o.UpstreamTimeout > 0 {
		return o.UpstreamTimeout
	}
	for _, key := range []string{"ZCODE_HEADER_TIMEOUT_MS", "ZCODE_UPSTREAM_TIMEOUT_MS"} {
		if v := strings.TrimSpace(osGetenv(key)); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				return time.Duration(n) * time.Millisecond
			}
		}
	}
	return 2 * time.Minute
}

func resolveIdleTimeout(o SendOpts) time.Duration {
	if o.IdleTimeout > 0 {
		return o.IdleTimeout
	}
	for _, key := range []string{"ZCODE_STREAM_IDLE_TIMEOUT_MS", "ZCODE_UPSTREAM_TIMEOUT_MS"} {
		if v := strings.TrimSpace(osGetenv(key)); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				return time.Duration(n) * time.Millisecond
			}
		}
	}
	if o.UpstreamTimeout > 0 {
		return o.UpstreamTimeout
	}
	return 2 * time.Minute
}

// SendAnthropic sends one Anthropic-shaped request to the plan endpoint
// through the shared lane, rotating across stored accounts when a switchable
// error fires (allowance 1113/429, auth 401/403, account concurrency 3008).
// Each account gets its own transient retry budget (3009/5xx/network) first;
// failures cool that account down so the next request starts elsewhere. With
// a single account (or the legacy desktop/env token) the behavior matches
// the original plan-lane shape. The response body is NOT consumed here.
func SendAnthropic(o SendOpts) (*http.Response, error) {
	ctx := o.Context
	if ctx == nil {
		ctx = context.Background()
	}
	// Pre-flight check outside the lane so missing credentials fail fast
	// without acquiring or holding a lane slot.
	if _, err := Candidates(o.JWT, o.AccountID); err != nil {
		return nil, err
	}

	releaseLane, err := acquireLane(ctx)
	if err != nil {
		return nil, &UpstreamError{Msg: "client went away", Status: 499, Kind: "upstream"}
	}
	laneTransferred := false
	defer func() {
		if !laneTransferred {
			releaseLane()
		}
	}()

	// Re-resolve candidates inside the lane so cooldowns or token refreshes
	// performed by an earlier lane holder are immediately visible.
	cands, err := Candidates(o.JWT, o.AccountID)
	if err != nil {
		return nil, err
	}
	headerTimeout := resolveHeaderTimeout(o)
	idleTimeout := resolveIdleTimeout(o)
	client := o.HTTP
	if client == nil {
		client = sharedClient
	}

	var lastErr *UpstreamError
	tried := map[string]bool{}
	for attempt := 0; ; attempt++ {
		var acct *candidate
		remaining := 0
		for i := range cands {
			key := cands[i].ID
			if key == "" {
				key = fmt.Sprintf("legacy#%d", i)
			}
			if !tried[key] {
				if acct == nil {
					acct = &cands[i]
					tried[key] = true
				} else {
					remaining++
				}
			}
		}
		if acct == nil {
			break
		}
		hasMoreAccounts := remaining > 0
		res, err := runAccount(ctx, client, *acct, o, headerTimeout, idleTimeout, hasMoreAccounts)
		if err == nil {
			if acct.ID != "" {
				accounts.ClearCooldown(acct.ID)
			}
			if cb, ok := res.Body.(*cancelBody); ok {
				cb.release = releaseLane
			} else {
				res.Body = &cancelBody{ReadCloser: res.Body, release: releaseLane}
			}
			laneTransferred = true
			return res, nil
		}
		ue, ok := err.(*UpstreamError)
		if !ok {
			ue = &UpstreamError{Msg: err.Error(), Status: 502, Kind: "upstream"}
		}
		lastErr = ue
		if !ue.AccountSwitch {
			return nil, ue // global failure: no point switching
		}
		if acct.ID != "" {
			switch {
			case ue.Status == http.StatusUnauthorized || ue.Status == http.StatusForbidden:
				accounts.MarkReauth(acct.ID)
			case codeIs(ue.Code, 3008):
				// 3008 is transient account concurrency (desktop app active), not
				// quota exhaustion. Never lock out a single-account setup; when
				// multiple accounts exist, apply only a brief cooldown so the next
				// request prefers an idle account.
				if len(cands) > 1 {
					accounts.MarkCooldown(acct.ID, concurrencyCooldown())
				}
			case codeIs(ue.Code, 1113) || (ue.Status == http.StatusTooManyRequests && !codeIs(ue.Code, 3009)):
				accounts.MarkCooldown(acct.ID, 0)
			default:
				accounts.MarkCooldown(acct.ID, 2*time.Minute)
			}
		}
		_ = attempt
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, &UpstreamError{Msg: "no usable zcode account", Status: 502, Kind: "config"}
}

// runAccount runs one account's turn: transient retries in place, one
// singleflight refresh on 401, then a switchable error => throw for rotation.
func runAccount(ctx context.Context, client *http.Client, acct candidate, o SendOpts, headerTimeout, idleTimeout time.Duration, hasMoreAccounts bool) (*http.Response, error) {
	maxAttempts := 3
	if v := osGetenv("ZCODE_MAX_ATTEMPTS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			maxAttempts = n
		}
	}
	baseMs := 1500
	if v := osGetenv("ZCODE_RETRY_BASE_MS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			baseMs = n
		}
	}
	bearer := acct.JWT
	refreshed := false
	var lastErr *UpstreamError
	for n := 0; n < maxAttempts; n++ {
		res, attemptErr := attemptOnce(ctx, client, acct, bearer, o, headerTimeout, idleTimeout)
		if res != nil && res.StatusCode >= 200 && res.StatusCode < 300 {
			return res, nil
		}
		if attemptErr != nil {
			if ctx.Err() != nil {
				return nil, attemptErr // client hung up or deadline
			}
			if ue, ok := attemptErr.(*UpstreamError); ok {
				lastErr = ue
			} else {
				lastErr = &UpstreamError{Msg: attemptErr.Error(), Status: 502, Kind: "upstream"}
			}
		} else {
			body, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
			res.Body.Close()
			lastErr = errorFromResponse(res.StatusCode, body)
			// Re-arm the switchable flag for this turn's error.
			if isSwitchable(lastErr, lastErr.Status) {
				lastErr.AccountSwitch = true
			}
		}
		// 401 handling: a stored account gets one singleflight refresh (shared
		// across concurrent requests for the same account); the legacy token
		// re-reads its sources (the desktop app may have rotated the JWT
		// underneath us). Either way the retry does not consume the transient
		// budget. Runs before the switch return so a revoked-then-refreshed
		// account recovers in place.
		if lastErr.Status == http.StatusUnauthorized && !refreshed {
			refreshed = true
			handled := false
			switch {
			case acct.ID != "":
				if freshJWT, _, err := accounts.RefreshAccountSingleflight(ctx, acct.ID, bearer); err == nil && freshJWT != "" {
					bearer = freshJWT
					handled = true
					n--
				}
			case o.JWT == "":
				if fresh := auth.ResolvePlanJWT(""); fresh != "" && fresh != bearer {
					bearer = fresh
					handled = true
					n--
				}
			}
			if handled {
				continue
			}
		}
		// For 3008 (account concurrency), switch immediately if another healthy
		// account is waiting in the ring; otherwise stay and use the bounded
		// in-place retry budget before giving up.
		if lastErr.AccountSwitch && (!codeIs(lastErr.Code, 3008) || hasMoreAccounts) {
			return nil, lastErr
		}
		if !isTransient(lastErr) || n >= maxAttempts-1 {
			break
		}
		select {
		case <-ctx.Done():
			return nil, &UpstreamError{Msg: "client went away", Status: 499, Kind: "upstream"}
		case <-time.After(time.Duration(baseMs*(1<<n)) * time.Millisecond):
		}
	}
	if lastErr == nil {
		lastErr = &UpstreamError{Msg: "upstream request failed", Status: 502, Kind: "upstream"}
	}
	return nil, lastErr
}

// attemptOnce performs one upstream POST with a response-header deadline and
// an activity-based stream idle timeout (so long active streams are never cut
// short by a fixed wall-clock timeout).
func attemptOnce(ctx context.Context, client *http.Client, acct candidate, bearer string, o SendOpts, headerTimeout, idleTimeout time.Duration) (*http.Response, error) {
	deviceID := firstNonEmpty(acct.DeviceID, identity.ResolveDeviceMid(), newUUID())
	sessionID := systemprompt.SessionIDFor(o.ClientSessionID, acct.SessionSeed)
	requestID := newUUID()
	traceID := newUUID()
	queryID := "01a1" + requestID[4:]

	headers := identity.BuildLLMHeaders(systemprompt.Captured().AnthropicVersion, systemprompt.Captured().AnthropicBeta, identity.LLMHeaderOpts{
		SessionID: sessionID, RequestID: requestID, TraceID: traceID, QueryID: queryID,
	})
	headers["authorization"] = "Bearer " + bearer
	headers["content-type"] = "application/json"
	if streamTrue(o.Body) {
		headers["accept"] = "text/event-stream"
	} else {
		headers["accept"] = "application/json"
	}
	payload, err := ShapePlanBody(o.Body, sessionID, deviceID, acct.UserID)
	if err != nil {
		return nil, &UpstreamError{Msg: "shape body: " + err.Error(), Status: 500, Kind: "config"}
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, &UpstreamError{Msg: "marshal body: " + err.Error(), Status: 500, Kind: "config"}
	}

	baseOrigin, err := auth.ValidateOriginURL(InferenceOrigin)
	if err != nil {
		return nil, &UpstreamError{Msg: "invalid inference origin: " + err.Error(), Status: 500, Kind: "config"}
	}

	callCtx, cancel := context.WithCancel(ctx)
	req, err := http.NewRequestWithContext(callCtx, http.MethodPost, baseOrigin+PlanAnthropicPath, bytes.NewReader(raw))
	if err != nil {
		cancel()
		return nil, &UpstreamError{Msg: "build request: " + err.Error(), Status: 500, Kind: "config"}
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	var hdrTimedOut atomic.Bool
	var hdrTimer *time.Timer
	if headerTimeout > 0 {
		hdrTimer = time.AfterFunc(headerTimeout, func() {
			hdrTimedOut.Store(true)
			cancel()
		})
	}
	resp, err := client.Do(req)
	if hdrTimer != nil {
		hdrTimer.Stop()
	}
	if err != nil {
		cancel()
		if (hdrTimedOut.Load() || callCtx.Err() != nil) && ctx.Err() == nil {
			return nil, &UpstreamError{Msg: "upstream timeout", Status: 504, Kind: "upstream"}
		}
		return nil, &UpstreamError{Msg: "upstream fetch failed: " + auth.RedactSecrets(err.Error(), bearer), Status: 502, Kind: "upstream"}
	}
	resp.Body = &cancelBody{
		ReadCloser:  resp.Body,
		parentCtx:   ctx,
		cancel:      cancel,
		idleTimeout: idleTimeout,
	}
	return resp, nil
}

// cancelBody runs the request cancel when the consumer closes the body and
// enforces a per-Read inactivity timeout without limiting total stream duration.
type cancelBody struct {
	io.ReadCloser
	parentCtx   context.Context
	cancel      context.CancelFunc
	idleTimeout time.Duration
	release     func()

	timerMu  sync.Mutex
	timer    *time.Timer
	closed   bool
	timedOut atomic.Bool
	once     sync.Once
}

func (b *cancelBody) Read(p []byte) (int, error) {
	if b.idleTimeout > 0 {
		if !b.armIdleTimer() {
			return 0, io.EOF
		}
		defer b.disarmIdleTimer()
	}
	n, err := b.ReadCloser.Read(p)
	if err != nil && b.timedOut.Load() && (b.parentCtx == nil || b.parentCtx.Err() == nil) {
		return n, ErrStreamIdleTimeout
	}
	return n, err
}

func (b *cancelBody) armIdleTimer() bool {
	b.timerMu.Lock()
	defer b.timerMu.Unlock()
	if b.closed {
		return false
	}
	if b.timer == nil {
		b.timer = time.AfterFunc(b.idleTimeout, func() {
			b.timedOut.Store(true)
			if b.cancel != nil {
				b.cancel()
			}
		})
	} else {
		b.timer.Reset(b.idleTimeout)
	}
	return true
}

func (b *cancelBody) disarmIdleTimer() {
	b.timerMu.Lock()
	defer b.timerMu.Unlock()
	if b.timer != nil {
		b.timer.Stop()
	}
}

func (b *cancelBody) Close() error {
	b.once.Do(func() {
		b.timerMu.Lock()
		b.closed = true
		if b.timer != nil {
			b.timer.Stop()
		}
		b.timerMu.Unlock()
		if b.cancel != nil {
			b.cancel()
		}
		if b.release != nil {
			b.release()
		}
	})
	return b.ReadCloser.Close()
}

func streamTrue(body map[string]any) bool {
	b, _ := body["stream"].(bool)
	return b
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// osGetenv exists so tests can stub the environment.
var osGetenv = os.Getenv

// DefaultMaxSSEEventBytes caps the memory a single SSE event may buffer.
const DefaultMaxSSEEventBytes = 2 << 20

// ErrSSEEventTooLarge is returned when an upstream SSE stream exceeds the
// configured per-event buffer limit without terminating the event.
var ErrSSEEventTooLarge = errors.New("upstream SSE event exceeded buffer limit")

// SSEEvent represents one parsed Server-Sent Event frame.
type SSEEvent struct {
	Event string
	Data  string
}

var (
	sseFieldEvent = []byte("event")
	sseFieldData  = []byte("data")
)

// SSEReader parses an SSE stream across arbitrary chunk boundaries,
// supporting LF, CRLF, and CR line endings, multiline data fields, and a hard
// per-event buffer bound.
type SSEReader struct {
	r             io.Reader
	maxEventBytes int
	readBuf       [4 << 10]byte
	pos           int
	end           int
	pendingCR     bool
	eof           bool
	readErr       error

	lineBuf    bytes.Buffer
	dataBuf    bytes.Buffer
	eventType  string
	hasData    bool
	eventBytes int
}

// NewSSEReader returns a bounded SSE event reader. If maxEventBytes <= 0,
// DefaultMaxSSEEventBytes (2 MiB) is used.
func NewSSEReader(r io.Reader, maxEventBytes int) *SSEReader {
	if maxEventBytes <= 0 {
		maxEventBytes = DefaultMaxSSEEventBytes
	}
	return &SSEReader{r: r, maxEventBytes: maxEventBytes}
}

// Next reads and returns the next non-empty SSE event from the stream, or
// io.EOF when the stream ends cleanly.
func (s *SSEReader) Next() (SSEEvent, error) {
	for {
		line, hasLine, err := s.readLine()
		if hasLine {
			if len(line) == 0 {
				if s.hasData || s.eventType != "" {
					ev := SSEEvent{Event: s.eventType, Data: s.dataBuf.String()}
					s.resetEvent()
					return ev, nil
				}
			} else if err := s.consumeLine(line); err != nil {
				s.resetEvent()
				return SSEEvent{}, err
			}
		}
		if err != nil {
			if err == io.EOF && (s.hasData || s.eventType != "") {
				ev := SSEEvent{Event: s.eventType, Data: s.dataBuf.String()}
				s.resetEvent()
				return ev, nil
			}
			s.resetEvent()
			return SSEEvent{}, err
		}
	}
}

func (s *SSEReader) resetEvent() {
	s.eventType = ""
	s.dataBuf.Reset()
	s.hasData = false
	s.eventBytes = 0
}

func (s *SSEReader) consumeLine(line []byte) error {
	s.eventBytes += len(line)
	if s.eventBytes > s.maxEventBytes {
		return ErrSSEEventTooLarge
	}
	if line[0] == ':' {
		return nil // SSE comment
	}
	field, val, hasColon := bytes.Cut(line, []byte{':'})
	if hasColon && len(val) > 0 && val[0] == ' ' {
		val = val[1:]
	}
	if bytes.Equal(field, sseFieldData) {
		if s.hasData {
			s.dataBuf.WriteByte('\n')
		}
		s.dataBuf.Write(val)
		s.hasData = true
	} else if bytes.Equal(field, sseFieldEvent) {
		s.eventType = string(bytes.TrimSpace(val))
	}
	return nil
}

func (s *SSEReader) readLine() ([]byte, bool, error) {
	s.lineBuf.Reset()
	for {
		if s.pos >= s.end {
			if s.eof {
				if s.lineBuf.Len() > 0 {
					out := s.lineBuf.Bytes()
					return out, true, nil
				}
				return nil, false, io.EOF
			}
			if s.readErr != nil {
				err := s.readErr
				s.readErr = nil
				if s.lineBuf.Len() > 0 {
					out := s.lineBuf.Bytes()
					return out, true, err
				}
				return nil, false, err
			}
			n, err := s.r.Read(s.readBuf[:])
			s.pos = 0
			s.end = n
			if err != nil {
				if err == io.EOF {
					s.eof = true
				} else {
					s.readErr = err
				}
			}
			if n == 0 {
				continue
			}
		}

		if s.pendingCR {
			s.pendingCR = false
			if s.readBuf[s.pos] == '\n' {
				s.pos++
				if s.pos >= s.end {
					continue
				}
			}
		}

		rem := s.readBuf[s.pos:s.end]
		eol := -1
		var delim byte
		for i := 0; i < len(rem); i++ {
			if rem[i] == '\n' || rem[i] == '\r' {
				eol = i
				delim = rem[i]
				break
			}
		}

		if eol >= 0 {
			if s.lineBuf.Len()+eol+s.eventBytes > s.maxEventBytes {
				return nil, false, ErrSSEEventTooLarge
			}
			s.pos += eol + 1
			if delim == '\r' {
				s.pendingCR = true
			}
			if s.lineBuf.Len() == 0 {
				return rem[:eol], true, nil
			}
			s.lineBuf.Write(rem[:eol])
			return s.lineBuf.Bytes(), true, nil
		}

		if s.lineBuf.Len()+len(rem)+s.eventBytes > s.maxEventBytes {
			return nil, false, ErrSSEEventTooLarge
		}
		s.lineBuf.Write(rem)
		s.pos = s.end
	}
}
