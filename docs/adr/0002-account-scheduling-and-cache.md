# ADR 0002: Account Scheduling Architecture & In-Memory Store Cache

- **Status:** Accepted
- **Tanggal:** 2026-04-16
- **Konteks:** Phase 6.1 (`feature/account-scheduling-cache-p6.1`)
- **Terkait:** `REL-01`, `REL-06`, `REL-07`, `SEC-04`

---

## 1. Alur Request End-to-End (Request Reception -> Account Selection -> Upstream Execution)

```text
[Client HTTP POST /v1/chat/completions atau /v1/messages]
   │
   ├─► 1.server.route()
   │     ├─ validHostHeader(), cors(), allowRequest() [Rate Limiter]
   │     └─ requireAuth() [Constant-Time ZCODE_BRIDGE_TOKEN check]
   │
   ├─► 2. server.handlePost()
   │     ├─ readBody() [http.MaxBytesReader + SetReadDeadline(BodyReadTimeout) -> reset deadline]
   │     └─ models.ValidateRequest(body, dialect)
   │
   ├─► 3. transport.SendAnthropic(SendOpts)
   │     ├─ Pre-flight credential check: Candidates(o.JWT, o.AccountID) [In-Memory Cache]
   │     ├─ acquireLane(ctx) ──► [Global Process Semaphore: ZCODE_LANE_WIDTH, default 1]
   │     │
   │     └─ Loop Rotasi Kandidat Akun (tried map[string]bool):
   │          ├─ accounts.AcquireCandidate(ctx, o.AccountID, tried)
   │          │    ├─ Filter: skip excluded, NeedsReauth, cooledDown(now), expired(now)
   │          │    ├─ Urutkan: (1) inFlight ASC, (2) seq ASC [Round-Robin], (3) isDefault DESC
   │          │    ├─ Pilih akun pertama dengan st.inFlight < ZCODE_ACCOUNT_CONCURRENCY (default 1)
   │          │    │    └─ Jika semua akun sehat sedang penuh (inFlight >= limit),
   │          │    │       tunggu pada channel slotWaiters atau ctx.Done()
   │          │    └─ st.inFlight++, selectSeq++, st.seq = selectSeq
   │          │
   │          ├─ runAccount(ctx, client, acct, ...)
   │          │    ├─ ShapePlanBody() + identity.BuildLLMHeaders()
   │          │    ├─ HTTP POST ke Z.ai dengan hdrTimer (HeaderTimeout)
   │          │    ├─ Jika 401 & ada RefreshToken:
   │          │    │    └─ accounts.RefreshAccountSingleflight(ctx, acct.ID, staleJWT)
   │          │    └─ Jika 3008 & tidak ada akun lain (remaining == 0):
   │          │         └─ Retry in-place dengan linear backoff (attempt * 600ms)
   │          │
   │          ├─ Jika runAccount SUKSES (HTTP 200):
   │          │    ├─ accounts.ClearCooldown(acct.ID) [Fast in-memory check jika NeedsReauth == false]
   │          │    ├─ Bungkus res.Body dalam cancelBody{release: releaseAcct() + releaseLane()}
   │          │    └─ Return res ke pipeStream() / JSON reader
   │          │         └─ Saat stream selesai / client disconnect / idle timeout:
   │          │            cancelBody.Close() ──► sync.Once: releaseAcct() lalu releaseLane()
   │          │
   │          └─ Jika runAccount GAGAL:
   │               ├─ releaseAcct() SEGERA dipanggil (st.inFlight-- & bangunkan slotWaiters)
   │               ├─ Jika 401/403: accounts.MarkReauth(acct.ID)
   │               ├─ Jika 3008 & multi-akun: accounts.MarkCooldown(acct.ID, 5s)
   │               ├─ Jika 1113 / 429 kuota: accounts.MarkCooldown(acct.ID, 15m)
   │               └─ Lanjut ke iterasi berikutnya (rotasi ke akun sehat lain)
```

---

## 2. Evaluasi Arsitektur Penjadwalan Akun: Opsi A vs Opsi B

- **Opsi A:** *Stateless Round-Robin Account Selection* (tanpa semaphore per akun).
- **Opsi B (Dipilih):** *Per-Account Concurrency Semaphore (`ZCODE_ACCOUNT_CONCURRENCY`, default `1`) + Least-InFlight Round-Robin Scheduler + Global Lane Semaphore (`ZCODE_LANE_WIDTH`)*.

