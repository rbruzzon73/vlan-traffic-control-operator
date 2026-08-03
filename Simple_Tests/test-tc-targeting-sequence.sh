#!/usr/bin/env bash
set -eo pipefail

NAMESPACE="openshift-vlan-tc-operator"
INTERFACE="enp1s0.100"
MASTER_NODE="hub-master01.ocp4-hub.test.com"
WORKER_NODE="hub-worker01.ocp4-hub.test.com"

# Standard Master Tolerations block to allow CR execution on control-plane nodes
MASTER_TOLERATIONS='
  tolerations:
    - key: "node-role.kubernetes.io/master"
      operator: "Exists"
      effect: "NoSchedule"
    - key: "node-role.kubernetes.io/control-plane"
      operator: "Exists"
      effect: "NoSchedule"'

echo "========================================================================"
echo "🚀 Starting Comprehensive 9-Step TC Rule Lifecycle Verification Suite"
echo "Interface Target: ${INTERFACE}"
echo "Master Verification Node: ${MASTER_NODE}"
echo "Worker Verification Node: ${WORKER_NODE}"
echo "========================================================================"

# Fetch active HTB qdisc classes directly from the Linux host kernel via Agent Pod
get_kernel_tc_classes() {
  local target_node="$1"
  local agent_pod

  agent_pod=$(oc get pods -n ${NAMESPACE} -l app=vlan-traffic-control-agent \
    --field-selector "spec.nodeName=${target_node},status.phase=Running" \
    -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)

  if [ -n "${agent_pod}" ]; then
    local retries=5
    local count="0"
    while [ $retries -gt 0 ]; do
      local raw_output
      raw_output=$(oc exec -n ${NAMESPACE} "${agent_pod}" -- tc class show dev "${INTERFACE}" 2>/dev/null || true)
      
      # Extract class IDs (column 3) via AWK and exclude root (1:1) & fallback (1:99)
      count=$(echo "${raw_output}" | awk '$1=="class" && $2=="htb" { if ($3 != "1:1" && $3 != "1:99") print $3 }' | wc -l | tr -d '[:space:]' || true)
      
      if [ "${count}" -gt 0 ]; then
        break
      fi
      sleep 1
      retries=$((retries - 1))
    done
    echo "${count}"
  else
    echo "0"
  fi
}

# Check if Root HTB Qdisc ("1:") exists on host kernel interface
check_kernel_qdisc_exists() {
  local target_node="$1"
  local agent_pod

  agent_pod=$(oc get pods -n ${NAMESPACE} -l app=vlan-traffic-control-agent \
    --field-selector "spec.nodeName=${target_node},status.phase=Running" \
    -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)

  if [ -n "${agent_pod}" ]; then
    local raw_output
    raw_output=$(oc exec -n ${NAMESPACE} "${agent_pod}" -- tc qdisc show dev "${INTERFACE}" 2>/dev/null || true)
    if echo "${raw_output}" | grep -q "qdisc htb 1:"; then
      echo "true"
    else
      echo "false"
    fi
  else
    echo "false"
  fi
}

