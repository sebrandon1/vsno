# Usage: signature.sh <dest host> <repo>
# stdin: a tar with auth.json (the cluster pull secret plus the registry's
# credentials), ca.crt (the registry's CA) and images.txt (the release image
# and its component images, one <repository>@sha256:<hex> per line).
# Copies each image's sigstore signature, the tag sha256-<hex>.sig next to the
# image, into <dest host>/<repo>. A guest pulling from the mirror looks for
# signatures there (use-sigstore-attachments), and its policy refuses an image
# without one. Lists the mirror's tags once, so signatures already there cost
# no lookups. Prints "copied N, present M, none K" (none: no signature
# upstream, as for nightlies).
set -euo pipefail
host=$1 repo=$2

dir=$(mktemp -d)
trap 'rm -rf "$dir"' EXIT
tar -x -C "$dir"
sk=(--authfile "$dir/auth.json")

have=$(skopeo list-tags "${sk[@]}" --cert-dir "$dir" "docker://$host/$repo" 2>/dev/null | grep -o '"sha256-[0-9a-f]*\.sig"' | tr -d '"' || true)

# one copies one image's signature unless the mirror has it, and prints
# copied, present or none.
one() {
  local ref=$1 src digest tag out
  src=${ref%@*} digest=${ref#*@}
  tag=sha256-${digest#sha256:}.sig
  if grep -qxF "$tag" <<<"$have"; then
    echo present
    return
  fi
  if ! out=$(skopeo inspect --raw --authfile "$dir/auth.json" "docker://$src:$tag" 2>&1); then
    case $out in
    *"manifest unknown"* | *"not found"* | *"404"*) echo none; return ;;
    esac
    echo "reading $src:$tag: $out" >&2
    return 1
  fi
  skopeo copy --quiet --preserve-digests --authfile "$dir/auth.json" --dest-cert-dir "$dir" \
    "docker://$src:$tag" "docker://$host/$repo:$tag"
  echo copied
}
export -f one
export dir host repo have

# Eight at a time: a new release has about 190 to copy. A failed copy fails
# xargs, and with it the script, after the others finish.
# shellcheck disable=SC2016 # $1 expands in the child shell
results=$(grep -v '^$' "$dir/images.txt" | xargs -P 8 -I{} bash -c 'one "$1"' _ {})
count() { grep -cx "$1" <<<"$results" || true; }
echo "copied $(count copied), present $(count present), none $(count none)"
