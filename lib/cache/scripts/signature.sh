# Usage: signature.sh <release repository> <digest> <dest host> <repo>
# stdin: a tar with auth.json (the cluster pull secret plus the registry's
# credentials) and ca.crt (the registry's CA).
# Copies the release image's sigstore signature, the tag sha256-<hex>.sig next
# to the image, into <dest host>/<repo>. A guest pulling the release image from
# the mirror looks for its signature there (use-sigstore-attachments), and its
# policy refuses the image without one. Prints "copied", "present" or "none"
# (the release has no signature to copy, as nightlies don't).
set -euo pipefail
src=$1 digest=$2 host=$3 repo=$4
tag=sha256-${digest#sha256:}.sig

dir=$(mktemp -d)
trap 'rm -rf "$dir"' EXIT
tar -x -C "$dir"
sk=(--authfile "$dir/auth.json")

if skopeo inspect --raw "${sk[@]}" --cert-dir "$dir" "docker://$host/$repo:$tag" >/dev/null 2>&1; then
  echo present
  exit 0
fi
if ! out=$(skopeo inspect --raw "${sk[@]}" "docker://$src:$tag" 2>&1); then
  case $out in
  *"manifest unknown"* | *"not found"* | *"404"*) echo none; exit 0 ;;
  esac
  echo "reading $src:$tag: $out" >&2
  exit 1
fi
skopeo copy --quiet --preserve-digests "${sk[@]}" --dest-cert-dir "$dir" \
  "docker://$src:$tag" "docker://$host/$repo:$tag"
echo copied
