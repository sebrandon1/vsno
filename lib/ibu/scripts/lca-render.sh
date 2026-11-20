# Usage: lca-render.sh <sha> <image>
# Renders LCA's manifests from the source lca-build.sh cloned into
# /work/lca/<sha> (config/default, as `make deploy` does), with the controller's
# image replaced by <image>. Prints the manifests.
# Like `make deploy`, it first writes config/manager/related-images/patch.yaml
# (not in git) from in.yaml, which points the precache workload at the image.
set -euo pipefail
sha=$1 image=$2
src=/work/lca/$sha
[ -d "$src/config/default" ] || { echo "no LCA source for $sha in /work/lca; build it first" >&2; exit 1; }
rel=$src/config/manager/related-images
if [ -f "$rel/in.yaml" ]; then
  sed "s#\$PRECACHE_WORKLOAD_IMG#$image#g; s#\${PRECACHE_WORKLOAD_IMG}#$image#g" "$rel/in.yaml" >"$rel/patch.yaml"
fi
oc kustomize "$src/config/default" |
  sed -E "s#quay\.io/openshift-kni/lifecycle-agent-operator:[^\"[:space:]]+#$image#g"
