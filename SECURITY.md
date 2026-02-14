# Security

## Production checklist

Before deploying to production:

1. **Set `JWT_SECRET`**  
   Use a long random value (e.g. 32+ chars). If unset, a default dev secret is used and tokens can be forged.

2. **Set `CORS_ORIGINS`**  
   Comma-separated list of allowed frontend origins (e.g. `https://registry.example.com`). Prevents unauthorized sites from calling your API with user credentials.

3. **Set `FTP_KNOWN_HOSTS`**  
   Path to an SSH `known_hosts` file so the SFTP client verifies the server host key. Without this, connections are vulnerable to MITM. Example:
   ```bash
   ssh-keyscan -t rsa,ecdsa,ed25519 your-sftp-host >> /etc/refity/known_hosts
   export FTP_KNOWN_HOSTS=/etc/refity/known_hosts
   ```

4. **Change default admin password**  
   Default user `admin` / `admin` is created on first run. Change it immediately after first login.

5. **Protect the Docker Registry API (`/v2/`)**  
   The registry endpoints (`/v2/*`) do **not** require authentication. Anyone who can reach the backend can push/pull images. In production:
   - Put the backend behind a reverse proxy (nginx/traefik) and restrict access (VPN, IP allowlist, or HTTP basic/auth / token auth at the proxy), or
   - Expose the backend only on an internal network and use the web UI (which uses JWT) for management.

6. **Secrets**  
   Do not commit `.env`. Use a secrets manager or env injection in your deployment. Do not log passwords or tokens.

## Addressed vulnerabilities

- **JWT secret**: Now configurable via `JWT_SECRET`; no hardcoded production secret.
- **CORS**: Configurable via `CORS_ORIGINS` for production origins.
- **SSH host key**: Optional `FTP_KNOWN_HOSTS` enables host key verification; otherwise MITM is possible on SFTP.
- **Path traversal**: Repository and manifest reference from URLs are validated; local storage driver rejects paths that escape its root.
- **Sensitive logging**: Removed log lines that could reveal token presence or internal IDs.

## Reporting vulnerabilities

Please report security issues privately (e.g. GitHub Security Advisories or a private contact), not in public issues.
