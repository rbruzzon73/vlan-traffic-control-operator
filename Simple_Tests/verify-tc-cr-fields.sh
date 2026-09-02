#!/usr/bin/env bash

set -euo pipefail

# ==============================================================================
# CLI HELP MENU & USAGE MANUAL
# ==============================================================================
show_help() {
  cat << 'EOF'
VlanTrafficControl CRD Dynamic Field Verification Suite

USAGE:
  ./verify-tc-cr-fields.sh [FLAGS]
  [ENV_VARS...] ./verify-tc-cr-fields.sh

DESCRIPTION:
  Applies a comprehensive VlanTrafficControl Custom Resource (CR) manifest to 
  the cluster containing all configurable parameters (VLAN, Subnet, Mark, Auto),
  triggers an agent reconciliation pass, and validates that the host Linux kernel
  and Node Alignment REST API (/config) accurately reflect all target settings.

FLAGS:
  -h, --help    Show this detailed help manual and exit.

CONFIGURATION ARGUMENTS & SCOPE:

  1. TARGET & CLUSTER SCOPE
     TARGET_WORKER              Specific worker node name to target for tests.
                                (Default: "" -> Auto-discovers first worker node)
     NAMESPACE                  Kubernetes namespace for operator & agent pods.
                                (Default: "openshift-vlan-tc-operator")
     CR_NAME                    Name of the test VlanTrafficControl CR instance.
                                (Default: "verify-all-fields-tc")
     MANAGER_DEPLOY             Manager deployment resource identifier.
                                (Default: "deploy/vlan-traffic-control-manager")
     AGENT_LABEL                Pod label selector for agent instances.
                                (Default: "app=vlan-traffic-control-agent")

  2. ROOT HTB CONFIGURATION (htbRoot)
     TEST_IFACE                 Target host network interface (e.g., enp1s0, eth0).
                                (Default: "enp1s0")
     TEST_ROOT_RATE             Overall link capacity ceiling for parent root class.
                                (Default: "10Gbit")
     TEST_ROOT_HTB_ID           Root HTB qdisc handle ID (e.g., 1 -> 1:, 6 -> 6:).
                                (Default: 1)
     TEST_DEFAULT_CLASS_ID      Class ID for unclassified fallback traffic.
                                (Default: "1:99")
     TEST_RECONCILE_INTERVAL    Operator status sync interval in seconds.
                                (Default: 15)
     TEST_STRATEGY              Classifier selection strategy ("flower" or "fw").
                                (Default: "flower")

  3. CLASS 1: 802.1Q VLAN CLASSIFICATION (matchType: vlan)
     VLAN_CLASS_NAME            Class descriptor name (Default: "test-vlan-class")
     VLAN_CLASS_ID              HTB Class Handle (Default: "1:100")
     VLAN_ID                    802.1Q VLAN Tag ID to match (Default: 100)
     VLAN_PRIO                  HTB Priority 1-8 (Default: 1)
     VLAN_EGRESS_RATE           Guaranteed egress rate (Default: "50Mbit")
     VLAN_EGRESS_CEIL           Burst ceiling egress rate (Default: "100Mbit")
     VLAN_EGRESS_BURST          HTB egress burst size (Default: "15k")
     VLAN_INGRESS_RATE          Ingress policing rate (Default: "25Mbit")
     VLAN_INGRESS_BURST         Ingress policing burst size (Default: "50k")
     VLAN_INGRESS_ACTION        Policing violation action (Default: "drop")
     VLAN_ENABLE_FQCODEL        Attach child fq_codel qdisc (Default: "true")

  4. CLASS 2: IP SUBNET CLASSIFICATION (matchType: subnet)
     SUBNET_CLASS_NAME          Class descriptor name (Default: "test-subnet-class")
     SUBNET_CLASS_ID            HTB Class Handle (Default: "1:200")
     SUBNET_CIDR                IP Subnet CIDR to match (Default: "10.200.0.0/24")
     SUBNET_PRIO                HTB Priority 1-8 (Default: 2)
     SUBNET_EGRESS_RATE         Guaranteed egress rate (Default: "200Mbit")
     SUBNET_EGRESS_CEIL         Burst ceiling egress rate (Default: "400Mbit")
     SUBNET_EGRESS_BURST        HTB egress burst size (Default: "30k")
     SUBNET_INGRESS_RATE        Ingress policing rate (Default: "100Mbit")
     SUBNET_INGRESS_BURST       Ingress policing burst size (Default: "60k")
     SUBNET_INGRESS_ACTION      Policing violation action (Default: "drop")
     SUBNET_ENABLE_FQCODEL      Attach child fq_codel qdisc (Default: "true")

  5. CLASS 3: SKB MARK CLASSIFICATION (matchType: mark)
     MARK_CLASS_NAME            Class descriptor name (Default: "test-mark-class")
     MARK_CLASS_ID              HTB Class Handle (Default: "1:300")
     MARK_VAL                   32-bit Socket Buffer Mark integer (Default: 8)
     MARK_PRIO                  HTB Priority 1-8 (Default: 3)
     MARK_EGRESS_RATE           Guaranteed egress rate (Default: "300Mbit")
     MARK_EGRESS_CEIL           Burst ceiling egress rate (Default: "300Mbit")
     MARK_EGRESS_BURST          HTB egress burst size (Default: "20k")
     MARK_INGRESS_RATE          Ingress policing rate (Default: "50Mbit")
     MARK_INGRESS_BURST         Ingress policing burst size (Default: "20k")
     MARK_INGRESS_ACTION        Policing violation action (Default: "drop")
     MARK_ENABLE_FQCODEL        Attach child fq_codel qdisc (Default: "true")

  6. CLASS 4: AUTO-DETECTION CLASSIFICATION (matchType: auto)
     AUTO_CLASS_NAME            Class descriptor name (Default: "test-auto-class")
     AUTO_CLASS_ID              HTB Class Handle (Default: "1:400")
     AUTO_VLAN_ID               Auto-detected VLAN Tag ID (Default: 400)
     AUTO_PRIO                  HTB Priority 1-8 (Default: 4)
     AUTO_EGRESS_RATE           Guaranteed egress rate (Default: "500Mbit")
     AUTO_EGRESS_CEIL           Burst ceiling egress rate (Default: "1Gbit")
     AUTO_EGRESS_BURST          HTB egress burst size (Default: "25k")
     AUTO_INGRESS_RATE          Ingress policing rate (Default: "150Mbit")
     AUTO_INGRESS_BURST         Ingress policing burst size (Default: "40k")
     AUTO_INGRESS_ACTION        Policing violation action (Default: "drop")
     AUTO_ENABLE_FQCODEL        Attach child fq_codel qdisc (Default: "false")

EXAMPLES:

  1. Target a specific worker node explicitly with defaults:
     $ TARGET_WORKER="hub-worker01.ocp4-hub.test.com" ./verify-tc-cr-fields.sh

  2. Test explicit Interface, VLAN IDs, and Subnet on target node:
     $ TARGET_WORKER="hub-worker02.ocp4-hub.test.com" \
       TEST_IFACE="enp1s0" \
       VLAN_ID=150 \
       AUTO_VLAN_ID=250 \
       SUBNET_CIDR="172.16.50.0/24" \
       ./verify-tc-cr-fields.sh

  3. Test custom non-default HTB Root Handle (htbId: 6) with explicit Interface, VLANs, and Subnet:
     $ TARGET_WORKER="hub-worker01.ocp4-hub.test.com" \
       TEST_IFACE="enp1s0" \
       TEST_ROOT_HTB_ID=6 \
       TEST_DEFAULT_CLASS_ID="6:99" \
       VLAN_CLASS_ID="6:100" \
       VLAN_ID=150 \
       SUBNET_CLASS_ID="6:200" \
       SUBNET_CIDR="172.16.50.0/24" \
       MARK_CLASS_ID="6:300" \
       MARK_VAL=16 \
       AUTO_CLASS_ID="6:400" \
       AUTO_VLAN_ID=300 \
       ./verify-tc-cr-fields.sh

  4. Test secondary NIC (ens1f0) with custom Subnet and VLAN rates:
     $ TARGET_WORKER="hub-worker02.ocp4-hub.test.com" \
       TEST_IFACE="ens1f0" \
       VLAN_ID=200 \
       VLAN_EGRESS_RATE="1Gbit" \
       VLAN_INGRESS_RATE="500Mbit" \
       SUBNET_CIDR="10.100.0.0/16" \
       SUBNET_EGRESS_RATE="2Gbit" \
       ./verify-tc-cr-fields.sh

EOF
  exit 0
}