# Verification helper function for steps
verify_step() {
  local step_name="$1"
  local exp_worker="$2"
  local exp_master="$3"
  local verify_qdisc_deleted_node="${4:-none}" # Optional: "worker", "master", or "none"

  echo ""
  echo "------------------------------------------------------------------------"
  echo "🔍 [VERIFICATION] ${step_name}"
  echo "Expected User TC Classes -> Workers: ${exp_worker} | Masters: ${exp_master}"
  echo "------------------------------------------------------------------------"

  # Pause for Operator & Agent reconcile cycle to complete in kernel
  sleep 5

  # 1. API Verification via Agent Endpoint (/stats)
  echo "--> Querying Agent API /stats endpoint..."
  local worker_api_classes="0"
  local master_api_classes="0"

  for pod in $(oc get pods -n ${NAMESPACE} -l app=vlan-traffic-control-agent -o name 2>/dev/null || true); do
    local node_name
    node_name=$(oc get "${pod}" -n ${NAMESPACE} -o jsonpath='{.spec.nodeName}' 2>/dev/null || true)

    local user_class_count="0"
    local retries=5
    while [ $retries -gt 0 ]; do
      local stats_json
      stats_json=$(oc exec -n ${NAMESPACE} "${pod}" -- curl -s "http://127.0.0.1:8080/stats?interface=${INTERFACE}" || true)
      if [ -n "${stats_json}" ] && echo "${stats_json}" | jq -e '.classStats' >/dev/null 2>&1; then
        user_class_count=$(echo "${stats_json}" | jq '[.classStats[] | select(.classId != "1:1" and .classId != "1:99")] | length')
        break
      fi
      sleep 2
      retries=$((retries - 1))
    done

    echo "    - Node [${node_name}]: ${user_class_count} user class(es) active"

    if [[ "${node_name}" == "${WORKER_NODE}" ]]; then
      worker_api_classes="${user_class_count}"
    elif [[ "${node_name}" == "${MASTER_NODE}" ]]; then
      master_api_classes="${user_class_count}"
    fi
  done

  # 2. Host Linux Kernel Verification
  echo "--> Kernel Verification ('tc class show dev ${INTERFACE}')..."
  local worker_kernel_classes
  worker_kernel_classes=$(get_kernel_tc_classes "${WORKER_NODE}")

  local master_kernel_classes
  master_kernel_classes=$(get_kernel_tc_classes "${MASTER_NODE}")

  echo "    - Host Kernel [${WORKER_NODE}]: ${worker_kernel_classes} user HTB class(es)"
  echo "    - Host Kernel [${MASTER_NODE}]: ${master_kernel_classes} user HTB class(es)"

  # 3. Optional Qdisc Deletion Verification (Flushed interface check)
  if [[ "${verify_qdisc_deleted_node}" == "worker" ]]; then
    echo "--> Verifying Root HTB Qdisc ('1:') deletion on WORKER nodes..."
    local qdisc_exists
    qdisc_exists=$(check_kernel_qdisc_exists "${WORKER_NODE}")
    if [[ "${qdisc_exists}" == "false" ]]; then
      echo "    ✓ Root HTB qdisc '1:' successfully FLUSHED from ${WORKER_NODE}"
    else
      echo "❌ ERROR: Root HTB qdisc '1:' still exists on ${WORKER_NODE}!"
      exit 1
    fi
  elif [[ "${verify_qdisc_deleted_node}" == "master" ]]; then
    echo "--> Verifying Root HTB Qdisc ('1:') deletion on MASTER nodes..."
    local qdisc_exists
    qdisc_exists=$(check_kernel_qdisc_exists "${MASTER_NODE}")
    if [[ "${qdisc_exists}" == "false" ]]; then
      echo "    ✓ Root HTB qdisc '1:' successfully FLUSHED from ${MASTER_NODE}"
    else
      echo "❌ ERROR: Root HTB qdisc '1:' still exists on ${MASTER_NODE}!"
      exit 1
    fi
  fi

  # Assertions
  if [ "${worker_api_classes}" -eq "${exp_worker}" ] && [ "${master_api_classes}" -eq "${exp_master}" ] && \
     [ "${worker_kernel_classes}" -eq "${exp_worker}" ] && [ "${master_kernel_classes}" -eq "${exp_master}" ]; then
    echo "✅ TEST STEP PASSED!"
  else
    echo "❌ TEST STEP FAILED: State mismatch!"
    echo "   Workers: expected=${exp_worker}, api=${worker_api_classes}, kernel=${worker_kernel_classes}"
    echo "   Masters: expected=${exp_master}, api=${master_api_classes}, kernel=${master_kernel_classes}"
    exit 1
  fi
}

# --- INITIAL CLEANUP PASS ---
echo "🧹 Deleting all existing cluster VlanTrafficControl CRs..."
oc delete vlantrafficcontrol --all --ignore-not-found
sleep 3

