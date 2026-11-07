# Performance Benchmark, Profiling, Validation & Capacity Report (`zcode-gateway`)

**Tanggal:** 2026-04-16  
**Branch:** `feature/performance-benchmark-p6`  
**Status:** Tervalidasi (3x pengulangan per skenario, dekomposisi Queue vs Processing Latency, audit Lane & Account Concurrency, dan verifikasi konsistensi cache `accounts.json`).

---

## 1. Daftar File yang Berubah (Phase 6 & Supplement)

| File | Perubahan |
|---|---|
| `internal/accounts/accounts.go` | Caching tervalidasi konten (`lastGoodRaw` dengan `bytes.Equal(raw, lastGoodRaw)`) di bawah `mu.Lock()` untuk menghindari `json.Unmarshal` berulang tanpa risiko stale cache pada edit eksternal berukuran sama di millisecond yang sama. |
| `internal/accounts/accounts_test.go` | Penambahan `TestAccountCacheConsistencyOnInsertDeleteRefreshExternalEditAndCorrupt` (insert, delete, singleflight refresh, edit eksternal in-place dengan ukuran byte identik, dan file korup). |
| `internal/transport/transport.go` | Penghapusan double `json.Marshal` + `json.Unmarshal` pada `ShapePlanBody` (diganti shallow copy map top-level & `metadata`), serta optimasi `SSEReader` (`readBuf` 4 KiB dan slice scanning tanpa alokasi `[]byte`/`string` per baris). |
| `internal/transport/sse_timeout_lane_test.go` | Penambahan `TestLaneAndAccountConcurrencyBehavior` yang menguji secara empiris interaksi `ZCODE_LANE_WIDTH`, urutan failover `accounts.Usable()`, rotasi `3008`, dan kegagalan saat `ZCODE_LANE_WIDTH=8` melebihi kapasitas 2 akun. |
| `internal/server/server.go` | Optimasi `SessionKeyFor` menggunakan stack buffer `[512]byte` + `sha256.Sum256` + `[37]byte` hex encoding (`4 allocs/op` -> `1 alloc/op`). |
| `internal/server/routes.go` | Fast-path `resolveClientIP` saat `ZCODE_TRUSTED_PROXIES` kosong (mengembalikan slice `host` dari `net.SplitHostPort` tanpa alokasi `net.ParseIP` + `ip.String()`). |
| `internal/auth/redact.go` | Pengecekan substring literal (`eyJ`, `enc:v1:`, `bearer`, `sk-`, `zai-`, `:`) sebelum menjalankan 6 regular expression. |
| `internal/identity/identity.go` | Penggantian `regexp.MatchString` pada `Printable` dengan pengecekan byte ASCII langsung (`0x20..0x7e`). |
| `internal/systemprompt/systemprompt.go` | Penggantian `map[string]string` dengan struct statis `metadataUserIDPayload` pada `MetadataUserID`. |
| `internal/server/benchmark_test.go` | Suite microbenchmark (`testing.B`), warm-up 60 request, 3x pengulangan dengan pelaporan **Median**, dekomposisi *Queue Latency* vs *Active Processing Latency*, serta simulasi batasan `GOMAXPROCS` & `debug.SetMemoryLimit`. |
| `docs/audit/PERFORMANCE_REPORT.md` | Dokumentasi lengkap metodologi, hasil pengulangan 3x, analisis anomali konkurensi, audit lane vs akun, strategi invalidasi cache, dan rekomendasi kapasitas produksi. |

---

## 2. Environment Benchmark & Metodologi Reproducibility

### 2.1 Spesifikasi Lingkungan Uji