# Parse CLI flags
if [[ "${1:-}" == "-h" ]] || [[ "${1:-}" == "--help" ]]; then
  show_help
fi

# ==============================================================================
# CONFIGURATION & CUSTOMIZABLE PARAMETERS (OVERRIDABLE VIA ENV VARS)
# ==============================================================================
TARGET_WORKER="${TARGET_WORKER:-}"
NAMESPACE="${NAMESPACE:-openshift-vlan-tc-operator}"
CR_NAME="${CR_NAME:-verify-all-fields-tc}"
MANAGER_DEPLOY="${MANAGER_DEPLOY:-deploy/vlan-traffic-control-manager}"
AGENT_LABEL="${AGENT_LABEL:-app=vlan-traffic-control-agent}"

# Root Configuration
TEST_IFACE="${TEST_IFACE:-enp1s0}"
TEST_ROOT_RATE="${TEST_ROOT_RATE:-10Gbit}"
TEST_ROOT_HTB_ID="${TEST_ROOT_HTB_ID:-1}"
TEST_DEFAULT_CLASS_ID="${TEST_DEFAULT_CLASS_ID:-1:99}"
TEST_RECONCILE_INTERVAL="${TEST_RECONCILE_INTERVAL:-15}"
TEST_STRATEGY="${TEST_STRATEGY:-flower}"

