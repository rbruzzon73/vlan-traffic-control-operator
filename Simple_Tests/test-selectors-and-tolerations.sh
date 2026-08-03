#!/usr/bin/env bash
set -eo pipefail

NAMESPACE="openshift-vlan-tc-operator"
INTERFACE="enp1s0.100"
MASTER_NODE="hub-master01.ocp4-hub.test.com"
WORKER_NODE="hub-worker01.ocp4-hub.test.com"

echo "========================================================================"
echo "🧪 Starting Dedicated Selector & Toleration Verification Suite"
echo "Target Interface: ${INTERFACE}"
echo "Master Verification Node: ${MASTER_NODE}"
echo "Worker Verification Node: ${WORKER_NODE}"
echo "========================================================================"

# Helper function to wait for DaemonSet updates and pod ready state
sync_daemonset() {
  echo "--> Waiting for agent DaemonSet to synchronize..."
  oc rollout status daemonset/vlan-traffic-control-agent -n ${NAMESPACE} --timeout=60s || true
  sleep 3
}

verify_capability() {
  local test_name="$1"
  local exp_worker="$2"
  local exp_master="$3"

  echo ""
  echo "------------------------------------------------------------------------"
  echo "🔍 [TEST CASE] ${test_name}"
  echo "Expected User TC Classes -> Workers: ${exp_worker} | Masters: ${exp_master}"
  echo "------------------------------------------------------------------------"

  # Ensure DaemonSet pods are updated and stable before querying
  sync_daemonset

  # 1. API Check via Agent /stats Endpoint
  echo "--> Querying Agent API /stats endpoint on target nodes..."
  local worker_api_classes="0"
  local master_api_classes="0"

  # Check Worker Node
  local worker_pod
  worker_pod=$(oc get pods -n ${NAMESPACE} -l app=vlan-traffic-control-agent \
    --field-selector "spec.nodeName=${WORKER_NODE},status.phase=Running" \
    -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)

  if [ -n "${worker_pod}" ]; then
    local retries=5
    while [ $retries -gt 0 ]; do
      local stats_json
      stats_json=$(oc exec -n ${NAMESPACE} "${worker_pod}" -- curl -s "http://127.0.0.1:8080/stats?interface=${INTERFACE}" || true)
      if [ -n "${stats_json}" ] && echo "${stats_json}" | jq -e '.classStats' >/dev/null 2>&1; then
        worker_api_classes=$(echo "${stats_json}" | jq '[.classStats[] | select(.classId != "1:1" and .classId != "1:99")] | length')
        break
      fi
      sleep 2
      retries=$((retries - 1))
    done
  fi
  echo "    - Agent [${WORKER_NODE}]: ${worker_api_classes} user class(es) active"

  # Check Master Node
  local master_pod
  master_pod=$(oc get pods -n ${NAMESPACE} -l app=vlan-traffic-control-agent \
    --field-selector "spec.nodeName=${MASTER_NODE},status.phase=Running" \
    -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)

  if [ -n "${master_pod}" ]; then
    local retries=5
    while [ $retries -gt 0 ]; do
      local stats_json
      stats_json=$(oc exec -n ${NAMESPACE} "${master_pod}" -- curl -s "http://127.0.0.1:8080/stats?interface=${INTERFACE}" || true)
      if [ -n "${stats_json}" ] && echo "${stats_json}" | jq -e '.classStats' >/dev/null 2>&1; then
        master_api_classes=$(echo "${stats_json}" | jq '[.classStats[] | select(.classId != "1:1" and .classId != "1:99")] | length')
        break
      fi
      sleep 2
      retries=$((retries - 1))
    done
  fi
  echo "    - Agent [${MASTER_NODE}]: ${master_api_classes} user class(es) active"

  # 2. Host Linux Kernel Verification via oc debug node
  echo "--> Verifying Host Linux Kernel ('tc class show dev ${INTERFACE}')..."
  
  local master_kernel_raw
  master_kernel_raw=$(oc debug "node/${MASTER_NODE}" -- bash -c "chroot /host tc class show dev ${INTERFACE}" 2>/dev/null || true)
  
  local worker_kernel_raw
  worker_kernel_raw=$(oc debug "node/${WORKER_NODE}" -- bash -c "chroot /host tc class show dev ${INTERFACE}" 2>/dev/null || true)

  # Count user class IDs (> 1:1 and > 1:99, e.g., 1:100, 1:200, 1:300)
  local master_kernel_classes
  master_kernel_classes=$(echo "${master_kernel_raw}" | grep -E 'class htb 1:[1-8][0-9][0-9]' | wc -l || echo 0)
  
  local worker_kernel_classes
  worker_kernel_classes=$(echo "${worker_kernel_raw}" | grep -E 'class htb 1:[1-8][0-9][0-9]' | wc -l || echo 0)

  master_kernel_classes=$(echo "${master_kernel_classes}" | tr -d '[:space:]')
  worker_kernel_classes=$(echo "${worker_kernel_classes}" | tr -d '[:space:]')

  echo "    - Host Kernel [${MASTER_NODE}]: ${master_kernel_classes} user HTB class(es)"
  echo "    - Host Kernel [${WORKER_NODE}]: ${worker_kernel_classes} user HTB class(es)"

  # Assertions
  if [ "${worker_api_classes}" -eq "${exp_worker}" ] && [ "${master_api_classes}" -eq "${exp_master}" ]; then
    echo "✅ TEST PASSED: State matches expectations!"
  else
    echo "❌ TEST FAILED: Mismatch in expected active user classes!"
    echo "   Workers: expected=${exp_worker}, actual=${worker_api_classes}"
    echo "   Masters: expected=${exp_master}, actual=${master_api_classes}"
    exit 1
  fi
}