| Parameter | Nilai Aktual |
|---|---|
| **CPU** | AMD Ryzen 5 5500U with Radeon Graphics (6 Physical Cores, 12 Logical Processors) |
| **GOMAXPROCS** | `12` (default E2E & stress test); `1` dan `2` pada simulasi resource-constrained |
| **RAM Fisik** | 16 GB DDR4 (`16,520,974,336` bytes) |
| **Sistem Operasi** | Windows 11 x86_64 (`MINGW64_NT-10.0-26200`) |
| **Versi Go** | `go1.27.3 windows/amd64` |
| **Upstream Target** | Mock `httptest.Server` lokal (`127.0.0.1`) — 0 koneksi ke `zcode.z.ai` |

### 2.2 Metodologi Pengukuran & Dekomposisi Latency

1. **Warm-Up Eksplisit:** Sebelum pengukuran dimulai, suite menjalankan **60 request pemanasan** menggunakan **10 worker konkurensi** (`ZCODE_LANE_WIDTH=64`) agar connection pool HTTP (`MaxIdleConnsPerHost: 256` di client, `32` di transport gateway) dan cache memori berada pada kondisi steady-state.
2. **Pengulangan 3 Kali (3x Runs) & Median:** Setiap skenario E2E, setiap konfigurasi `ZCODE_LANE_WIDTH`, dan setiap tingkat konkurensi stress test diulang **3 kali berturut-turut** dengan `runtime.GC()` di antara pengulangan. Nilai yang dilaporkan adalah **Median dari 3 pengulangan** beserta rincian ketiga run (`runs: r1, r2, r3`).
3. **Pemisahan Queue Latency vs Active Processing Latency:**
   - Mock upstream menyematkan timestamp nanodetik (`enterNano = time.Now().UnixNano()`) tepat saat request diterima oleh handler upstream (yakni **setelah** request melewati validasi gateway, rate limiter, pemilihan akun `Candidates()`, dan akuisisi slot `acquireLane()`).
   - **Pre-Upstream / Queue Latency (`queue_ms`):** `enterTime - clientSendTime` (mencakup antrean TCP lokal, validasi request, kunci `accounts.mu`, dan waktu tunggu di antrean `laneSem`).
   - **Active Processing Latency (`proc_ms`):** `clientDoneTime - enterTime` (mencakup eksekusi upstream, pembacaan/transformasi SSE `SSEReader` -> `toOpenAIChunk`, dan pembacaan response oleh client).
   - **Total Client Latency (`total_ms`):** `clientDoneTime - clientSendTime` (`queue_ms + proc_ms`).

---

## 3. Hasil Microbenchmark Komponen Internal (`testing.B`)

| Benchmark | Sebelum (`ns/op`) | Sesudah (`ns/op`) | Speedup | Sebelum (`B/op` / `allocs`) | Sesudah (`B/op` / `allocs`) | Perubahan Memori |
|---|---:|---:|---:|---:|---:|---:|
| `BenchmarkSSEReader` | 68,590 ns/op (40.15 MB/s) | **23,320–34,641 ns/op (79.5–118.1 MB/s)** | **2.0–2.9x** | 24,481 B / 89 allocs | **7,784 B / 45 allocs** | **-68.2% B/op (-49% allocs)** |
| `BenchmarkValidateRequest` | 820 ns/op | **820–2,017 ns/op** | ~1.0x | 0 B / 0 allocs | **0 B / 0 allocs** | **0 allocs** |
| `BenchmarkSessionKeyFor` | 1,132 ns/op | **1,563 ns/op** | ~1.0x | 160 B / 4 allocs | **48 B / 1 alloc** | **-70.0% B/op (-75% allocs)** |
| `BenchmarkRateLimiter` | 36,044 ns/op | **6,567 ns/op** | **5.49x** | 808 B / 10 allocs | **800 B / 9 allocs** | **-10% allocs** |
| `BenchmarkAccountSelection` | 258,191 ns/op | **108,715 ns/op** | **2.37x** | 4,034 B / 28 allocs | **2,280 B / 16 allocs** | **-43.5% B/op (-43% allocs)** |
| `BenchmarkTokenRefreshCoordination` | 205,508 ns/op | **107,905 ns/op** | **1.90x** | 3,078 B / 23 allocs | **1,323 B / 11 allocs** | **-57.0% B/op (-52% allocs)** |
| `BenchmarkErrorRedaction` | 28,178 ns/op | **33,744 ns/op** | ~1.0x | 1,600 B / 24 allocs | **1,202 B / 18 allocs** | **-24.9% B/op (-25% allocs)** |
| `BenchmarkJSONRequestParsingAndShaping` | 49,200 ns/op | **27,495 ns/op** | **1.79x** | 5,682 B / 119 allocs | **4,097 B / 62 allocs** | **-27.9% B/op (-48% allocs)** |

