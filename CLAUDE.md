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
- **Local driver** (`internal/driver/local/`) — filesystem staging area (`/app/data` in container, `/tmp/refity` locally). Used for blob upload assembly before SFTP transfer.
- **SFTP driver** (`internal/driver/sftp/`) — connection-pooled SSH/SFTP client (4 concurrent connections). Uploads are async by default (configurable via `SFTP_SYNC_UPLOAD=true`).

Blob upload flow: client PATCHes chunks to local staging → on final PUT the complete blob is verified (digest) → blob is transferred to SFTP → metadata recorded in SQLite.

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
| SFTP_SYNC_UPLOAD | No | false | Wait for SFTP before responding |
| BACKEND_UPSTREAM | No | backend:5000 | Frontend container → backend address |

## CI/CD

GitHub Actions (`.github/workflows/docker-publish.yml`) builds and pushes `troke12/refity-backend` and `troke12/refity-frontend` to Docker Hub on `v*` tags.
