# Removes what a vSNO no longer needs once it has installed: the served ISO and
# the generated agent ISO. The build's auth/ (its admin kubeconfig) and
# .openshift_install_state.json stay: the probes, `vsno oc` and
# `vsno kubeconfig` read them.
set -euo pipefail
for name in "$@"; do
  rm -f "/work/www/$name.iso" "/work/www/$name.iso.tmp"
  rm -f "/work/guests/$name"/build/agent.*.iso
done