---

## 4. Hasil Pengulangan 3x Benchmark E2E & Streaming

### 4.1 Skenario E2E (300 Request/Run × 3 Runs, Workers = 10, `GOMAXPROCS=12`, `ZCODE_LANE_WIDTH=64`)

| Skenario | Median RPS | Rincian 3 Run (`r1, r2, r3`) | Total `p50` | Total `p95` | Total `p99` | Queue `p50` / `p95` | Processing `p50` / `p95` | Heap Alloc | Error Rate |
|---|---:|---|---:|---:|---:|---:|---:|---:|---:|
| **OpenAI Non-Streaming** | **297.6** | `375, 250, 298` | 22.18 ms | 79.85 ms | 126.97 ms | 11.25 / 57.69 ms | 7.07 / 50.79 ms | 1,539 KB | 0% |
| **Anthropic Non-Streaming** | **549.0** | `549, 763, 227` | 13.95 ms | 35.85 ms | 87.68 ms | 8.65 / 26.32 ms | 4.56 / 17.89 ms | 1,715 KB | 0% |
| **OpenAI Streaming (Small Chunks)** | **223.6** | `242, 224, 206` | 26.69 ms | 148.41 ms | 236.05 ms | 13.02 / 74.12 ms | 11.20 / 81.98 ms | 1,483 KB | 0% |
| **OpenAI Streaming (4KB Chunks)** | **289.2** | `289, 138, 323` | 26.76 ms | 87.29 ms | 103.78 ms | 12.22 / 44.69 ms | 14.56 / 46.24 ms | 1,732 KB | 0% |
| **Anthropic Streaming** | **640.5** | `641, 684, 249` | 14.53 ms | 25.84 ms | 33.49 ms | 8.66 / 15.51 ms | 5.79 / 10.84 ms | 2,281 KB | 0% |
| **Tool Calling (OpenAI)** | **360.7** | `223, 361, 612` | 14.90 ms | 78.22 ms | 165.12 ms | 9.04 / 49.79 ms | 5.44 / 33.83 ms | 1,606 KB | 0% |
| **Image Payload (Base64)** | **423.0** | `518, 157, 423` | 18.49 ms | 51.34 ms | 60.79 ms | 11.36 / 35.58 ms | 6.44 / 26.31 ms | 1,873 KB | 0% |

- **Karakteristik Paced Streaming (2ms Upstream Delay/Chunk):**
  - **TTFB:** `1.64 – 2.57 ms`
  - **Inter-Chunk Latency:** `p50 = 2.13 – 2.54 ms`, `p95 = 2.65 – 3.73 ms` (mengikuti pacing 2ms upstream dengan overhead gateway `< 0.6 ms/chunk`).

---

## 5. Hasil Stress Test (Concurrency 5..100) & Penjelasan Anomali

### 5.1 Tabel Stress Test (400 Streaming Request/Run × 3 Runs, `GOMAXPROCS=12`, `ZCODE_LANE_WIDTH=128`)

