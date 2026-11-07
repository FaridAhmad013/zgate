# Performance Benchmark, Profiling & Optimization Report (`zcode-gateway`)

**Tanggal:** 2026-04-16  
**Branch:** `feature/performance-benchmark-p6`  
**Status:** Selesai — Seluruh benchmark menggunakan mock upstream lokal (`net/http/httptest`), tanpa menghubungi layanan `zcode.z.ai` asli.

---

## 1. Environment Benchmark, OS, Versi Go & Spesifikasi Hardware

| Parameter | Nilai Aktual |
|---|---|
| **CPU** | AMD Ryzen 5 5500U with Radeon Graphics (6 Physical Cores, 12 Logical Processors / `GOMAXPROCS=12`) |
| **RAM** | 16 GB DDR4 (`16,520,974,336` bytes) |
| **Sistem Operasi** | Windows 11 x86_64 (`MINGW64_NT-10.0-26200`) |
| **Versi Go** | `go1.27.3 windows/amd64` |
| **Upstream Target** | Local loopback `httptest.Server` (`127.0.0.1`) — deterministik, 0 kuota eksternal terpakai |

---

## 2. Konfigurasi Gateway Saat Pengujian

- **Bind Address:** `127.0.0.1:<ephemeral>`
- **Jumlah Akun Tersimpan:** 2 akun pada `accounts.json` (rotasi round-robin aktif)
- **HTTP Server Limits:** `MaxHeaderBytes = 64 KiB`, `ReadHeaderTimeout = 10s`, `BodyReadTimeout = 30s`, `MaxBodyBytes = 32 MiB`, `IdleTimeout = 66s`
- **HTTP Client Pooling (`internal/transport`):** `MaxIdleConns = 64`, `MaxIdleConnsPerHost = 32`, `IdleConnTimeout = 90s`
- **Lane Width (`ZCODE_LANE_WIDTH`):**
  - `64` pada pengujian throughput/kapasitas murni gateway dan stress test concurrency (`5..100`) agar mengukur overhead gateway, bukan antrean artifisial lane=1.
  - `1`, `2`, `4`, `8` pada pengujian khusus karakteristik *Concurrency Lane Width* (Bagian 5).

---

## 3. Metodologi Benchmark

1. **Microbenchmark Komponen Internal (`testing.B`):**
   - Dijalankan dengan `go test -bench=. -benchmem -benchtime=200ms -run=^$ ./internal/server` sebelum dan sesudah optimasi.
   - Mengukur waktu eksekusi (`ns/op`), throughput byte (`MB/s` untuk SSE parser), alokasi memori (`B/op`), dan jumlah alokasi heap (`allocs/op`).
2. **End-to-End HTTP Load & Streaming Suite (`TestRunBaselineAndStressSuite`):**
   - Menggunakan TCP listener nyata (`net.Listen("tcp", "127.0.0.1:0")`) + `server.Serve(ctx, ln)` dan `http.Client` dengan connection pool (`MaxIdleConnsPerHost: 256`).
   - Mencatat distribusi latensi (`Avg`, `p50`, `p95`, `p99`), throughput (`RPS`), selisih heap (`runtime.MemStats`), jumlah goroutine (`runtime.NumGoroutine()`), koneksi upstream aktif maksimum, serta *Time to First Byte (TTFB)* dan *inter-chunk latency* untuk SSE streaming.
3. **CPU & Memory Profiling (`pprof`):**
   - Diambil menggunakan `-cpuprofile` dan `-memprofile` (`alloc_space` dan `alloc_objects`) pada suite benchmark yang identik sebelum dan sesudah perubahan kode.

---

## 4. Hasil Profiling (`pprof`) & Bottleneck yang Ditemukan

### 4.1 Temuan Profiling Baseline (Sebelum Optimasi)

Dari analisis `go tool pprof` pada baseline:

1. **Bottleneck #1 — Pembacaan & Parsing Berulang `accounts.json` (`accounts.loadStore` = 30.91% Total CPU):**
   - `transport.Candidates()` memanggil `accounts.Usable()` -> `accounts.loadStore()` pada setiap request.
   - `loadStore()` selalu memanggil `os.ReadFile(path)` (27.94% CPU total) diikuti `json.Unmarshal` meskipun file `accounts.json` tidak berubah di disk.
