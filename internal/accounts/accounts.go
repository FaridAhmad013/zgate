// Package accounts is the multi-account store for the plan lane.
//
// Each entry is one ZCode plan authorization obtained by `zcode-gateway login`
// (OAuth device flow — approve the printed URL in any browser signed in to
// Z.ai; the desktop app is not required). The bridge rotates inference calls
// across healthy accounts: when one trips its allowance (429/1113), a hard
// auth failure (401), or persistent account concurrency (3008), it cools
// down and the next account takes over.
//
// File shape (0600, path overridable via ZCODE_ACCOUNTS_FILE):
//
//	{ "version": 1, "default": "<id>",
//	  "accounts": [ { "id", "label", "jwt", "refreshToken", "accessToken",
//	                  "userId", "email", "deviceId", "sessionSeed",
//	                  "obtainedAt", "expiresAt", "needsReauth" } ] }
//
// The single-token fallbacks keep working: when this store is empty the
// bridge answers as one account from ZCODE_PLAN_JWT -> plan-jwt.json -> the
// desktop credential store, exactly as before.
package accounts

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/apissaj/zcode-gateway/internal/auth"
)

// ErrCorruptStore indicates that accounts.json exists on disk but contains
// invalid or truncated JSON. Mutating operations refuse to overwrite it.
var ErrCorruptStore = errors.New("corrupt accounts store")

// Account is one stored plan authorization.
type Account struct {
	ID           string `json:"id"`
	Label        string `json:"label"`
	JWT          string `json:"jwt"`
	RefreshToken string `json:"refreshToken,omitempty"`
	AccessToken  string `json:"accessToken,omitempty"`
	UserID       string `json:"userId,omitempty"`
	Email        string `json:"email,omitempty"`
	DeviceID     string `json:"deviceId,omitempty"`
	SessionSeed  string `json:"sessionSeed,omitempty"`
	ObtainedAt   string `json:"obtainedAt,omitempty"`
	ExpiresAt    int64  `json:"expiresAt,omitempty"` // unix ms; 0 = no exp claim
	NeedsReauth  bool   `json:"needsReauth,omitempty"`
}

type store struct {
	Version  int       `json:"version"`
	Default  string    `json:"default,omitempty"`
	Accounts []Account `json:"accounts"`
}

// Summary is the non-secret view for /health and login --list.
type Summary struct {
	ID            string `json:"id"`
	Label         string `json:"label"`
	Email         string `json:"email,omitempty"`
	Default       bool   `json:"default"`
	HasRefresh    bool   `json:"hasRefresh"`
	ExpiresAt     int64  `json:"expiresAt"`
	Expired       bool   `json:"expired"`
	NeedsReauth   bool   `json:"needsReauth"`
	CooldownUntil string `json:"cooldownUntil"`
}

type refreshCall struct {
	done      chan struct{}
	jwt       string
	expiresAt int64
	err       error
}

var (
	mu              sync.Mutex
	runtime         = map[string]*state{} // id -> live cooldown state (not persisted)
	inflightRefresh = map[string]*refreshCall{}
	lastGoodStore   *store
	lastGoodPath    string
	lastGoodModTime time.Time
	lastGoodSize    int64
	lastStoreErr    error
	renameFile      = os.Rename
)

type state struct {
	cooldownUntil time.Time
	failures      int
}

func cooldownDefault() time.Duration {
	if v := envMs("ZCODE_ACCOUNT_COOLDOWN_MS", 15*60*1000); v > 0 {
		return time.Duration(v) * time.Millisecond
	}
	return 15 * time.Minute
}

func AccountsFile() string {
	if f := os.Getenv("ZCODE_ACCOUNTS_FILE"); f != "" {
		return f
	}
	return filepath.Join("data", "accounts.json")
}

func cloneStore(s store) store {
	out := s
	out.Accounts = append([]Account(nil), s.Accounts...)
	return out
}

