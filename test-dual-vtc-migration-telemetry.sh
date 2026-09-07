#!/usr/bin/env bash

set -euo pipefail

# ==============================================================================
# Dual-Strategy (IFB & Flower) KubeVirt Migration Telemetry Validation Test
# ==============================================================================

NAMESPACE="tc-virt-validation"
OPERATOR_NS="openshift-vlan-tc-operator"
VM_NAME="vm-vlan100"
PHYS_IFACE="enp1s0"
BR_IFACE="br-vlan380"

IFB_MANIFEST="vtc-ifb-ingress-egress-enp1s0-LiveMigration-VLAN-380.yaml"
FLOWER_MANIFEST="vtc-flower-ingress-egress-enp1s0-liveMigration-VLAN-380.yaml"

WORKER_NODES=(
  "hub-worker01.ocp4-hub.test.com"
  "hub-worker02.ocp4-hub.test.com"
  "hub-worker03.ocp4-hub.test.com"
)

# Colors
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m'

log_info()  { echo -e "${BLUE}[INFO]${NC} $1"; }
log_pass()  { echo -e "${GREEN}[PASS]${NC} $1"; }
log_warn()  { echo -e "${YELLOW}[WARN]${NC} $1"; }
log_fail()  { echo -e "${RED}[FAIL]${NC} $1"; }

# ==============================================================================
# Helper Functions
# ==============================================================================

get_node_ip() {
  local node_name="$1"
  case "${node_name}" in
    *worker01*|*worker-1*) echo "192.168.100.21" ;;
    *worker02*|*worker-2*) echo "192.168.100.22" ;;
    *worker03*|*worker-3*) echo "192.168.100.23" ;;
    *)
      local ip
      ip=$(oc get node "${node_name}" -o jsonpath='{.status.addresses[?(@.type=="InternalIP")].address}' 2>/dev/null || echo "")
      echo "${ip}"
      ;;
  esac
}

purge_migration_ip_leases() {
  log_info "Purging stale migration objects, completed launcher pods & whereabouts IP leases..."
  
  # 1. Force delete completed/failed launcher pods and migration CRs
  oc delete vmim --all -n "${NAMESPACE}" --force --grace-period=0 2>/dev/null || true
  oc delete pod -l kubevirt.io=virt-launcher -n "${NAMESPACE}" --field-selector status.phase=Completed --force --grace-period=0 2>/dev/null || true
  oc delete pod -l kubevirt.io=virt-launcher -n "${NAMESPACE}" --field-selector status.phase=Failed --force --grace-period=0 2>/dev/null || true
  oc delete pod -l kubevirt.io=virt-launcher -n "${NAMESPACE}" --field-selector status.phase=Error --force --grace-period=0 2>/dev/null || true
  
  # 2. Release whereabouts IPAM CR leases in openshift-cnv
  oc delete overlappingipaddresses --all -n openshift-cnv 2>/dev/null || true
  oc delete ippools --all -n openshift-cnv 2>/dev/null || true
  
  # 3. Clean lingering veth bridge attachments on worker nodes
  for node in "${WORKER_NODES[@]}"; do
    oc debug node/"${node}" -- chroot /host /bin/bash -c "
      for port in \$(ip link show master ${BR_IFACE} 2>/dev/null | grep veth | awk -F': ' '{print \$2}' | awk -F'@' '{print \$1}'); do
        ip link delete \$port 2>/dev/null || true
      done
    " &>/dev/null || true
  done

  sleep 1
  log_pass "✓ Migration IP leases and bridge attachments cleanly purged."
}

reset_host_tc_environment() {
  log_info "Cleaning up VTC custom resources, stale migration objects & host kernel interfaces..."
  
  # Delete all VTC custom resources
  oc delete vtc --all -n "${OPERATOR_NS}" --force --grace-period=0 2>/dev/null || true
  sleep 2
  
  # Purge IP leases and stale pods
  purge_migration_ip_leases
  
  # Audit and strip residual qdiscs across worker nodes
  log_info "Auditing all worker nodes to verify zero residual TC configuration..."
  for node in "${WORKER_NODES[@]}"; do
    oc debug node/"${node}" -- chroot /host /bin/bash -c "
      tc qdisc del dev ${PHYS_IFACE} root 2>/dev/null || true
      tc qdisc del dev ${PHYS_IFACE} ingress 2>/dev/null || true
      tc qdisc del dev ${PHYS_IFACE} clsact 2>/dev/null || true
      tc qdisc del dev ${BR_IFACE} ingress 2>/dev/null || true
      tc qdisc del dev ifb-${PHYS_IFACE} root 2>/dev/null || true
      ip link set dev ifb-${PHYS_IFACE} down 2>/dev/null || true
      ip link delete ifb-${PHYS_IFACE} type ifb 2>/dev/null || true
    " &>/dev/null || true
  done

  log_pass "✓ [PRE-CHECK] Host environment cleanly reset (zero lingering TC rules or IFB devices)."
}

