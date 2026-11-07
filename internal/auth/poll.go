package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// PollOnce checks the device authorization status exactly once without looping.
// Returns (result, true, nil) if approved and ready.
// Returns (nil, false, nil) if still pending/waiting for user approval.
// Returns (nil, false, err) if rejected, expired, or failed.
func PollOnce(ctx context.Context, flow *DeviceFlow) (*FlowResult, bool, error) {
	if flow == nil {
		return nil, false, &AuthError{Msg: "device flow is nil", Kind: "config"}
	}
	base, err := ValidateOriginURL(OAuthBase)
	if err != nil {
		return nil, false, &AuthError{Msg: "invalid oauth origin: " + err.Error(), Kind: "config"}
	}
	if err := validateFlowID(flow.FlowID); err != nil {
		return nil, false, &AuthError{Msg: "build poll: " + err.Error(), Kind: "config"}
	}
	if !flow.ExpiresAt.IsZero() && time.Now().After(flow.ExpiresAt) {
		return nil, false, &AuthError{Msg: "device flow expired before approval", Kind: "expired"}
	}
	pollURL := fmt.Sprintf("%s/api/v1/oauth/cli/poll/%s", base, url.PathEscape(strings.TrimSpace(flow.FlowID)))
	req, err := newRequest(ctx, "GET", pollURL, nil,
		map[string]string{"Authorization": "Bearer " + flow.PollToken})
	if err != nil {
		return nil, false, &AuthError{Msg: "build poll: " + err.Error(), Kind: "config"}
	}
	body, status, err := doRequest(req)
	if err != nil {
		return nil, false, &AuthError{Msg: "device flow poll: " + RedactSecrets(err.Error(), flow.PollToken), Status: status, Kind: "auth"}
	}
	var out struct {
		Data struct {
			Status string         `json:"status"`
			Token  string         `json:"token"`
			User   map[string]any `json:"user"`
			Zai    *oauthTokens   `json:"zai"`
			Big    *oauthTokens   `json:"bigmodel"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &out); err != nil || out.Data.Status == "" {
		return nil, false, &AuthError{Msg: "device flow poll: parse failed", Status: status, Kind: "protocol"}
	}
	switch out.Data.Status {
	case "ready":
		if out.Data.Token == "" {
			return nil, false, &AuthError{Msg: "device flow returned no plan token", Kind: "protocol"}
		}
		res := &FlowResult{JWT: out.Data.Token, User: out.Data.User}
		if out.Data.Zai != nil {
			res.AccessToken, res.RefreshToken = out.Data.Zai.AccessToken, out.Data.Zai.RefreshToken
		} else if out.Data.Big != nil {
			res.AccessToken, res.RefreshToken = out.Data.Big.AccessToken, out.Data.Big.RefreshToken
		}
		return res, true, nil
	case "pending", "waiting":
		return nil, false, nil
	case "failed":
		return nil, false, &AuthError{Msg: "device flow was rejected", Kind: "rejected"}
	case "expired":
		return nil, false, &AuthError{Msg: "device flow expired before approval", Kind: "expired"}
	default:
		// Default to pending for any in-progress status from OAuth server
		return nil, false, nil
	}
}