func backupCorruptFile(path string, raw []byte) (string, error) {
	dir := filepath.Dir(path)
	base := filepath.Base(path)
	sum := sha256.Sum256(raw)
	hash8 := hex.EncodeToString(sum[:4])

	// If a backup with the identical content hash already exists, reuse it.
	if entries, err := os.ReadDir(dir); err == nil {
		suffix := "." + hash8
		prefix := base + ".corrupt."
		for _, e := range entries {
			name := e.Name()
			if strings.HasPrefix(name, prefix) && strings.HasSuffix(name, suffix) {
				return filepath.Join(dir, name), nil
			}
		}
	}

	ts := time.Now().UTC().Format("20060102T150405Z")
	for seq := 0; seq < 100; seq++ {
		name := fmt.Sprintf("%s.corrupt.%s.%s", base, ts, hash8)
		if seq > 0 {
			name = fmt.Sprintf("%s.corrupt.%s.%d.%s", base, ts, seq, hash8)
		}
		candidate := filepath.Join(dir, name)
		f, err := os.OpenFile(candidate, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			if errors.Is(err, os.ErrExist) {
				continue
			}
			return "", err
		}
		_, werr := f.Write(raw)
		serr := f.Sync()
		cerr := f.Close()
		_ = os.Chmod(candidate, 0o600)
		if werr != nil {
			return "", werr
		}
		if serr != nil {
			return "", serr
		}
		if cerr != nil {
			return "", cerr
		}
		return candidate, nil
	}
	return "", errors.New("exhausted corrupt backup filenames")
}

// loadStore reads and validates the accounts file under mu.
// If the file is missing, it returns an empty store with nil error.
// If the file is unreadable or corrupt, it preserves the original file,
// creates a non-clobbering 0600 backup on corruption, and returns an error
// (alongside any previously cached in-memory accounts for read-only callers).
func loadStore() (store, error) {
	path := AccountsFile()
	info, statErr := os.Stat(path)
	if statErr == nil && lastGoodStore != nil && lastGoodPath == path && lastStoreErr == nil &&
		!lastGoodModTime.IsZero() && info.ModTime().Equal(lastGoodModTime) && info.Size() == lastGoodSize {
		return cloneStore(*lastGoodStore), nil
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			lastStoreErr = nil
			lastGoodModTime = time.Time{}
			lastGoodSize = 0
			empty := store{Version: 1, Accounts: []Account{}}
			return empty, nil
		}
		lastStoreErr = fmt.Errorf("read accounts file %s: %w", path, err)
		if lastGoodStore != nil && lastGoodPath == path {
			return cloneStore(*lastGoodStore), lastStoreErr
		}
		return store{Version: 1, Accounts: []Account{}}, lastStoreErr
	}

	var rec store
	if len(bytes.TrimSpace(raw)) == 0 || json.Unmarshal(raw, &rec) != nil || rec.Accounts == nil {
		backupPath, _ := backupCorruptFile(path, raw)
		if backupPath != "" {
			lastStoreErr = fmt.Errorf("%w: %s (backup saved to %s)", ErrCorruptStore, path, backupPath)
		} else {
			lastStoreErr = fmt.Errorf("%w: %w", ErrCorruptStore, errors.New(path))
		}
		lastGoodModTime = time.Time{}
		if lastGoodStore != nil && lastGoodPath == path {
			return cloneStore(*lastGoodStore), lastStoreErr
		}
		return store{Version: 1, Accounts: []Account{}}, lastStoreErr
	}

	if rec.Version <= 0 {
		rec.Version = 1
	}
	cp := cloneStore(rec)
	lastGoodStore = &cp
	lastGoodPath = path
	if statErr == nil {
		lastGoodModTime = info.ModTime()
		lastGoodSize = info.Size()
	} else if st, err := os.Stat(path); err == nil {
		lastGoodModTime = st.ModTime()
		lastGoodSize = st.Size()
	}
	lastStoreErr = nil
	return rec, nil
}

// writeStore atomically persists rec via a 0600 temporary file in the same
// directory, fsync, close, and rename.
func writeStore(rec store) error {
	path := AccountsFile()
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		lastStoreErr = err
		return err
	}
	if info, err := os.Stat(dir); err == nil && info.Mode().Perm() != 0o700 {
		_ = os.Chmod(dir, 0o700)
	}
	if rec.Version <= 0 {
		rec.Version = 1
	}
	raw, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		lastStoreErr = err
		return err
	}
	tmp, err := os.CreateTemp(dir, ".accounts-*.tmp")
	if err != nil {
		lastStoreErr = err
		return err
	}
	tmpName := tmp.Name()
	cleanup := func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
	}
	if err := tmp.Chmod(0o600); err != nil {
		cleanup()
		lastStoreErr = err
		return err
	}
	if n, err := tmp.Write(raw); err != nil || n != len(raw) {
		cleanup()
		if err == nil {
			err = io.ErrShortWrite
		}
		lastStoreErr = err
		return err
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		lastStoreErr = err
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		lastStoreErr = err
		return err
	}
	if err := renameFile(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		lastStoreErr = err
		return err
	}
	if info, err := os.Stat(path); err == nil {
		if info.Mode().Perm() != 0o600 {
			_ = os.Chmod(path, 0o600)
		}
		lastGoodModTime = info.ModTime()
		lastGoodSize = info.Size()
	}
	cp := cloneStore(rec)
	lastGoodStore = &cp
	lastGoodPath = path
	lastStoreErr = nil
	return nil
}