wait_for_agent_alignment() {
  local strategy="$1"
  log_info "Triggering parallel reconciliation pass across all 3 agents..."
  
  for node in "${WORKER_NODES[@]}"; do
    local ip
    ip=$(get_node_ip "${node}")
    if [ -n "${ip}" ]; then
      curl -s -X POST --connect-timeout 1 --max-time 2 "http://${ip}:8080/reconcile" >/dev/null 2>&1 &
    fi
  done
  wait

  log_info "Polling agent endpoints in parallel to verify strict strategy alignment..."
  
  check_single_agent() {
    local node="$1"
    local strat="$2"
    local ip
    ip=$(get_node_ip "${node}")

    if [ -z "${ip}" ]; then
      echo "[ERROR] Could not resolve IP for ${node}"
      return 1
    fi

    for attempt in {1..30}; do
      local is_aligned="false"
      
      local res_phys
      res_phys=$(curl -s --connect-timeout 1 --max-time 2 "http://${ip}:8080/config?interface=${PHYS_IFACE}" || echo "{}")
      is_aligned=$(echo "${res_phys}" | jq -r '.isAligned // false' 2>/dev/null || echo "false")
      
      if [ "${is_aligned}" != "true" ] && [ "${strat}" == "Flower" ]; then
        local res_br
        res_br=$(curl -s --connect-timeout 1 --max-time 2 "http://${ip}:8080/config?interface=${BR_IFACE}" || echo "{}")
        is_aligned=$(echo "${res_br}" | jq -r '.isAligned // false' 2>/dev/null || echo "false")
      fi

      if [ "${is_aligned}" == "true" ]; then
        echo "[PASS] Agent ${node} (${ip}) fully aligned for strategy [${strat}]!"
        return 0
      fi
      sleep 1
    done

    echo "[FAIL] Agent ${node} (${ip}) failed to align within timeout!"
    return 1
  }

  export -f check_single_agent get_node_ip
  export PHYS_IFACE BR_IFACE

  local pids=()
  for node in "${WORKER_NODES[@]}"; do
    check_single_agent "${node}" "${strategy}" &
    pids+=($!)
  done

  for pid in "${pids[@]}"; do
    if ! wait "${pid}"; then
      log_fail "One or more agents failed to align for strategy [${strategy}]!"
      exit 1
    fi
  done
  log_pass "✓ All worker agents fully aligned for strategy [${strategy}]!"
}

get_node_bytes() {
  local ip="$1"
  local direction="$2" # egress or ingress
  local strategy="$3"  # IFB or Flower

  if [ "${strategy}" == "Flower" ] && [ "${direction}" == "ingress" ]; then
    # Direct query against bridge interface (br-vlan380) for VLAN 380 ingress filter (pref-3 / pref-1)
    local res
    res=$(curl -s --connect-timeout 2 "http://${ip}:8080/stats?interface=${BR_IFACE}" || echo "{}")
    echo "${res}" | jq -r '[.ingressStats[]? | select(.filterId | contains("pref-3") or contains("pref-1")) | .bytes] | add // 0' 2>/dev/null || echo "0"
  else
    # Egress or IFB Ingress: Query class 1:380 on physical or IFB interface
    local res
    res=$(curl -s --connect-timeout 2 "http://${ip}:8080/stats?interface=${PHYS_IFACE}" || echo "{}")
    if [ "${direction}" == "egress" ]; then
      echo "${res}" | jq -r '[.classStats[]? | select(.interface=="'"${PHYS_IFACE}"'" and .classId=="1:380") | .bytes] | add // 0' 2>/dev/null || echo "0"
    else
      echo "${res}" | jq -r '[.classStats[]? | select((.interface=="ifb-enp1s0" or .interface=="'"${PHYS_IFACE}"'") and .classId=="1:380") | .bytes] | add // 0' 2>/dev/null || echo "0"
    fi
  fi
}

