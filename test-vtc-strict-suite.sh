#!/usr/bin/env bash

set -euo pipefail

# ==============================================================================
# CONFIGURATION
# ==============================================================================
NAMESPACE="openshift-vlan-tc-operator"
PHYS_IFACE="enp1s0"
BRIDGE_IFACE="br-vlan380"
AGENT_NODE_IP="192.168.100.21"
AGENT_NODE_NAME="hub-worker01.ocp4-hub.test.com"
MASTER_NODE_IP="192.168.100.11"

LOG_DIR="./vtc-strict-test-logs-$(date +%Y%m%d_%H%M%S)"
TMP_YAML="/tmp/vtc-test-manifest.yaml"

mkdir -p "${LOG_DIR}"

echo "======================================================================"
echo " Starting Full Comprehensive 15-Scenario VTC Test Suite"
echo " Physical Interface: ${PHYS_IFACE}"
echo " Bridge Interface:   ${BRIDGE_IFACE}"
echo " Agent Node IP:      ${AGENT_NODE_IP}"
echo " Agent Node Name:    ${AGENT_NODE_NAME}"
echo " Output Log Dir:     ${LOG_DIR}"
echo "======================================================================"

# ==============================================================================
# HELPER & LOGGING FUNCTIONS
# ==============================================================================

log_info() { echo -e "[\033[1;34mINFO\033[0m] $1"; }
log_pass() { echo -e "[\033[1;32mPASS\033[0m] $1"; }
log_fail() { echo -e "[\033[1;31mFAIL\033[0m] $1"; }

assert_equals() {
    local actual="$1"
    local expected="$2"
    local field_description="$3"

    if [ "${actual}" == "${expected}" ]; then
        log_pass "    ✓ [ASSERT] ${field_description}: '${actual}'"
    else
        log_fail "    ✗ [ASSERT ERROR] ${field_description}: expected '${expected}', got '${actual}'"
        return 1
    fi
}

resolve_target_interface() {
    local raw_output="$1"
    if echo "${raw_output}" | grep -qE '^ifb-'; then
        echo "${raw_output}" | grep -E '^ifb-' | head -n 1
    else
        echo "${raw_output}" | head -n 1
    fi
}

collect_failure_diagnostics() {
    local scenario_id="$1"
    local fail_dir="${LOG_DIR}/${scenario_id}_FAILURE"
    mkdir -p "${fail_dir}"

    log_info "Collecting diagnostic failure dumps into ${fail_dir}..."

    oc get vtc -n "${NAMESPACE}" -o yaml > "${fail_dir}/vtc-cr.yaml" 2>&1 || true
    oc get vtcclass -n "${NAMESPACE}" -o yaml > "${fail_dir}/vtcclass-crs.yaml" 2>&1 || true
    oc logs -n "${NAMESPACE}" -l app=vlan-traffic-control-agent --tail=400 > "${fail_dir}/agent.log" 2>&1 || true

    for iface in "${PHYS_IFACE}" "${BRIDGE_IFACE}"; do
        curl -s "http://${AGENT_NODE_IP}:8080/config?interface=${iface}" | jq . > "${fail_dir}/curl-config-${iface}.json" 2>&1 || true
        curl -s "http://${AGENT_NODE_IP}:8080/stats?interface=${iface}" | jq . > "${fail_dir}/curl-stats-${iface}.json" 2>&1 || true
    done

    oc debug node/"${AGENT_NODE_NAME}" -- chroot /host tc -s qdisc show > "${fail_dir}/tc-qdisc-all.txt" 2>&1 || true
    oc debug node/"${AGENT_NODE_NAME}" -- chroot /host tc -s class show dev "${PHYS_IFACE}" > "${fail_dir}/tc-class-phys.txt" 2>&1 || true
    oc debug node/"${AGENT_NODE_NAME}" -- chroot /host tc filter show dev "${PHYS_IFACE}" ingress > "${fail_dir}/tc-filter-phys-ingress.txt" 2>&1 || true
    oc debug node/"${AGENT_NODE_NAME}" -- chroot /host tc filter show dev "${BRIDGE_IFACE}" ingress > "${fail_dir}/tc-filter-bridge-ingress.txt" 2>&1 || true

    IFB_DEV="ifb-${PHYS_IFACE}"
    IFB_DEV="${IFB_DEV:0:15}"
    oc debug node/"${AGENT_NODE_NAME}" -- chroot /host ip link show dev "${IFB_DEV}" > "${fail_dir}/ifb-link-status.txt" 2>&1 || true
    oc debug node/"${AGENT_NODE_NAME}" -- chroot /host tc -s qdisc show dev "${IFB_DEV}" > "${fail_dir}/tc-qdisc-ifb.txt" 2>&1 || true
}

cleanup_resources() {
    log_info "Flushing VTC cluster resources..."
    oc delete vtc --all -n "${NAMESPACE}" --timeout=30s 2>/dev/null || true
    sleep 5
}

# ==============================================================================
# DEEP ATTRIBUTE & INTERFACE VERIFICATION
# ==============================================================================

