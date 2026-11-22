# Usage: probe.sh <base domain> <name>=<service IP>...
# Prints one JSON line per vSNO with what its install reports:
#   agent:     the agent installer's cluster (status, info, progress %, validations), before the first reboot
#   cv:        ClusterVersion (version, available, message), once the API is up
#   unhealthy: ClusterOperators that are Degraded or not Available
#   ibu:       LCA's ImageBasedUpgrade stage, and whether it completed
# Each is null when that endpoint isn't answering.
set -u
base=$1
shift
for arg in "$@"; do
  name=${arg%%=*} ip=${arg#*=}
  dir=/work/guests/$name/build
  token=$(jq -r '.["*gencrypto.AuthConfig"].UserAuthToken // empty' "$dir/.openshift_install_state.json" 2>/dev/null)
  agent=$(curl -sf -m 5 -H "Authorization: $token" "http://$ip:8090/api/assisted-install/v2/clusters" 2>/dev/null |
    jq -c '.[0] // empty | {status, info: .status_info, progress: (.progress.total_percentage // 0), validations: .validations_info}' 2>/dev/null)
  cv='' unhealthy='' ibu=''
  if [ -f "$dir/auth/kubeconfig" ]; then
    oc=(oc --kubeconfig "$dir/auth/kubeconfig" --server "https://$ip:6443" --tls-server-name "api.$name.$base" --request-timeout=8s)
    cv=$("${oc[@]}" get clusterversion version -o json 2>/dev/null |
      jq -c '{version: .status.desired.version,
              available: ([.status.conditions[]? | select(.type == "Available") | .status][0] == "True"),
              message: ([.status.conditions[]? | select(.type == "Progressing") | .message][0] // ""),
              since: ([.status.conditions[]? | select(.type == "Available") | .lastTransitionTime][0] // null)}' 2>/dev/null)
    if [ -n "$cv" ]; then
      unhealthy=$("${oc[@]}" get clusteroperators -o json 2>/dev/null |
        jq '[.items[] | select(any(.status.conditions[]?; (.type == "Degraded" and .status == "True") or (.type == "Available" and .status == "False")))] | length' 2>/dev/null)
      # LCA names each stage's completion condition <Stage>Completed.
      ibu=$("${oc[@]}" get imagebasedupgrades upgrade --ignore-not-found -o json 2>/dev/null |
        jq -c '.spec.stage as $s | {stage: $s, completed: any(.status.conditions[]?; .type == ($s + "Completed") and .status == "True")}' 2>/dev/null)
    fi
  fi
  jq -cn --arg name "$name" --argjson agent "${agent:-null}" --argjson cv "${cv:-null}" --argjson unhealthy "${unhealthy:-null}" --argjson ibu "${ibu:-null}" \
    '{name: $name, agent: $agent, cv: $cv, unhealthy: $unhealthy, ibu: $ibu}' 2>/dev/null || jq -cn --arg name "$name" '{name: $name}'
done
