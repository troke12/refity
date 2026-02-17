# Refity

Docker private registry with **SFTP backend storage**.

Refity stores image blobs and manifests on any SFTP server (e.g. Hetzner Storage Box) while keeping the standard Docker Registry v2 API (`docker push` / `docker pull`).

![Dashboard](screenshot/dashboard.png)

<p align="center">
  <a href="https://hub.docker.com/r/troke12/refity-backend"><img alt="Docker pulls (backend)" src="https://img.shields.io/docker/pulls/troke12/refity-backend?style=flat-square"></a>
  <a href="https://hub.docker.com/r/troke12/refity-frontend"><img alt="Docker pulls (frontend)" src="https://img.shields.io/docker/pulls/troke12/refity-frontend?style=flat-square"></a>
  <a href="https://github.com/troke12/refity"><img alt="GitHub stars" src="https://img.shields.io/github/stars/troke12/refity?style=flat-square"></a>
  <a href="https://troke.id/refity/"><img alt="Website" src="https://img.shields.io/badge/website-troke.id%2Frefity-0066CC?style=flat-square"></a>
  <a href="https://troke.id/refity/docs.html"><img alt="Docs" src="https://img.shields.io/badge/docs-quickstart%20%2B%20api-004499?style=flat-square"></a>
</p>

## Documentation

- Docs: [troke.id/refity/docs.html](https://troke.id/refity/docs.html)
- Why SFTP (comparison): [troke.id/refity/compare.html](https://troke.id/refity/compare.html)
- Features: [troke.id/refity/features.html](https://troke.id/refity/features.html)

## Key features

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

- Web UI: [http://localhost:8080](http://localhost:8080)
- Backend API: [http://localhost:5000](http://localhost:5000)

Default user:

- Username: `admin`
- Password: `admin`

Change it after first login.

## License

MIT. See `LICENSE`.

