# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

Refity is a Docker private registry with SFTP backend storage. It stores image blobs and manifests on any SFTP server (e.g. Hetzner Storage Box) while exposing the standard Docker Registry HTTP API v2 for `docker push`/`docker pull`. It includes a web UI with JWT-authenticated REST API for managing repositories and groups.

## Build & Run Commands

### Full stack (Docker)
```sh
cp .env.example .env   # fill in SFTP credentials
docker-compose up -d   # backend :5000, frontend :8080
```

### Backend (Go)
```sh
cd backend
go build -o server ./cmd/server        # build
go run ./cmd/server                     # run (requires .env vars exported)
go test ./...                           # run all tests
go test ./internal/registry/...         # run tests for a single package
```
Requires CGO (sqlite3 driver): `CGO_ENABLED=1`. Alpine builds need `gcc musl-dev`.

### Frontend (React/Vite)
```sh
cd frontend
npm install
npm run dev        # dev server on :8080, proxies /api to localhost:5000
npm run build      # production build to dist/
```

## Architecture

### Two-router backend
The Go backend (`cmd/server/main.go`) runs a single HTTP server on port 5000 with two separate routers multiplexed by path prefix:

- **Registry router** (`internal/registry/`) — implements Docker Registry HTTP API v2 at `/v2/*`. No authentication (designed to be protected by a reverse proxy). Handles blob uploads (chunked/monolithic with resumable session tokens), manifest PUT/GET/DELETE, and tag listing.
- **API router** (`internal/api/`) — REST API at `/api/*` for the web UI. JWT-protected. Provides dashboard stats, repository/group CRUD, auth endpoints, and optional Hetzner FTP usage stats.

### Storage layer
Two storage drivers implement the same interface:
- **Local driver** (`internal/driver/local/`) — filesystem staging area (`/tmp/refity`). Used for blob upload assembly before SFTP transfer.
- **SFTP driver** (`internal/driver/sftp/`) — connection-pooled SSH/SFTP client. One 4-connection pool serves both the registry and the web API. Pipelined reads/writes, multi-connection parallel upload for large blobs, atomic temp+rename uploads, and a stall watchdog (`SFTP_STALL_TIMEOUT`) that kills connections that stop making progress.
- **Upload spool** (`internal/spool/`) — durable on-disk queue (`<data>/spool`, one `.data` + `.job` pair per pending upload). Background workers retry with exponential backoff until the upload succeeds; jobs resume after restart.
- **Read cache** (`internal/readcache/`) — LRU disk cache (`<data>/cache`) of digest-verified blobs and digest-addressed manifests; concurrent cold reads share one remote download.

Blob upload flow: client PATCHes chunks to local staging → on final PUT the staged file is hashed (streamed, never loaded into memory) and verified → async mode: moved into the spool and `201` returned immediately, workers upload it; sync mode (`SFTP_SYNC_UPLOAD=true`) or spool over `SPOOL_MAX_BYTES`: uploaded before responding → metadata recorded in SQLite.

Read flow (blob/manifest GET/HEAD): spool → read cache → SFTP. Remote blobs are streamed (headers first, `Range` supported) and resumed from the current offset if a connection stalls.

Why parallel connections: x/crypto/ssh uses a fixed 2 MiB channel window, so one SSH connection is capped near window/RTT (about 1 MB/s at 0.2–0.5 s RTT). Multiple connections are the only throughput lever.

Connection budget: Hetzner allows up to 10 simultaneous connections per Storage Box account. Refity uses 4 (one pool shared by registry and web API); keep other tools (backup jobs, manual `sftp` sessions) within the remaining 6. Uploads and downloads together hold at most pool size − 1 connections (one shared budget), so metadata calls (HEAD, Stat, folder creation) always find one free. Consequence: downloads can queue behind upload parts (and vice versa) while a large push is being uploaded; a queued download simply starts later.