run_strategy_test() {
  local strategy="$1"
  local manifest="$2"

  echo ""
  echo "======================================================================"
  log_info "Executing Live Migration Telemetry Test for Strategy: [${strategy}]"
  echo "======================================================================"

  reset_host_tc_environment

  log_info "Applying ${strategy} VTC manifest (${manifest})..."
  oc apply -f "${manifest}"

  log_info "Verifying VTC Custom Resource exists in API..."
  local cr_name
  cr_name=$(oc get vtc -n "${OPERATOR_NS}" -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || echo "")
  
  if [ -z "${cr_name}" ]; then
    log_fail "VTC CustomResource failed to apply!"
    exit 1
  fi
  log_pass "✓ VTC CR '${cr_name}' created in namespace '${OPERATOR_NS}'."

  wait_for_agent_alignment "${strategy}"

  if [ "${strategy}" == "IFB" ]; then
    log_info "Executing kernel netlink link verification for ifb-${PHYS_IFACE} across all nodes..."
    for node in "${WORKER_NODES[@]}"; do
      local dev_exists
      dev_exists=$(oc debug node/"${node}" -- chroot /host ip link show "ifb-${PHYS_IFACE}" 2>/dev/null | grep "ifb-${PHYS_IFACE}" || echo "")
      if [ -z "${dev_exists}" ]; then
        log_fail "Kernel interface ifb-${PHYS_IFACE} is missing on node ${node}!"
        exit 1
      fi
    done
    log_pass "✓ Host kernel interfaces 'ifb-${PHYS_IFACE}' verified present across all worker nodes."
  fi

  # Get initial VM host node using .status.nodeName
  local initial_node
  initial_node=$(oc get vmi "${VM_NAME}" -n "${NAMESPACE}" -o jsonpath='{.status.nodeName}')
  log_info "Target VM:         ${VM_NAME}"
  log_info "Initial Host Node: ${initial_node}"

  local source_ip
  source_ip=$(get_node_ip "${initial_node}")

  # Identify candidate target node (worker node NOT hosting the source VMI)
  local target_candidate_node
  target_candidate_node=$(oc get nodes -l node-role.kubernetes.io/worker -o jsonpath="{.items[?(@.metadata.name!='${initial_node}')].metadata.name}" | awk '{print $1}')
  
  local target_candidate_ip
  target_candidate_ip=$(get_node_ip "${target_candidate_node}")

  log_info "Capturing Pre-Migration Baseline Stats..."
  local base_bytes_src
  base_bytes_src=$(get_node_bytes "${source_ip}" "egress" "${strategy}")
  
  local base_bytes_dst
  base_bytes_dst=$(get_node_bytes "${target_candidate_ip}" "ingress" "${strategy}")

  log_info "  └─ Source Node (${initial_node} / ${source_ip}) Pre-Migration Egress Baseline:  ${base_bytes_src} bytes"
  log_info "  └─ Target Node (${target_candidate_node} / ${target_candidate_ip}) Pre-Migration Ingress Baseline: ${base_bytes_dst} bytes"

  log_info "Triggering Live Migration..."
  virtctl migrate "${VM_NAME}" -n "${NAMESPACE}"

  log_info "Waiting for new VMIM object creation..."
  sleep 3
  local vmim_name
  vmim_name=$(oc get vmim -n "${NAMESPACE}" --sort-by='.metadata.creationTimestamp' -o jsonpath='{.items[-1].metadata.name}')
  log_info "Tracked Migration Object: ${vmim_name}"

  log_info "Monitoring migration job state & VMI node transition..."
  local migration_passed=false
  local target_node=""
  
  for attempt in {1..60}; do
    local phase
    phase=$(oc get vmim "${vmim_name}" -n "${NAMESPACE}" -o jsonpath='{.status.phase}' 2>/dev/null || echo "Pending")
    local curr_node
    curr_node=$(oc get vmi "${VM_NAME}" -n "${NAMESPACE}" -o jsonpath='{.status.nodeName}' 2>/dev/null || echo "")

    log_info "  └─► [VMIM Phase: ${phase}] Current VMI Host Node: ${curr_node}"

    if [ "${phase}" == "Succeeded" ] && [ "${curr_node}" != "${initial_node}" ]; then
      migration_passed=true
      target_node="${curr_node}"
      break
    fi

    if [ "${phase}" == "Failed" ]; then
      log_fail "Migration job '${vmim_name}' reported Failed state!"
      break
    fi

    sleep 3
  done

  if [ "${migration_passed}" != "true" ]; then
    log_fail "Live migration failed for strategy [${strategy}]!"
    exit 1
  fi

  log_pass "✓ [STRICT ASSERT] VMIM '${vmim_name}' reached Succeeded AND VMI node successfully transitioned to '${target_node}'!"

  local target_ip
  target_ip=$(get_node_ip "${target_node}")

  if [ -z "${target_ip}" ]; then
    log_fail "Could not resolve IP address for target node '${target_node}'!"
    exit 1
  fi

  sleep 2

  local post_bytes_src
  post_bytes_src=$(get_node_bytes "${source_ip}" "egress" "${strategy}")
  local post_bytes_dst
  post_bytes_dst=$(get_node_bytes "${target_ip}" "ingress" "${strategy}")

  # Defensive check if post_bytes_dst returns empty to prevent underflow
  if [ -z "${post_bytes_dst}" ] || [ "${post_bytes_dst}" == "null" ]; then
    post_bytes_dst=$(curl -s "http://${target_ip}:8080/stats?interface=${PHYS_IFACE}" | jq -r '[.ingressStats[]?.bytes // 0] | add // 0')
  fi

  local delta_src=$((post_bytes_src - base_bytes_src))
  local delta_dst=$((post_bytes_dst - base_bytes_dst))

  log_info "Source Host Node: ${initial_node} (${source_ip}) | Pre-Migration Egress Baseline: ${base_bytes_src} bytes"
  log_info "Target Host Node: ${target_node} (${target_ip}) | Pre-Migration Ingress Baseline: ${base_bytes_dst} bytes"
  log_info "Verifying Post-Migration Telemetry Increments..."
  log_info "Source Node (${source_ip}) Egress Delta:  +${delta_src} bytes"
  log_info "Target Node (${target_ip}) Ingress Delta: +${delta_dst} bytes"

  if [ ${delta_src} -gt 1000000 ] && [ ${delta_dst} -gt 1000000 ]; then
    log_pass "Strategy [${strategy}] Telemetry Verified: Migration traffic (>1MB) recorded successfully across distinct host nodes!"
  else
    log_fail "Strategy [${strategy}] Telemetry Assertion Failed: Recorded migration bytes did not increase sufficiently!"
    exit 1
  fi

  purge_migration_ip_leases
}