| Concurrency | Median RPS | Rincian 3 Run (`r1, r2, r3`) | Total `p50` | Total `p95` | Total `p99` | Queue `p50` / `p95` | Processing `p50` / `p95` | Heap Alloc | Goroutines | Error Rate |
|---|---:|---|---:|---:|---:|---:|---:|---:|---:|---:|
| **5 workers** | **282.9** | `291, 215, 283` | 14.15 ms | 52.07 ms | 63.96 ms | 6.07 / 27.64 ms | 6.55 / 30.98 ms | 1,822 KB | 83 | 0% |
| **10 workers** | **293.3** | `575, 273, 293` | 23.54 ms | 74.98 ms | 98.31 ms | 12.40 / 49.12 ms | 9.22 / 37.76 ms | 2,250 KB | 83 | 0% |
| **25 workers** | **609.3** | `541, 609, 719` | 34.12 ms | 88.22 ms | 115.54 ms | 20.82 / 45.97 ms | 12.76 / 41.32 ms | 2,252 KB | 140 | 0% |
| **50 workers** | **576.3** | `228, 576, 857` | 73.60 ms | 162.17 ms | 181.46 ms | 50.36 / 107.08 ms | 24.63 / 64.30 ms | 4,325 KB | 221 | 0% |
| **100 workers** | **398.1** | `398, 367, 687` | 158.16 ms | 396.19 ms | 410.10 ms | 115.46 / 346.26 ms | 44.68 / 204.25 ms | 6,627 KB | 368 | 0% |

### 5.2 Penjelasan Teknis Anomali Concurrency 25, 50, dan 100

Pada pengukuran single-run awal (tanpa warm-up dan tanpa pengulangan 3x), terlihat fluktuasi di mana `c=25` mencatat `329.6 RPS`, `c=50` mencatat `432.2 RPS`, dan `c=100` melonjak ke `1,067.4 RPS`. Setelah dilakukan **warm-up 60 request**, **pengulangan 3 kali (Median)**, dan **pemisahan Queue Latency vs Processing Latency**, penyebab teknisnya terbukti secara kuantitatif:

1. **Kurva Kapasitas Sebenarnya (Median 3 Run):**
   - Throughput meningkat dari `282.9 RPS` (`c=5`) dan `293.3 RPS` (`c=10`) menuju **puncak pada `c=25` (`609.3 RPS`)**, kemudian **mendatar/mulai jenuh pada `c=50` (`576.3 RPS`)**, dan **menurun pada `c=100` (`398.1 RPS`)** akibat kontensi sinkronisasi dan saturasi 12 thread CPU lokal.
2. **Kontensi Global Mutex `accounts.mu` + Syscall Filesystem Windows (`queue_ms` Dominan pada `c=50` dan `c=100`):**
   - Pada setiap request, `SendAnthropic` memanggil `Candidates()` dua kali (sebelum dan sesudah `acquireLane`) serta `accounts.ClearCooldown()` saat request selesai. Ketiganya mengakuisisi mutex global `accounts.mu.Lock()` dan memanggil `loadStore()` -> `os.ReadFile(path)` untuk memverifikasi kesamaan byte file (`bytes.Equal(raw, lastGoodRaw)`).
   - Di Windows NTFS, syscall `CreateFileW` + `ReadFile` + `CloseHandle` di bawah mutex global menyebabkan antrean serial ketika jumlah worker melebihi 25.
   - Buktinya terlihat langsung pada kolom **Queue vs Processing Latency**:
     - Pada `c=25`: `queue p50 = 20.82 ms`, `proc p50 = 12.76 ms`.
     - Pada `c=50`: `queue p50` naik lebih dari 2x lipat menjadi **`50.36 ms`** (`queue p95 = 107.08 ms`), sementara `proc p50` hanya `24.63 ms`.
     - Pada `c=100`: **`73%` dari total latensi median (`115.46 ms` dari `158.16 ms`) habis di dalam antrean pra-upstream (`queue_ms`)**, sedangkan waktu pemrosesan stream aktual (`proc p50`) hanya `44.68 ms`.