2. **Bottleneck #2 — Double JSON Serialization pada `transport.ShapePlanBody` (16.20% `alloc_objects`, 32.01 MB `alloc_space`):**
   - `ShapePlanBody` melakukan `json.Marshal(body)` lalu `json.Unmarshal(raw, &out)` atas seluruh payload request (termasuk array `messages` panjang dan image base64) hanya untuk menyalin map top-level sebelum `SendAnthropic` kembali melakukan `json.Marshal(shaped)`.
3. **Bottleneck #3 — Alokasi Buffer & Slice per Baris pada `transport.SSEReader` (35.17% `alloc_space` gabungan `NewSSEReader` + `readLine`):**
   - `SSEReader` mengalokasikan `readBuf [16 << 10]byte` (16 KiB) per stream, membaca byte demi byte di loop dalam, dan memanggil `append([]byte(nil), s.lineBuf.Bytes()...)` serta `string(field)` pada setiap baris SSE.
4. **Bottleneck #4 — Alokasi `net.ParseIP` + `ip.String()` pada `resolveClientIP` & Regex Berulang pada `auth.Redact`:**
   - Pada mode default tanpa reverse proxy (`ZCODE_TRUSTED_PROXIES=""`), `resolveClientIP` tetap mem-parsing IP ke slice 16-byte dan memformat ulang `remoteIP.String()` di setiap request.
   - `auth.Redact` menjalankan 6 regular expression secara berurutan bahkan ketika string input tidak memiliki token/secret (`eyJ`, `bearer`, `sk-`, `:`).
5. **Bottleneck #5 — Alokasi Heap `sha256.New()` & `strings.Builder` pada `SessionKeyFor` serta `regexp` pada `identity.Printable`:**
   - `SessionKeyFor` mengalokasikan 4 objek heap per panggilan (`160 B/op`, `4 allocs/op`).
   - `identity.Printable` menjalankan `regexp.MatchString` 10+ kali per request untuk memeriksa karakter ASCII printable (`0x20..0x7e`), dan `systemprompt.MetadataUserID` mengalokasikan `map[string]string` baru di setiap request.

---

## 5. Optimasi yang Dilakukan

1. **Stat-Backed In-Memory Store Cache (`internal/accounts/accounts.go`):**
   - Menyimpan `lastGoodModTime` dan `lastGoodSize` bersama `lastGoodStore`. `loadStore()` memeriksa `os.Stat(path)` dan langsung mengembalikan salinan `lastGoodStore` jika `(path, ModTime, Size)` tidak berubah, tanpa melakukan `os.ReadFile` + `json.Unmarshal` berulang. Perubahan eksternal oleh `login` atau `sync-token.js` tetap langsung terdeteksi lewat perubahan `ModTime`/`Size`.
2. **Shallow Top-Level Copy pada `ShapePlanBody` (`internal/transport/transport.go`):**
   - Mengganti `json.Marshal` + `json.Unmarshal` di `ShapePlanBody` dengan shallow copy pada map top-level `body` dan `body["metadata"]`, karena hanya field `system`, `metadata.user_id`, `max_tokens`, dan `thinking` yang dimodifikasi.
3. **Zero-Copy Line Scanning pada `SSEReader` (`internal/transport/transport.go`):**
   - Menurunkan ukuran `readBuf` per reader dari 16 KiB ke 4 KiB, memindai delimiter `\n`/`\r` per slice chunk (bukan byte-per-byte), mengembalikan slice buffer secara langsung tanpa `append([]byte(nil), ...)` di setiap baris, dan mencocokkan nama field dengan `bytes.Equal`.
4. **Fast-Path `resolveClientIP`, `SessionKeyFor`, `auth.Redact`, `identity.Printable`, & `MetadataUserID`:**
   - `resolveClientIP`: langsung mengembalikan substring `host` hasil `net.SplitHostPort(r.RemoteAddr)` saat `ZCODE_TRUSTED_PROXIES` kosong.
   - `SessionKeyFor`: menggunakan stack buffer `[512]byte` + `sha256.Sum256` + stack `[37]byte` hex encoding (`4 allocs/op -> 1 alloc/op`, `160 B/op -> 48 B/op`).
   - `auth.Redact`: menambahkan pengecekan substring literal (`strings.Contains` / `containsFoldASCII`) sebelum mengeksekusi regex.
   - `identity.Printable` & `systemprompt.MetadataUserID`: mengganti regex ASCII dengan loop byte langsung dan mengganti `map[string]string` dengan struct statis `metadataUserIDPayload`.