# Class 1: VLAN Class (1:100)
VLAN_CLASS_NAME="${VLAN_CLASS_NAME:-test-vlan-class}"
VLAN_CLASS_ID="${VLAN_CLASS_ID:-1:100}"
VLAN_ID="${VLAN_ID:-100}"
VLAN_PRIO="${VLAN_PRIO:-1}"
VLAN_EGRESS_RATE="${VLAN_EGRESS_RATE:-50Mbit}"
VLAN_EGRESS_CEIL="${VLAN_EGRESS_CEIL:-100Mbit}"
VLAN_EGRESS_BURST="${VLAN_EGRESS_BURST:-15k}"
VLAN_INGRESS_RATE="${VLAN_INGRESS_RATE:-25Mbit}"
VLAN_INGRESS_BURST="${VLAN_INGRESS_BURST:-50k}"
VLAN_INGRESS_ACTION="${VLAN_INGRESS_ACTION:-drop}"
VLAN_ENABLE_FQCODEL="${VLAN_ENABLE_FQCODEL:-true}"

# Class 2: Subnet Class (1:200)
SUBNET_CLASS_NAME="${SUBNET_CLASS_NAME:-test-subnet-class}"
SUBNET_CLASS_ID="${SUBNET_CLASS_ID:-1:200}"
SUBNET_CIDR="${SUBNET_CIDR:-10.200.0.0/24}"
SUBNET_PRIO="${SUBNET_PRIO:-2}"
SUBNET_EGRESS_RATE="${SUBNET_EGRESS_RATE:-200Mbit}"
SUBNET_EGRESS_CEIL="${SUBNET_EGRESS_CEIL:-400Mbit}"
SUBNET_EGRESS_BURST="${SUBNET_EGRESS_BURST:-30k}"
SUBNET_INGRESS_RATE="${SUBNET_INGRESS_RATE:-100Mbit}"
SUBNET_INGRESS_BURST="${SUBNET_INGRESS_BURST:-60k}"
SUBNET_INGRESS_ACTION="${SUBNET_INGRESS_ACTION:-drop}"
SUBNET_ENABLE_FQCODEL="${SUBNET_ENABLE_FQCODEL:-true}"

# Class 3: SKB Mark Class (1:300)
MARK_CLASS_NAME="${MARK_CLASS_NAME:-test-mark-class}"
MARK_CLASS_ID="${MARK_CLASS_ID:-1:300}"
MARK_VAL="${MARK_VAL:-8}"
MARK_PRIO="${MARK_PRIO:-3}"
MARK_EGRESS_RATE="${MARK_EGRESS_RATE:-300Mbit}"
MARK_EGRESS_CEIL="${MARK_EGRESS_CEIL:-300Mbit}"
MARK_EGRESS_BURST="${MARK_EGRESS_BURST:-20k}"
MARK_INGRESS_RATE="${MARK_INGRESS_RATE:-50Mbit}"
MARK_INGRESS_BURST="${MARK_INGRESS_BURST:-20k}"
MARK_INGRESS_ACTION="${MARK_INGRESS_ACTION:-drop}"
MARK_ENABLE_FQCODEL="${MARK_ENABLE_FQCODEL:-true}"

