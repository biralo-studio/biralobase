#!/usr/bin/env bash
set -euo pipefail
runtime_image=${BIRALOBASE_TEST_RUNTIME_IMAGE:-biralobase:ci}
builder_image=${BIRALOBASE_TEST_BUILDER_IMAGE:-biralobase-builder:ci}
scratch=$(mktemp -d)
container=''
volume=''
cleanup() {
  if [ -n "$container" ]; then docker rm -f "$container" >/dev/null 2>&1 || true; fi
  if [ -n "$volume" ]; then docker volume rm "$volume" >/dev/null 2>&1 || true; fi
  rm -rf -- "$scratch"
}
trap cleanup EXIT
mkdir -p "$scratch/app" "$scratch/secrets"
# Public, disposable test credentials; never reuse these in a deployment.
printf '%s' 'container-test-password' > "$scratch/secrets/admin_password"
printf '%s' '01234567890123456789012345678901' > "$scratch/secrets/encryption_key"
chmod 755 "$scratch" "$scratch/secrets"
chmod 444 "$scratch/secrets/"*
docker run --rm --user "$(id -u):$(id -g)" -e HOME=/tmp -v "$scratch/app:/workspace" -w /workspace "$builder_image" bash -euc '
  biralobase init .
  npm install --install-links --no-audit --no-fund
  npm test
  npm run build
  biralobase --app-dir . schema check
  npm prune --omit=dev --no-audit --no-fund
  biralobase --app-dir . bundle build --revision container-smoke --output /workspace/release
'
volume=$(docker volume create)
container=$(docker run -d --read-only --cap-drop ALL --security-opt no-new-privileges \
  --tmpfs /tmp:rw,exec,nosuid,mode=1777,size=256m \
  -v "$scratch/app/release:/app:ro" -v "$volume:/var/lib/biralobase" \
  -v "$scratch/secrets:/run/secrets:ro" -p 127.0.0.1::8090 \
  -e BIRALOBASE_PUBLIC_URL=https://container.example.test \
  -e BIRALOBASE_ADMIN_EMAIL=admin@example.com \
  -e BIRALOBASE_ADMIN_PASSWORD_FILE=/run/secrets/admin_password \
  -e BIRALOBASE_ENCRYPTION_KEY_FILE=/run/secrets/encryption_key "$runtime_image")
binding=$(docker port "$container" 8090/tcp)
base="http://127.0.0.1:${binding##*:}"
ready() {
  for attempt in {1..120}; do
    if curl -fsS "$base/api/biralobase/ready" >/dev/null 2>&1; then return; fi
    if [ "$(docker inspect --format '{{.State.Running}}' "$container")" != true ]; then break; fi
    sleep 1
  done
  docker logs "$container"
  return 1
}
ready
curl -fsS "$base/api/functions/hello" > "$scratch/hello.json"
docker run --rm -v "$scratch:/test:ro" "$builder_image" node -e '
  const fs=require("node:fs");if(JSON.parse(fs.readFileSync("/test/hello.json")).message!=="Hello from Biralobase")process.exit(1)
'
test "$(docker exec "$container" id -u)" = 10001
docker restart "$container" >/dev/null
ready
curl -fsS "$base/api/functions/hello" >/dev/null
echo 'Non-root, read-only container and persistent restart passed'