| Dimensi Evaluasi | Opsi A: Stateless Round-Robin | Opsi B: Per-Account Semaphore + Least-InFlight Round-Robin + Global Lane |
|---|---|---|
| **1. Fairness** | Adil hanya jika durasi setiap request identik. Jika Request #1 adalah stream 60 detik di Akun A dan Request #2 adalah request 1 detik di Akun B, maka Request #3 akan kembali memilih Akun A yang masih sibuk. | **Sangat adil.** Mengurutkan berdasarkan `(inFlight ASC, seq ASC, isDefault DESC)`. Akun idle (`inFlight == 0`) selalu diprioritaskan; jika sama-sama idle, giliran berpindah secara round-robin (`seq ASC`). |
| **2. Throughput** | Menurun saat beberapa request paralel jatuh ke akun yang sama dan ditolak Z.ai dengan `429 code 3008`. | **Optimal.** `K` akun sehat dapat melayani `K` stream paralel tanpa tabrakan `3008` internal. |
| **3. Queue Latency** | Membengkak akibat round-trip sia-sia ke upstream yang menghasilkan `3008` + backoff retry (`600ms * attempt`). | **Minimal.** Jika semua akun sedang mencapai batas konkurensi (`inFlight == limit`), request mengantre secara lokal di memori (`slotWaiters`) dan langsung dieksekusi pada mikrodetik saat stream sebelumnya selesai. |
| **4. Account Utilization** | Pincang pada beban durasi campuran (stream panjang vs chat pendek). | **Merata.** Setiap akun yang bebas langsung menyerap request berikutnya. |
| **5. Cooldown Behavior** | Akun cooldown dilewati, namun setelah cooldown berakhir dapat menerima burst paralel jika `ZCODE_LANE_WIDTH > 1`. | Akun cooldown dilewati (`cooledDown(now)`), dan saat `MarkCooldown` dipanggil, `notifySlotWaitersLocked()` membangunkan waiter. Saat pulih, akun hanya menerima maksimal `ZCODE_ACCOUNT_CONCURRENCY` request aktif. |
| **6. Token Refresh** | Dua request paralel yang memilih akun yang sama dapat memicu dua percobaan 401 bersamaan. | Dengan default `ZCODE_ACCOUNT_CONCURRENCY=1`, hanya 1 request yang memegang slot akun saat terjadi 401; jika `ZCODE_ACCOUNT_CONCURRENCY > 1`, `RefreshAccountSingleflight` tetap menggabungkan refresh per akun. |
| **7. Upstream 3008 Handling** | Sering memicu `3008` karena gateway sendiri mengirim `>1` request paralel ke akun yang sama saat `ZCODE_LANE_WIDTH > jumlah_akun`. | Mencegah `3008` yang berasal dari gateway sendiri. `3008` hanya terjadi jika aplikasi eksternal (ZCode Desktop IDE) memakai akun yang sama di luar gateway, yang ditangani dengan rotasi instan (multi-akun) atau retry in-place (single-akun). |
| **8. Deadlock Risk** | Rendah (tanpa kunci per akun). | **Bebas circular-wait.** Akuisisi semaphore selalu satu arah (`Global Lane -> Satu Account Slot`), dan pelepasan dijamin oleh `sync.Once` pada `cancelBody.Close()` atau saat error. Namun, kebebasan deadlock tetap bergantung pada kebenaran penanganan context cancellation dan pelepasan body oleh caller. |
| **9. Context Cancellation** | Hanya membatalkan di global lane atau HTTP call. | Baik `acquireLane(ctx)` maupun `AcquireCandidate(ctx, ...)` memilih `select { case <-waitCh: case <-ctx.Done(): }`, dan `cancelBody.Close()` melepas kedua semaphore via `sync.Once`. |

---

## 3. Pinned-Account Head-of-Line (HoL) Blocking & Panduan Operasional

### 3.1 Skenario HoL Terbukti Secara Empiris (`TestExtendedHeadOfLineBlockingWithPinnedRequestHoldingLane`)
Pada Phase 7.1, skenario HoL terbukti dapat direproduksi dengan konfigurasi:
- `ZCODE_ACCOUNT_CONCURRENCY = 1` (Akun A & B)
- `ZCODE_LANE_WIDTH = 2` (sama dengan jumlah akun)
- Urutan request:
  1. R1 menggunakan Akun A (stream panjang 120ms).
  2. R2 di-pin ke Akun A (`?account=a`). R2 mengambil Global Lane Slot 2, tetapi terblokir di `AcquireCandidate` menunggu Akun A.
  3. R3 bersifat unpinned (bisa memakai Akun A atau B). Akun B saat itu 100% idle (`inFlight == 0`).
  4. Akibat R2 menahan Global Lane Slot 2, R3 tertahan di antrean global lane selama **`118.9 ms`** meskipun Akun B menganggur.

