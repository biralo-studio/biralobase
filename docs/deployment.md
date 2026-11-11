# Deployment

Biralobase uses one active process per application and a local persistent volume. It is not a multi-writer cluster. Do not mount the same data directory into several replicas or place it on a network filesystem. Horizontal scaling means separate application instances with separate databases.

## Build runtime images

The Biralobase repository's `runtime-images` workflow runs release checks before publishing a prerelease runtime and builder to GHCR. It accepts only `alpha`, `beta`, and `rc` versions. There is deliberately no stable publishing path until the extended gates pass. The repository is private; configure package access for your application CI and host, or explicitly publish the package after reviewing it.

Local builds:

```sh
docker build --target runtime --build-arg RELEASE_VERSION=0.1.0-alpha.1 -t biralobase:0.1.0-alpha.1 .
docker build --target builder --build-arg RELEASE_VERSION=0.1.0-alpha.1 -t biralobase-builder:0.1.0-alpha.1 .
```

Pin both published images by digest. A builder's version is embedded into the bundle and checked by the runtime. Bundle checksums detect corruption or accidental changes; authenticity comes from the trusted CI/upload channel. They are not digital signatures.

## Prepare a host once

Install Docker Engine and Compose v2 on Linux. Copy `deploy/compose.yaml`, `deploy/release.sh`, and `deploy/activate.sh` into an application-specific directory such as `/srv/my-app`. The deployment SSH user must own that directory and be authorized to use Docker. Put a TLS reverse proxy in front of the loopback-only port. Configure PocketBase trusted proxies/headers through settings to match that proxy.

Create `config.env` beside those scripts:

```sh
BIRALOBASE_PROJECT=my-app
BIRALOBASE_IMAGE=ghcr.io/biralo-studio/biralobase@sha256:REPLACE_WITH_RUNTIME_DIGEST
BIRALOBASE_PUBLIC_URL=https://api.example.com
BIRALOBASE_ADMIN_EMAIL=admin@example.com
BIRALOBASE_PORT=8090
BIRALOBASE_SECRETS_DIR=/srv/my-app/secrets
```

This file is sourced by Bash: use ordinary quoted shell assignments when a value contains special characters. Do not put passwords in it.

Create `secrets/admin_password` with a password of at least 12 characters and `secrets/encryption_key` with exactly 32 bytes. Store the key in a secure backup as well. The non-root container runs as UID/GID 10001; secret files must be readable by that UID and protected from other host users (for example ownership 10001:10001 and mode 0400). The containing directory can be private to the deployment user; Docker mounts the files. Keep the named data volume across releases. Its initial ownership comes from the image.

`BIRALOBASE_*` configuration accepts a literal environment value or an exclusive `_FILE` variant. An ending newline in a secret file is removed. Supplying both forms is rejected.

Additional settings can be injected with `BIRALOBASE_SETTINGS_JSON_FILE`; add a Compose secret/mount and environment entry for it. Collection OAuth provider secrets use `BIRALOBASE_OAUTH_SECRETS_JSON_FILE` with this format:

```json
{"STABLE_AUTH_COLLECTION_ID":{"google":"provider-client-secret"}}
```

Provider names/client IDs and non-secret options belong in the versioned schema. Settings secrets are encrypted at rest using the encryption key. User records and files are not encrypted by that key; use encrypted disks when required. Encryption-key rotation requires deliberate re-encryption, not simply replacing the file.

For function-specific secrets, use `BIRALOBASE_FUNCTION_<NAME>` environment variables. The worker inherits only this prefix and a small system-variable allowlist. It is trusted application code with privileged APIs and filesystem access, not a sandbox for untrusted tenants.

## Application GitHub Action

Copy `deploy/app-deploy.yaml` into the application's `.github/workflows/deploy.yaml`. Set repository/environment variables:

| Variable | Meaning |
| --- | --- |
| `BIRALOBASE_BUILDER_IMAGE` | Matching builder image, pinned by digest |
| `BIRALOBASE_RUNNER` | `ubuntu-24.04` or `ubuntu-24.04-arm`, matching the host |
| `BIRALOBASE_SSH_TARGET` | `deploy-user@host` |
| `BIRALOBASE_HOST_DIR` | Application-specific absolute directory such as `/srv/my-app` |

Set secrets `BIRALOBASE_DEPLOY_KEY` and `BIRALOBASE_KNOWN_HOSTS`. Obtain and verify the host key independently. For private GHCR images, add a registry-login step using credentials with read-package access; also log in on the host. Keep schema generation in the development/review workflow: CI checks that committed schema and migrations match and fails if they do not.

The Action runs application tests, compiles functions, prunes development dependencies, builds an immutable bundle, uploads it, and invokes `activate.sh`. The runtime image is unchanged. Host deployment and runtime data-directory locks serialize transitions. Startup verifies the bundle, checks drift/history, creates a backup for pending migrations, applies migrations, reconciles secrets/admin, and then exposes readiness.

## Backups, rollback, and operations

- Backups include application data/files and the migration ledger. Copy them off-host with the PocketBase backup configuration or your backup system, and regularly test restoration.
- Never downgrade a bundle past applied migration history. The runtime rejects that startup. Use a forward migration or restore a consistent backup into an isolated volume before starting the matching earlier bundle.
- Password changes update the same managed superuser. Existing unmanaged accounts are never silently adopted. A collision with an unmanaged account fails provisioning.
- Production collection and settings mutations and the SQL console are intentionally blocked. Record CRUD, auth, files, realtime, and the SDKs remain available.
- Readiness: `/api/biralobase/ready`. A worker failure shuts down the server so requests cannot bypass hooks.
- Shutdown waits for a running job, for up to two minutes. Keep the stop timeout above that (`stop_grace_period: 3m` in the Compose file); a platform default of 10-30 seconds kills the process instead.
- Memory grows slowly with the number of committed writes and is only returned by a restart: budget roughly 6-24 MB per million writes with the pinned engine, and watch resident memory on write-heavy applications.
- OAuth clients that no user authorized within a day are removed once 1000 clients are registered. The OAuth rate limit counts IPv4 addresses and IPv6 /64 networks; it sees the proxy's address unless trusted proxy headers are configured.
- Superuser-only jobs API: `GET /api/biralobase/jobs?status=dead` and `POST /api/biralobase/jobs/{id}/retry`. Payloads are omitted from listings. Jobs currently need operator-managed archival/retention; completed jobs and expired OAuth sessions are not automatically purged.
- Replacing a release causes a short interruption. Clients reconnect/retry; this single-instance design does not promise zero-downtime deployment.