# Class 4: Auto-detect Class (1:400)
AUTO_CLASS_NAME="${AUTO_CLASS_NAME:-test-auto-class}"
AUTO_CLASS_ID="${AUTO_CLASS_ID:-1:400}"
AUTO_VLAN_ID="${AUTO_VLAN_ID:-400}"
AUTO_PRIO="${AUTO_PRIO:-4}"
AUTO_EGRESS_RATE="${AUTO_EGRESS_RATE:-500Mbit}"
AUTO_EGRESS_CEIL="${AUTO_EGRESS_CEIL:-1Gbit}"
AUTO_EGRESS_BURST="${AUTO_EGRESS_BURST:-25k}"
AUTO_INGRESS_RATE="${AUTO_INGRESS_RATE:-150Mbit}"
AUTO_INGRESS_BURST="${AUTO_INGRESS_BURST:-40k}"
AUTO_INGRESS_ACTION="${AUTO_INGRESS_ACTION:-drop}"
AUTO_ENABLE_FQCODEL="${AUTO_ENABLE_FQCODEL:-false}"

# Derived hex format for mark display
MARK_VAL_HEX=$(printf "0x%x" "${MARK_VAL}")

# Global state tracking
TEST_FAILED=false
WORKER_NODE=""
AGENT_POD=""
AGENT_IP=""

# Output Formatting
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
CYAN='\033[0;36m'
NC='\033[0m' # No Color

echo -e "${BLUE}========================================================================${NC}"
echo -e "${BLUE}🧪 Starting Dynamic Field Verification for VlanTrafficControl CRD${NC}"
echo -e "${BLUE}   • Target Interface : ${YELLOW}${TEST_IFACE}${NC} (Rate: ${TEST_ROOT_RATE}, Root Handle: ${TEST_ROOT_HTB_ID}:)"
echo -e "${BLUE}   • VLAN Configured  : ID ${YELLOW}${VLAN_ID}${NC} (${VLAN_EGRESS_RATE} / ${VLAN_INGRESS_RATE})"
echo -e "${BLUE}   • Subnet Configured: ${YELLOW}${SUBNET_CIDR}${NC} (${SUBNET_EGRESS_RATE} / ${SUBNET_INGRESS_RATE})"
echo -e "${BLUE}   • Mark Configured  : ${YELLOW}${MARK_VAL}${NC} [${MARK_VAL_HEX}] (${MARK_EGRESS_RATE} / ${MARK_INGRESS_RATE})"
echo -e "${BLUE}   • Auto Configured  : VLAN ${YELLOW}${AUTO_VLAN_ID}${NC} (${AUTO_EGRESS_RATE} / ${AUTO_INGRESS_RATE})"
echo -e "${BLUE}========================================================================${NC}"

# ==============================================================================
# DIAGNOSTIC & EVIDENCE FUNCTIONS
# ==============================================================================
dump_evidence() {
  local title="$1"
  echo -e "\n${CYAN}========================================================================${NC}"
  echo -e "${CYAN}📸 SETUP EVIDENCE SUMMARY: ${title}${NC}"
  echo -e "${CYAN}========================================================================${NC}"

  echo -e "\n${BLUE}1. Active Kernel HTB Classes on '${TEST_IFACE}':${NC}"
  oc exec -n "${NAMESPACE}" "${AGENT_POD}" -- tc class show dev "${TEST_IFACE}" 2>/dev/null || true

  echo -e "\n${BLUE}2. Active Kernel Leaf Qdiscs on '${TEST_IFACE}':${NC}"
  oc exec -n "${NAMESPACE}" "${AGENT_POD}" -- tc qdisc show dev "${TEST_IFACE}" 2>/dev/null || true

  echo -e "\n${BLUE}3. Active Kernel Ingress Policing Filters on '${TEST_IFACE}':${NC}"
  oc exec -n "${NAMESPACE}" "${AGENT_POD}" -- tc filter show dev "${TEST_IFACE}" parent ffff: 2>/dev/null || true

  if [[ -n "${AGENT_IP}" ]]; then
    echo -e "\n${BLUE}4. Node Agent REST API Config Report (/config):${NC}"
    oc exec -n "${NAMESPACE}" "${MANAGER_DEPLOY}" -- \
      curl -s "http://${AGENT_IP}:8080/config?interface=${TEST_IFACE}" 2>/dev/null | jq '{
        node: .node,
        interface: .interface,
        isAligned: .isAligned,
        actualClasses: .actual.classes,
        actualIngressFilters: .actual.ingressFilters,
        driftDeltas: .driftDeltas
      }' || true
  fi
  echo -e "${CYAN}========================================================================${NC}"
}

