# Copies one image between the environment's registry and another one.
#
# Usage: sync.sh out <src> <dst> <insecure> <overwrite>
#        sync.sh in <src> <dst> <insecure> <overwrite>
#
# stdin: a tar with auth.json and ca.crt for the environment's registry, and
#   remote-auth.json (its dockerconfigjson) and remote-ca.crt when the other
#   side needs them.
#   out: src is in the environment's registry, dst is elsewhere.
#   in:  src is elsewhere, dst is in the environment's registry.
#
# The environment's registry serves HTTPS with its own CA and htpasswd auth, so
# its side always gets --cert-dir; the other side uses the system trust store
# unless insecure, and may be anonymous. --all keeps a manifest list whole, and
# the manifest is copied unchanged, so LCA's seed labels survive.
#
# Prints one JSON line: the destination's digest and the bytes its layers take.
set -euo pipefail
dir=$1 src=$2 dst=$3 insecure=${4:-false} overwrite=${5:-false}
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
tar -x -C "$tmp"
la=$tmp/auth.json
ra=$tmp/remote-auth.json

# Each registry's CA gets its own directory: cert-dir entries are looked up as
# ca.crt, so two CAs cannot share one.
mkdir -p "$tmp/local" "$tmp/remote"
mv "$tmp/ca.crt" "$tmp/local/ca.crt"
if [ -f "$tmp/remote-ca.crt" ]; then mv "$tmp/remote-ca.crt" "$tmp/remote/ca.crt"; fi

# inspect takes --authfile/--cert-dir/--tls-verify only, so each branch spells
# out the copy's flags and the destination's inspect flags separately.
case $dir in
out)
  copy=(--all --retry-times 3 --src-authfile "$la" --src-cert-dir "$tmp/local")
  inspect=()
  if [ -f "$ra" ]; then
    copy+=(--dest-authfile "$ra")
    inspect=(--authfile "$ra")
  fi
  if [ -f "$tmp/remote/ca.crt" ]; then
    copy+=(--dest-cert-dir "$tmp/remote")
    inspect+=(--cert-dir "$tmp/remote")
  fi
  if [ "$insecure" = true ]; then
    copy+=(--dest-tls-verify=false)
    inspect+=(--tls-verify=false)
  fi
  ;;
in)
  copy=(--all --retry-times 3 --dest-authfile "$la" --dest-cert-dir "$tmp/local")
  inspect=(--authfile "$la" --cert-dir "$tmp/local")
  if [ -f "$ra" ]; then copy+=(--src-authfile "$ra"); fi
  if [ -f "$tmp/remote/ca.crt" ]; then copy+=(--src-cert-dir "$tmp/remote"); fi
  if [ "$insecure" = true ]; then copy+=(--src-tls-verify=false); fi
  ;;
*)
  echo "unknown direction $dir" >&2
  exit 2
  ;;
esac

# A tag someone may already pull is not replaced by accident.
if skopeo inspect "${inspect[@]}" "docker://$dst" >/dev/null 2>&1 && [ "$overwrite" != true ]; then
  echo "$dst already exists; pass --overwrite to replace it" >&2
  exit 1
fi

skopeo copy --quiet "${copy[@]}" "docker://$src" "docker://$dst"
skopeo inspect "${inspect[@]}" "docker://$dst" |
  jq -c '{digest: .Digest, sizeBytes: ([.LayersData[]?.Size] | add // 0)}'
