# Usage: mirror.sh <release image> <dest host> <repo> <tag>
# stdin: a tar with auth.json (the cluster pull secret plus the registry's
# credentials) and ca.crt (the registry's CA).
# Mirrors the release's payload into <dest host>/<repo>, tagging the release
# image <tag>. Prints oc's progress lines.
set -euo pipefail
image=$1 host=$2 repo=$3 tag=$4
dir=/work/cache/mirror-$(printf %s "$image" | sha256sum | cut -c1-16)

# One mirror at a time: it shares the volume and the registry with ISO builds.
# The lock comes first: two mirrors of one release share this work dir, and
# wiping it under the one that holds the lock is how both would fail.
exec 8>/work/.mirror.lock
flock 8

rm -rf "$dir"
mkdir -p "$dir"
tar -x -C "$dir"
trap 'rm -rf "$dir"' EXIT

# oc verifies every registry against the system bundle, and the registry's CA
# is not in it. --certificate-authority would replace the bundle for quay.io
# too, so extend it instead.
cat /etc/pki/ca-trust/extracted/pem/tls-ca-bundle.pem "$dir/ca.crt" >"$dir/bundle.pem"
export SSL_CERT_FILE="$dir/bundle.pem"

echo "mirroring $image"
oc adm release mirror \
  --registry-config "$dir/auth.json" \
  --from "$image" \
  --to "$host/$repo" \
  --to-release-image "$host/$repo:$tag" \
  --keep-manifest-list \
  --max-per-registry 6
echo "mirrored $image"