cleanup() {
  if [[ "${TEST_FAILED}" == "true" ]]; then
    dump_evidence "TEST FAILURE DIAGNOSTICS"
  fi

  echo -e "\n${YELLOW}🧹 Cleaning up verification CR '${CR_NAME}'...${NC}"
  oc delete vlantrafficcontrol "${CR_NAME}" --ignore-not-found=true
}

# Ensure cleanup runs on exit
trap cleanup EXIT

fail_test() {
  TEST_FAILED=true
  echo -e "\n${RED}❌ VERIFICATION FAILED: $1${NC}"
  exit 1
}

pass_step() {
  echo -e "${GREEN}✅ PASSED: $1${NC}"
}

# ==============================================================================
# 1. DISCOVER / VALIDATE TARGET WORKER NODE
# ==============================================================================
echo -e "\n🔍 Resolving target worker node..."

if [[ -n "${TARGET_WORKER}" ]]; then
  # Validate that user-provided TARGET_WORKER actually exists in cluster
  if ! oc get node "${TARGET_WORKER}" >/dev/null 2>&1; then
    fail_test "User-specified TARGET_WORKER node '${TARGET_WORKER}' does not exist in cluster."
  fi
  WORKER_NODE="${TARGET_WORKER}"
  echo -e "   • Target Worker Node : ${YELLOW}${WORKER_NODE}${NC} (Specified via TARGET_WORKER)"
else
  # Auto-discover first available worker node
  WORKER_NODE=$(oc get nodes -l node-role.kubernetes.io/worker -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)
  if [[ -z "${WORKER_NODE}" ]]; then
    WORKER_NODE=$(oc get nodes --no-headers -l node-role.kubernetes.io/worker | awk '{print $1}' | head -n 1)
  fi

  if [[ -z "${WORKER_NODE}" ]]; then
    fail_test "No worker nodes found in cluster."
  fi
  echo -e "   • Target Worker Node : ${YELLOW}${WORKER_NODE}${NC} (Auto-discovered)"
fi

# ==============================================================================
# 2. APPLY MANIFEST WITH ALL CUSTOMIZABLE PARAMETERS & TARGET NODE SELECTOR
# ==============================================================================
echo -e "\n📝 Applying VlanTrafficControl CR targeting node '${WORKER_NODE}'..."

cat <<EOF | oc apply -f -
apiVersion: networking.med.io/v1alpha1
kind: VlanTrafficControl
metadata:
  name: ${CR_NAME}