verify_scenario_attributes() {
    local scenario_id="$1"
    local phys_config_json="$2"
    local phys_stats_json="$3"
    local bridge_config_json="$4"
    local bridge_stats_json="$5"

    log_info "Running Deep Value & Interface Assertion for [${scenario_id}]..."

    # 1. Config Alignment Status on Physical Interface
    local is_aligned
    is_aligned=$(echo "${phys_config_json}" | jq -r '.isAligned')
    assert_equals "${is_aligned}" "true" "Host Alignment Status (isAligned)" || return 1

    local drift_count
    drift_count=$(echo "${phys_config_json}" | jq '.driftDeltas | length')
    assert_equals "${drift_count}" "0" "Drift Deltas Count" || return 1

    local actual_phys_iface
    actual_phys_iface=$(echo "${phys_config_json}" | jq -r '.actual.interface // .interface')
    assert_equals "${actual_phys_iface}" "${PHYS_IFACE}" "Actual Config Physical Interface" || return 1

    # 2. VlanTrafficControlClass Projections in Kube API
    local vtcclass_json
    vtcclass_json=$(oc get vtcclass -n "${NAMESPACE}" -o json)

    case "${scenario_id}" in
        "S1_EGRESS_NOIFB")
            assert_equals "$(echo "${vtcclass_json}" | jq -r '.items[] | select(.spec.className=="vlan-100-high") | .spec.direction')" "egress" "VTCClass Direction (VLAN 100)" || return 1
            assert_equals "$(echo "${vtcclass_json}" | jq -r '.items[] | select(.spec.className=="vlan-100-high") | .spec.guaranteed')" "2Gbit" "VTCClass Egress Rate (VLAN 100)" || return 1
            assert_equals "$(echo "${vtcclass_json}" | jq -r '.items[] | select(.spec.className=="vlan-100-high") | .spec.ceilBorrow')" "8Gbit" "VTCClass Egress Ceil (VLAN 100)" || return 1
            assert_equals "$(echo "${phys_config_json}" | jq -r '.actual.classes[] | select(.classId=="1:100") | .egressRate')" "2Gbit" "Kernel Class 1:100 Egress Rate" || return 1

            local non_phys_classes
            non_phys_classes=$(echo "${phys_stats_json}" | jq -r '[.classStats[] | select(.interface!="'"${PHYS_IFACE}"'" or .direction!="egress")] | length')
            assert_equals "${non_phys_classes}" "0" "Egress-only classStats Interface & Direction Mismatches" || return 1
            ;;

        "S2_INGRESS_NOIFB")
            assert_equals "$(echo "${vtcclass_json}" | jq -r '.items[] | select(.spec.className=="vlan-100-high") | .spec.direction')" "ingress" "VTCClass Direction (VLAN 100)" || return 1
            assert_equals "$(echo "${vtcclass_json}" | jq -r '.items[] | select(.spec.className=="vlan-100-high") | .spec.ingressRate')" "3Gbit" "VTCClass Ingress Rate (VLAN 100)" || return 1

            local filter_action
            filter_action=$(echo "${phys_config_json}" | jq -r '.actual.ingressFilters[] | select(.priority==1) | .action')
            assert_equals "${filter_action}" "police rate 3Gbit burst 40k conform-exceed drop" "Filter Action (VLAN 100)" || return 1

            local raw_ingress_iface
            raw_ingress_iface=$(echo "${phys_config_json}" | jq -r '.actual.ingressFilters[] | select(.priority==1) | .interface')
            local ingress_filter_iface
            ingress_filter_iface=$(resolve_target_interface "${raw_ingress_iface}")
            assert_equals "${ingress_filter_iface}" "${PHYS_IFACE}" "Stateless Ingress Filter Interface (VLAN 100)" || return 1
            ;;

        "S3_COMBINED_NOIFB")
            assert_equals "$(echo "${vtcclass_json}" | jq -r '.items[] | select(.spec.className=="vlan-100-high") | .spec.direction')" "ingress+egress" "VTCClass Direction (VLAN 100)" || return 1
            assert_equals "$(echo "${vtcclass_json}" | jq -r '.items[] | select(.spec.className=="vlan-100-high") | .spec.guaranteed')" "2Gbit" "VTCClass Guaranteed Rate (VLAN 100)" || return 1
            assert_equals "$(echo "${vtcclass_json}" | jq -r '.items[] | select(.spec.className=="vlan-100-high") | .spec.ingressRate')" "3Gbit" "VTCClass Ingress Rate (VLAN 100)" || return 1
            ;;

        "S4_EGRESS_IFB")
            assert_equals "$(echo "${vtcclass_json}" | jq -r '.items[] | select(.spec.className=="vlan-280-medium") | .spec.direction')" "egress" "VTCClass Direction (VLAN 280)" || return 1
            assert_equals "$(echo "${phys_config_json}" | jq -r '.actual.classes[] | select(.classId=="1:280") | .egressRate')" "1200Mbit" "Kernel Class 1:280 Egress Rate" || return 1
            ;;

        "S5_INGRESS_IFB"|"S7_IFB_CLEANUP_TEARDOWN")
            assert_equals "$(echo "${vtcclass_json}" | jq -r '.items[] | select(.spec.className=="vlan-380-migration") | .spec.direction')" "ingress" "VTCClass Direction (VLAN 380)" || return 1
            assert_equals "$(echo "${phys_config_json}" | jq -r '.actual.ifbInterface // "ifb-" + .interface')" "ifb-${PHYS_IFACE}" "Actual Config IFB Interface" || return 1
            assert_equals "$(echo "${phys_config_json}" | jq -r '.actual.ingressFilters[] | select(.type=="matchall") | .action')" "mirred redirect dev ifb-enp1s0" "Catch-All Redirect Action" || return 1

            local ifb_class_iface
            ifb_class_iface=$(echo "${phys_stats_json}" | jq -r '.classStats[] | select(.classId=="1:380") | .interface')
            assert_equals "${ifb_class_iface}" "ifb-${PHYS_IFACE}" "IFB Ingress classStat Interface Label" || return 1

            local ifb_class_dir
            ifb_class_dir=$(echo "${phys_stats_json}" | jq -r '.classStats[] | select(.classId=="1:380") | .direction')
            assert_equals "${ifb_class_dir}" "ingress" "IFB Ingress classStat Direction Label" || return 1

            local phys_ingress_stat_count
            phys_ingress_stat_count=$(echo "${phys_stats_json}" | jq -r '[.ingressStats[] | select(.interface=="'"${PHYS_IFACE}"'")] | length')
            assert_equals "${phys_ingress_stat_count}" "0" "Ghost Physical Ingress Stats Purge Check" || return 1
            ;;

        "S6_COMBINED_IFB")
            assert_equals "$(echo "${vtcclass_json}" | jq -r '.items[] | select(.spec.className=="vlan-100-high") | .spec.direction')" "ingress+egress" "VTCClass Direction (VLAN 100)" || return 1
            assert_equals "$(echo "${phys_config_json}" | jq -r '.actual.classes[] | select(.classId=="1:100") | .egressRate')" "2Gbit" "Class 1:100 Egress Rate" || return 1
            assert_equals "$(echo "${phys_config_json}" | jq -r '.actual.classes[] | select(.classId=="1:100") | .egressCeil')" "5Gbit" "Class 1:100 Egress Ceil" || return 1
            assert_equals "$(echo "${phys_config_json}" | jq -r '.actual.classes[] | select(.classId=="1:100") | .ingressRate')" "3Gbit" "Class 1:100 Ingress Rate" || return 1
            assert_equals "$(echo "${phys_config_json}" | jq -r '.actual.classes[] | select(.classId=="1:100") | .ingressCeil')" "10Gbit" "Class 1:100 Ingress Ceil" || return 1

            assert_equals "$(echo "${phys_config_json}" | jq -r '.actual.ingressFilters[] | select(.type=="matchall") | .interface')" "${PHYS_IFACE}" "Redirect Filter Interface" || return 1
            assert_equals "$(echo "${phys_config_json}" | jq -r '.actual.ingressFilters[] | select(.type=="matchall") | .action')" "mirred redirect dev ifb-enp1s0" "Redirect Filter Action" || return 1

            local vlan_380_filter_name
            vlan_380_filter_name=$(echo "${phys_config_json}" | jq -r '.actual.ingressFilters[] | select(.vlanId==380) | .name')
            assert_equals "${vlan_380_filter_name}" "vlan-380-migration" "Filter Priority 3 Name Alignment (VLAN 380)" || return 1

            local egress_class_iface
            egress_class_iface=$(echo "${phys_stats_json}" | jq -r '.classStats[] | select(.classId=="1:100" and .direction=="egress") | .interface')
            assert_equals "${egress_class_iface}" "${PHYS_IFACE}" "Combined Egress Class Interface (${PHYS_IFACE})" || return 1

            local ingress_class_iface
            ingress_class_iface=$(echo "${phys_stats_json}" | jq -r '.classStats[] | select(.classId=="1:100" and .direction=="ingress") | .interface')
            assert_equals "${ingress_class_iface}" "ifb-${PHYS_IFACE}" "Combined Ingress Class Interface (ifb-${PHYS_IFACE})" || return 1

            local raw_ingress_filter_iface
            raw_ingress_filter_iface=$(echo "${phys_config_json}" | jq -r '.actual.ingressFilters[] | select(.vlanId==100 and .action=="htb classify") | .interface')
            local ingress_filter_iface
            ingress_filter_iface=$(resolve_target_interface "${raw_ingress_filter_iface}")
            assert_equals "${ingress_filter_iface}" "ifb-${PHYS_IFACE}" "Combined Ingress Filter Interface (ifb-${PHYS_IFACE})" || return 1

            local phys_ingress_stat_count
            phys_ingress_stat_count=$(echo "${phys_stats_json}" | jq -r '[.ingressStats[] | select(.interface=="'"${PHYS_IFACE}"'")] | length')
            assert_equals "${phys_ingress_stat_count}" "0" "Ghost Physical Ingress Stats Purge Check (S6)" || return 1
            ;;

        "S8_DUAL_VTC_MULTI_HTBID")
            log_info "Verifying Multi-htbId & Multi-Class ID Mappings (htbId:1 on enp1s0 vs htbId:2 on br-vlan380)..."

            local phys_htb_id bridge_htb_id bridge_class_380
            phys_htb_id=$(echo "${phys_config_json}" | jq -r '.desired.htbId')
            assert_equals "${phys_htb_id}" "1" "Physical Interface htbId" || return 1

            bridge_htb_id=$(echo "${bridge_config_json}" | jq -r '.desired.htbId')
            assert_equals "${bridge_htb_id}" "2" "Bridge Interface htbId" || return 1

            bridge_class_380=$(echo "${bridge_config_json}" | jq -r '.desired.classes[] | select(.priority==3) | .classId')
            assert_equals "${bridge_class_380}" "2:380" "Bridge Ingress Filter Mapping (ClassID 2:380)" || return 1

            local bridge_filter_380_action
            bridge_filter_380_action=$(echo "${bridge_config_json}" | jq -r '.actual.ingressFilters[] | select(.priority==3) | .action')
            assert_equals "${bridge_filter_380_action}" "police rate 10Gbit burst 256Mb conform-exceed pass" "Bridge Priority 3 Action (Non-drop)" || return 1
            ;;

        "S9_FULL_PARAMETER_MATRIX")
            log_info "Asserting Full Parameter Matrix in Kernel (htbId: 10, custom defaults, fq_codel, mark, subnet, vlan)..."
            
            local htb_id default_class cls_100_rate cls_100_ceil cls_100_burst mark_filter_type
            htb_id=$(echo "${phys_config_json}" | jq -r '.desired.htbId')
            assert_equals "${htb_id}" "10" "Custom Root htbId (10)" || return 1

            default_class=$(echo "${phys_config_json}" | jq -r '.desired.defaultClassId')
            assert_equals "${default_class}" "10:88" "Custom Default Class ID (10:88)" || return 1

            cls_100_rate=$(echo "${phys_config_json}" | jq -r '.actual.classes[] | select(.classId=="10:100") | .egressRate')
            cls_100_ceil=$(echo "${phys_config_json}" | jq -r '.actual.classes[] | select(.classId=="10:100") | .egressCeil')
            cls_100_burst=$(echo "${phys_config_json}" | jq -r '.actual.classes[] | select(.classId=="10:100") | .egressBurst')

            assert_equals "${cls_100_rate}" "1Gbit" "Class 10:100 Egress Rate" || return 1
            assert_equals "${cls_100_ceil}" "4Gbit" "Class 10:100 Egress Ceil" || return 1
            assert_equals "${cls_100_burst}" "32Mb" "Class 10:100 Egress Burst" || return 1

            mark_filter_type=$(echo "${phys_config_json}" | jq -r '.actual.ingressFilters[] | select(.priority==5) | .matchType')
            assert_equals "${mark_filter_type}" "mark" "Class 10:500 FW Mark Match Type" || return 1
            ;;
    esac

    log_pass "All attributes and interfaces for Scenario [${scenario_id}] strictly verified!"
    return 0
}

