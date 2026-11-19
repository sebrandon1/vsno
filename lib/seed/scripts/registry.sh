# Usage: registry.sh <host> list | registry.sh <host> delete <repo:tag>...
# stdin: {"user": ..., "password": ..., "ca": ...} for the registry.
#   list:   one JSON line per tag of every seeds/ repository, from skopeo inspect
#   delete: deletes each tag's manifest (the blobs stay until the registry is garbage collected)
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
  repos=$(curl -sSf -K "$tmp/curlrc" --cacert "$tmp/ca.crt" "https://$host/v2/_catalog?n=1000" |
    jq -r '.repositories[]? | select(startswith("seeds/"))')
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
*)
  echo "unknown operation $op" >&2
  exit 2
  ;;
esac