spec:
  nodeSelector:
    kubernetes.io/hostname: "${WORKER_NODE}"
  tolerations:
    - key: "node-role.kubernetes.io/master"
      operator: "Exists"
      effect: "NoSchedule"
  reconcileIntervalSeconds: ${TEST_RECONCILE_INTERVAL}
  tcStrategy: "${TEST_STRATEGY}"
  htbRoot:
    interface: "${TEST_IFACE}"
    rate: "${TEST_ROOT_RATE}"
    defaultClassId: "${TEST_DEFAULT_CLASS_ID}"
    htbId: ${TEST_ROOT_HTB_ID}
    classes:
      # 1. Full spec using matchType: vlan
      - name: "${VLAN_CLASS_NAME}"
        matchType: "vlan"
        classId: "${VLAN_CLASS_ID}"
        vlanId: ${VLAN_ID}
        priority: ${VLAN_PRIO}
        egressRate: "${VLAN_EGRESS_RATE}"
        egressCeil: "${VLAN_EGRESS_CEIL}"
        egressBurst: "${VLAN_EGRESS_BURST}"
        ingressRate: "${VLAN_INGRESS_RATE}"
        ingressBurst: "${VLAN_INGRESS_BURST}"
        ingressAction: "${VLAN_INGRESS_ACTION}"
        enableFqCodel: ${VLAN_ENABLE_FQCODEL}

      # 2. Full spec using matchType: subnet
      - name: "${SUBNET_CLASS_NAME}"
        matchType: "subnet"
        classId: "${SUBNET_CLASS_ID}"
        subnet: "${SUBNET_CIDR}"
        priority: ${SUBNET_PRIO}
        egressRate: "${SUBNET_EGRESS_RATE}"
        egressCeil: "${SUBNET_EGRESS_CEIL}"
        egressBurst: "${SUBNET_EGRESS_BURST}"
        ingressRate: "${SUBNET_INGRESS_RATE}"
        ingressBurst: "${SUBNET_INGRESS_BURST}"
        ingressAction: "${SUBNET_INGRESS_ACTION}"
        enableFqCodel: ${SUBNET_ENABLE_FQCODEL}

      # 3. Full spec using matchType: mark
      - name: "${MARK_CLASS_NAME}"
        matchType: "mark"
        classId: "${MARK_CLASS_ID}"
        mark: ${MARK_VAL}
        priority: ${MARK_PRIO}
        egressRate: "${MARK_EGRESS_RATE}"
        egressCeil: "${MARK_EGRESS_CEIL}"
        egressBurst: "${MARK_EGRESS_BURST}"
        ingressRate: "${MARK_INGRESS_RATE}"
        ingressBurst: "${MARK_INGRESS_BURST}"
        ingressAction: "${MARK_INGRESS_ACTION}"
        enableFqCodel: ${MARK_ENABLE_FQCODEL}

      # 4. Spec testing matchType: auto (auto-detecting vlan)
      - name: "${AUTO_CLASS_NAME}"
        matchType: "auto"
        classId: "${AUTO_CLASS_ID}"
        vlanId: ${AUTO_VLAN_ID}
        priority: ${AUTO_PRIO}
        egressRate: "${AUTO_EGRESS_RATE}"
        egressCeil: "${AUTO_EGRESS_CEIL}"
        egressBurst: "${AUTO_EGRESS_BURST}"
        ingressRate: "${AUTO_INGRESS_RATE}"
        ingressBurst: "${AUTO_INGRESS_BURST}"
        ingressAction: "${AUTO_INGRESS_ACTION}"
        enableFqCodel: ${AUTO_ENABLE_FQCODEL}
EOF

# ==============================================================================
# 3. AWAIT AGENT DAEMONSET CREATION & POD READY STATE
# ==============================================================================
echo -e "\n⏳ Waiting for Agent Pod to be created and reach Ready status on '${WORKER_NODE}'..."

for i in {1..30}; do
  AGENT_POD=$(oc get pods -n "${NAMESPACE}" -l "${AGENT_LABEL}" --field-selector "spec.nodeName=${WORKER_NODE}" --no-headers 2>/dev/null | awk '{print $1}' | head -n 1 || true)
  
  if [[ -n "${AGENT_POD}" ]]; then
    POD_STATUS=$(oc get pod -n "${NAMESPACE}" "${AGENT_POD}" -o jsonpath='{.status.phase}' 2>/dev/null || true)
    if [[ "${POD_STATUS}" == "Running" ]]; then
      echo -e "   • Agent Pod spawned : ${YELLOW}${AGENT_POD}${NC} (Running)"
      break
    fi
  fi
  sleep 2
done

if [[ -z "${AGENT_POD}" ]]; then
  fail_test "Agent pod was not spawned on node '${WORKER_NODE}' after applying the CR."
fi

# Fetch Agent Pod IP
AGENT_IP=$(oc get pod -n "${NAMESPACE}" "${AGENT_POD}" -o jsonpath='{.status.podIP}' 2>/dev/null || true)

if [[ -z "${AGENT_IP}" ]]; then
  fail_test "Failed to retrieve Pod IP for agent pod '${AGENT_POD}'."
fi

echo -e "   • Agent Pod IP       : ${YELLOW}${AGENT_IP}${NC}"

# ==============================================================================
# 4. TRIGGER RECONCILIATION & AWAIT ALIGNMENT
# ==============================================================================
echo -e "\n⚡ Triggering instant agent reconciliation pass..."
oc exec -n "${NAMESPACE}" "${MANAGER_DEPLOY}" -- \
  curl -s -X POST "http://${AGENT_IP}:8080/reconcile?interface=${TEST_IFACE}" > /dev/null

echo "⏳ Waiting for local TC rules to settle (5 seconds)..."
sleep 5