# ==============================================================================
# SCENARIO EXECUTION ENGINE
# ==============================================================================

execute_scenario() {
    local scenario_id="$1"
    local scenario_title="$2"
    local yaml_manifest="$3"

    echo ""
    echo "======================================================================"
    log_info "Executing Scenario [${scenario_id}]: ${scenario_title}"
    echo "======================================================================"

    cleanup_resources

    log_info "1. Applying CR Manifest..."
    echo "${yaml_manifest}" > "${TMP_YAML}"
    oc apply -f "${TMP_YAML}"

    log_info "Waiting 10 seconds for agent synchronization pass..."
    sleep 10

    log_info "2. Verifying Kube API CR Status..."
    if ! oc get vtc -n "${NAMESPACE}" | grep -q "True"; then
        log_fail "VTC CR failed to reach Ready status!"
        collect_failure_diagnostics "${scenario_id}"
        return 1
    fi

    log_info "3. Querying Agent REST Endpoints across interfaces..."
    local phys_config_resp phys_stats_resp bridge_config_resp bridge_stats_resp

    phys_config_resp=$(curl -s "http://${AGENT_NODE_IP}:8080/config?interface=${PHYS_IFACE}")
    echo "${phys_config_resp}" | jq . > "${LOG_DIR}/${scenario_id}_curl_config_phys.json"

    phys_stats_resp=$(curl -s "http://${AGENT_NODE_IP}:8080/stats?interface=${PHYS_IFACE}")
    echo "${phys_stats_resp}" | jq . > "${LOG_DIR}/${scenario_id}_curl_stats_phys.json"

    bridge_config_resp=$(curl -s "http://${AGENT_NODE_IP}:8080/config?interface=${BRIDGE_IFACE}" 2>/dev/null || echo "{}")
    echo "${bridge_config_resp}" | jq . > "${LOG_DIR}/${scenario_id}_curl_config_bridge.json" 2>&1 || true

    bridge_stats_resp=$(curl -s "http://${AGENT_NODE_IP}:8080/stats?interface=${BRIDGE_IFACE}" 2>/dev/null || echo "{}")
    echo "${bridge_stats_resp}" | jq . > "${LOG_DIR}/${scenario_id}_curl_stats_bridge.json" 2>&1 || true

    log_info "4. Running Strict Attribute Suite..."
    if ! verify_scenario_attributes "${scenario_id}" "${phys_config_resp}" "${phys_stats_resp}" "${bridge_config_resp}" "${bridge_stats_resp}"; then
        log_fail "Strict attribute assertion failed!"
        collect_failure_diagnostics "${scenario_id}"
        return 1
    fi

    log_info "5. Testing CR Deletion & Host Cleanup..."
    cleanup_resources

    # Teardown Verification Check 1: HTB Qdisc Removal
    local post_delete_config
    post_delete_config=$(curl -s "http://${AGENT_NODE_IP}:8080/config?interface=${PHYS_IFACE}")
    local htb_present
    htb_present=$(echo "${post_delete_config}" | jq -r '.actual.htbQdiscPresent')

    if [ "${htb_present}" == "true" ]; then
        log_fail "HTB qdisc remained active on physical interface after CR deletion!"
        collect_failure_diagnostics "${scenario_id}"
        return 1
    fi

    # Teardown Verification Check 2: IFB Destruction Check (for IFB scenarios)
    if [[ "${scenario_id}" =~ IFB ]]; then
        IFB_DEV="ifb-${PHYS_IFACE}"
        IFB_DEV="${IFB_DEV:0:15}"
        
        local ifb_link_check
        ifb_link_check=$(oc debug node/"${AGENT_NODE_NAME}" -- chroot /host ip link show dev "${IFB_DEV}" 2>&1 || true)

        if ! echo "${ifb_link_check}" | grep -q "does not exist"; then
            log_fail "[TEARDOWN BUG DETECTED] Virtual device '${IFB_DEV}' still exists on host kernel after VTC CR deletion!"
            log_fail "Device status output: ${ifb_link_check}"
            collect_failure_diagnostics "${scenario_id}"
            return 1
        else
            log_pass "✓ [ASSERT] Virtual device '${IFB_DEV}' was cleanly deleted from host kernel."
        fi
    fi

    log_pass "Scenario [${scenario_id}] completed and verified successfully!"
    return 0
}