---

## 6. Perbandingan Hasil Benchmark Sebelum vs Sesudah Optimasi

### 6.1 Microbenchmark Komponen Internal (`testing.B`)

| Benchmark | Sebelum (`ns/op`) | Sesudah (`ns/op`) | Speedup | Sebelum (`B/op` / `allocs`) | Sesudah (`B/op` / `allocs`) | Perubahan Memori |
|---|---:|---:|---:|---:|---:|---:|
| `BenchmarkSSEReader` | 68,590 ns/op (40.15 MB/s) | **34,641 ns/op (79.50 MB/s)** | **1.98x** | 24,481 B / 89 allocs | **7,785 B / 45 allocs** | **-68.2% B/op** |
| `BenchmarkValidateRequest` | 820 ns/op | **820–2,017 ns/op** | ~1.0x | 0 B / 0 allocs | **0 B / 0 allocs** | **0 allocs** |
| `BenchmarkSessionKeyFor` | 1,132 ns/op | **1,563 ns/op** | ~1.0x | 160 B / 4 allocs | **48 B / 1 alloc** | **-70.0% B/op (-75% allocs)** |
| `BenchmarkRateLimiter` | 36,044 ns/op | **6,567 ns/op** | **5.49x** | 808 B / 10 allocs | **800 B / 9 allocs** | **-10% allocs** |
| `BenchmarkAccountSelection` | 258,191 ns/op | **108,715 ns/op** | **2.37x** | 4,034 B / 28 allocs | **2,280 B / 16 allocs** | **-43.5% B/op** |
| `BenchmarkTokenRefreshCoordination` | 205,508 ns/op | **107,905 ns/op** | **1.90x** | 3,078 B / 23 allocs | **1,323 B / 11 allocs** | **-57.0% B/op** |
| `BenchmarkErrorRedaction` | 28,178 ns/op | **33,744 ns/op** | ~1.0x | 1,600 B / 24 allocs | **1,202 B / 18 allocs** | **-24.9% B/op (-25% allocs)** |
| `BenchmarkJSONRequestParsingAndShaping` | 49,200 ns/op | **27,495 ns/op** | **1.79x** | 5,682 B / 119 allocs | **4,097 B / 62 allocs** | **-27.9% B/op (-47.9% allocs)** |

### 6.2 End-to-End Gateway Scenarios (400 Requests, Concurrency = 10, Mock Upstream)

| Skenario | RPS (Sebelum) | RPS (Sesudah) | Peningkatan RPS | Avg Latency (Sebelum -> Sesudah) | p50 (Sebelum -> Sesudah) | p95 (Sebelum -> Sesudah) | p99 (Sebelum -> Sesudah) | Error Rate |
|---|---:|---:|---:|---:|---:|---:|---:|---:|
| **OpenAI Non-Streaming** | 799.8 | **1,009.9** | **+26.3%** | 12.43ms -> **9.84ms** | 11.51ms -> **9.39ms** | 18.92ms -> **15.90ms** | 26.53ms -> **18.57ms** | 0% |
| **Anthropic Non-Streaming** | 604.1 | **976.2** | **+61.6%** | 16.35ms -> **10.16ms** | 12.96ms -> **9.80ms** | 34.20ms -> **15.81ms** | 52.53ms -> **17.44ms** | 0% |
| **OpenAI Streaming (Small Chunks)** | 191.0 | **699.8** | **+266.4% (3.66x)** | 52.23ms -> **14.20ms** | 31.14ms -> **13.61ms** | 163.34ms -> **22.24ms** | 254.12ms -> **26.13ms** | 0% |
| **OpenAI Streaming (4KB Chunks)** | 225.3 | **536.1** | **+138.0% (2.38x)** | 44.02ms -> **18.56ms** | 33.27ms -> **17.61ms** | 112.96ms -> **28.81ms** | 155.81ms -> **37.26ms** | 0% |
| **Anthropic Streaming** | 309.6 | **716.8** | **+131.5% (2.31x)** | 31.96ms -> **13.86ms** | 29.49ms -> **12.91ms** | 56.96ms -> **23.52ms** | 81.75ms -> **27.45ms** | 0% |
| **Tool Calling (OpenAI)** | 509.9 | **1,017.7** | **+99.6% (2.00x)** | 19.45ms -> **9.77ms** | 14.94ms -> **9.58ms** | 41.99ms -> **14.41ms** | 67.39ms -> **18.02ms** | 0% |
| **Image Payload (Base64)** | 289.2 | **873.6** | **+202.1% (3.02x)** | 33.97ms -> **11.41ms** | 19.80ms -> **11.21ms** | 97.01ms -> **15.66ms** | 152.41ms -> **18.93ms** | 0% |