# ==============================================================================
# 5. VERIFY AGENT CONFIG API (/config) ALIGNMENT & DRIFT DELTAS
# ==============================================================================
echo -e "\n🕵️  Auditing Node Alignment API endpoint (http://${AGENT_IP}:8080/config)..."

CONFIG_JSON=$(oc exec -n "${NAMESPACE}" "${MANAGER_DEPLOY}" -- \
  curl -s "http://${AGENT_IP}:8080/config?interface=${TEST_IFACE}")

IS_ALIGNED=$(echo "${CONFIG_JSON}" | jq -r '.isAligned // "false"')
DRIFT_COUNT=$(echo "${CONFIG_JSON}" | jq -r '(.driftDeltas // []) | length')

if [[ "${IS_ALIGNED}" != "true" ]]; then
  echo -e "${RED}Drift Deltas Reported in Payload:${NC}"
  echo "${CONFIG_JSON}" | jq '.driftDeltas'
  fail_test "Agent API reported node as NOT ALIGNED (isAligned: false)."
fi

if [[ "${DRIFT_COUNT}" -ne 0 ]]; then
  fail_test "Agent API reported alignment as true, but driftDeltas count is ${DRIFT_COUNT}."
fi

pass_step "Node Agent API confirms 100% configuration alignment (isAligned: true, driftDeltas: [])."

# ==============================================================================
# 6. VERIFY HARDWARE/KERNEL LEVEL EGRESS TC CLASSES (tc class show)
# ==============================================================================
echo -e "\n🐧 Verifying Linux Kernel HTB Class Hierarchy on host (${TEST_IFACE})..."

KERNEL_CLASSES=$(oc exec -n "${NAMESPACE}" "${AGENT_POD}" -- tc class show dev "${TEST_IFACE}")

# Helper function to check a class entry
check_class() {
  local class_id="$1"
  local rate="$2"
  local ceil="$3"
  local prio="$4"
  local name="$5"

  local line=$(echo "${KERNEL_CLASSES}" | grep "class htb ${class_id}" || true)

  if echo "${line}" | grep -q "rate ${rate}" && \
     echo "${line}" | grep -E -q "ceil (${ceil}|${rate})" && \
     echo "${line}" | grep -q "prio ${prio}"; then
    pass_step "Kernel HTB Class ${class_id} verified (${name} -> Rate: ${rate}, Ceil: ${ceil}, Priority: ${prio})."
  else
    fail_test "Kernel HTB Class ${class_id} parameters do not match spec!\nActual Line: ${line}"
  fi
}

check_class "${VLAN_CLASS_ID}" "${VLAN_EGRESS_RATE}" "${VLAN_EGRESS_CEIL}" "${VLAN_PRIO}" "VLAN"
check_class "${SUBNET_CLASS_ID}" "${SUBNET_EGRESS_RATE}" "${SUBNET_EGRESS_CEIL}" "${SUBNET_PRIO}" "Subnet"
check_class "${MARK_CLASS_ID}" "${MARK_EGRESS_RATE}" "${MARK_EGRESS_CEIL}" "${MARK_PRIO}" "Mark"
check_class "${AUTO_CLASS_ID}" "${AUTO_EGRESS_RATE}" "${AUTO_EGRESS_CEIL}" "${AUTO_PRIO}" "Auto"

# ==============================================================================
# 7. VERIFY FQ_CODEL LEAF QDISC ATTACHMENTS
# ==============================================================================
echo -e "\n🌾 Verifying Active Leaf Qdiscs (fq_codel)..."

KERNEL_QDISCS=$(oc exec -n "${NAMESPACE}" "${AGENT_POD}" -- tc qdisc show dev "${TEST_IFACE}")

# Extract minor handles
VLAN_MINOR=$(echo "${VLAN_CLASS_ID}" | cut -d':' -f2)
AUTO_MINOR=$(echo "${AUTO_CLASS_ID}" | cut -d':' -f2)

if [[ "${VLAN_ENABLE_FQCODEL}" == "true" ]]; then
  if echo "${KERNEL_QDISCS}" | grep -q "qdisc fq_codel ${VLAN_MINOR}: parent ${VLAN_CLASS_ID}"; then
    pass_step "fq_codel leaf qdisc correctly attached beneath Class ${VLAN_CLASS_ID} (enableFqCodel: true)."
  else
    fail_test "fq_codel leaf qdisc missing beneath Class ${VLAN_CLASS_ID}!"
  fi
fi

