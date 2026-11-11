#!/usr/bin/env bash
set -euo pipefail
# Run on the application host after uploading an unpacked, immutable bundle.
# Required environment is shared with compose.yaml. No secrets enter Git.
: "${BIRALOBASE_RELEASE:?absolute uploaded release path}"
: "${BIRALOBASE_IMAGE:?runtime image pinned by digest}"
: "${BIRALOBASE_PROJECT:?unique Compose project for this application}"
[[ "$BIRALOBASE_RELEASE" = /* && -f "$BIRALOBASE_RELEASE/biralobase.release.json" ]]
[[ "$BIRALOBASE_IMAGE" = *@sha256:* ]]
deploy_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
# Serialize host deploys; the runtime also holds a nonblocking data-directory lock.
exec 9>"$deploy_dir/.deploy.lock"
flock -n 9
docker run --rm --read-only --tmpfs /tmp:exec -v "$BIRALOBASE_RELEASE:/app:ro" "$BIRALOBASE_IMAGE" bundle verify
docker compose -p "$BIRALOBASE_PROJECT" -f "$deploy_dir/compose.yaml" up -d --wait --wait-timeout 300 --force-recreate biralobase
# A failed migration remains stopped/unready. Never automatically start an older
# bundle over a newer schema: restore a verified backup or deploy a forward fix.
