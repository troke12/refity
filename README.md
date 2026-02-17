# Refity

Docker private registry with **SFTP backend storage**.

Refity stores image blobs and manifests on any SFTP server (e.g. Hetzner Storage Box) while keeping the standard Docker Registry v2 API (`docker push` / `docker pull`).

![Dashboard](screenshot/dashboard.png)

## Documentation

- Docs: `https://troke.id/refity/docs.html`
- Why SFTP (comparison): `https://troke.id/refity/compare.html`
- Features: `https://troke.id/refity/features.html`

## Features (high level)

- Docker Registry HTTP API v2 (`/v2/*`)
- SFTP storage backend
- Web UI + REST API (`/api/*`) with JWT auth
- Multi-arch support (manifest lists)
- Async upload (optional sync mode)

## Quick start

```sh
git clone https://github.com/troke12/refity.git
cd refity
cp .env.example .env
docker-compose up -d
```

Open:

- Web UI: `http://localhost:8080`
- Backend API: `http://localhost:5000`

Default user:

- Username: `admin`
- Password: `admin`

Change it after first login.

## License

MIT. See `LICENSE`.