# ==============================================================================
# RECOVERY, MUTATION & RESILIENCE SCENARIOS (S10 - S15)
# ==============================================================================

execute_drift_recovery_test() {
    local scenario_id="S10_KERNEL_DRIFT_SELF_HEALING"
    echo ""
    echo "======================================================================"
    log_info "Executing Scenario [${scenario_id}]: Kernel Wipe & Drift Recovery Test"
    echo "======================================================================"

    cleanup_resources

    log_info "1. Deploying Baseline VTC Resource (with 10s Reconcile Interval)..."
    
    S10_MANIFEST=$(cat <<EOF
apiVersion: networking.med.io/v1alpha1
kind: VlanTrafficControl
metadata:
  name: vtc-test-s10
  namespace: ${NAMESPACE}
spec:
  nodeSelector:
    node-role.kubernetes.io/worker: ""
  reconcileIntervalSeconds: 10
  tcStrategy: flower
  htbRoot:
    interface: ${PHYS_IFACE}
    htbId: 1
    rate: 10Gbit
    defaultClassId: "1:99"
    defaultClassMinor: 99
    classes:
      - name: default-fallback
        classId: "1:99"
        priority: 0
        egressRate: 100Mbit
      - name: vlan-100-high
        classId: "1:100"
        vlanId: 100
        matchType: subnet
        subnet: 10.0.100.0/24
        priority: 1
        egressRate: 2Gbit
        egressCeil: 8Gbit
EOF
)
    echo "${S10_MANIFEST}" > "${TMP_YAML}"
    oc apply -f "${TMP_YAML}"
    sleep 8

    log_info "2. Corrupting Node Kernel Configuration (Wiping root HTB qdisc)..."
    oc debug node/"${AGENT_NODE_NAME}" -- chroot /host tc qdisc del dev "${PHYS_IFACE}" root 2>/dev/null || true

    log_info "3. Triggering Agent Self-Healing Reconciliation..."
    curl -s -X POST "http://${AGENT_NODE_IP}:8080/reconcile" > /dev/null || true
    sleep 5

    log_info "4. Verifying Restored State via Agent Endpoint..."
    local recovered_config
    recovered_config=$(curl -s "http://${AGENT_NODE_IP}:8080/config?interface=${PHYS_IFACE}")

    local is_aligned class_100_rate htb_present
    is_aligned=$(echo "${recovered_config}" | jq -r '.isAligned')
    htb_present=$(echo "${recovered_config}" | jq -r '.actual.htbQdiscPresent')
    class_100_rate=$(echo "${recovered_config}" | jq -r '.actual.classes[] | select(.classId=="1:100") | .egressRate')

    assert_equals "${htb_present}" "true" "Root HTB Qdisc Re-created Status" || { collect_failure_diagnostics "${scenario_id}"; return 1; }
    assert_equals "${is_aligned}" "true" "Post-Recovery Alignment Status" || { collect_failure_diagnostics "${scenario_id}"; return 1; }
    assert_equals "${class_100_rate}" "2Gbit" "Restored Class 1:100 Rate" || { collect_failure_diagnostics "${scenario_id}"; return 1; }

    log_pass "✓ Scenario [${scenario_id}] Self-Healing Verified Successfully!"
    cleanup_resources
}