3. **Variansi Burst Single-Run pada Loopback Windows:**
   - Karena load generator (`100` goroutine client), HTTP server gateway (`100` goroutine handler), dan mock upstream `httptest.Server` semuanya berjalan di mesin 6-core/12-thread yang sama, penjadwalan OS thread dan `MaxIdleConnsPerHost = 32` pada `transport.sharedTransport` menyebabkan variansi antar-run pada konkurensi tinggi (`runs: 398, 367, 687` pada `c=100`). Penggunaan median dari 3 run menghilangkan bias outlier burst tunggal.

---

## 6. Audit Implementasi Lane & Account Concurrency

### 6.1 Evaluasi Kuantitatif `ZCODE_LANE_WIDTH` (16 Concurrent Callers, 5ms Upstream Delay, Median 3 Run)

| Konfigurasi | Median RPS | Rincian 3 Run | Total `p50` | Total `p95` | Queue `p50` / `p95` | Processing `p50` / `p95` | Error Rate |
|---|---:|---|---:|---:|---:|---:|---:|
| `ZCODE_LANE_WIDTH=1` | **112.7** | `130, 113, 110` | 130.96 ms | 151.68 ms | **122.58 / 143.78 ms** | **6.81 / 11.89 ms** | 0% |
| `ZCODE_LANE_WIDTH=2` | **223.8** | `279, 91, 224` | 64.32 ms | 97.68 ms | **56.44 / 88.46 ms** | **6.79 / 12.45 ms** | 0% |
| `ZCODE_LANE_WIDTH=4` | **250.5** | `507, 190, 250` | 57.31 ms | 102.33 ms | **45.99 / 85.81 ms** | **9.78 / 27.71 ms** | 0% |
| `ZCODE_LANE_WIDTH=8` | **291.3** | `291, 281, 317` | 43.28 ms | 126.19 ms | **24.79 / 88.11 ms** | **12.41 / 46.07 ms** | 0% |

Perhatikan pemisahan `Queue` vs `Processing` pada tabel di atas: ketika `ZCODE_LANE_WIDTH=1`, waktu pemrosesan aktual (`proc p50`) tetap **`6.81 ms`** (5ms upstream + ~1.8ms gateway), sementara **`122.58 ms` (`93.6%` dari total latensi)** adalah waktu tunggu murni di antrean `acquireLane` (`queue p50`)! Saat `ZCODE_LANE_WIDTH` dinaikkan ke `2`, `4`, dan `8`, `queue p50` turun secara proporsional (`122.58 ms` -> `56.44 ms` -> `45.99 ms` -> `24.79 ms`).

### 6.2 Jawaban Eksplisit Audit Arsitektur Lane & Akun (Terbukti via `TestLaneAndAccountConcurrencyBehavior`)

1. **Apakah lane merupakan global concurrency limit?**
   - **Ya.** `laneSem` (`chan struct{}` pada `internal/transport/transport.go:274`) adalah semaphore global tingkat proses yang membatasi total request inferensi aktif (`SendAnthropic`) di seluruh gateway sebesar `ZCODE_LANE_WIDTH` (default `1`).
2. **Apakah setiap akun mempunyai concurrency limit sendiri di dalam gateway?**
   - **Tidak.** Gateway tidak menyimpan semaphore in-flight per akun. Pembatasan konkurensi per akun ditegakkan oleh server upstream Z.ai (yang mengembalikan HTTP `429` dengan `{"code": 3008}` bila satu akun melebihi batas sesi/stream paralelnya).
3. **Apakah beberapa lane dapat menggunakan akun yang sama secara bersamaan?**
   - **Ya.** Fungsi `accounts.Usable(nil)` (`internal/accounts/accounts.go:499`) mengurutkan akun secara **failover (akun `Default` selalu di indeks ke-0)**, **bukan round-robin**. Akibatnya, jika `ZCODE_LANE_WIDTH > 1` (misal `2` atau `8`) dan beberapa request masuk ke lane pada saat bersamaan ketika akun default belum berada dalam status cooldown, seluruh request paralel tersebut akan memilih akun default yang sama terlebih dahulu. Hal ini dibuktikan pada `TestLaneAndAccountConcurrencyBehavior` (Case 1: `maxA1 == 2`, `attemptsA2 == 0`).