var ioErrShortWrite = errors.New("short write")

type ioPkg struct{ ErrShortWrite error }

var io = ioPkg{ErrShortWrite: ioErrShortWrite}

// LastError returns the most recent store read/write error, or nil if healthy.
func LastError() error {
	mu.Lock()
	defer mu.Unlock()
	_, err := loadStore()
	return err
}

// Load reads all stored accounts and returns any underlying storage/corruption error.
func Load() ([]Account, error) {
	mu.Lock()
	defer mu.Unlock()
	rec, err := loadStore()
	return rec.Accounts, err
}

// RecoverFromBackup validates a backup store file and atomically restores it
// to AccountsFile().
func RecoverFromBackup(backupPath string) error {
	mu.Lock()
	defer mu.Unlock()
	raw, err := os.ReadFile(backupPath)
	if err != nil {
		return fmt.Errorf("read backup %s: %w", backupPath, err)
	}
	var rec store
	if len(bytes.TrimSpace(raw)) == 0 || json.Unmarshal(raw, &rec) != nil || rec.Accounts == nil {
		return fmt.Errorf("%w: backup file %s is not valid", ErrCorruptStore, backupPath)
	}
	return writeStore(rec)
}

func idForJWT(jwt string) string {
	claims, ok := auth.DecodeJWT(jwt)
	if ok {
		if uid, _ := claims["user_id"].(string); uid != "" {
			if len(uid) > 8 {
				return uid[:8]
			}
			return uid
		}
	}
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func randomUUID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// UpsertAccount stores one authorization (add or refresh by id), keeping the
// account's existing deviceId/sessionSeed/email. If the existing store on disk
// is corrupt, it refuses to overwrite the file and returns the error.
func UpsertAccount(id, label, jwt, refreshToken, accessToken string, expiresAt int64) (Account, error) {
	mu.Lock()
	defer mu.Unlock()
	rec, err := loadStore()
	if err != nil {
		return Account{}, err
	}
	claims, _ := auth.DecodeJWT(jwt)
	userID := ""
	if claims != nil {
		if v, _ := claims["user_id"].(string); v != "" {
			userID = v
		} else if v, _ := claims["sub"].(string); v != "" {
			userID = v
		}
	}
	acctID := firstNonEmpty(id, idForJWT(jwt))
	entry := Account{
		ID:           acctID,
		Label:        firstNonEmpty(label, acctID),
		JWT:          jwt,
		RefreshToken: refreshToken,
		AccessToken:  accessToken,
		UserID:       userID,
		DeviceID:     randomUUID(),
		SessionSeed:  randomUUID(),
		ObtainedAt:   time.Now().UTC().Format(time.RFC3339),
		ExpiresAt:    expiresAt,
	}
	for i := range rec.Accounts {
		if rec.Accounts[i].ID == acctID {
			entry.DeviceID = firstNonEmpty(rec.Accounts[i].DeviceID, entry.DeviceID)
			entry.SessionSeed = firstNonEmpty(rec.Accounts[i].SessionSeed, entry.SessionSeed)
			entry.Email = rec.Accounts[i].Email
			rec.Accounts[i] = entry
			break
		}
	}
	if !containsID(rec.Accounts, acctID) {
		rec.Accounts = append(rec.Accounts, entry)
	}
	if rec.Default == "" {
		rec.Default = acctID
	}
	if err := writeStore(rec); err != nil {
		return Account{}, err
	}
	return entry, nil
}

// Upsert stores one authorization (add or refresh by id), keeping the
// account's existing deviceId/sessionSeed/email.
func Upsert(id, label, jwt, refreshToken, accessToken string, expiresAt int64) Account {
	entry, _ := UpsertAccount(id, label, jwt, refreshToken, accessToken, expiresAt)
	return entry
}

func containsID(accounts []Account, id string) bool {
	for i := range accounts {
		if accounts[i].ID == id {
			return true
		}
	}
	return false
}

// Remove forgets one account; the default moves to the first survivor.
// Returns false without overwriting disk if the store is corrupt.
func Remove(id string) bool {
	mu.Lock()
	defer mu.Unlock()
	rec, err := loadStore()
	if err != nil {
		return false
	}
	before := len(rec.Accounts)
	kept := rec.Accounts[:0]
	for _, a := range rec.Accounts {
		if a.ID != id {
			kept = append(kept, a)
		}
	}
	rec.Accounts = kept
	if rec.Default == id {
		if len(rec.Accounts) > 0 {
			rec.Default = rec.Accounts[0].ID
		} else {
			rec.Default = ""
		}
	}
	if len(rec.Accounts) == before {
		return false
	}
	if err := writeStore(rec); err != nil {
		return false
	}
	return true
}

// SetDefault pins one account as the rotation head.
func SetDefault(id string) bool {
	mu.Lock()
	defer mu.Unlock()
	rec, err := loadStore()
	if err != nil {
		return false
	}
	if !containsID(rec.Accounts, id) {
		return false
	}
	rec.Default = id
	return writeStore(rec) == nil
}

// List returns every stored account (with secrets — internal use).
func List() []Account {
	mu.Lock()
	defer mu.Unlock()
	rec, _ := loadStore()
	return rec.Accounts
}

func cooledDown(id string, now time.Time) bool {
	st := runtime[id]
	return st != nil && st.cooldownUntil.After(now)
}

// Usable returns the accounts a new attempt may use, default first. Expired
// JWTs are skipped (and flagged) so rotation never parks on a dead token.
// Tokens without an `exp` claim have ExpiresAt == 0 and are NOT treated as expired.
func Usable(exclude map[string]bool) []Account {
	mu.Lock()
	defer mu.Unlock()
	rec, _ := loadStore()
	now := time.Now()
	out := make([]Account, 0, len(rec.Accounts))
	ordered := append([]Account(nil), rec.Accounts...)
	sort.SliceStable(ordered, func(i, j int) bool {
		if ordered[i].ID == rec.Default {
			return true
		}
		if ordered[j].ID == rec.Default {
			return false
		}
		return false
	})
	for _, a := range ordered {
		if exclude[a.ID] || a.NeedsReauth || cooledDown(a.ID, now) {
			continue
		}
		if a.ExpiresAt > 0 && a.ExpiresAt <= now.UnixMilli() {
			continue
		}
		out = append(out, a)
	}
	return out
}

// MarkCooldown parks an account for ms (the default when ms <= 0) and
// bumps its failure count. Live state only — a restart starts everyone
// healthy.
func MarkCooldown(id string, ms time.Duration) {
	mu.Lock()
	defer mu.Unlock()
	if ms <= 0 {
		ms = cooldownDefault()
	}
	st := runtime[id]
	if st == nil {
		st = &state{}
		runtime[id] = st
	}
	st.cooldownUntil = time.Now().Add(ms)
	st.failures++
}

// MarkReauth flags the account durably (persists in the file when store is healthy)
// and parks it indefinitely in live state.
func MarkReauth(id string) {
	mu.Lock()
	defer mu.Unlock()
	runtime[id] = &state{cooldownUntil: time.Now().Add(1 << 62)}
	rec, err := loadStore()
	if err != nil {
		return
	}
	for i := range rec.Accounts {
		if rec.Accounts[i].ID == id {
			rec.Accounts[i].NeedsReauth = true
			_ = writeStore(rec)
			break
		}
	}
}

// ClearCooldown revives an account after a success, clearing both the live
// park and a persisted needsReauth flag.
func ClearCooldown(id string) {
	mu.Lock()
	defer mu.Unlock()
	if st := runtime[id]; st != nil {
		st.cooldownUntil = time.Time{}
		st.failures = 0
	}
	rec, err := loadStore()
	if err != nil {
		return
	}
	for i := range rec.Accounts {
		if rec.Accounts[i].ID == id && rec.Accounts[i].NeedsReauth {
			rec.Accounts[i].NeedsReauth = false
			_ = writeStore(rec)
			break
		}
	}
}

// Summarize renders the non-secret ring view for /health and login --list.
func Summarize() []Summary {
	mu.Lock()
	defer mu.Unlock()
	rec, _ := loadStore()
	now := time.Now()
	out := make([]Summary, 0, len(rec.Accounts))
	for _, a := range rec.Accounts {
		s := Summary{
			ID:          a.ID,
			Label:       a.Label,
			Email:       a.Email,
			Default:     a.ID == rec.Default,
			HasRefresh:  a.RefreshToken != "",
			ExpiresAt:   a.ExpiresAt,
			Expired:     a.ExpiresAt > 0 && a.ExpiresAt <= now.UnixMilli(),
			NeedsReauth: a.NeedsReauth,
		}
		if st := runtime[a.ID]; st != nil && st.cooldownUntil.After(now) && st.cooldownUntil.Unix() < 1<<59 {
			s.CooldownUntil = st.cooldownUntil.UTC().Format(time.RFC3339)
		}
		out = append(out, s)
	}
	return out
}

// RefreshAccountSingleflight coordinates token refresh per account so concurrent
// 401 responses for the same account share a single refresh-token request
// without blocking refreshes for other accounts.
// If staleJWT is non-empty and the stored account already holds a newer JWT,
// the newer JWT is returned immediately without hitting the network.
func RefreshAccountSingleflight(ctx context.Context, id, staleJWT string) (string, int64, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return "", 0, err
	}

	mu.Lock()
	rec, loadErr := loadStore()
	if loadErr != nil && len(rec.Accounts) == 0 {
		mu.Unlock()
		return "", 0, loadErr
	}
	var entry *Account
	for i := range rec.Accounts {
		if rec.Accounts[i].ID == id {
			cp := rec.Accounts[i]
			entry = &cp
			break
		}
	}
	if entry == nil {
		mu.Unlock()
		return "", 0, fmt.Errorf("account %s not found", id)
	}
	if staleJWT != "" && entry.JWT != "" && entry.JWT != staleJWT {
		freshJWT, freshExp := entry.JWT, entry.ExpiresAt
		mu.Unlock()
		return freshJWT, freshExp, nil
	}
	if call, active := inflightRefresh[id]; active {
		mu.Unlock()
		select {
		case <-call.done:
			return call.jwt, call.expiresAt, call.err
		case <-ctx.Done():
			return "", 0, ctx.Err()
		}
	}
	if entry.RefreshToken == "" {
		mu.Unlock()
		return "", 0, fmt.Errorf("account %s has no refresh token", id)
	}

	call := &refreshCall{done: make(chan struct{})}
	inflightRefresh[id] = call
	refreshToken := entry.RefreshToken
	label := entry.Label
	mu.Unlock()

	// Use an independent bounded context for the upstream token exchange so a
	// single impatient caller canceling its request does not abort the shared
	// refresh for other waiting callers.
	reqCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	result, err := auth.RefreshPlanJWT(reqCtx, refreshToken)
	cancel()

	if err != nil {
		MarkReauth(id)
		call.err = err
	} else {
		expiresAt, _ := auth.ExpiryUnixMilli(result.JWT)
		if _, uerr := UpsertAccount(id, label, result.JWT, result.RefreshToken, result.AccessToken, expiresAt); uerr != nil {
			call.err = uerr
		} else {
			ClearCooldown(id)
			call.jwt = result.JWT
			call.expiresAt = expiresAt
		}
	}

	mu.Lock()
	delete(inflightRefresh, id)
	close(call.done)
	mu.Unlock()

	if err := ctx.Err(); err != nil {
		return "", 0, err
	}
	return call.jwt, call.expiresAt, call.err
}

// TryRefreshAccount attempts one best-effort JWT refresh for an account
// using its stored refresh token (serialized per account). On success the
// record is updated in place and the new expiry (unix ms; 0 if the JWT has no
// exp claim) returned; on failure the account is marked needsReauth (never
// deleted) and the error is returned.
func TryRefreshAccount(ctx context.Context, id string) (int64, error) {
	_, exp, err := RefreshAccountSingleflight(ctx, id, "")
	return exp, err
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func envMs(key string, fallback int64) int64 {
	if v := os.Getenv(key); v != "" {
		var n int64
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil && n > 0 {
			return n
		}
	}
	return fallback
}

// ResetForTest drops the in-memory runtime state; used by the test suite
// to swap account files between checks.
func ResetForTest() {
	mu.Lock()
	defer mu.Unlock()
	runtime = map[string]*state{}
	inflightRefresh = map[string]*refreshCall{}
	lastGoodStore = nil
	lastGoodPath = ""
	lastStoreErr = nil
}
