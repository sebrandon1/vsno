# Lists what the builder keeps for vSNOs and OpenShift releases: one JSON
# object per line. Read-only.
set -uo pipefail
work=/work

when() { date -u -d "@$(stat -c %Y "$1")" +%Y-%m-%dT%H:%M:%SZ; }
emit() { printf '%s\n' "$1"; }

# The volume's usage, so the footer can say how much room is left.
if read -r used total < <(df --output=used,size -k "$work" | tail -1); then
  emit "{\"kind\":\"volume\",\"used\":$((used * 1024)),\"total\":$((total * 1024))}"
fi

# Served ISOs: /work/www/<vsno>.iso.
for f in "$work"/www/*.iso; do
  [ -f "$f" ] || continue
  emit "{\"kind\":\"served\",\"name\":\"$(basename "$f" .iso)\",\"size\":$(stat -c %s "$f"),\"modified\":\"$(when "$f")\"}"
done

# Build directories: the installer's inputs and output for one vSNO.
for d in "$work"/guests/*; do
  [ -d "$d" ] || continue
  emit "{\"kind\":\"build\",\"name\":\"$(basename "$d")\",\"size\":$(( $(du -sk "$d" | cut -f1) * 1024 )),\"modified\":\"$(when "$d")\"}"
done

# Cached openshift-install binaries, one per release image, with the matching
# oc (oc-<sha>) counted as part of the same entry: prune keeps or removes both.
# A .ref sidecar, written by iso.sh, says which image a binary came from.
for b in "$work"/bin/openshift-install-*; do
  [ -f "$b" ] || continue
  case "$b" in *.ref) continue ;; esac
  ref=""
  [ -f "$b.ref" ] && ref=$(cat "$b.ref")
  v=$("$b" version 2>/dev/null | awk 'NR==1 {print $2}')
  size=$(stat -c %s "$b")
  o="$work/bin/oc-${b##*/openshift-install-}"
  [ -f "$o" ] && size=$((size + $(stat -c %s "$o")))
  emit "{\"kind\":\"installer\",\"name\":\"$(basename "$b")\",\"release\":\"$ref\",\"version\":\"$v\",\"size\":$size,\"modified\":\"$(when "$b")\"}"
done

# The installer's base ISO cache, about 1 GB for the latest release.
if [ -d "$work/cache" ]; then
  emit "{\"kind\":\"base\",\"name\":\"base-iso\",\"size\":$(( $(du -sk "$work/cache" | cut -f1) * 1024 )),\"modified\":\"$(when "$work/cache")\"}"
fi