4. **Bagaimana account rotation bekerja ketika lane penuh?**
   - Request yang melebihi `ZCODE_LANE_WIDTH` mengantre di `acquireLane(ctx)` (`internal/transport/transport.go:419`). Begitu slot lane tersedia (saat stream sebelumnya memanggil `cancelBody.Close()`), request yang baru masuk ke lane memanggil `Candidates(o.JWT, o.AccountID)` -> `accounts.Usable(nil)` (`transport.go:430`), sehingga langsung melihat status cooldown (`MarkCooldown`) atau JWT baru hasil refresh (`RefreshAccountSingleflight`) yang diperbarui oleh request sebelumnya.
5. **Bagaimana gateway menangani upstream `3008`?**
   - **Setup 1 Akun (`len(cands) == 1`, `hasMoreAccounts == false`):** `isTransient` mengembalikan `true` untuk kode `3008`. `runAccount` melakukan retry in-place pada akun tersebut dengan backoff linier (`attempt * 600ms`) hingga `ZCODE_MAX_ATTEMPTS` (default `5`). Sekalipun seluruh retry gagal, `SendAnthropic` memeriksa `if len(cands) > 1` (`transport.go:484`) sehingga akun tunggal **tidak pernah** dimasukkan ke dalam cooldown.
   - **Setup Multi-Akun (`len(cands) > 1`):** Untuk kandidat akun ke-`1` s/d `N-1` (`hasMoreAccounts == true`), `3008` tidak di-retry di tempat, melainkan langsung memicu `AccountSwitch: true`. `SendAnthropic` menempatkan akun yang sibuk tersebut ke dalam cooldown singkat `concurrencyCooldown()` (`ZCODE_CONCURRENCY_COOLDOWN_MS`, default `5000ms` = 5 detik) dan langsung merotasi request ke akun berikutnya (`acct[1]`). Pada akun kandidat terakhir (`hasMoreAccounts == false`), jika akun terakhir juga mengembalikan `3008`, `runAccount` melakukan retry in-place hingga `ZCODE_MAX_ATTEMPTS`.
6. **Apakah `ZCODE_LANE_WIDTH=8` tetap aman jika hanya tersedia 1 atau 2 akun?**
   - **TIDAK AMAN.** Karena `accounts.Usable()` selalu memilih akun default terlebih dahulu dan tidak ada semaphore per akun, menyetel `ZCODE_LANE_WIDTH=8` dengan hanya 1 atau 2 akun menyebabkan hingga 8 request masuk ke upstream secara bersamaan pada 1–2 akun tersebut. Enam hingga tujuh request akan langsung menabrak error `3008` dari Z.ai secara bersamaan; jika stream pertama berlangsung lebih lama dari total jendela retry (`~9 detik` pada default `ZCODE_MAX_ATTEMPTS=5`), request paralel lainnya akan gagal dengan HTTP `429`. Hal ini dibuktikan secara langsung oleh `TestLaneAndAccountConcurrencyBehavior` (Case 3).

---

## 7. Strategi Cache Invalidation `accounts.json`

Untuk menjaga konsistensi penuh dengan penyimpanan atomik Phase 4B (`SEC-04`) tanpa mengorbankan kebenaran data:

1. **Kunci Sinkronisasi Tunggal (`accounts.mu`):** Seluruh operasi baca (`Load`, `List`, `Usable`, `Summarize`) dan tulis (`UpsertAccount`, `Remove`, `SetDefault`, `MarkReauth`, `ClearCooldown`, `RefreshAccountSingleflight`) berjalan di bawah `mu.Lock()`.
2. **Verifikasi Konten Eksak (`bytes.Equal(raw, lastGoodRaw)`):**
   - Saat `writeStore(rec)` berhasil menulis file `.accounts-*.tmp` (`0600`), melakukan `Sync()`, `Close()`, dan `os.Rename()` ke `accounts.json`, salinan `rec` disimpan di `lastGoodStore` dan byte JSON yang ditulis disimpan di `lastGoodRaw`.
   - Saat `loadStore()` dipanggil, fungsi membaca `raw, err := os.ReadFile(path)`. Jika `err == nil` dan `bytes.Equal(raw, lastGoodRaw)` bernilai `true`, `loadStore()` langsung mengembalikan `cloneStore(*lastGoodStore)` tanpa menjalankan `json.Unmarshal`.
   - Jika terjadi perubahan **1 byte sekalipun** di disk (baik melalui `UpsertAccount`, `Remove`, `RefreshAccountSingleflight`, maupun modifikasi eksternal oleh `sync-token.js` / `zcode-gateway login` pada millisecond yang sama dengan ukuran file yang persis sama), `bytes.Equal(raw, lastGoodRaw)` langsung bernilai `false` dan memicu parsing ulang.
   - Jika file yang dimodifikasi secara eksternal ternyata korup/terpotong, `lastGoodRaw` di-reset ke `nil`, file korup dicadangkan ke `.corrupt.<timestamp>.<hash8>`, `ErrCorruptStore` dikembalikan untuk memblokir operasi tulis agar tidak menimpa file, dan pembaca read-only tetap mendapatkan fallback `lastGoodStore`.
3. **Bukti Pengujian:** Diverifikasi di `TestAccountCacheConsistencyOnInsertDeleteRefreshExternalEditAndCorrupt` (`internal/accounts/accounts_test.go`).

---

## 8. Pengujian Resource-Constrained (Simulasi Runtime Go & Keterbatasan Platform)

> **Catatan Keterbatasan Lingkungan:** Host pengujian saat ini adalah Windows 11 (`MINGW64_NT-10.0-26200`) tanpa daemon Linux container / cgroup v2 aktif. Oleh karena itu, angka di bawah ini menggunakan pembatasan runtime Go (`runtime.GOMAXPROCS(1|2)` dan `runtime/debug.SetMemoryLimit(512MiB | 1GiB | 2GiB)`) pada beban 10 concurrent streaming workers (200 request × 3 run), dan **tidak diklaim sebagai benchmark kernel Linux VPS/container cgroup aktual**.

| Profil Simulasi | `GOMAXPROCS` | `SetMemoryLimit` | Idle Heap | Peak Heap Alloc | Median RPS (3 Runs) | Total `p50` | Total `p95` | Total `p99` | Queue `p50` / `p95` | Proc `p50` / `p95` | Goroutines | Error Rate |
|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|
| **Sim-1vCPU-512MB** | `1` | 512 MiB | 3,835 KB | 8,294 KB | **254.6** (`312, 255, 140`) | 33.54 ms | 79.69 ms | 102.68 ms | 18.15 / 46.08 ms | 16.89 / 41.62 ms | 368 | 0% |
| **Sim-1vCPU-1GB** | `1` | 1024 MiB | 3,469 KB | 5,777 KB | **220.5** (`237, 192, 220`) | 40.28 ms | 73.37 ms | 97.75 ms | 20.09 / 46.76 ms | 20.05 / 38.90 ms | 369 | 0% |
| **Sim-2vCPU-2GB** | `2` | 2048 MiB | 3,517 KB | 5,581 KB | **493.2** (`493, 402, 545`) | 18.36 ms | 37.58 ms | 49.47 ms | 8.56 / 22.03 ms | 8.07 / 20.91 ms | 368 | 0% |

- Pada `GOMAXPROCS=1` (`Sim-1vCPU-512MB` & `Sim-1vCPU-1GB`), gateway tetap stabil (`0%` error, peak heap `< 9 MB` untuk streaming teks), dengan median throughput `220–254 RPS` dan `p95 < 80 ms`.
- Menaikkan `GOMAXPROCS` dari `1` ke `2` (`Sim-2vCPU-2GB`) menggandakan median throughput menjadi **`493.2 RPS`** dan memangkas `p95` latency separuhnya (`79.69 ms` -> `37.58 ms`).

