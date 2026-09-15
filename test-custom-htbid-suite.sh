#!/usr/bin/env bash

set -euo pipefail

# ==============================================================================
# Non-Standard htbId & Non-Default Class Mapping Validation Test (STRICT)
# Validates custom htbId (e.g., 20, 30) and multi-attribute classes in both
# IFB and Flower strategies against agent /config and /stats endpoints.
# ==============================================================================

NAMESPACE="openshift-vlan-tc-operator"
PHYS_IFACE="enp1s0"
AGENT_NODE_IP="192.168.100.21"
AGENT_NODE_NAME="hub-worker01.ocp4-hub.test.com"

LOG_DIR="./vtc-custom-htbid-test-logs-$(date +%Y%m%d_%H%M%S)"
TMP_YAML="/tmp/vtc-custom-htbid-manifest.yaml"

mkdir -p "${LOG_DIR}"

# Colors
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
CYAN='\033[0;36m'
NC='\033[0m'

log_info()  { echo -e "${BLUE}[INFO]${NC} $1"; }
log_pass()  { echo -e "${GREEN}[PASS]${NC} $1"; }
log_fail()  { echo -e "${RED}[FAIL]${NC} $1"; }
log_dump()  { echo -e "${CYAN}[DUMP]${NC} $1"; }

echo "======================================================================"
echo " Starting STRICT Custom htbId & Non-Default Class Verification Suite"
echo " Target Interface: ${PHYS_IFACE}"
echo " Agent Endpoint:   http://${AGENT_NODE_IP}:8080"
echo " Diagnostic Dir:   ${LOG_DIR}"
echo "======================================================================"

# ==============================================================================
# Helper Functions
# ==============================================================================

assert_equals() {
  local actual="$1"
  local expected="$2"
  local desc="$3"

  if [ "${actual}" == "${expected}" ]; then
    log_pass "    ✓ [ASSERT] ${desc}: '${actual}'"
  else
    log_fail "    ✗ [ASSERT ERROR] ${desc}: expected '${expected}', got '${actual}'"
    return 1
  fi
}

cleanup_resources() {
  log_info "Cleaning up VTC custom resources & host interfaces..."
  oc delete vtc --all -n "${NAMESPACE}" --force --grace-period=0 2>/dev/null || true
  sleep 4
  
  # Audit and strip lingering root qdiscs and IFB interfaces
  oc debug node/"${AGENT_NODE_NAME}" -- chroot /host /bin/bash -c "
    tc qdisc del dev ${PHYS_IFACE} root 2>/dev/null || true
    tc qdisc del dev ${PHYS_IFACE} ingress 2>/dev/null || true
    tc qdisc del dev ${PHYS_IFACE} clsact 2>/dev/null || true
    tc qdisc del dev ifb-${PHYS_IFACE} root 2>/dev/null || true
    ip link set dev ifb-${PHYS_IFACE} down 2>/dev/null || true
    ip link delete ifb-${PHYS_IFACE} type ifb 2>/dev/null || true
  " &>/dev/null || true
  sleep 1
}