execute_live_mutation_test() {
    local scenario_id="S11_LIVE_CR_MUTATION"
    echo ""
    echo "======================================================================"
    log_info "Executing Scenario [${scenario_id}]: Dynamic CR Mutation & Real-Time Propagation"
    echo "======================================================================"

    cleanup_resources

    log_info "1. Applying Initial VTC Manifest (S1 Baseline)..."
    echo "${S1_YAML}" > "${TMP_YAML}"
    oc apply -f "${TMP_YAML}"
    sleep 8

    log_info "2. Mutating Active VTC CR (Updating VLAN 100 Rate from 2Gbit -> 5Gbit & adding VLAN 400)..."
    
    MUTATED_YAML=$(cat <<EOF
apiVersion: networking.med.io/v1alpha1
kind: VlanTrafficControl
metadata:
  name: vtc-test-s1
  namespace: ${NAMESPACE}
spec:
  nodeSelector:
    node-role.kubernetes.io/worker: ""
  tcStrategy: flower
  htbRoot:
    interface: ${PHYS_IFACE}
    htbId: 1
    rate: 10Gbit
    defaultClassId: "1:99"
    defaultClassMinor: 99
    classes:
      - name: default-fallback
        classId: "1:99"
        priority: 0
        egressRate: 100Mbit
      - name: vlan-100-high
        classId: "1:100"
        vlanId: 100
        matchType: subnet
        subnet: 10.0.100.0/24
        priority: 1
        egressRate: 5Gbit
        egressCeil: 9Gbit
      - name: vlan-400-new-service
        classId: "1:400"
        vlanId: 400
        matchType: vlan
        priority: 4
        egressRate: 3Gbit
EOF
)
    echo "${MUTATED_YAML}" > "${TMP_YAML}"
    oc apply -f "${TMP_YAML}"

    log_info "Waiting 8 seconds for mutation reconciliation pass..."
    sleep 8

    log_info "3. Verifying Propagated Changes on Worker Host..."
    local mutated_config
    mutated_config=$(curl -s "http://${AGENT_NODE_IP}:8080/config?interface=${PHYS_IFACE}")

    local new_rate_100 class_400_present
    new_rate_100=$(echo "${mutated_config}" | jq -r '.actual.classes[] | select(.classId=="1:100") | .egressRate')
    class_400_present=$(echo "${mutated_config}" | jq -r '.actual.classes[] | select(.classId=="1:400") | .name')

    assert_equals "${new_rate_100}" "5Gbit" "Mutated Class 1:100 Rate" || { collect_failure_diagnostics "${scenario_id}"; return 1; }
    assert_equals "${class_400_present}" "vlan-400-new-service" "Newly Added Class 1:400 Existence" || { collect_failure_diagnostics "${scenario_id}"; return 1; }

    log_pass "✓ Scenario [${scenario_id}] Live Mutation Verified Successfully!"
    cleanup_resources
}

execute_multi_node_isolation_test() {
    local scenario_id="S12_MULTI_NODE_ISOLATION"
    echo ""
    echo "======================================================================"
    log_info "Executing Scenario [${scenario_id}]: Control Plane Isolation Test"
    echo "======================================================================"

    cleanup_resources

    log_info "1. Applying Worker-Only VTC Manifest..."
    echo "${S1_YAML}" > "${TMP_YAML}"
    oc apply -f "${TMP_YAML}"
    sleep 8

    log_info "2. Verifying Control Plane (Master) Node Isolation..."
    local master_config
    master_config=$(curl -s "http://${MASTER_NODE_IP}:8080/config?interface=${PHYS_IFACE}" 2>/dev/null || echo "{}")
    local master_htb_present
    master_htb_present=$(echo "${master_config}" | jq -r '.actual.htbQdiscPresent // "false"')

    assert_equals "${master_htb_present}" "false" "Master Node HTB Qdisc Exclusion" || return 1

    log_info "3. Verifying Worker Node Alignment..."
    local worker_config
    worker_config=$(curl -s "http://${AGENT_NODE_IP}:8080/config?interface=${PHYS_IFACE}")
    local worker_aligned
    worker_aligned=$(echo "${worker_config}" | jq -r '.isAligned')

    assert_equals "${worker_aligned}" "true" "Worker Node Alignment Status" || return 1

    log_pass "✓ Scenario [${scenario_id}] Isolation Verified Successfully!"
    cleanup_resources
}

execute_invalid_spec_test() {
    local scenario_id="S13_INVALID_SPEC_SAFETY"
    echo ""
    echo "======================================================================"
    log_info "Executing Scenario [${scenario_id}]: Malformed Spec Agent Crash Prevention"
    echo "======================================================================"

    cleanup_resources

    log_info "1. Applying Malformed Spec (Subnet match type with empty subnet field)..."
    
    MALFORMED_YAML=$(cat <<EOF
apiVersion: networking.med.io/v1alpha1
kind: VlanTrafficControl
metadata:
  name: vtc-test-s13-invalid
  namespace: ${NAMESPACE}
spec:
  nodeSelector:
    node-role.kubernetes.io/worker: ""
  tcStrategy: flower
  htbRoot:
    interface: ${PHYS_IFACE}
    classes:
      - name: broken-class
        classId: "1:100"
        matchType: subnet
        subnet: ""                     # <========== MALFORMED EMPTY SUBNET
        priority: 1
        egressRate: 1Gbit
EOF
)
    echo "${MALFORMED_YAML}" > "${TMP_YAML}"
    oc apply -f "${TMP_YAML}"
    sleep 8

    log_info "2. Verifying Agent Binary Alive Status..."
    local agent_health
    agent_health=$(curl -s "http://${AGENT_NODE_IP}:8080/healthz")
    assert_equals "${agent_health}" "ok" "Agent REST Service Alive Status" || return 1

    log_pass "✓ Scenario [${scenario_id}] Malformed Spec Safety Verified Successfully!"
    cleanup_resources
}