# ========================================================================
# STEP 1: First TC rule added to ALL nodes
# ========================================================================
echo ""
echo "▶ STEP 1: Applying 'rule-all-1' (No node restrictions, targeting ALL nodes)..."
oc apply -f - <<EOF
apiVersion: networking.med.io/v1alpha1
kind: VlanTrafficControl
metadata:
  name: rule-all-1
spec:
${MASTER_TOLERATIONS}
  reconcileIntervalSeconds: 30
  tcStrategy: flower
  htbRoot:
    interface: ${INTERFACE}
    htbId: 1
    rate: 10Gbit
    classes:
      - name: global-class-100
        classId: "1:100"
        matchType: subnet
        subnet: 10.0.100.0/24
        egressRate: 100Mbit
EOF
verify_step "Step 1: First TC rule added to ALL nodes" 1 1

# ========================================================================
# STEP 2: Second TC rule added to ALL nodes
# ========================================================================
echo ""
echo "▶ STEP 2: Applying 'rule-all-2' (NodeLabelSelector.matchLabels targeting all)..."
oc apply -f - <<EOF
apiVersion: networking.med.io/v1alpha1
kind: VlanTrafficControl
metadata:
  name: rule-all-2
spec:
${MASTER_TOLERATIONS}
  reconcileIntervalSeconds: 30
  tcStrategy: flower
  htbRoot:
    interface: ${INTERFACE}
    htbId: 1
    rate: 10Gbit
    classes:
      - name: global-class-200
        classId: "1:200"
        matchType: subnet
        subnet: 10.0.200.0/24
        egressRate: 200Mbit
EOF
verify_step "Step 2: Second TC rule added to ALL nodes" 2 2

# ========================================================================
# STEP 3: Third TC rule added to MASTER nodes only
# ========================================================================
echo ""
echo "▶ STEP 3: Applying 'rule-master-3' (nodeSelector & tolerations for Masters)..."
oc apply -f - <<EOF
apiVersion: networking.med.io/v1alpha1
kind: VlanTrafficControl
metadata:
  name: rule-master-3
spec:
  nodeSelector:
    node-role.kubernetes.io/master: ""
${MASTER_TOLERATIONS}
  reconcileIntervalSeconds: 30
  tcStrategy: flower
  htbRoot:
    interface: ${INTERFACE}
    htbId: 1
    rate: 10Gbit
    classes:
      - name: master-class-300
        classId: "1:300"
        matchType: subnet
        subnet: 10.0.218.0/24
        egressRate: 300Mbit
EOF
verify_step "Step 3: Third TC rule added to MASTER nodes only" 2 3

# ========================================================================
# STEP 4: First TC rule removed from WORKER nodes only
# ========================================================================
echo ""
echo "▶ STEP 4: Restricting 'rule-all-1' to target Master nodes exclusively..."
oc apply -f - <<EOF
apiVersion: networking.med.io/v1alpha1
kind: VlanTrafficControl
metadata:
  name: rule-all-1
spec:
  nodeSelector:
    node-role.kubernetes.io/master: ""
${MASTER_TOLERATIONS}
  reconcileIntervalSeconds: 30
  tcStrategy: flower
  htbRoot:
    interface: ${INTERFACE}
    htbId: 1
    rate: 10Gbit
    classes:
      - name: global-class-100
        classId: "1:100"
        matchType: subnet
        subnet: 10.0.100.0/24
        egressRate: 100Mbit
EOF
verify_step "Step 4: First TC rule removed from WORKER nodes only" 1 3

# ========================================================================
# STEP 5: Second TC rule removed from WORKER nodes only (Interface Flushed)
# ========================================================================
echo ""
echo "▶ STEP 5: Restricting 'rule-all-2' nodeLabelSelector to target Master nodes exclusively..."
oc apply -f - <<EOF
apiVersion: networking.med.io/v1alpha1
kind: VlanTrafficControl
metadata:
  name: rule-all-2
spec:
  nodeLabelSelector:
    matchLabels:
      node-role.kubernetes.io/master: ""