if [[ "${AUTO_ENABLE_FQCODEL}" == "false" ]]; then
  if echo "${KERNEL_QDISCS}" | grep -q "qdisc fq_codel ${AUTO_MINOR}: parent ${AUTO_CLASS_ID}"; then
    fail_test "fq_codel leaf qdisc attached beneath Class ${AUTO_CLASS_ID} despite enableFqCodel: false!"
  else
    pass_step "fq_codel leaf qdisc correctly omitted beneath Class ${AUTO_CLASS_ID} (enableFqCodel: false)."
  fi
fi

# ==============================================================================
# 8. VERIFY INGRESS POLICING FILTERS & MATCH CLASSIFIERS
# ==============================================================================
echo -e "\n🛡️  Verifying Ingress Policing Filters (parent ffff:)..."

KERNEL_INGRESS=$(oc exec -n "${NAMESPACE}" "${AGENT_POD}" -- tc filter show dev "${TEST_IFACE}" parent ffff:)

# Verify VLAN Flower Classifier
if echo "${KERNEL_INGRESS}" | grep -q "vlan_id ${VLAN_ID}" && \
   echo "${KERNEL_INGRESS}" | grep -q "police.*rate ${VLAN_INGRESS_RATE}" && \
   echo "${KERNEL_INGRESS}" | grep -q "action ${VLAN_INGRESS_ACTION}"; then
  pass_step "Ingress VLAN Flower Filter verified (VLAN ID: ${VLAN_ID} -> Police: ${VLAN_INGRESS_RATE}, Action: ${VLAN_INGRESS_ACTION})."
else
  fail_test "Ingress VLAN ${VLAN_ID} policing filter not found or incorrectly configured!"
fi

# Verify Subnet Flower Classifier
if echo "${KERNEL_INGRESS}" | grep -q "dst_ip ${SUBNET_CIDR}" && \
   echo "${KERNEL_INGRESS}" | grep -q "police.*rate ${SUBNET_INGRESS_RATE}" && \
   echo "${KERNEL_INGRESS}" | grep -q "action ${SUBNET_INGRESS_ACTION}"; then
  pass_step "Ingress Subnet Flower Filter verified (${SUBNET_CIDR} -> Police: ${SUBNET_INGRESS_RATE}, Action: ${SUBNET_INGRESS_ACTION})."
else
  fail_test "Ingress Subnet policing filter not found or incorrectly configured!"
fi

# Verify SKB Mark Classifier via Agent Netlink API Payload and Kernel pref block
API_MARK=$(echo "${CONFIG_JSON}" | jq -r --arg prio "${MARK_PRIO}" '.actual.ingressFilters[] | select(.priority==($prio|tonumber) or .name=="'${MARK_CLASS_NAME}'") | .mark // empty')
MARK_PREF_BLOCK=$(echo "${KERNEL_INGRESS}" | grep -A 8 "pref ${MARK_PRIO}" || true)

if [[ "${API_MARK}" == "${MARK_VAL}" ]] && \
   echo "${MARK_PREF_BLOCK}" | grep -q "police.*rate ${MARK_INGRESS_RATE}" && \
   echo "${MARK_PREF_BLOCK}" | grep -q "action ${MARK_INGRESS_ACTION}"; then
  pass_step "Ingress SKB Mark Filter verified via Netlink API & Kernel (Mark: ${MARK_VAL} [${MARK_VAL_HEX}] -> Police: ${MARK_INGRESS_RATE}, Action: ${MARK_INGRESS_ACTION})."
else
  fail_test "Ingress SKB Mark filter not found or mark value ${MARK_VAL} [${MARK_VAL_HEX}] is missing!\nAPI Mark Value: ${API_MARK}\nActual Kernel Block:\n${MARK_PREF_BLOCK}"
fi

# ==============================================================================
# DUMP LIVE SETUP EVIDENCE & SUMMARY
# ==============================================================================
dump_evidence "SUCCESSFUL SETUP EVIDENCE"

echo -e "\n${GREEN}========================================================================${NC}"
echo -e "${GREEN}🎉 ALL FIELD VERIFICATION TESTS PASSED SUCCESSFULLY!${NC}"
echo -e "${GREEN}All CR fields were successfully validated, reconciled, and applied${NC}"
echo -e "${GREEN}to host '${WORKER_NODE}' kernel.${NC}"
echo -e "${GREEN}========================================================================${NC}"