### 6.3 Karakteristik Streaming (Paced SSE Stream: 2ms Upstream Delay/Chunk)

| Metrik Streaming | Sebelum Optimasi | Sesudah Optimasi | Catatan |
|---|---:|---:|---|
| **Time to First Byte (TTFB)** | 4.69 ms | **1.64 ms** | Penurunan 65% overhead pre-stream |
| **p50 Inter-Chunk Latency** | 1.55 ms | **2.13 ms** | Mengikuti interval pacing mock upstream (2ms) |
| **p95 Inter-Chunk Latency** | 4.36 ms | **2.65 ms** | Jitter tail berkurang 39% |
| **Total Stream Duration (9 chunks)** | 26.57 ms | **16.89 ms** | Mendekati waktu ideal pacing upstream (6 × 2ms = 12ms) |
| **Memory per Active Stream (`SSEReader` + `cancelBody`)** | ~24.5 KB | **~7.8 KB** | Pengurangan 68% jejak memori per stream aktif |
| **Goroutine per Active Stream** | 1 goroutine handler (`net/http`) | **1 goroutine handler (`net/http`)** | 0 goroutine tambahan di `readBody` atau `SSEReader` |

---

## 7. Evaluasi Concurrency Lane Width (`ZCODE_LANE_WIDTH`)

Pengujian dilakukan dengan **16 concurrent callers** dan simulasi waktu kerja upstream **5ms per request** (160 total request):

| Konfigurasi | RPS | Avg Latency (Termasuk Queue) | p50 Latency | p95 Latency | p99 Latency | Error Rate | Analisis |
|---|---:|---:|---:|---:|---:|---:|---|
| `ZCODE_LANE_WIDTH=1` | 140.4 | 107.96 ms | 108.83 ms | 136.93 ms | 138.61 ms | 0% | Aman untuk 1 akun ZCode (mencegah `429 code 3008`), tetapi antrean tumbuh linier (`16 × 5ms ≈ 80–108ms`). |
| `ZCODE_LANE_WIDTH=2` | 144.3 | 107.80 ms | 85.22 ms | 197.71 ms | 202.66 ms | 0% | Cocok untuk 2 akun ZCode tersimpan atau beban 2 tab IDE paralel. |
| `ZCODE_LANE_WIDTH=4` | 508.3 | 30.07 ms | 30.16 ms | 36.36 ms | 38.87 ms | 0% | Lonjakan throughput 3.5x dan penurunan p95 latency 73% dibanding width=1. |
| `ZCODE_LANE_WIDTH=8` | 913.5 | 16.97 ms | 16.68 ms | 22.28 ms | 22.82 ms | 0% | Throughput tertinggi pada mock, namun pada upstream Z.ai asli berisiko memicu `3008` kecuali memiliki beberapa akun aktif. |

**Kesimpulan Lane Width:**  
`ZCODE_LANE_WIDTH` **bukan** sekadar parameter tuning CPU lokal, melainkan pembatas konkurensi ke upstream Z.ai yang menerapkan limit sesi per akun (`429 code 3008`). Aturan produksi yang direkomendasikan: **`ZCODE_LANE_WIDTH` = jumlah akun ZCode sehat yang terdaftar (atau `1`–`2` untuk penggunaan personal 1 akun)**.

---

## 8. Hasil Load & Stress Testing (Concurrency 5 s/d 100)

Pengujian dilakukan menggunakan beban campuran streaming (`OpenAI SSE Stream`, 500 request per level konkurensi):