# --- INITIAL CLEANUP PASS ---
echo "🧹 Deleting all existing cluster VlanTrafficControl CRs..."
oc delete vlantrafficcontrol --all --ignore-not-found
sleep 3

echo "ext Flushing host TC interface ${INTERFACE} across all active agent pods..."
for pod in $(oc get pods -n ${NAMESPACE} -l app=vlan-traffic-control-agent -o name); do
  oc exec -n ${NAMESPACE} "${pod}" -- curl -s -X DELETE "http://127.0.0.1:8080/cleanup?interface=${INTERFACE}" >/dev/null 2>&1 || true
done
sleep 3

# ========================================================================
# TEST 1: nodeSelector (Legacy Map-Based Targeting)
# ========================================================================
echo ""
echo "▶ TEST 1: Testing 'nodeSelector' map targeting (Workers only)..."
oc apply -f - <<EOF
apiVersion: networking.med.io/v1alpha1
kind: VlanTrafficControl
metadata:
  name: test-nodeselector-worker
spec:
  nodeSelector:
    node-role.kubernetes.io/worker: ""
  reconcileIntervalSeconds: 30
  tcStrategy: flower
  htbRoot:
    interface: ${INTERFACE}
    htbId: 1
    rate: 10Gbit
    classes:
      - name: worker-class-100
        classId: "1:100"
        matchType: subnet
        subnet: 10.0.100.0/24
        egressRate: 100Mbit
EOF
verify_capability "Test 1: nodeSelector map (Workers only)" 1 0
oc delete vlantrafficcontrol test-nodeselector-worker

# ========================================================================
# TEST 2: Tolerations Enforcement on Tainted Master Nodes
# ========================================================================
echo ""
echo "▶ TEST 2: Testing missing 'tolerations' on tainted Master nodes..."
oc apply -f - <<EOF
apiVersion: networking.med.io/v1alpha1
kind: VlanTrafficControl
metadata:
  name: test-missing-toleration
spec:
  nodeSelector:
    node-role.kubernetes.io/master: ""
  reconcileIntervalSeconds: 30
  tcStrategy: flower
  htbRoot:
    interface: ${INTERFACE}
    htbId: 1
    rate: 10Gbit
    classes:
      - name: master-class-200
        classId: "1:200"
        matchType: subnet
        subnet: 10.0.218.0/24
        egressRate: 200Mbit
EOF
verify_capability "Test 2: Missing tolerations (Master nodes skip CR)" 0 0
oc delete vlantrafficcontrol test-missing-toleration

# ========================================================================
# TEST 3: Correct Tolerations + nodeSelector
# ========================================================================
echo ""
echo "▶ TEST 3: Testing 'nodeSelector' WITH correct 'tolerations' on Master nodes..."
oc apply -f - <<EOF
apiVersion: networking.med.io/v1alpha1
kind: VlanTrafficControl
metadata:
  name: test-with-toleration