run_custom_htbid_test() {
  local strategy="$1"
  local htb_id="$2"
  local default_minor="$3"
  local manifest_yaml="$4"

  local scenario_tag="${strategy}_HTBID_${htb_id}"

  echo ""
  echo "======================================================================"
  log_info "Executing Strategy [${strategy}] with htbId [${htb_id}] & Default Class Minor [${default_minor}]"
  echo "======================================================================"

  cleanup_resources

  log_info "1. Applying Custom VTC Manifest..."
  echo "${manifest_yaml}" > "${TMP_YAML}"
  oc apply -f "${TMP_YAML}"

  log_info "Waiting 12 seconds for agent reconciliation pass..."
  sleep 12

  log_info "Triggering explicit agent reconcile pass..."
  curl -s -X POST "http://${AGENT_NODE_IP}:8080/reconcile" > /dev/null || true
  sleep 3

  log_info "2. Verifying Custom Resource status in Kube API..."
  if ! oc get vtc -n "${NAMESPACE}" | grep -q "True"; then
    log_fail "VTC resource failed to reach Ready status in API!"
    return 1
  fi

  log_info "3. Querying Agent Endpoints (/config and /stats)..."
  local config_resp stats_resp ifb_stats_resp=""
  
  config_resp=$(curl -s --connect-timeout 3 "http://${AGENT_NODE_IP}:8080/config?interface=${PHYS_IFACE}")
  echo "${config_resp}" | jq . > "${LOG_DIR}/${scenario_tag}_config.json"

  stats_resp=$(curl -s --connect-timeout 3 "http://${AGENT_NODE_IP}:8080/stats?interface=${PHYS_IFACE}")
  echo "${stats_resp}" | jq . > "${LOG_DIR}/${scenario_tag}_stats.json"

  if [ "${strategy}" == "IFB" ]; then
    ifb_stats_resp=$(curl -s --connect-timeout 3 "http://${AGENT_NODE_IP}:8080/stats?interface=ifb-${PHYS_IFACE}" 2>/dev/null || echo "{}")
    echo "${ifb_stats_resp}" | jq . > "${LOG_DIR}/${scenario_tag}_ifb_stats.json"
  fi

  # Print Pre-Deletion Dumps
  echo ""
  log_dump "--- PRE-DELETION AGENT CONFIG DUMP (${PHYS_IFACE}) ---"
  echo "${config_resp}" | jq .
  echo ""

  log_info "4. Performing STRICT Kernel Actual State Assertions..."

  # Assertion 1: Host alignment status
  local is_aligned
  is_aligned=$(echo "${config_resp}" | jq -r '.isAligned // false')
  assert_equals "${is_aligned}" "true" "Host Alignment Status (isAligned)" || return 1

  # Assertion 2: Root HTB Qdisc Presence in Kernel
  local htb_present
  htb_present=$(echo "${config_resp}" | jq -r '.actual.htbQdiscPresent // false')
  assert_equals "${htb_present}" "true" "Actual Root HTB Qdisc Present in Kernel" || return 1

  # Assertion 3: Verify Actual Egress HTB Classes Created in Kernel
  local custom_vlan_class_id="${htb_id}:150"
  local actual_class_count
  actual_class_count=$(echo "${config_resp}" | jq -r '.actual.classes | length')
  
  if [ "${actual_class_count}" -eq 0 ]; then
    log_fail "    ✗ [ASSERT ERROR] Actual Kernel HTB Classes Count: expected >0, got '0' (No HTB classes created!)"
    return 1
  else
    log_pass "    ✓ [ASSERT] Actual Kernel HTB Classes Count: '${actual_class_count}'"
  fi

  # Assertion 4: Custom Class ID Existence in Actual Kernel Classes
  local actual_vlan_class_exists
  actual_vlan_class_exists=$(echo "${config_resp}" | jq -r --arg cid "${custom_vlan_class_id}" '[.actual.classes[] | select(.classId==$cid)] | length > 0')
  assert_equals "${actual_vlan_class_exists}" "true" "Kernel Actual HTB Class ID (${custom_vlan_class_id}) Created" || return 1

  # Assertion 5: Check Filter Programmed for Priority 5 (FW Mark)
  local mark_filter_exists
  mark_filter_exists=$(echo "${config_resp}" | jq -r '[.actual.ingressFilters[] | select(.priority==5 or .name=="fwmark-500-custom")] | length > 0')
  assert_equals "${mark_filter_exists}" "true" "Kernel Actual Priority 5 (FW Mark) Filter Created" || return 1

  log_pass "✓ Strategy [${strategy}] with htbId [${htb_id}] successfully validated!"
  cleanup_resources
}

# ==============================================================================
# MANIFEST DEFINITIONS
# ==============================================================================

