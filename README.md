# Refity

A simple, modern Docker private registry with SFTP backend storage.

Store and manage your container images securely on any SFTP server. Refity is designed for teams and organizations who want a self-hosted Docker registry with flexible, secure, and legacy-friendly storage options.

---

## Features

- **Docker Registry v2 API compatible**: Works with standard Docker CLI and tools
- **SFTP backend storage**: All images, manifests, and metadata are stored directly on your SFTP server
- **Multi-architecture support**: Handles manifest lists for multi-arch images
- **Async SFTP upload**: Fast local buffering, then async upload to SFTP with progress and retry
- **Strict group/folder control**: No auto-create, push fails if group/folder missing (for better access control)
- **Digest validation**: Ensures image integrity and compatibility with Docker clients
- **Tag listing**: List all tags for a repository
- **Lightweight & easy to deploy**: Single binary, Docker-ready

---

## Quick Start

### 1. Build or pull the image

```sh
git clone https://github.com/troke12/refity.git
cd refity
docker build -t refity .
```

### 2. Configure environment

Copy `.env.example` to `.env` and edit with your SFTP and registry credentials:

```env
REGISTRY_USERNAME=youruser
REGISTRY_PASSWORD=yourpass
FTP_HOST=sftp.example.com
FTP_PORT=22
FTP_USERNAME=sftpuser
FTP_PASSWORD=sftppass
```

### 3. Run with Docker

```sh
docker run -p 5000:5000 --env-file .env refity
```

### 4. Push & pull images

Tag and push your image:

```sh
docker tag nginx localhost:5000/yourgroup/nginx:latest
docker push localhost:5000/yourgroup/nginx:latest
```

---

## Example: List tags

```sh
curl -u youruser:yourpass http://localhost:5000/v2/yourgroup/nginx/tags/list
```

---

## Why SFTP?
- Use existing SFTP infrastructure for secure, centralized storage
- Integrate with legacy systems or restricted environments
- Avoid cloud lock-in or object storage costs

---

## License
MIT 