spec:
  nodeSelector:
    node-role.kubernetes.io/master: ""
  tolerations:
    - key: "node-role.kubernetes.io/master"
      operator: "Exists"
      effect: "NoSchedule"
    - key: "node-role.kubernetes.io/control-plane"
      operator: "Exists"
      effect: "NoSchedule"
  reconcileIntervalSeconds: 30
  tcStrategy: flower
  htbRoot:
    interface: ${INTERFACE}
    htbId: 1
    rate: 10Gbit
    classes:
      - name: master-class-200
        classId: "1:200"
        matchType: subnet
        subnet: 10.0.218.0/24
        egressRate: 200Mbit
EOF
verify_capability "Test 3: Correct toleration applied (Master nodes active)" 0 1
oc delete vlantrafficcontrol test-with-toleration

# ========================================================================
# TEST 4: NodeLabelSelector - matchLabels
# ========================================================================
echo ""
echo "▶ TEST 4: Testing 'NodeLabelSelector.matchLabels'..."
oc apply -f - <<EOF
apiVersion: networking.med.io/v1alpha1
kind: VlanTrafficControl
metadata:
  name: test-matchlabels
spec:
  nodeLabelSelector:
    matchLabels:
      node-role.kubernetes.io/worker: ""
  reconcileIntervalSeconds: 30
  tcStrategy: flower
  htbRoot:
    interface: ${INTERFACE}
    htbId: 1
    rate: 10Gbit
    classes:
      - name: worker-class-300
        classId: "1:300"
        matchType: subnet
        subnet: 10.0.238.0/24
        egressRate: 300Mbit
EOF
verify_capability "Test 4: NodeLabelSelector matchLabels (Worker nodes active)" 1 0
oc delete vlantrafficcontrol test-matchlabels

# ========================================================================
# TEST 5: NodeLabelSelector - matchExpressions (Exists Operator)
# ========================================================================
echo ""
echo "▶ TEST 5: Testing 'NodeLabelSelector.matchExpressions' (Operator: Exists)..."
oc apply -f - <<EOF
apiVersion: networking.med.io/v1alpha1
kind: VlanTrafficControl
metadata:
  name: test-matchexpressions
spec:
  nodeLabelSelector:
    matchExpressions:
      - key: "node-role.kubernetes.io/master"
        operator: "Exists"
  tolerations:
    - key: "node-role.kubernetes.io/master"
      operator: "Exists"
      effect: "NoSchedule"
  reconcileIntervalSeconds: 30
  tcStrategy: flower
  htbRoot:
    interface: ${INTERFACE}
    htbId: 1
    rate: 10Gbit
    classes:
      - name: master-class-400
        classId: "1:400"
        matchType: subnet
        subnet: 10.0.100.0/24
        egressRate: 400Mbit
EOF
verify_capability "Test 5: NodeLabelSelector matchExpressions (Master nodes active)" 0 1
oc delete vlantrafficcontrol test-matchexpressions

# ========================================================================
# TEST 6: Combined Selectors (NodeLabelSelector + Custom Labels)
# ========================================================================
echo ""
echo "▶ TEST 6: Testing custom node label matching ('group=traffic-test')..."
oc label node ${WORKER_NODE} group=traffic-test --overwrite

oc apply -f - <<EOF
apiVersion: networking.med.io/v1alpha1
kind: VlanTrafficControl
metadata:
  name: test-custom-label
spec:
  nodeLabelSelector:
    matchLabels:
      group: "traffic-test"
  reconcileIntervalSeconds: 30
  tcStrategy: flower
  htbRoot:
    interface: ${INTERFACE}
    htbId: 1
    rate: 10Gbit
    classes:
      - name: custom-label-class-500
        classId: "1:500"
        matchType: subnet
        subnet: 10.0.100.0/24
        egressRate: 500Mbit
EOF
verify_capability "Test 6: Custom label match (group=traffic-test on single worker)" 1 0

oc label node ${WORKER_NODE} group-
oc delete vlantrafficcontrol test-custom-label

echo ""
echo "========================================================================"
echo "🎉 SUCCESS: All capability tests passed verification cleanly!"
echo "========================================================================"
