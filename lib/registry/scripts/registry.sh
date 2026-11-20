# Usage: registry.sh <host> list [<repo prefix>]
#        registry.sh <host> delete <repo:tag>...
#        registry.sh <host> delete-manifests <repo> <digest>...
# stdin: {"user": ..., "password": ..., "ca": ...} for the registry.
#   list:             one JSON line per tag of the repositories under prefix
#                     (default seeds/), from skopeo inspect
#   delete:           deletes each tag's manifest (the blobs stay until the
#                     registry is garbage collected)
#   delete-manifests: deletes one repository's manifests by digest, which is
#                     how a mirrored release is emptied (its images are stored
#                     by digest, with one tag for the release image)
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
    curl -sSf -K "$tmp/curlrc" --cacert "$tmp/ca.crt" -X DELETE \
      "https://$host/v2/$repo/manifests/$digest" >/dev/null
    echo "deleted $repo@$digest"
  done
  ;;
*)
  echo "unknown operation $op" >&2
  exit 2
  ;;
esac
