// Command zcode-gateway is a local bridge onto the Z.ai start-plan
// inference lane. It accepts Anthropic and OpenAI wire formats, maps them
// onto the plan lane, rotates stored accounts, and reports plan quota.
//
//	zcode-gateway                 # serve on 127.0.0.1:20131
//	zcode-gateway login [--label work]
//	zcode-gateway login --list | --remove <id> | --set-default <id>
package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/apissaj/zcode-gateway/internal/accounts"
	"github.com/apissaj/zcode-gateway/internal/auth"
	"github.com/apissaj/zcode-gateway/internal/identity"
	"github.com/apissaj/zcode-gateway/internal/server"
	"github.com/apissaj/zcode-gateway/internal/transport"
)

const version = "0.1.0"

func envInt(key string, fallback int) int {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return fallback
}

func main() {
	args := os.Args[1:]
	if len(args) > 0 && args[0] == "healthcheck" {
		runHealthcheck(args[1:])
		return
	}
	if len(args) > 0 && args[0] == "login" {
		runLogin(args[1:])
		return
	}
	if len(args) > 0 && (args[0] == "-v" || args[0] == "--version" || args[0] == "version") {
		fmt.Printf("zcode-gateway %s\n", version)
		return
	}
	serve()
}

func runLogin(args []string) {
	switch {
	case len(args) > 0 && args[0] == "--list":
		listAccounts()
	case len(args) > 1 && args[0] == "--remove":
		if accounts.Remove(args[1]) {
			fmt.Printf("removed %s\n", args[1])
		} else {
			fmt.Printf("no account %s\n", args[1])
		}
	case len(args) > 1 && args[0] == "--set-default":
		if accounts.SetDefault(args[1]) {
			fmt.Printf("default account is now %s\n", args[1])
		} else {
			fmt.Printf("no account %s\n", args[1])
		}
	case len(args) == 0 || args[0] == "--label":
		label := ""
		for i, a := range args {
			if a == "--label" && i+1 < len(args) {
				label = args[i+1]
			}
		}
		deviceLogin(label)
	default:
		fmt.Fprintln(os.Stderr, "usage: zcode-gateway login [--label <name>] | --list | --remove <id> | --set-default <id>")
		os.Exit(2)
	}
}

func listAccounts() {
	ring := accounts.Summarize()
	if len(ring) == 0 {
		fmt.Println("No accounts stored yet. Run `zcode-gateway login` and approve the URL.")
		return
	}
	for _, a := range ring {
		state := "ready"
		switch {
		case a.NeedsReauth:
			state = "NEEDS RE-AUTH"
		case a.Expired:
			state = "EXPIRED"
		case a.CooldownUntil != "":
			state = "cooling until " + a.CooldownUntil
		}
		marker := " "
		if a.Default {
			marker = "*"
		}
		extra := ""
		if a.HasRefresh {
			extra = "  (refreshable)"
		}
		fmt.Printf("  %s %-10s %-14s %s%s\n", marker, a.ID, a.Label, state, extra)
	}
}

func deviceLogin(label string) {
	ctx := context.Background()
	flow, err := auth.StartDeviceFlow(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "login failed: %s\n", auth.Redact(err.Error()))
		os.Exit(1)
	}
	minutes := int(time.Until(flow.ExpiresAt).Minutes() + 0.5)
	if minutes < 1 {
		minutes = 1
	}
	fmt.Printf("Approve this URL in a browser where you are signed in to Z.ai (%d min limit):\n\n  %s\n\nWaiting for approval...\n",
		minutes, flow.AuthorizeURL)

	result, err := auth.PollDeviceFlow(ctx, flow)
	if err != nil {
		fmt.Fprintf(os.Stderr, "login failed: %s\n", auth.Redact(err.Error()))
		os.Exit(1)
	}
	claims, _ := auth.DecodeJWT(result.JWT)
	expiresAt, _ := auth.ExpiryUnixMilli(result.JWT)
	userID, _ := claims["user_id"].(string)
	if userID == "" {
		userID, _ = claims["sub"].(string)
	}
	// UpsertAccount derives the stable account id from the JWT's user_id.
	// Plan JWTs without an `exp` claim keep expiresAt == 0 (revocation is
	// detected via upstream 401 responses, never via a synthetic 24h timer).
	tmp, err := accounts.UpsertAccount("", label, result.JWT, result.RefreshToken, result.AccessToken, expiresAt)
	if err != nil {
		fmt.Fprintf(os.Stderr, "login failed: could not persist account: %s\n", auth.Redact(err.Error()))
		os.Exit(1)
	}
	fmt.Printf("\nAuthorized. account %s (user %s) stored in %s\n",
		tmp.ID, orUnknown(userID), accounts.AccountsFile())
	if result.RefreshToken != "" {
		fmt.Println("refresh token stored; the bridge will try it on auth expiry (best effort).")
	} else {
		fmt.Println("no refresh token returned; re-run `zcode-gateway login` before the plan JWT expires.")
	}
}

