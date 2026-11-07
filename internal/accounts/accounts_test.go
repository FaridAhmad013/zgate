package accounts

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	goRuntime "runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/apissaj/zcode-gateway/internal/auth"
)

func makeTestJWT(user string, expSec int64) string {
	claims := map[string]any{"user_id": user, "iat": 1700000000}
	if expSec != 0 {
		claims["exp"] = expSec
	}
	raw, _ := json.Marshal(claims)
	hdr := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))
	pay := base64.RawURLEncoding.EncodeToString(raw)
	return hdr + "." + pay + ".sig"
}

func setupTestAccounts(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	file := filepath.Join(dir, "data", "accounts.json")
	t.Setenv("ZCODE_ACCOUNTS_FILE", file)
	ResetForTest()
	t.Cleanup(ResetForTest)
	return file
}

func TestAtomicAccountStorageReadWriteAndPermissions(t *testing.T) {
	file := setupTestAccounts(t)
	jwtA := makeTestJWT("user_alpha", 0)
	jwtB := makeTestJWT("user_beta", 0)

	a, err := UpsertAccount("alpha", "Alpha", jwtA, "rt-a", "at-a", 0)
	if err != nil {
		t.Fatalf("UpsertAccount alpha: %v", err)
	}
	if a.ID != "alpha" || a.ExpiresAt != 0 {
		t.Fatalf("unexpected account: %+v", a)
	}
	if _, err := UpsertAccount("beta", "Beta", jwtB, "rt-b", "at-b", 0); err != nil {
		t.Fatalf("UpsertAccount beta: %v", err)
	}

	list, err := Load()
	if err != nil || len(list) != 2 {
		t.Fatalf("Load() = %d, err = %v", len(list), err)
	}

	// Ensure no leftover temporary files exist.
	entries, err := os.ReadDir(filepath.Dir(file))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".accounts-") {
			t.Fatalf("leftover temp file found: %s", e.Name())
		}
	}

	if goRuntime.GOOS != "windows" {
		info, err := os.Stat(file)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("accounts.json mode = %o, want 0600", info.Mode().Perm())
		}
		dirInfo, err := os.Stat(filepath.Dir(file))
		if err != nil {
			t.Fatal(err)
		}
		if dirInfo.Mode().Perm() != 0o700 {
			t.Fatalf("data dir mode = %o, want 0700", dirInfo.Mode().Perm())
		}
	}

	if !SetDefault("beta") {
		t.Fatal("SetDefault(beta) failed")
	}
	usable := Usable(nil)
	if len(usable) != 2 || usable[0].ID != "beta" {
		t.Fatalf("Usable order = %+v", usable)
	}
	if !Remove("alpha") {
		t.Fatal("Remove(alpha) failed")
	}
	if len(List()) != 1 {
		t.Fatalf("List() after remove = %d, want 1", len(List()))
	}
}