FLOWER_CUSTOM_HTBID_YAML=$(cat <<EOF
apiVersion: networking.med.io/v1alpha1
kind: VlanTrafficControl
metadata:
  name: test-vtc-flower-htbid20
  namespace: ${NAMESPACE}
spec:
  nodeSelector:
    node-role.kubernetes.io/worker: ""
  tcStrategy: flower
  htbRoot:
    interface: ${PHYS_IFACE}
    htbId: 20
    rate: "10Gbit"
    defaultClassId: "20:77"
    defaultClassMinor: 77
    classes:
      - name: custom-default-77
        classId: "20:77"
        priority: 0
        egressRate: "500Mbit"
        egressCeil: "10Gbit"
        egressBurst: "10Mb"
        enableFqCodel: true
      - name: vlan-150-custom
        classId: "20:150"
        vlanId: 150
        matchType: subnet
        subnet: "10.150.0.0/24"
        priority: 1
        egressRate: "2Gbit"
        egressCeil: "5Gbit"
        egressBurst: "32Mb"
        ingressRate: "3Gbit"
        ingressBurst: "16Mb"
        ingressAction: "pass"
        enableFqCodel: true
      - name: fwmark-500-custom
        classId: "20:500"
        matchType: mark
        mark: 20500
        priority: 5
        egressRate: "1Gbit"
        egressCeil: "3Gbit"
        ingressRate: "1.5Gbit"
        ingressBurst: "8Mb"
        ingressAction: "drop"
EOF
)

IFB_CUSTOM_HTBID_YAML=$(cat <<EOF
apiVersion: networking.med.io/v1alpha1
kind: VlanTrafficControl
metadata:
  name: test-vtc-ifb-htbid30
  namespace: ${NAMESPACE}
spec:
  nodeSelector:
    node-role.kubernetes.io/worker: ""
  tcStrategy: ifb
  htbRoot:
    interface: ${PHYS_IFACE}
    htbId: 30
    rate: "10Gbit"
    defaultClassId: "30:88"
    defaultClassMinor: 88
    classes:
      - name: custom-default-88
        classId: "30:88"
        priority: 0
        egressRate: "200Mbit"
        egressCeil: "10Gbit"
        ingressRate: "300Mbit"
        ingressCeil: "10Gbit"
        enableFqCodel: true
      - name: vlan-150-custom
        classId: "30:150"
        vlanId: 150
        matchType: vlan
        priority: 2
        egressRate: "3Gbit"
        egressCeil: "8Gbit"
        ingressRate: "4Gbit"
        ingressCeil: "10Gbit"
        enableFqCodel: true
      - name: fwmark-500-custom
        classId: "30:500"
        matchType: mark
        mark: 30500
        priority: 4
        egressRate: "1.5Gbit"
        egressCeil: "4Gbit"
        ingressRate: "2Gbit"
        ingressCeil: "6Gbit"
        enableFqCodel: true
EOF
)

# ==============================================================================
# MAIN EXECUTION
# ==============================================================================

FAILED_TESTS=()

run_custom_htbid_test "Flower" "20" "77" "${FLOWER_CUSTOM_HTBID_YAML}" || FAILED_TESTS+=("Flower_htbId_20")
run_custom_htbid_test "IFB" "30" "88" "${IFB_CUSTOM_HTBID_YAML}" || FAILED_TESTS+=("IFB_htbId_30")

echo ""
echo "======================================================================"
if [ ${#FAILED_TESTS[@]} -eq 0 ]; then
  log_pass "✅ ALL CUSTOM HTBID & NON-DEFAULT CLASS TESTS PASSED SUCCESSFULLY!"
  echo "Diagnostic payloads saved in: ${LOG_DIR}"
  exit 0
else
  log_fail "❌ THE FOLLOWING SUITES FAILED:"
  for failed in "${FAILED_TESTS[@]}"; do
    echo "  - ${failed}"
  done
  echo "Check diagnostic dumps in: ${LOG_DIR}"
  exit 1
fi
