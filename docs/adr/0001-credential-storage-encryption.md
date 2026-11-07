# ADR 0001: Credential Storage Protection & Encryption-at-Rest Evaluation

- **Status:** Accepted (Filesystem ACL/Mode Enforcement Implemented; Application-Level Encryption Deferred)
- **Date:** 2026-11-07
- **Context:** Phase 4B Security Hardening (`zcode-gateway`)

## 1. Assets Evaluated

| Asset | Default Path | Contents | Sensitivity |
|---|---|---|---|
| Multi-account ring | `data/accounts.json` (`ZCODE_ACCOUNTS_FILE`) | Plan `jwt`, OAuth `refreshToken`, `accessToken`, `userId`, `deviceId`, `sessionSeed` | Critical (grants inference & token refresh) |
| Single-token fallback | `./plan-jwt.json` (`ZCODE_PLAN_JWT_FILE`) | Plan `jwt` | High (grants inference until revoked) |
| Desktop client store | `~/.zcode/v2/credentials.json` (`ZCODE_STATE_DIR`) | `enc:v1:<iv>.<tag>.<ciphertext>` (read-only by gateway) | High |

## 2. Threat Model & Evaluation of AES-256-GCM Encryption at Rest

We evaluated encrypting `jwt`, `refreshToken`, and `accessToken` inside `data/accounts.json` using AES-256-GCM. Before implementing encryption, we answered the six mandatory key-management questions:

### Q1. Where does the encryption key come from?
Three candidate sources exist for a headless CLI/daemon on Linux VPS or local workstation:
1. **Machine-derived deterministic key** (e.g., `SHA-256(os + homedir + username)` as used by the ZCode desktop app's `desktopStoreKey()`).
   - *Verdict:* **Rejected for `accounts.json`.** Any process running as the same OS user (or root) can compute the exact same key trivially. Calling this "encryption at rest" provides a false sense of security.
2. **Environment variable (`ZCODE_STORAGE_KEY`)** passed via `.env` file in the working directory.
   - *Verdict:* **Unsafe if stored on the same disk.** If `.env` and `data/accounts.json` reside on the same filesystem with the same owner, an attacker who can read `data/accounts.json` can also read `.env` (or `/proc/<pid>/environ`).
3. **External secret manager / OS keychain / `systemd-creds` (`LoadCredentialEncrypted=`)** injected via tmpfs (`$CREDENTIALS_DIRECTORY`) or interactive passphrase prompt.
   - *Verdict:* **Only cryptographically sound option** on a single host when backed by TPM2 or an external secret store.

### Q2. How is the key stored?
- The encryption key must **never** be stored in `data/accounts.json`, `config.json`, or a plaintext file in the repository directory.
- On Linux VPS with `systemd`, keys must reside in `systemd-creds` (`/run/credentials/...` on non-swappable `ramfs`, readable only by the service UID) or an external secret manager.

### Q3. How is key rotation performed?
- Ciphertext values must carry a version and key identifier prefix: `enc:v2:<key_id>:<base64url_iv>.<base64url_tag>.<base64url_ciphertext>`.
- Rotation requires reading `accounts.json` with the active keyring (`current_key` + `previous_keys`), re-encrypting all fields under `current_key` with fresh 96-bit cryptographic random nonces (`crypto/rand`), and committing via atomic write-and-rename.

### Q4. How does recovery work if the key is lost?
- `zcode-gateway` does not own authoritative user data; `accounts.json` only caches OAuth tokens issued by `zcode.z.ai`.
- If the encryption key is permanently lost, ciphertext entries cannot be recovered; the operator recovers by running `zcode-gateway login` (15-second OAuth device flow) to issue fresh tokens.

### Q5. How is backward compatibility maintained with existing credentials?
- A reader inspects the field prefix: values without `enc:v2:` are treated as legacy plaintext (when migration mode is enabled), while `DesktopStoreJWT()` continues to support `enc:v1:` for reading `~/.zcode/v2/credentials.json`.

### Q6. How is migration performed without losing accounts?
- Before migrating a plaintext `accounts.json`, the gateway must verify that the encryption key is valid (32 bytes entropy), create a `0600` backup (`accounts.json.bak.<timestamp>`), encrypt in memory, verify decryption round-trip, and write atomically via `CreateTemp` + `Sync` + `Rename`.

## 3. Decision

1. **Do NOT implement pseudo-encryption with a machine-derived or co-located key.** Storing an encryption key next to ciphertext or deriving it from `os/user` does not add security beyond POSIX file permissions.
2. **Enforce strict OS-level isolation and atomic storage now (Phase 4B):**
   - `data/` directory enforced at `0700`.
   - `data/accounts.json`, temporary write files, and corrupt-store backups enforced at `0600`.
   - `plan-jwt.json` written by `sync-token.js` enforced at `0600`.
   - Dedicated unprivileged service user (`zcode`) and `ProtectSystem=strict` / `ReadWritePaths=` in systemd deployment units (Phase 7).
3. **Defer AES-256-GCM field encryption** until an external key provider (`ZCODE_STORAGE_KEY_FILE` pointing to `/run/credentials/...` or OS keyring) is explicitly required by the operator.