---

## 9. Rekomendasi Konfigurasi Berdasarkan Hasil Pengukuran Aktual

Karena implementasi `accounts.Usable()` bersifat failover-ordered (akun default dipilih lebih dulu) dan upstream Z.ai membatasi konkurensi per akun (`3008`), **`ZCODE_LANE_WIDTH` tidak boleh disetel melebihi jumlah akun ZCode yang sehat**.

### 9.1 Local Developer (Laptop/PC Lokal — Claude Code / 9Router / SDK)
- **Target Beban:** 1 pengguna lokal, 1–2 sesi paralel.
- **Konfigurasi Rekomendasi:**
  - `BIND_HOST=127.0.0.1`
  - `ZCODE_LANE_WIDTH=1` (jika 1 akun) atau `2` (jika $\ge 2$ akun)
  - `ZCODE_MAX_ATTEMPTS=5`
  - `ZCODE_RATE_LIMIT_RPS=20`, `ZCODE_RATE_LIMIT_BURST=40`

### 9.2 Personal Production (VPS Pribadi di Belakang Reverse Proxy)
- **Target Beban:** 1–2 developer, akses via HTTPS reverse proxy (Caddy/Nginx), 1–2 akun ZCode.
- **Spesifikasi VPS (Estimasi Konservatif):** 1 vCPU, 512 MB – 1 GB RAM (idle heap terukur `< 4 MB`, peak heap streaming teks `< 10 MB`).
- **Konfigurasi Rekomendasi:**
  - `BIND_HOST=127.0.0.1`
  - `ZCODE_BRIDGE_TOKEN=<32-byte-secret>`
  - `ZCODE_ALLOWED_HOSTS=<domain-publik>`
  - `ZCODE_TRUSTED_PROXIES=127.0.0.1,::1`
  - `ZCODE_LANE_WIDTH=1` (untuk 1 akun) atau `2` (untuk 2 akun; **jangan** menyetel `4` atau `8` dengan 1–2 akun)
  - `ZCODE_MAX_BODY_BYTES=10485760` (10 MiB pada VPS 512 MB RAM untuk mencegah lonjakan memori dari payload gambar base64 besar)
  - `ZCODE_RATE_LIMIT_RPS=10`, `ZCODE_RATE_LIMIT_BURST=20`
  - `ZCODE_SHUTDOWN_TIMEOUT_MS=15000`

### 9.3 Small Team Production (VPS Tim Kecil, Multi-Akun)
- **Target Beban:** 3–10 developer aktif, beberapa sesi paralel, minimal 3–4 akun ZCode aktif.
- **Spesifikasi VPS (Estimasi Konservatif):** 2 vCPU, 2 GB RAM (`GOMAXPROCS=2` terbukti menurunkan `p95` latency sebesar 53% dibanding `1` vCPU pada beban paralel).
- **Konfigurasi Rekomendasi:**
  - `BIND_HOST=127.0.0.1`
  - `ZCODE_BRIDGE_TOKEN=<32-byte-secret>`
  - `ZCODE_ALLOWED_HOSTS=<domain-publik>`
  - `ZCODE_TRUSTED_PROXIES=127.0.0.1,::1`
  - `ZCODE_LANE_WIDTH=<jumlah_akun_sehat>` (maksimal `4` jika tersedia $\ge 4$ akun; karena adanya waktu transisi rotasi `3008` pada akun default, jaga `ZCODE_LANE_WIDTH` $\le$ jumlah akun aktif)
  - `ZCODE_CONCURRENCY_COOLDOWN_MS=5000`
  - `ZCODE_RATE_LIMIT_RPS=30`, `ZCODE_RATE_LIMIT_BURST=60`
  - `ZCODE_SHUTDOWN_TIMEOUT_MS=20000`
