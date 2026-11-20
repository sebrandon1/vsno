# Usage: registry.sh <host> list [<repo prefix>]
#        registry.sh <host> delete <repo:tag>...
#        registry.sh <host> delete-manifests <repo> <digest>...
#        registry.sh <host> exists <repo:tag>
#        registry.sh <host> size <repo>
# stdin: {"user": ..., "password": ..., "ca": ...} for the registry.
#   list:             one JSON line per tag of the repositories under prefix
#                     (default seeds/), from skopeo inspect
#   delete:           deletes each tag's manifest (the blobs stay until the
#                     registry is garbage collected)
#   delete-manifests: deletes one repository's manifests by digest, which is
#                     how a mirrored release is emptied (its images are stored
#                     by digest, with one tag for the release image)
#   size:             the bytes a repository's tagged images take: each
#                     distinct config and layer blob once, following manifest
#                     lists to their images. Blobs other repositories share
#                     count too, so it is what the repository holds, not what
#                     deleting it would free.
set -euo pipefail
host=$1 op=$2
shift 2
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
in=$(cat)
jq -r .ca <<<"$in" >"$tmp/ca.crt"
user=$(jq -r .user <<<"$in")
pass=$(jq -r .password <<<"$in")
printf 'user = "%s:%s"\n' "$user" "$pass" >"$tmp/curlrc"
jq -n --arg h "$host" --arg a "$(printf '%s:%s' "$user" "$pass" | base64 -w0)" '{auths: {($h): {auth: $a}}}' >"$tmp/auth.json"
sk=(--cert-dir "$tmp" --authfile "$tmp/auth.json")
case $op in
list)
  prefix=${1:-seeds/}
  repos=$(curl -sSf -K "$tmp/curlrc" --cacert "$tmp/ca.crt" "https://$host/v2/_catalog?n=1000" |
    jq -r --arg p "$prefix" '.repositories[]? | select(startswith($p))')
  for repo in $repos; do
    tags=$(skopeo list-tags "${sk[@]}" "docker://$host/$repo" 2>/dev/null | jq -r '.Tags[]?') || continue
    for tag in $tags; do
      skopeo inspect "${sk[@]}" "docker://$host/$repo:$tag" |
        jq -c --arg repo "$repo" --arg tag "$tag" \
          '{repo: $repo, tag: $tag, digest: .Digest, created: .Created, labels: (.Labels // {}), size: ([.LayersData[]?.Size] | add // 0)}'
    done
  done
  ;;
delete)
  for ref in "$@"; do
    skopeo delete "${sk[@]}" "docker://$host/$ref"
    echo "deleted $ref"
  done
  ;;
delete-manifests)
  repo=$1
  shift
  for digest in "$@"; do
    # A manifest that is already gone is the outcome we want, and deleting one
    # takes its tag with it, so a second prune of the same release is a no-op
    # rather than a failure.
    code=$(curl -s -o /dev/null -w '%{http_code}' -K "$tmp/curlrc" --cacert "$tmp/ca.crt" -X DELETE \
      "https://$host/v2/$repo/manifests/$digest")
    case $code in
    202|404) echo "deleted $repo@$digest" ;;
    *) echo "deleting $repo@$digest: HTTP $code" >&2; exit 1 ;;
    esac
  done
  ;;
exists)
  if skopeo inspect "${sk[@]}" "docker://$host/$1" >/dev/null 2>&1; then echo yes; else echo no; fi
  ;;
size)
  repo=$1
  accept='application/vnd.oci.image.index.v1+json,application/vnd.docker.distribution.manifest.list.v2+json,application/vnd.oci.image.manifest.v1+json,application/vnd.docker.distribution.manifest.v2+json'
  get() { curl -sSf -K "$tmp/curlrc" --cacert "$tmp/ca.crt" -H "Accept: $accept" "https://$host/v2/$repo/manifests/$1"; }
  : >"$tmp/blobs"
  : >"$tmp/seen"
  queue=$(curl -sSf -K "$tmp/curlrc" --cacert "$tmp/ca.crt" "https://$host/v2/$repo/tags/list?n=10000" | jq -r '.tags[]?')
  # Tags first, then the images a manifest list points at, by digest. A
  # manifest is read once however many tags or lists name it.
  while [ -n "$queue" ]; do
    next=
    for ref in $queue; do
      m=$(get "$ref")
      d=$(printf %s "$m" | sha256sum | cut -d' ' -f1)
      grep -qx "$d" "$tmp/seen" && continue
      echo "$d" >>"$tmp/seen"
      if [ "$(jq -r 'has("manifests")' <<<"$m")" = true ]; then
        next="$next $(jq -r '.manifests[].digest' <<<"$m")"
      else
        jq -r '(.config // empty), (.layers[]? ) | "\(.digest) \(.size)"' <<<"$m" >>"$tmp/blobs"
      fi
    done
    queue=$next
  done
  sort -u "$tmp/blobs" | awk '{s += $2} END {printf "%d\n", s}'
  ;;
*)
  echo "unknown operation $op" >&2
  exit 2
  ;;
esac
