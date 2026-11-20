# Builds the agent ISO for vSNO $1 from release image $2, then serves it as
# /work/www/$1.iso. stdin is a tar with config/ (the installer's inputs) and
# pull-secret.json. Prints one progress line per step.
set -euo pipefail
name=$1 image=$2
dir=/work/guests/$name
rm -rf "$dir"
mkdir -p "$dir"
tar -x -C "$dir"
trap 'rm -f "$dir/pull-secret.json"' EXIT

# One build at a time: parallel builds have hit a libnmstate.so race.
exec 9>/work/.iso.lock
if ! flock -n 9; then
  echo "waiting for another ISO build"
  flock 9
fi

# Keep the installer's base ISO cache (about 1 GB per release) on the volume,
# so a builder restart doesn't download it again.
export XDG_CACHE_HOME=/work/cache

# openshift-install and oc are cached per release image: the same release
# yields the same versions, so a second create of it extracts nothing.
sha=$(printf %s "$image" | sha256sum | cut -c1-16)
bin=/work/bin/openshift-install-$sha
ocbin=/work/bin/oc-$sha
if [ ! -x "$bin" ] || [ ! -x "$ocbin" ]; then
  echo "extracting openshift-install"
  tmp=$(mktemp -d /work/extract.XXXXXX)
  oc adm release extract --registry-config "$dir/pull-secret.json" \
    --command=openshift-install --command=oc --to "$tmp" "$image"
  [ -x "$bin" ] || mv "$tmp/openshift-install" "$bin"
  [ -x "$ocbin" ] || mv "$tmp/oc" "$ocbin"
  rm -rf "$tmp"
fi
# Which release image the binary came from, so `vsno isos` can tell whether a
# vSNO still uses it. Older builders have binaries without one.
[ -f "$bin.ref" ] || printf %s "$image" > "$bin.ref"
echo "version $("$bin" version | awk 'NR==1 {print $2}')"

echo "creating the agent ISO"
mkdir -p "$dir/build" /work/www
cp -r "$dir/config/." "$dir/build/"
"$bin" agent create image --dir "$dir/build" --log-level warn
# Move the ISO rather than copy it: the build directory doesn't need it, and a
# copy doubles its 1.3 GB.
mv "$dir/build/agent.x86_64.iso" "/work/www/$name.iso.tmp"
mv "/work/www/$name.iso.tmp" "/work/www/$name.iso"
echo "ISO ready"
