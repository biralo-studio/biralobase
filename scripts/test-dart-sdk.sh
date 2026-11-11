#!/usr/bin/env bash
set -euo pipefail
root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
binary="$root/bin/biralobase"
scratch=$(mktemp -d)
pid=''
cleanup() { if [ -n "$pid" ]; then kill "$pid" 2>/dev/null || true; wait "$pid" 2>/dev/null || true; fi; rm -rf -- "$scratch"; }
trap cleanup EXIT
"$binary" init "$scratch/app"
export BIRALOBASE_ADMIN_EMAIL=admin@example.com
export BIRALOBASE_ADMIN_PASSWORD=compatibility-test-password
port=${BIRALOBASE_TEST_PORT:-18091}
export BIRALOBASE_TEST_URL="http://127.0.0.1:$port"
export BIRALOBASE_TEST_MODE=${BIRALOBASE_TEST_MODE:-production}
app_dir="$scratch/app"
mode=(--dev)
if [ "$BIRALOBASE_TEST_MODE" = production ]; then
  node "$root/tests/compat/prepare.mjs" "$app_dir"
  "$binary" --app-dir "$app_dir" schema generate
  "$binary" --app-dir "$app_dir" bundle build --revision sdk-fixture --output "$scratch/release"
  app_dir="$scratch/release"
  mode=(--dev=false)
  export BIRALOBASE_PUBLIC_URL=https://sdk.example.test
  export BIRALOBASE_ENCRYPTION_KEY=01234567890123456789012345678901
fi
"$binary" --app-dir "$app_dir" --data-dir "$scratch/data" serve "${mode[@]}" --http "127.0.0.1:$port" > "$scratch/server.log" 2>&1 &
pid=$!
ready=false
for attempt in {1..400}; do
  if "$binary" healthcheck --url "$BIRALOBASE_TEST_URL/api/biralobase/ready" >/dev/null 2>&1; then ready=true; break; fi
  if ! kill -0 "$pid" 2>/dev/null; then cat "$scratch/server.log"; exit 1; fi
  sleep 0.1
done
[ "$ready" = true ]
cd "$root/tests/compat/dart"
dart pub get --enforce-lockfile
dart run compatibility.dart