Outages: the pool reconnects in the background forever (backoff 5s → 5min per slot); while no connection is available, registry calls fail after `SFTP_ACQUIRE_TIMEOUT` with 503 (Docker retries) instead of hanging. After 3 consecutive SSH authentication failures (or the very first one at startup, before any login worked) all dials pause for 5 minutes. When a pause ends exactly one probe login goes out; the other refills wait for it. A rejected probe doubles the pause up to a cap (5 → 10 → 15 → 15 min with the default 15 min cap; `PoolOptions.AuthPauseMax`); a successful one resumes normal reconnects. A caller that gives up while a connection's health check is still running never costs that connection; only a failed check discards it. Reason: this is generic resilience for a genuine auth failure or outage, not a description of an observed block. Hetzner Storage Boxes are reported by the community to throttle repeated failed password logins (fail2ban style; duration undocumented, not verified by us), so a burst of failed logins from every pool slot is avoided; starting without SFTP (instead of crash-looping under `restart: unless-stopped`) follows from the same rule.

Debugging logins by hand: `sftp -b` implies `BatchMode=yes`, which disables password authentication and fails with "Permission denied" even with correct credentials. Use `sftp -o BatchMode=no ...` to test a password login.

Restarts are survivable without SFTP: the process always starts, even when no login works (the pool starts empty and fills in the background; only bad configuration such as missing FTP settings or unusable directories is fatal). Behaviour while SFTP logins fail or the Storage Box is unreachable: pushes in async mode still succeed (spooled locally, uploaded once the remote is back); pulls of anything in the spool or read cache are served locally and tag lists show spooled tags; everything else (blobs/manifests only on the remote, tag lists with nothing spooled, synchronous uploads) answers 503 `UNAVAILABLE` after `SFTP_ACQUIRE_TIMEOUT`, which Docker treats as retryable. Watch the `spool` block in the dashboard API (`pending`, `oldest_age_seconds`, `last_error`) and the `WARN [SPOOL]` log line emitted every minute once the oldest pending upload is older than 10 minutes.

Startup log: the backend logs either `Server supports posix-rename@openssh.com: uploads replace files atomically` or `Server lacks posix-rename@openssh.com: uploads use remove+rename after the new temp file is verified (old version stays until then)`. In the second case a tag manifest is briefly absent between the remove and the rename (never partial).

Container shutdown: on SIGTERM the server drains HTTP requests for up to 30 s, then stops the upload workers (pending jobs stay on disk and resume on the next start). Docker's default grace period is 10 s, so set `stop_grace_period: 45s` on the backend service, otherwise in-flight pushes are killed mid-drain.

Sync mode caveat: with `SFTP_SYNC_UPLOAD=true` the request body is staged locally first and uploaded after the digest check (the old direct body-to-SFTP stream could not be retried on another connection). The final PUT therefore waits for the whole remote upload with no bytes flowing to the client, which can exceed an upstream proxy's response timeout (e.g. Cloudflare's 100 s) for large layers. Prefer the default async mode behind such proxies.

Healing corrupt blobs: before the spool, the sync streaming path wrote blobs straight to their final remote name and did not remove the file when digest verification failed afterwards, so blobs written that way can exist with the right size and wrong content. The "same size means already uploaded" shortcut would then trust that corrupt copy forever. Three things now deal with this:

- **On the upload path.** Size alone is not proof, so before skipping an upload because the remote already holds a file of that size, the blob is verified against the digest its path is named after (`remoteBlobIsCorrect`). Anything short of a confirmed match means write — including a blob too large to re-read here (`healVerifyMaxBytes`) and a remote that could not be read, since neither is evidence that what is stored is what the client just pushed. Without this a corrupt same-size copy survived every push, and the moment the heal or the audit removed it, the client was left holding a 201 for bytes that no longer existed anywhere. Verification answers in three states (`verifyResult`: matches / mismatch / unchecked), because "could not check" means different things to the two callers: the heal leaves it for the audit, the upload path writes it. The cost is a full download of the blob on a re-push, avoided entirely when a digest-verified copy is already in the read cache (`cachedBlobMatches`), which is the common case for the unchanged layers a rebuild re-sends. That short-circuit trusts the local copy and never looks at the remote, so damage that appears on the remote behind the cache's back is not noticed by this path until a read verifies it again and the heal or the audit removes it; the read path is the backstop for that, not the upload path. Only the read cache counts, not the spool: a spool job is an upload that has not happened yet, and treating it as proof would make every first push skip itself.
- **On the read path.** The read cache hashes every object it fills and reports a mismatch as `readcache.ErrCorrupt`. `serveBlob` reacts by deleting the remote file. Guards: only digest-named (immutable) paths are ever deleted; the digest argument must equal the path's base name (so a mis-wired call site fails closed instead of deleting a good file); and the bytes at the path are hashed again under the same per-path lock the writers hold across a whole upload, because a pull's fill keeps an open handle to the file it started reading and a concurrent push can leave it hashing bytes that are already unlinked. A file that has since become correct is left alone, and a failed re-check simply defers to a later pull. Re-verification is capped at `healVerifyMaxBytes` (64 MiB) and `healVerifyTimeout` (2 min); above the cap, or when the read cannot be completed, the heal leaves the blob alone rather than delete something it could not read — so large layers are the audit's job, and a storage blip never reads as corruption. The re-read uses `ReaderWithDeadline`, which puts a transport deadline on the connection and takes a slot from the shared long-operation budget, so a slow link ends the attempt and background verification cannot starve live traffic. The re-read uses `ReaderWithDeadline`, which puts a transport deadline on the connection for as long as the reader is open, so a slow or dead link ends the attempt instead of holding the lock — the stall watchdog is no longer the only backstop.
- **Removed paths are marked** (`corrupt` map in `storage.go`, persisted to `<spool>/corrupt-paths` so a restart cannot re-arm the size-trust bug). `markCorrupt` only accepts a digest-named path inside a `blobs/` directory (`isCorruptMarkable`) — `isDigestPath` matches on the base name alone and would also accept a `manifests/<digest>` copy, which no caller marks and which must never be able to pin an unrelated re-upload. A re-push of a marked digest is actually written even though its size matches the corrupt copy that was there, and the mark is cleared once that path is uploaded through the normal path (`retireLocal`, reached by every upload mode) — a blob restored by other means (a manual copy, say) keeps its mark until it is pushed once. Deleting a repository clears its marks, since the remote folder is going away and a surviving mark would follow the name into its next life. Note what the mark is for: it makes a re-push correct, it does not restore the blob by itself. After a heal removes a corrupt copy, a client that merely pulls gets a 404 and only a re-push (or a rebuild that pushes it) brings it back. So the damage is repaired but the image is not usable again until then, and the dashboard reports those outstanding marks in `corrupt` (`removed`, sorted `paths` capped at 50, `oldest_age_seconds`), with a periodic `WARN [CORRUPT]` log line.

`POST /api/maintenance/audit-blobs` (admin) walks every digest-named blob sitting directly in a repository's `blobs/` directory and removes mismatches. It is how damage is found for blobs nobody has pulled since, and for blobs over the read-path size cap. It never audits `manifests/`: a manifest stored in Docker v2 form is deliberately not byte-identical to the digest it is stored under (it is rewritten to OCI on read), so auditing one would delete valid data. A repository is recognised structurally — a `blobs` directory counts only when its parent also holds a `manifests` directory — because no depth or path-substring rule works: a repository is one or two segments below the root depending on whether it has a group, `CreateRepositoryFolder` accepts arbitrary names, and a group or repository literally named `blobs` is otherwise indistinguishable from the blobs directory itself. (An earlier substring rule let a repo-scoped `remove=true` run delete a sibling repository's manifest.) The walk consequently descends into names it has not recognised yet, so it does ask the server to list tag manifests and gets "not a directory" back; those are expected and are not counted in `failed` (only genuinely unreadable directories are). `?repository=group/repo` limits the walk to that repository; the name is validated with `validateRepoName`, because it becomes a path the walk deletes from and `..` would otherwise reach outside `registry/`, `?remove=true` performs the deletion (the default is report-only), and `?timeout=…` bounds the run (default 2 h; it stops cleanly and reports `timed_out`). Removal takes the same per-path lock and re-confirms the damage first, so a blob pushed while the audit is walking is never deleted — but blobs that appear after the directory listing are not scanned in that run, and each directory is listed once, so a path reachable more than one way is not counted twice. Hashing one blob is bounded by `auditHashTimeout` (5 min) with the same transport deadline as the heal, so a single slow blob cannot sit on a path lock for the whole run. Blobs over `auditMaxBytes` (1 GiB) are reported as `too_large` rather than streamed. Run it when the box is idle: it is not on any push or pull path.

