# Usage: release-info.sh <release image>
# stdin: a dockerconfigjson (the cluster's pull secret).
# Prints `oc adm release info -o json`: its digest keys the cache, and its
# references name the repositories an install-config must mirror.
set -euo pipefail
image=$1
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
cat >"$tmp/auth.json"
oc adm release info -a "$tmp/auth.json" -o json "$image"
