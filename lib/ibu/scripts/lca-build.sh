# Usage: lca-build.sh <git repo> <ref> <registry host> [<image>]
# Runs in a privileged buildah pod that mounts the builder's /work. Clones the
# repo at ref into /work/lca/<sha>, so the builder can render its manifests,
# then, unless image is given, builds the repo's Dockerfile for linux/amd64 and
# pushes it to <registry host>/lca:<sha>, skipping the build when that tag is
# already there. /auth holds auth.json (the host pull secret, for the base
# images, plus the registry's login) and ca.crt (the registry's CA).
# Prints sha=, version= (the Makefile's VERSION) and image= lines, then built.
set -euo pipefail
repo=$1 ref=$2 host=$3 image=${4:-}

command -v git >/dev/null || dnf -y -q install git >/dev/null
mkdir -p /work/lca
tmp=$(mktemp -d /work/lca/clone.XXXXXX)
trap 'rm -rf "$tmp"' EXIT
echo "cloning $repo at $ref"
git -C "$tmp" init -q
git -C "$tmp" fetch -q --depth 1 "$repo" "$ref"
git -C "$tmp" checkout -q FETCH_HEAD
sha=$(git -C "$tmp" rev-parse --short=12 HEAD)
src=/work/lca/$sha
if [ ! -d "$src" ]; then
  mv "$tmp" "$src"
  mkdir "$tmp" # for the trap
fi
echo "sha=$sha"
echo "version=$(sed -n 's/^VERSION ?= *//p' "$src/Makefile" | head -1)"

if [ -z "$image" ]; then
  image=$host/lca:$sha
  if skopeo inspect --authfile /auth/auth.json --cert-dir /auth "docker://$image" >/dev/null 2>&1; then
    echo "already built: $image"
  else
    echo "building $image"
    buildah --storage-driver vfs build --isolation chroot --platform linux/amd64 \
      --authfile /auth/auth.json -f "$src/Dockerfile" -t "$image" "$src"
    buildah --storage-driver vfs push --authfile /auth/auth.json --cert-dir /auth "$image"
  fi
fi
echo "image=$image"
echo "built"