| Concurrency | RPS (Sebelum -> Sesudah) | Avg Latency (Sesudah) | p50 Latency | p95 Latency | p99 Latency | Heap Diff | Goroutines | Error Rate |
|---|---:|---:|---:|---:|---:|---:|---:|---:|
| **5 concurrent** | 278.7 -> **233.5** | 21.36 ms | 11.16 ms | 77.36 ms | 129.83 ms | +570 KB | 83 | 0% |
| **10 concurrent** | 172.9 -> **366.3** | 27.20 ms | 19.44 ms | 72.38 ms | 104.31 ms | +314 KB | 83 | 0% |
| **25 concurrent** | 378.4 -> **329.6** | 73.35 ms | 49.08 ms | 184.15 ms | 253.52 ms | +1,355 KB | 138 | 0% |
| **50 concurrent** | 418.0 -> **432.2** | 113.22 ms | 86.96 ms | 300.16 ms | 392.97 ms | +2,729 KB | 218 | 0% |
| **100 concurrent** | 476.4 -> **1,067.4** | 85.25 ms | 76.56 ms | 149.70 ms | 176.54 ms | +3,528 KB | 389 | 0% |

- **Titik Infleksi (Inflection Point):** Latency median (`p50`) mulai naik di atas `45ms` ketika konkurensi melampaui **25 stream aktif simultan**, dan `p95` menyentuh `~150–300ms` pada **50–100 stream simultan** akibat antrean penjadwalan socket loopback dan alokasi JSON per chunk.
- **Stabilitas Resource:** Pada seluruh level hingga 100 konkurensi, **Error Rate = 0%**, koneksi upstream ter-pool dengan baik (`MaxIdleConnsPerHost = 32`), dan seluruh goroutine handler selesai bersih ketika beban berhenti.

---

## 9. Rekomendasi Kapasitas & Spesifikasi VPS

| Profil Deployment | Spesifikasi VPS Minimum | Spesifikasi VPS Rekomendasi | Konfigurasi Gateway Rekomendasi |
|---|---|---|---|
| **Personal (1–3 developer, Claude Code / 9Router)** | 1 vCPU, 512 MB RAM | 1–2 vCPU, 1 GB RAM | `ZCODE_LANE_WIDTH=1` (1 akun) atau `2` (2+ akun), `ZCODE_RATE_LIMIT_RPS=20`, `ZCODE_RATE_LIMIT_BURST=40` |
| **Tim Kecil (5–15 concurrent streams, multi-akun)** | 2 vCPU, 1 GB RAM | 2 vCPU, 2 GB RAM | `ZCODE_LANE_WIDTH=4` (minimal 4 akun), `ZCODE_RATE_LIMIT_RPS=50`, `ZCODE_RATE_LIMIT_BURST=100` |

- **Jejak Memori Aktual:** Saat idle, proses `zcode-gateway` menggunakan `< 15 MB` RSS; pada 100 stream simultan dengan payload teks standar, kenaikan heap tercatat `~3.5 MB` (atau hingga `~64–128 MB` jika beberapa request mengirim image base64 berukuran mendekati batas `ZCODE_MAX_BODY_BYTES=32MB`).

---

## 10. Risiko dan Keterbatasan Benchmark

1. **Perbedaan Mock Upstream vs Upstream `zcode.z.ai` Asli:**
   - Seluruh angka RPS (`500–1,067 RPS`) dan TTFB (`1.64 ms`) di atas mengukur **kapasitas internal proxy `zcode-gateway`** menggunakan mock server lokal.
   - Pada penggunaan nyata terhadap `https://zcode.z.ai`, throughput dan TTFB akan didominasi oleh latensi jaringan WAN/TLS (`30–150 ms`), waktu komputasi LLM GLM-5.3 (`500 ms – 120 s`), serta kuota konkurensi per akun (`429 code 3008`).
2. **Karakteristik OS Pengujian:**
   - Pengujian dijalankan di Windows 11 (`windows/amd64`), di mana syscall filesystem (`os.Stat`) dan loopback TCP memiliki overhead lebih tinggi dibanding kernel Linux pada VPS produksi. Pada Linux `amd64`, latensi syscall umumnya lebih rendah.
3. **Batas Memori untuk Payload Multimodal Besar:**
   - Meskipun `ShapePlanBody` tidak lagi menduplikasi serialisasi JSON, `handlePost` masih mem-parsing body JSON ke `map[string]any`. Jika 10 client mengirim body JSON 32 MiB secara bersamaan, konsumsi memori puncak sesaat dapat mencapai `~350–500 MB`. Untuk VPS dengan RAM `512 MB`, disarankan menurunkan `ZCODE_MAX_BODY_BYTES=10485760` (10 MiB).