func orUnknown(s string) string {
	if strings.TrimSpace(s) == "" {
		return "unknown"
	}
	return s
}

func runHealthcheck(args []string) {
	port := envInt("PORT", 20131)
	bindHost := server.ResolveBindHost()
	if bindHost == "0.0.0.0" || bindHost == "::" {
		bindHost = "127.0.0.1"
	}
	url := fmt.Sprintf("http://%s/healthz", net.JoinHostPort(bindHost, strconv.Itoa(port)))
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		fmt.Fprintf(os.Stderr, "healthcheck failed: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "healthcheck failed with HTTP status %d\n", resp.StatusCode)
		os.Exit(1)
	}
	os.Exit(0)
}

func resolveBridgeToken() string {
	if tok := strings.TrimSpace(os.Getenv("ZCODE_BRIDGE_TOKEN")); tok != "" {
		return tok
	}
	// systemd-creds support (ADR 0001): LoadCredential / LoadCredentialEncrypted
	if credDir := strings.TrimSpace(os.Getenv("CREDENTIALS_DIRECTORY")); credDir != "" {
		if raw, err := os.ReadFile(filepath.Join(credDir, "bridge_token")); err == nil {
			if tok := strings.TrimSpace(string(raw)); tok != "" {
				return tok
			}
		}
	}
	return ""
}

func serve() {
	cfg := server.Config{
		Port:        envInt("PORT", 20131),
		Host:        server.ResolveBindHost(),
		BridgeToken: resolveBridgeToken(),
	}
	if err := server.ValidateConfig(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "configuration error: %s\n", auth.RedactSecrets(err.Error(), cfg.BridgeToken))
		os.Exit(1)
	}
	server.Configure(cfg)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	jwtState := "MISSING (run: zcode-gateway login)"
	if auth.ResolvePlanJWT("") != "" {
		jwtState = "available"
	}
	midState := identity.ResolveDeviceMid()
	if midState == "" {
		midState = "MISSING"
	}
	authMode := "open (localhost)"
	if cfg.BridgeToken != "" {
		authMode = "bridge token required"
	}
	dashHost := cfg.Host
	if dashHost == "0.0.0.0" || dashHost == "::" {
		dashHost = "127.0.0.1"
	}
	dashURL := fmt.Sprintf("http://%s:%d/dashboard", dashHost, cfg.Port)
	apiURL := fmt.Sprintf("http://%s:%d/v1", dashHost, cfg.Port)

	fmt.Printf("\n  ┌────────────────────────────────────────────────────────┐\n")
	fmt.Printf("  │ ZGate v%s (Hardened AI Gateway)                    │\n", version)
	fmt.Printf("  │ Listener       : %s:%d\n", cfg.Host, cfg.Port)
	fmt.Printf("  │ Web Dashboard  : %s\n", dashURL)
	fmt.Printf("  │ API Endpoint   : %s\n", apiURL)
	fmt.Printf("  │ Plan JWT       : %s\n", jwtState)
	fmt.Printf("  │ Auth Mode      : %s\n", authMode)
	fmt.Printf("  │ Upstream Lane  : %s\n", transport.InferenceOrigin+transport.PlanAnthropicPath)
	fmt.Printf("  └────────────────────────────────────────────────────────┘\n\n")

	// Background refresh: check every 15m (ZCODE_REFRESH_INTERVAL_MS),
	// refresh accounts expiring within 2h.
	refreshEvery := time.Duration(envInt("ZCODE_REFRESH_INTERVAL_MS", 15*60*1000)) * time.Millisecond
	go func() {
		ticker := time.NewTicker(refreshEvery)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				refreshExpiring(ctx)
			}
		}
	}()

	if err := server.ListenAndServeContext(ctx); err != nil {
		if ctx.Err() != nil && errors.Is(err, context.DeadlineExceeded) {
			fmt.Fprintln(os.Stderr, "server: graceful shutdown timeout reached; remaining connections closed")
			return
		}
		fmt.Fprintf(os.Stderr, "server: %s\n", auth.RedactSecrets(err.Error(), cfg.BridgeToken))
		os.Exit(1)
	}
}

func refreshExpiring(ctx context.Context) {
	const refreshBeforeExpiry = 2 * time.Hour
	now := time.Now().UnixMilli()
	for _, a := range accounts.List() {
		if a.RefreshToken == "" || a.ExpiresAt == 0 {
			continue
		}
		if time.Duration(a.ExpiresAt-now)*time.Millisecond > refreshBeforeExpiry {
			continue
		}
		fmt.Printf("[jwt-refresh] account %s (%s) expiring — refreshing\n", a.ID, a.Label)
		newExpiry, err := accounts.TryRefreshAccount(ctx, a.ID)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[jwt-refresh] account %s refresh failed: %s — re-login required\n", a.ID, auth.RedactSecrets(err.Error(), a.JWT, a.RefreshToken))
			continue
		}
		fmt.Printf("[jwt-refresh] account %s refreshed, new expiry %s\n",
			a.ID, time.UnixMilli(newExpiry).UTC().Format(time.RFC3339))
	}
}