execute_pod_restart_resilience_test() {
    local scenario_id="S14_POD_RESTART_RESILIENCE"
    echo ""
    echo "======================================================================"
    log_info "Executing Scenario [${scenario_id}]: Agent Container Restart Persistence"
    echo "======================================================================"

    cleanup_resources

    log_info "1. Deploying Baseline VTC Resource..."
    echo "${S1_YAML}" > "${TMP_YAML}"
    oc apply -f "${TMP_YAML}"
    sleep 8

    log_info "2. Restarting Agent Pod on Worker Node..."
    oc delete pod -n "${NAMESPACE}" -l app=vlan-traffic-control-agent --field-selector spec.nodeName="${AGENT_NODE_NAME}" --timeout=30s
    sleep 10

    log_info "3. Querying Restored Agent Endpoint..."
    local restarted_config
    restarted_config=$(curl -s "http://${AGENT_NODE_IP}:8080/config?interface=${PHYS_IFACE}")

    local is_aligned htb_present
    is_aligned=$(echo "${restarted_config}" | jq -r '.isAligned')
    htb_present=$(echo "${restarted_config}" | jq -r '.actual.htbQdiscPresent')

    assert_equals "${htb_present}" "true" "Post-Restart HTB Qdisc Persistence" || return 1
    assert_equals "${is_aligned}" "true" "Post-Restart Node Alignment Status" || return 1

    log_pass "✓ Scenario [${scenario_id}] Container Restart Resilience Verified Successfully!"
    cleanup_resources
}

execute_concurrent_race_test() {
    local scenario_id="S15_CONCURRENT_RACE_SAFETY"
    echo ""
    echo "======================================================================"
    log_info "Executing Scenario [${scenario_id}]: Rapid Concurrent CR Race Condition Test"
    echo "======================================================================"

    cleanup_resources

    log_info "1. Rapidly Applying and Deleting Multiple Resources Simultaneously..."
    for i in {1..3}; do
        echo "${S1_YAML}" > "${TMP_YAML}"
        oc apply -f "${TMP_YAML}" >/dev/null 2>&1 &
        oc delete vtc --all -n "${NAMESPACE}" >/dev/null 2>&1 &
    done
    wait
    sleep 8

    log_info "2. Applying Final Clean Manifest..."
    echo "${S1_YAML}" > "${TMP_YAML}"
    oc apply -f "${TMP_YAML}"
    sleep 8

    log_info "3. Verifying Final Kernel Consistency..."
    local final_config
    final_config=$(curl -s "http://${AGENT_NODE_IP}:8080/config?interface=${PHYS_IFACE}")
    local is_aligned
    is_aligned=$(echo "${final_config}" | jq -r '.isAligned')

    assert_equals "${is_aligned}" "true" "Post-Race State Alignment Status" || return 1

    log_pass "✓ Scenario [${scenario_id}] Concurrent Race Safety Verified Successfully!"
    cleanup_resources
}

# ==============================================================================
# MANIFEST DEFINITIONS
# ==============================================================================

S1_YAML=$(cat <<EOF
apiVersion: networking.med.io/v1alpha1
kind: VlanTrafficControl
metadata:
  name: vtc-test-s1
  namespace: ${NAMESPACE}
spec:
  nodeSelector:
    node-role.kubernetes.io/worker: ""
  tcStrategy: flower
  htbRoot:
    interface: ${PHYS_IFACE}
    rate: 10Gbit
    defaultClassId: "1:99"
    defaultClassMinor: 99
    classes:
      - name: default-fallback
        classId: "1:99"
        priority: 0
        egressRate: 100Mbit
        egressCeil: 2Gbit
        egressBurst: 20k
        enableFqCodel: true
      - name: vlan-100-high
        classId: "1:100"
        vlanId: 100
        matchType: subnet
        subnet: 10.0.100.0/24
        priority: 1
        egressRate: 2Gbit
        egressCeil: 8Gbit
        egressBurst: 50k
        enableFqCodel: true
      - name: vlan-280-medium
        classId: "1:280"
        vlanId: 280
        matchType: vlan
        priority: 2
        egressRate: 1Gbit
        egressCeil: 5Gbit
        egressBurst: 30k
        enableFqCodel: true
      - name: vlan-380-migration
        classId: "1:380"
        vlanId: 380
        matchType: vlan
        priority: 3
        egressRate: 500Mbit
        egressCeil: 10Gbit
        egressBurst: 60k
        enableFqCodel: true
EOF
)

S2_YAML=$(cat <<EOF
apiVersion: networking.med.io/v1alpha1
kind: VlanTrafficControl
metadata:
  name: vtc-test-s2
  namespace: ${NAMESPACE}
spec:
  nodeSelector:
    node-role.kubernetes.io/worker: ""
  tcStrategy: flower
  htbRoot:
    interface: ${PHYS_IFACE}
    classes:
      - name: vlan-100-high
        classId: "1:100"
        vlanId: 100
        matchType: subnet
        subnet: 10.0.100.0/24
        priority: 1
        ingressRate: 3Gbit
        ingressBurst: 40k
        ingressAction: drop
      - name: vlan-280-medium
        classId: "1:280"
        vlanId: 280
        matchType: vlan
        priority: 2
        ingressRate: 1.5Gbit
        ingressBurst: 25k
        ingressAction: drop
      - name: vlan-380-migration
        classId: "1:380"
        vlanId: 380
        matchType: vlan
        priority: 3
        ingressRate: 10Gbit
        ingressBurst: 80k
        ingressAction: drop
EOF
)

S3_YAML=$(cat <<EOF
apiVersion: networking.med.io/v1alpha1
kind: VlanTrafficControl
metadata:
  name: vtc-test-s3
  namespace: ${NAMESPACE}
spec:
  nodeSelector:
    node-role.kubernetes.io/worker: ""
  tcStrategy: flower
  htbRoot:
    interface: ${PHYS_IFACE}
    rate: 10Gbit
    defaultClassId: "1:99"
    defaultClassMinor: 99
    classes:
      - name: default-fallback
        classId: "1:99"
        priority: 0
        egressRate: 100Mbit
        egressCeil: 1Gbit
        egressBurst: 15k
        enableFqCodel: true
      - name: vlan-100-high
        classId: "1:100"
        vlanId: 100
        matchType: subnet
        subnet: 10.0.100.0/24
        priority: 1
        egressRate: 2Gbit
        egressCeil: 6Gbit
        egressBurst: 45k
        ingressRate: 3Gbit
        ingressBurst: 50k
        enableFqCodel: true
      - name: vlan-280-medium
        classId: "1:280"
        vlanId: 280
        matchType: vlan
        priority: 2
        egressRate: 1Gbit
        egressCeil: 4Gbit
        egressBurst: 25k
        ingressRate: 1.5Gbit
        ingressBurst: 30k
        enableFqCodel: true
      - name: vlan-380-migration
        classId: "1:380"
        vlanId: 380
        matchType: vlan
        priority: 3
        egressRate: 500Mbit
        egressCeil: 8Gbit
        egressBurst: 70k
        ingressRate: 2Gbit
        ingressBurst: 60k
        enableFqCodel: true
EOF
)