${MASTER_TOLERATIONS}
  reconcileIntervalSeconds: 30
  tcStrategy: flower
  htbRoot:
    interface: ${INTERFACE}
    htbId: 1
    rate: 10Gbit
    classes:
      - name: global-class-200
        classId: "1:200"
        matchType: subnet
        subnet: 10.0.200.0/24
        egressRate: 200Mbit
EOF
verify_step "Step 5: Second TC rule removed from WORKERS (Worker interface flushed)" 0 3 "worker"

# ========================================================================
# STEP 6: First TC rule re-added to ALL nodes
# ========================================================================
echo ""
echo "▶ STEP 6: Re-applying 'rule-all-1' to target ALL nodes..."
oc apply -f - <<EOF
apiVersion: networking.med.io/v1alpha1
kind: VlanTrafficControl
metadata:
  name: rule-all-1
spec:
${MASTER_TOLERATIONS}
  reconcileIntervalSeconds: 30
  tcStrategy: flower
  htbRoot:
    interface: ${INTERFACE}
    htbId: 1
    rate: 10Gbit
    classes:
      - name: global-class-100
        classId: "1:100"
        matchType: subnet
        subnet: 10.0.100.0/24
        egressRate: 100Mbit
EOF
verify_step "Step 6: First TC rule re-added to ALL nodes" 1 3

# ========================================================================
# STEP 7: Second TC rule re-added to ALL nodes
# ========================================================================
echo ""
echo "▶ STEP 7: Re-applying 'rule-all-2' to target ALL nodes..."
oc apply -f - <<EOF
apiVersion: networking.med.io/v1alpha1
kind: VlanTrafficControl
metadata:
  name: rule-all-2
spec:
${MASTER_TOLERATIONS}
  reconcileIntervalSeconds: 30
  tcStrategy: flower
  htbRoot:
    interface: ${INTERFACE}
    htbId: 1
    rate: 10Gbit
    classes:
      - name: global-class-200
        classId: "1:200"
        matchType: subnet
        subnet: 10.0.200.0/24
        egressRate: 200Mbit
EOF
verify_step "Step 7: Second TC rule re-added to ALL nodes" 2 3

# ========================================================================
# STEP 8: Third TC rule added to WORKER nodes only
# ========================================================================
echo ""
echo "▶ STEP 8: Applying 'rule-worker-3' (NodeLabelSelector.matchExpressions Exists for Workers)..."
oc apply -f - <<EOF
apiVersion: networking.med.io/v1alpha1
kind: VlanTrafficControl
metadata:
  name: rule-worker-3
spec:
  nodeLabelSelector:
    matchExpressions:
      - key: "node-role.kubernetes.io/worker"
        operator: "Exists"
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
verify_step "Step 8: Third TC rule added to WORKER nodes only" 3 3

# ========================================================================
# STEP 9: All TC rules removed from MASTER nodes (Interface Flushed)
# ========================================================================
echo ""
echo "▶ STEP 9: Removing all TC rules from MASTER nodes..."
# 1. Delete Master-only rule
oc delete vlantrafficcontrol rule-master-3

# 2. Restrict global rules to Workers only
oc apply -f - <<EOF
apiVersion: networking.med.io/v1alpha1
kind: VlanTrafficControl
metadata:
  name: rule-all-1
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
      - name: global-class-100
        classId: "1:100"
        matchType: subnet
        subnet: 10.0.100.0/24
        egressRate: 100Mbit
EOF

oc apply -f - <<EOF
apiVersion: networking.med.io/v1alpha1
kind: VlanTrafficControl
metadata:
  name: rule-all-2
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
      - name: global-class-200
        classId: "1:200"
        matchType: subnet
        subnet: 10.0.200.0/24
        egressRate: 200Mbit
EOF
verify_step "Step 9: All TC rules removed from MASTER nodes (Master interface flushed)" 3 0 "master"

# --- FINAL CLEANUP PASS ---
echo ""
echo "🧹 Final Cleanup: Deleting all created test CRs..."
oc delete vlantrafficcontrol --all --ignore-not-found

echo ""
echo "========================================================================"
echo "🎉 SUCCESS: All 9 targeting and deletion steps passed perfectly!"
echo "========================================================================"