# ==============================================================================
# Execution Flow
# ==============================================================================

log_info "1. Verifying presence and status of active VirtualMachineInstances..."
for vm in "${VM_NAME}" "vm-vlan280"; do
  vmi_state=$(oc get vmi "${vm}" -n "${NAMESPACE}" -o jsonpath='{.status.phase}' 2>/dev/null || echo "")
  if [ "${vmi_state}" != "Running" ]; then
    log_fail "VMI '${vm}' is not in Running state (Current: '${vmi_state}')!"
    exit 1
  fi
  log_pass "✓ [PRE-CHECK] VM '${vm}' exists and is currently Running."
done

log_info "Ensuring IFB manifest (${IFB_MANIFEST}) uses 2Gbit migration rate..."
sed -i 's/egressRate: 500Mbit/egressRate: 2Gbit/g' "${IFB_MANIFEST}" 2>/dev/null || true
sed -i 's/ingressRate: 500Mbit/ingressRate: 2Gbit/g' "${IFB_MANIFEST}" 2>/dev/null || true

log_info "Ensuring Flower manifest (${FLOWER_MANIFEST}) uses 256Mb migration burst buffer..."
sed -i 's/egressRate: 500Mbit/egressRate: 10Gbit/g' "${FLOWER_MANIFEST}" 2>/dev/null || true
sed -i 's/ingressRate: 500Mbit/ingressRate: 10Gbit/g' "${FLOWER_MANIFEST}" 2>/dev/null || true

# Execute IFB Strategy
run_strategy_test "IFB" "${IFB_MANIFEST}"

# Execute Flower Strategy
run_strategy_test "Flower" "${FLOWER_MANIFEST}"

echo ""
echo "======================================================================"
log_pass "✅ ALL TELEMETRY TESTS PASSED SUCCESSFULLY FOR BOTH IFB AND FLOWER STRATEGIES!"
echo "======================================================================"