S4_YAML=$(cat <<EOF
apiVersion: networking.med.io/v1alpha1
kind: VlanTrafficControl
metadata:
  name: vtc-test-s4
  namespace: ${NAMESPACE}
spec:
  nodeSelector:
    node-role.kubernetes.io/worker: ""
  tcStrategy: ifb
  htbRoot:
    interface: ${PHYS_IFACE}
    rate: 10Gbit
    defaultClassId: "1:99"
    defaultClassMinor: 99
    classes:
      - name: default-fallback
        classId: "1:99"
        priority: 0
        egressRate: 150Mbit
        egressCeil: 3Gbit
        egressBurst: 25k
        enableFqCodel: true
      - name: vlan-100-high
        classId: "1:100"
        vlanId: 100
        matchType: subnet
        subnet: 10.0.100.0/24
        priority: 1
        egressRate: 2.5Gbit
        egressCeil: 9Gbit
        egressBurst: 55k
        enableFqCodel: true
      - name: vlan-280-medium
        classId: "1:280"
        vlanId: 280
        matchType: vlan
        priority: 2
        egressRate: 1.2Gbit
        egressCeil: 6Gbit
        egressBurst: 35k
        enableFqCodel: true
      - name: vlan-380-migration
        classId: "1:380"
        vlanId: 380
        matchType: vlan
        priority: 3
        egressRate: 800Mbit
        egressCeil: 10Gbit
        egressBurst: 75k
        enableFqCodel: true
EOF
)

S5_YAML=$(cat <<EOF
apiVersion: networking.med.io/v1alpha1
kind: VlanTrafficControl
metadata:
  name: vtc-test-s5
  namespace: ${NAMESPACE}
spec:
  nodeSelector:
    node-role.kubernetes.io/worker: ""
  tcStrategy: ifb
  htbRoot:
    interface: ${PHYS_IFACE}
    rate: 10Gbit
    defaultClassId: "1:99"
    defaultClassMinor: 99
    classes:
      - name: default-fallback
        classId: "1:99"
        priority: 0
        ingressRate: 100Mbit
        ingressCeil: 10Gbit
        ingressBurst: 20k
        enableFqCodel: true
      - name: vlan-100-high
        classId: "1:100"
        vlanId: 100
        matchType: subnet
        subnet: 10.0.100.0/24
        priority: 1
        ingressRate: 4Gbit
        ingressCeil: 10Gbit
        ingressBurst: 60k
        enableFqCodel: true
      - name: vlan-280-medium
        classId: "1:280"
        vlanId: 280
        matchType: vlan
        priority: 2
        ingressRate: 2Gbit
        ingressCeil: 7Gbit
        ingressBurst: 40k
        enableFqCodel: true
      - name: vlan-380-migration
        classId: "1:380"
        vlanId: 380
        matchType: vlan
        priority: 3
        ingressRate: 1Gbit
        ingressCeil: 10Gbit
        ingressBurst: 80k
        enableFqCodel: true
EOF
)

S6_YAML=$(cat <<EOF
apiVersion: networking.med.io/v1alpha1
kind: VlanTrafficControl
metadata:
  name: vtc-test-s6
  namespace: ${NAMESPACE}
spec:
  nodeSelector:
    node-role.kubernetes.io/worker: ""
  tcStrategy: ifb
  htbRoot:
    interface: ${PHYS_IFACE}
    rate: 10Gbit
    defaultClassId: "1:99"
    defaultClassMinor: 99
    classes:
      - name: default-fallback
        classId: "1:99"
        priority: 0
        egressRate: 100Mbit
        egressCeil: 2Gbit
        egressBurst: 15k
        ingressRate: 200Mbit
        ingressCeil: 10Gbit
        ingressBurst: 20k
        enableFqCodel: true
      - name: vlan-100-high
        classId: "1:100"
        vlanId: 100
        matchType: subnet
        subnet: 10.0.100.0/24
        priority: 1
        egressRate: 2Gbit
        egressCeil: 5Gbit
        egressBurst: 50k
        ingressRate: 3Gbit
        ingressCeil: 10Gbit
        ingressBurst: 60k
        enableFqCodel: true
      - name: vlan-280-medium
        classId: "1:280"
        vlanId: 280
        matchType: vlan
        priority: 2
        egressRate: 1Gbit
        egressCeil: 4Gbit
        egressBurst: 30k
        ingressRate: 1.5Gbit
        ingressCeil: 8Gbit
        ingressBurst: 40k
        enableFqCodel: true
      - name: vlan-380-migration
        classId: "1:380"
        vlanId: 380
        matchType: vlan
        priority: 3
        egressRate: 500Mbit
        egressCeil: 3Gbit
        egressBurst: 70k
        ingressRate: 1Gbit
        ingressCeil: 10Gbit
        ingressBurst: 90k
        enableFqCodel: true
EOF
)

S7_YAML=$(cat <<EOF
apiVersion: networking.med.io/v1alpha1
kind: VlanTrafficControl
metadata:
  name: vtc-test-s7-teardown
  namespace: ${NAMESPACE}
spec:
  nodeSelector:
    node-role.kubernetes.io/worker: ""
  tcStrategy: ifb
  htbRoot:
    interface: ${PHYS_IFACE}
    rate: 10Gbit
    defaultClassId: "1:99"
    defaultClassMinor: 99
    classes:
      - name: default-fallback
        classId: "1:99"
        priority: 0
        ingressRate: 500Mbit
        ingressCeil: 10Gbit
        enableFqCodel: true
      - name: vlan-380-migration
        classId: "1:380"
        vlanId: 380
        matchType: vlan
        priority: 3
        ingressRate: 1Gbit
        ingressCeil: 10Gbit
        enableFqCodel: true
EOF
)

