#!/usr/bin/env bash
set -euo pipefail
# Install beside compose.yaml/release.sh on the host. config.env contains
# non-secret deployment configuration and paths to the host's secret files.
revision=${1:?Git revision is required}
[[ "$revision" =~ ^[a-f0-9]{40}$ ]]
deploy_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
set -a
source "$deploy_dir/config.env"
set +a
export BIRALOBASE_RELEASE="$deploy_dir/releases/$revision"
mkdir -p "$deploy_dir/releases"
# Never overwrite an already verified release. Existing releases can be
# explicitly reactivated using release.sh with their original path.
mkdir "$BIRALOBASE_RELEASE"
tar --no-same-owner -xzf "$deploy_dir/incoming-$revision.tar.gz" -C "$BIRALOBASE_RELEASE"
chmod 755 "$BIRALOBASE_RELEASE"
"$deploy_dir/release.sh"
rm -- "$deploy_dir/incoming-$revision.tar.gz"