### 3.2 Mengapa Opsi Alternatif Ditolak
1. **Opsi B (Account Reservation -> Global Lane):** Ditolak karena memindahkan bottleneck. Request yang memesan akun akan menahan kapasitas akun tersebut saat mengantre di global lane, memblokir request lain yang memerlukan akun itu meski global lane sedang sibuk.
2. **Opsi C (Coordinated Admission Scheduler):** Ditolak karena kompleksitas berlebih (centralized orchestrator goroutine, multiple channels per request, dan overhead CPU) yang bertentangan dengan prinsip stdlib minimalis dan keandalan dasar.

### 3.3 Panduan Operasional Penjadwalan Akun
- **Rekomendasi Utama (Unpinned Routing):** Gunakan routing tanpa pinning (`?account=` dikosongkan). Gateway akan mendistribusikan request secara otomatis menggunakan *Least-InFlight Round-Robin*, menjamin akun yang menganggur selalu dipilih lebih dahulu.
- **Keterbatasan Pinning Akun:** Jika client harus menggunakan pinning akun (`?account=id`), pastikan konkurensi request untuk akun tersebut tidak melebihi `ZCODE_ACCOUNT_CONCURRENCY` agar tidak menahan slot lane global dari request akun lain.
- **Klarifikasi Kapasitas:** Menyetel `ZCODE_LANE_WIDTH >= jumlah_akun * ZCODE_ACCOUNT_CONCURRENCY` **hanya** mengeliminasi HoL pada request unpinned; HoL tetap dapat terjadi jika ada request pinned ke akun yang jenuh.

---

## 4. Desain Optimasi Cache `accounts.json` & Strategi Invalidasi

Untuk mengatasi bottleneck pembacaan disk (`os.ReadFile` + `json.Unmarshal`) pada `accounts.loadStore()` tanpa mengorbankan proteksi file korup (`SEC-04`) atau deteksi perubahan eksternal:

1. **In-Memory Cache (`lastGoodStore`, `lastGoodRaw`, `lastGoodModTime`, `lastGoodSize`):**
   - Setiap mutasi internal (`UpsertAccount`, `Remove`, `SetDefault`, `MarkReauth`, `ClearCooldown`, `RefreshAccountSingleflight`, `RecoverFromBackup`) menulis secara atomik melalui `writeStore(rec)` dan langsung memperbarui `lastGoodStore`, `lastGoodRaw`, `lastGoodModTime`, `lastGoodSize`, serta mencatat timestamp `lastWriteAt = time.Now()`.
2. **Tiga Jalur Validasi pada `loadStore()` (di bawah `accounts.mu`):**
   - **Jalur Cepat 1 (Steady-State Burst Suppression, `< 5ms` sejak stat terakhir & `> 25ms` sejak write terakhir):** Mengembalikan salinan `lastGoodStore` langsung dari RAM dengan **0 syscall (`0` `os.Stat`, `0` `os.ReadFile`, `0` `json.Unmarshal`)**.
   - **Jalur Cepat 2 (Validasi Metadata `os.Stat`, `> 25ms` sejak write terakhir):** Memeriksa `os.Stat(path)`. Jika `info.ModTime().Equal(lastGoodModTime) && info.Size() == lastGoodSize`, memperbarui `lastStatAt = now` dan mengembalikan `lastGoodStore` tanpa membaca isi file (`0` `os.ReadFile`, `0` `json.Unmarshal`).
   - **Jalur Penuh (Dalam jendela `25ms` pasca-write atau saat `ModTime`/`Size` berubah):** Membaca `os.ReadFile(path)` dan membandingkan `bytes.Equal(raw, lastGoodRaw)`. Hal ini menjamin bahwa modifikasi eksternal yang terjadi pada millisecond yang sama dengan ukuran byte yang persis sama (misal pada unit test) tetap terdeteksi 100%, sementara file korup tetap memicu `ErrCorruptStore` dan pencadangan `.corrupt.<timestamp>.<hash8>`.
3. **Fast-Path `ClearCooldown(id)`:**
   - Pada setiap request yang sukses (`200 OK`), `ClearCooldown(id)` memeriksa di memori apakah akun memiliki flag `NeedsReauth == true`. Jika `false` (kondisi normal), fungsi langsung keluar tanpa memanggil `loadStore()` atau menyentuh disk.