S8_YAML=$(cat <<EOF
apiVersion: networking.med.io/v1alpha1
kind: VlanTrafficControl
metadata:
  name: flower-vlan-tc-egress-phys
  namespace: ${NAMESPACE}
spec:
  nodeSelector:
    node-role.kubernetes.io/worker: ""
  tcStrategy: flower
  htbRoot:
    interface: ${PHYS_IFACE}
    htbId: 1
    rate: "10Gbit"
    defaultClassId: "1:99"
    defaultClassMinor: 99
    classes:
      - name: default-fallback
        classId: "1:99"
        egressRate: "10Gbit"
        priority: 0
      - name: vlan-100-high-priority
        classId: "1:100"
        matchType: subnet
        vlanId: 100
        subnet: "10.0.100.0/24"
        egressRate: "2Gbit"
        egressBurst: "16Mb"
        priority: 1
      - name: vlan-380-migration
        classId: "1:380"
        matchType: vlan
        vlanId: 380
        egressRate: "10Gbit"
        egressBurst: "256Mb"
        priority: 3
---
apiVersion: networking.med.io/v1alpha1
kind: VlanTrafficControl
metadata:
  name: vtc-ingress-br-vlan380
  namespace: ${NAMESPACE}
spec:
  nodeSelector:
    node-role.kubernetes.io/worker: ""
  tcStrategy: flower
  htbRoot:
    interface: ${BRIDGE_IFACE}
    htbId: 2
    classes:
      - name: vlan-100-high-priority-ingress
        classId: "2:100"
        matchType: subnet
        subnet: "10.0.100.0/24"
        priority: 1
        ingressRate: "2Gbit"
        ingressBurst: "16Mb"
        ingressAction: "pass"
      - name: vlan-380-migration-ingress
        classId: "2:380"
        matchType: subnet
        subnet: "10.0.238.0/24"
        priority: 3
        ingressRate: "10Gbit"
        ingressBurst: "256Mb"
        ingressAction: "pass"
EOF
)

S9_YAML=$(cat <<EOF
apiVersion: networking.med.io/v1alpha1
kind: VlanTrafficControl
metadata:
  name: vtc-test-s9-full-matrix
  namespace: ${NAMESPACE}
spec:
  nodeSelector:
    node-role.kubernetes.io/worker: ""
  reconcileIntervalSeconds: 15
  tcStrategy: flower
  htbRoot:
    interface: ${PHYS_IFACE}
    htbId: 10
    rate: "10Gbit"
    defaultClassId: "10:88"
    defaultClassMinor: 88
    classes:
      - name: custom-default
        classId: "10:88"
        priority: 0
        egressRate: "500Mbit"
        egressCeil: "10Gbit"
        egressBurst: "10Mb"
        enableFqCodel: true
      - name: full-spec-vlan
        classId: "10:100"
        vlanId: 100
        matchType: subnet
        subnet: "10.0.100.0/24"
        priority: 1
        egressRate: "1Gbit"
        egressCeil: "4Gbit"
        egressBurst: "32Mb"
        ingressRate: "2Gbit"
        ingressBurst: "16Mb"
        ingressAction: "pass"
        enableFqCodel: true
      - name: full-spec-mark
        classId: "10:500"
        matchType: mark
        mark: 105
        priority: 5
        egressRate: "500Mbit"
        egressCeil: "2Gbit"
        ingressRate: "1Gbit"
        ingressBurst: "8Mb"
        ingressAction: "drop"
EOF
)

# ==============================================================================
# MAIN SUITE EXECUTION
# ==============================================================================

FAILED_SUITES=()

execute_scenario "S1_EGRESS_NOIFB" "Egress only - No IFB" "${S1_YAML}" || FAILED_SUITES+=("S1_EGRESS_NOIFB")
execute_scenario "S2_INGRESS_NOIFB" "Ingress only - No IFB" "${S2_YAML}" || FAILED_SUITES+=("S2_INGRESS_NOIFB")
execute_scenario "S3_COMBINED_NOIFB" "Egress + Ingress Same VTC - No IFB" "${S3_YAML}" || FAILED_SUITES+=("S3_COMBINED_NOIFB")
execute_scenario "S4_EGRESS_IFB" "IFB Egress only" "${S4_YAML}" || FAILED_SUITES+=("S4_EGRESS_IFB")
execute_scenario "S5_INGRESS_IFB" "IFB Ingress only" "${S5_YAML}" || FAILED_SUITES+=("S5_INGRESS_IFB")
execute_scenario "S6_COMBINED_IFB" "IFB Egress + Ingress in Same VTC" "${S6_YAML}" || FAILED_SUITES+=("S6_COMBINED_IFB")
execute_scenario "S7_IFB_CLEANUP_TEARDOWN" "IFB Dedicated Teardown & Device Destruction Test" "${S7_YAML}" || FAILED_SUITES+=("S7_IFB_CLEANUP_TEARDOWN")
execute_scenario "S8_DUAL_VTC_MULTI_HTBID" "Dual VTC Multi-htbId (1:x on enp1s0 & 2:x on br-vlan380)" "${S8_YAML}" || FAILED_SUITES+=("S8_DUAL_VTC_MULTI_HTBID")
execute_scenario "S9_FULL_PARAMETER_MATRIX" "Full CRD Parameter Matrix Test (Custom htbId 10, fq_codel, mark match)" "${S9_YAML}" || FAILED_SUITES+=("S9_FULL_PARAMETER_MATRIX")

execute_drift_recovery_test || FAILED_SUITES+=("S10_KERNEL_DRIFT_SELF_HEALING")
execute_live_mutation_test || FAILED_SUITES+=("S11_LIVE_CR_MUTATION")
execute_multi_node_isolation_test || FAILED_SUITES+=("S12_MULTI_NODE_ISOLATION")
execute_invalid_spec_test || FAILED_SUITES+=("S13_INVALID_SPEC_SAFETY")
execute_pod_restart_resilience_test || FAILED_SUITES+=("S14_POD_RESTART_RESILIENCE")
execute_concurrent_race_test || FAILED_SUITES+=("S15_CONCURRENT_RACE_SAFETY")

echo ""
echo "======================================================================"
if [ ${#FAILED_SUITES[@]} -eq 0 ]; then
    log_pass "ALL 15 SCENARIOS PASSED STRICT ATTRIBUTE, TEARDOWN, DRIFT RECOVERY & RESILIENCE VERIFICATION!"
    echo "Logs saved in: ${LOG_DIR}"
    exit 0
else
    log_fail "THE FOLLOWING SCENARIOS FAILED VERIFICATION:"
    for failed in "${FAILED_SUITES[@]}"; do
        echo "  - ${failed}"
    done
    echo "Check failure diagnostic dumps in: ${LOG_DIR}"
    exit 1
fi