Repository delete (`DELETE /api/repositories/{repo}`) deletes the DB rows first; only if that succeeds it purges the repo's pending uploads, cached objects and any corrupt marks (`registry.PurgeRepo`) and then deletes the remote folder. Marks are dropped there because the remote folder is going away, so a mark has nothing left to guard against and would otherwise follow the repository name into its next life. Remote `*.uploading-*` temp files older than 24 h (crashed uploads) are swept at startup.

Superseded tag versions: re-pushing a tag while the previous version is still uploading cancels the older job (`supersedeLocked` cancels an in-flight attempt) so it is never committed after the newer one. The spool's queue order is `Job.Seq`, not `CreatedAt`, so a clock that steps backwards cannot reorder recovered jobs; `Seq` only matters on recovery from disk, which the spool package covers directly.

### Database
SQLite via `mattn/go-sqlite3`. Schema auto-created on startup in `internal/database/database.go`. Tables: users, images, repositories, layers, manifests, groups. Default admin user `admin:admin` is seeded on first run.

### Frontend
React 18 SPA with Vite. Routes defined in `src/App.jsx`. API client in `src/services/api.js` (Axios with JWT interceptor). Styling via Bootstrap 5 CDN. In production, Nginx serves the SPA and proxies `/api/*` and `/v2/*` to the backend (template in `nginx.conf.template`, substituted by `docker-entrypoint.sh`).

### Registry protocol details
- Chunked blob uploads use HMAC-signed state tokens in `Location` headers (no server-side session storage).
- Manifest lists (multi-arch / OCI index) are supported — layer sizes are aggregated from child manifests.
- The registry sends `100 Continue` for expects and supports nginx chunked transfer encoding.

## Key Environment Variables

| Variable | Required | Default | Notes |
|---|---|---|---|
| FTP_HOST, FTP_PORT, FTP_USERNAME, FTP_PASSWORD | Yes | — | SFTP server credentials |
| JWT_SECRET | Production | dev fallback | 32+ char random string |
| CORS_ORIGINS | No | localhost:8080 | Comma-separated allowed origins |
| PORT | No | 5000 | Backend listen port |
| SFTP_SYNC_UPLOAD | No | false | Wait for SFTP before responding (otherwise spool + background upload) |
| SPOOL_DIR | No | `<data>/spool` | Durable local copies of pushed blobs/manifests awaiting upload. Must not overlap READ_CACHE_DIR, and neither may *contain* the data dir that holds `refity.db` (they may live inside it, which is the default). Startup refuses otherwise |
| SPOOL_MAX_BYTES | No | 20GiB | Pending spool bytes before commits fall back to sync upload (0 = unlimited) |
| UPLOAD_WORKERS | No | 4 | Background upload workers (capped at SFTP pool size) |
| READ_CACHE_DIR | No | `<data>/cache` | Read-through cache of pulled blobs |
| READ_CACHE_BYTES | No | 5GiB | Read cache budget, LRU eviction (0 = disabled) |
| SFTP_STALL_TIMEOUT | No | 60s | Abort and replace an SFTP connection with no progress for this long (0 = off) |
| SFTP_PARALLEL_THRESHOLD | No | 8MiB | Blobs at least this size upload over several connections (0 = off) |
| SFTP_ACQUIRE_TIMEOUT | No | 30s | Max wait for a pooled SFTP connection; afterwards the call fails (503 to clients) |
| STREAM_WRITE_TIMEOUT | No | 5m | Abort a blob download whose client accepts no data for this long (frees its SFTP connection) |
| SPOOL_MIN_FREE_BYTES | No | 2GiB | Below this free space on the spool volume (shared with refity.db), commits upload synchronously |
| BACKEND_UPSTREAM | No | backend:5000 | Frontend container → backend address |

## CI/CD

GitHub Actions (`.github/workflows/docker-publish.yml`) builds and pushes `troke12/refity-backend` and `troke12/refity-frontend` to Docker Hub on `v*` tags.