func TestCorruptAndTruncatedStoreProtectionAndBackupRecovery(t *testing.T) {
	file := setupTestAccounts(t)
	jwtA := makeTestJWT("user_a", 0)
	jwtB := makeTestJWT("user_b", 0)

	if _, err := UpsertAccount("a", "Account A", jwtA, "rt-a", "", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := UpsertAccount("b", "Account B", jwtB, "rt-b", "", 0); err != nil {
		t.Fatal(err)
	}

	validBytes, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	validBackup := file + ".valid-backup"
	if err := os.WriteFile(validBackup, validBytes, 0o600); err != nil {
		t.Fatal(err)
	}

	// 1. Simulate truncated write (half of JSON file).
	truncated := validBytes[:len(validBytes)/2]
	if err := os.WriteFile(file, truncated, 0o600); err != nil {
		t.Fatal(err)
	}

	// In-memory cache still remembers accounts a & b so running process does not
	// think all accounts were deleted, while Load() surfaces ErrCorruptStore.
	cached, loadErr := Load()
	if !errors.Is(loadErr, ErrCorruptStore) {
		t.Fatalf("expected ErrCorruptStore on truncated JSON, got: %v", loadErr)
	}
	if len(cached) != 2 {
		t.Fatalf("expected in-memory accounts to remain available (len=2), got %d", len(cached))
	}

	// Mutating operations MUST refuse to overwrite the corrupt file.
	if _, err := UpsertAccount("c", "Account C", makeTestJWT("user_c", 0), "rt-c", "", 0); !errors.Is(err, ErrCorruptStore) {
		t.Fatalf("UpsertAccount on corrupt store must fail with ErrCorruptStore, got: %v", err)
	}
	if Remove("a") {
		t.Fatal("Remove must fail when store on disk is corrupt")
	}
	if SetDefault("b") {
		t.Fatal("SetDefault must fail when store on disk is corrupt")
	}

	// Verify the original corrupt file was preserved byte-for-byte.
	diskAfter, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if string(diskAfter) != string(truncated) {
		t.Fatal("corrupt accounts.json was overwritten!")
	}

	// Verify a backup file was created and a second distinct corruption does not clobber the first backup.
	invalidJSON := []byte(`{"version":1,"accounts":INVALID}`)
	if err := os.WriteFile(file, invalidJSON, 0o600); err != nil {
		t.Fatal(err)
	}
	_, loadErr2 := Load()
	if !errors.Is(loadErr2, ErrCorruptStore) {
		t.Fatalf("expected ErrCorruptStore on invalid JSON, got: %v", loadErr2)
	}

	entries, err := os.ReadDir(filepath.Dir(file))
	if err != nil {
		t.Fatal(err)
	}
	var corruptBackups []string
	for _, e := range entries {
		if strings.Contains(e.Name(), "accounts.json.corrupt.") {
			corruptBackups = append(corruptBackups, filepath.Join(filepath.Dir(file), e.Name()))
		}
	}
	if len(corruptBackups) < 2 {
		t.Fatalf("expected at least 2 distinct non-clobbering corrupt backups, got %d: %v", len(corruptBackups), corruptBackups)
	}

	// RecoverFromBackup must reject corrupt backup files and succeed on valid backup.
	if err := RecoverFromBackup(corruptBackups[0]); !errors.Is(err, ErrCorruptStore) {
		t.Fatalf("RecoverFromBackup on corrupt backup should fail, got: %v", err)
	}
	if err := RecoverFromBackup(validBackup); err != nil {
		t.Fatalf("RecoverFromBackup on valid backup failed: %v", err)
	}
	restored, err := Load()
	if err != nil || len(restored) != 2 {
		t.Fatalf("after RecoverFromBackup: len=%d, err=%v", len(restored), err)
	}
}

func TestAtomicWriteFailedRenameCleansTempAndPreservesOriginal(t *testing.T) {
	file := setupTestAccounts(t)
	if _, err := UpsertAccount("orig", "Original", makeTestJWT("u_orig", 0), "rt-orig", "", 0); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}

	origRename := renameFile
	renameFile = func(oldpath, newpath string) error {
		return errors.New("simulated rename failure")
	}
	defer func() { renameFile = origRename }()

	if _, err := UpsertAccount("new", "New", makeTestJWT("u_new", 0), "rt-new", "", 0); err == nil {
		t.Fatal("expected error when rename fails")
	}

	after, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("original file modified after failed rename")
	}

	entries, _ := os.ReadDir(filepath.Dir(file))
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".accounts-") {
			t.Fatalf("temp file %s was not cleaned up after failed rename", e.Name())
		}
	}
}

func TestConcurrentReadWriteSafety(t *testing.T) {
	setupTestAccounts(t)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		id := fmt.Sprintf("acct-%d", i)
		jwt := makeTestJWT(id, 0)
		wg.Add(2)
		go func(id, jwt string) {
			defer wg.Done()
			for j := 0; j < 10; j++ {
				_, _ = UpsertAccount(id, id, jwt, "rt-"+id, "", 0)
				MarkCooldown(id, 5*time.Millisecond)
				ClearCooldown(id)
			}
		}(id, jwt)
		go func() {
			defer wg.Done()
			for j := 0; j < 10; j++ {
				_ = List()
				_ = Usable(nil)
				_ = Summarize()
			}
		}()
	}
	wg.Wait()
	list, err := Load()
	if err != nil {
		t.Fatalf("Load() after concurrent access failed: %v", err)
	}
	if len(list) != 8 {
		t.Fatalf("expected 8 accounts after concurrent upserts, got %d", len(list))
	}
}

func TestConcurrentRefreshSingleflightPerAccount(t *testing.T) {
	setupTestAccounts(t)
	freshA := makeTestJWT("u_a", 0)
	freshB := makeTestJWT("u_b", 0)

	var callsA, callsB int32
	var inFlightBoth int32
	var maxInFlightBoth int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			RefreshToken string `json:"refresh_token"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		cur := atomic.AddInt32(&inFlightBoth, 1)
		for {
			m := atomic.LoadInt32(&maxInFlightBoth)
			if cur <= m || atomic.CompareAndSwapInt32(&maxInFlightBoth, m, cur) {
				break
			}
		}
		time.Sleep(60 * time.Millisecond)
		atomic.AddInt32(&inFlightBoth, -1)

		switch body.RefreshToken {
		case "rt-a":
			atomic.AddInt32(&callsA, 1)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"code": 0,
				"data": map[string]any{"token": freshA, "zai": map[string]any{"refresh_token": "rt-a-new"}},
			})
		case "rt-b":
			atomic.AddInt32(&callsB, 1)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"code": 0,
				"data": map[string]any{"token": freshB, "zai": map[string]any{"refresh_token": "rt-b-new"}},
			})
		default:
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"code":401,"msg":"bad refresh token"}`))
		}
	}))
	defer srv.Close()

	origOAuth := auth.OAuthBase
	auth.OAuthBase = srv.URL
	defer func() { auth.OAuthBase = origOAuth }()

	_, _ = UpsertAccount("a", "A", "stale-jwt-a", "rt-a", "", 0)
	_, _ = UpsertAccount("b", "B", "stale-jwt-b", "rt-b", "", 0)
	_, _ = UpsertAccount("fail", "Fail", "stale-jwt-f", "rt-bad", "", 0)

	// Launch 10 concurrent refresh calls for account "a" and 5 for account "b".
	const workersA = 10
	const workersB = 5
	var wg sync.WaitGroup
	errCh := make(chan error, workersA+workersB)

	for i := 0; i < workersA; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			jwt, exp, err := RefreshAccountSingleflight(context.Background(), "a", "stale-jwt-a")
			if err != nil {
				errCh <- fmt.Errorf("a refresh err: %w", err)
				return
			}
			if jwt != freshA || exp != 0 {
				errCh <- fmt.Errorf("a got jwt=%q exp=%d", jwt, exp)
			}
		}()
	}
	for i := 0; i < workersB; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			jwt, _, err := RefreshAccountSingleflight(context.Background(), "b", "stale-jwt-b")
			if err != nil {
				errCh <- fmt.Errorf("b refresh err: %w", err)
				return
			}
			if jwt != freshB {
				errCh <- fmt.Errorf("b got jwt=%q", jwt)
			}
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatal(err)
	}

	if gotA := atomic.LoadInt32(&callsA); gotA != 1 {
		t.Fatalf("expected exactly 1 refresh request for account a across %d concurrent callers, got %d", workersA, gotA)
	}
	if gotB := atomic.LoadInt32(&callsB); gotB != 1 {
		t.Fatalf("expected exactly 1 refresh request for account b across %d concurrent callers, got %d", workersB, gotB)
	}
	if atomic.LoadInt32(&maxInFlightBoth) < 2 {
		t.Fatalf("expected refreshes for distinct accounts a and b to run concurrently, maxInFlight=%d", atomic.LoadInt32(&maxInFlightBoth))
	}

	// Failed refresh must NOT delete the account; it must mark NeedsReauth=true.
	if _, err := TryRefreshAccount(context.Background(), "fail"); err == nil {
		t.Fatal("expected refresh failure for account fail")
	}
	var foundFail *Account
	for _, a := range List() {
		if a.ID == "fail" {
			cp := a
			foundFail = &cp
		}
	}
	if foundFail == nil {
		t.Fatal("failed refresh deleted the account from store!")
	}
	if !foundFail.NeedsReauth {
		t.Fatal("failed refresh must mark account NeedsReauth=true")
	}
}
