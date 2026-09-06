#!/usr/bin/env bash

set -euo pipefail

# ==============================================================================
# CONFIGURATION
# ==============================================================================
NAMESPACE="openshift-vlan-tc-operator"
TARGET_IFACE="enp1s0"
AGENT_NODE_IP="192.168.100.21"
AGENT_NODE_NAME="hub-worker01.ocp4-hub.test.com"

LOG_DIR="./vtc-strict-test-logs-$(date +%Y%m%d_%H%M%S)"
TMP_YAML="/tmp/vtc-test-manifest.yaml"

mkdir -p "${LOG_DIR}"

echo "======================================================================"
echo " Starting Full Attribute & Interface Verification Test Suite"
echo " Target Interface: ${TARGET_IFACE}"
echo " Agent Node IP:    ${AGENT_NODE_IP}"
echo " Agent Node Name:  ${AGENT_NODE_NAME}"
echo " Output Log Dir:   ${LOG_DIR}"
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
        log_pass "   ✓ [ASSERT] ${field_description}: '${actual}'"
    else
        log_fail "   ✗ [ASSERT ERROR] ${field_description}: expected '${expected}', got '${actual}'"
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

    curl -s "http://${AGENT_NODE_IP}:8080/config?interface=${TARGET_IFACE}" | jq . > "${fail_dir}/curl-config.json" 2>&1 || true
    curl -s "http://${AGENT_NODE_IP}:8080/stats?interface=${TARGET_IFACE}" | jq . > "${fail_dir}/curl-stats.json" 2>&1 || true

    oc debug node/"${AGENT_NODE_NAME}" -- chroot /host tc -s qdisc show dev "${TARGET_IFACE}" > "${fail_dir}/tc-qdisc.txt" 2>&1 || true
    oc debug node/"${AGENT_NODE_NAME}" -- chroot /host tc -s class show dev "${TARGET_IFACE}" > "${fail_dir}/tc-class.txt" 2>&1 || true
    oc debug node/"${AGENT_NODE_NAME}" -- chroot /host tc filter show dev "${TARGET_IFACE}" > "${fail_dir}/tc-filter.txt" 2>&1 || true

    IFB_DEV="ifb-${TARGET_IFACE}"
    IFB_DEV="${IFB_DEV:0:15}"
    oc debug node/"${AGENT_NODE_NAME}" -- chroot /host tc -s class show dev "${IFB_DEV}" > "${fail_dir}/tc-class-ifb.txt" 2>&1 || true
}

cleanup_resources() {
    log_info "Flushing VTC cluster resources & lingering host IFB links..."
    oc delete vtc --all -n "${NAMESPACE}" --timeout=30s 2>/dev/null || true
    IFB_DEV="ifb-${TARGET_IFACE}"
    IFB_DEV="${IFB_DEV:0:15}"
    oc debug node/"${AGENT_NODE_NAME}" -- chroot /host ip link del dev "${IFB_DEV}" 2>/dev/null || true
    sleep 5
}

# ==============================================================================
# DEEP ATTRIBUTE & INTERFACE VERIFICATION
# ==============================================================================

verify_scenario_attributes() {
    local scenario_id="$1"
    local config_json="$2"
    local stats_json="$3"

    log_info "Running Deep Value & Interface Assertion for [${scenario_id}]..."

    # 1. Config Alignment Status
    local is_aligned
    is_aligned=$(echo "${config_json}" | jq -r '.isAligned')
    assert_equals "${is_aligned}" "true" "Host Alignment Status (isAligned)" || return 1

    local drift_count
    drift_count=$(echo "${config_json}" | jq '.driftDeltas | length')
    assert_equals "${drift_count}" "0" "Drift Deltas Count" || return 1

    local actual_phys_iface
    actual_phys_iface=$(echo "${config_json}" | jq -r '.actual.interface // .interface')
    assert_equals "${actual_phys_iface}" "${TARGET_IFACE}" "Actual Config Physical Interface" || return 1

    # 2. VlanTrafficControlClass Projections in Kube API
    local vtcclass_json
    vtcclass_json=$(oc get vtcclass -n "${NAMESPACE}" -o json)

    case "${scenario_id}" in
        "S1_EGRESS_NOIFB")
            assert_equals "$(echo "${vtcclass_json}" | jq -r '.items[] | select(.spec.className=="vlan-100-high") | .spec.direction')" "egress" "VTCClass Direction (VLAN 100)" || return 1
            assert_equals "$(echo "${vtcclass_json}" | jq -r '.items[] | select(.spec.className=="vlan-100-high") | .spec.guaranteed')" "2Gbit" "VTCClass Egress Rate (VLAN 100)" || return 1
            assert_equals "$(echo "${vtcclass_json}" | jq -r '.items[] | select(.spec.className=="vlan-100-high") | .spec.ceilBorrow')" "8Gbit" "VTCClass Egress Ceil (VLAN 100)" || return 1
            assert_equals "$(echo "${config_json}" | jq -r '.actual.classes[] | select(.classId=="1:100") | .egressRate')" "2Gbit" "Kernel Class 1:100 Egress Rate" || return 1

            local non_phys_classes
            non_phys_classes=$(echo "${stats_json}" | jq -r '[.classStats[] | select(.interface!="'"${TARGET_IFACE}"'" or .direction!="egress")] | length')
            assert_equals "${non_phys_classes}" "0" "Egress-only classStats Interface & Direction Mismatches" || return 1
            ;;

        "S2_INGRESS_NOIFB")
            assert_equals "$(echo "${vtcclass_json}" | jq -r '.items[] | select(.spec.className=="vlan-100-high") | .spec.direction')" "ingress" "VTCClass Direction (VLAN 100)" || return 1
            assert_equals "$(echo "${vtcclass_json}" | jq -r '.items[] | select(.spec.className=="vlan-100-high") | .spec.ingressRate')" "3Gbit" "VTCClass Ingress Rate (VLAN 100)" || return 1

            local filter_action
            filter_action=$(echo "${config_json}" | jq -r '.actual.ingressFilters[] | select(.vlanId==100) | .action')
            if [ -z "${filter_action}" ] || [ "${filter_action}" == "null" ]; then
                filter_action=$(echo "${config_json}" | jq -r '.actual.ingressFilters[] | select(.priority==1) | .action')
            fi
            assert_equals "${filter_action}" "police drop" "Filter Action (VLAN 100)" || return 1

            local raw_ingress_iface
            raw_ingress_iface=$(echo "${config_json}" | jq -r '.actual.ingressFilters[] | select(.vlanId==100) | .interface')
            local ingress_filter_iface
            ingress_filter_iface=$(resolve_target_interface "${raw_ingress_iface}")
            assert_equals "${ingress_filter_iface}" "${TARGET_IFACE}" "Stateless Ingress Filter Interface (VLAN 100)" || return 1
            ;;

        "S3_COMBINED_NOIFB")
            assert_equals "$(echo "${vtcclass_json}" | jq -r '.items[] | select(.spec.className=="vlan-100-high") | .spec.direction')" "ingress+egress" "VTCClass Direction (VLAN 100)" || return 1
            assert_equals "$(echo "${vtcclass_json}" | jq -r '.items[] | select(.spec.className=="vlan-100-high") | .spec.guaranteed')" "2Gbit" "VTCClass Guaranteed Rate (VLAN 100)" || return 1
            assert_equals "$(echo "${vtcclass_json}" | jq -r '.items[] | select(.spec.className=="vlan-100-high") | .spec.ingressRate')" "3Gbit" "VTCClass Ingress Rate (VLAN 100)" || return 1
            ;;

        "S4_EGRESS_IFB")
            assert_equals "$(echo "${vtcclass_json}" | jq -r '.items[] | select(.spec.className=="vlan-280-medium") | .spec.direction')" "egress" "VTCClass Direction (VLAN 280)" || return 1
            assert_equals "$(echo "${config_json}" | jq -r '.actual.classes[] | select(.classId=="1:280") | .egressRate')" "1200Mbit" "Kernel Class 1:280 Egress Rate" || return 1
            ;;

        "S5_INGRESS_IFB")
            assert_equals "$(echo "${vtcclass_json}" | jq -r '.items[] | select(.spec.className=="vlan-380-migration") | .spec.direction')" "ingress" "VTCClass Direction (VLAN 380)" || return 1
            assert_equals "$(echo "${config_json}" | jq -r '.actual.ifbInterface // "ifb-" + .interface')" "ifb-${TARGET_IFACE}" "Actual Config IFB Interface" || return 1
            assert_equals "$(echo "${config_json}" | jq -r '.actual.ingressFilters[] | select(.type=="matchall") | .action')" "mirred redirect dev ifb-enp1s0" "Catch-All Redirect Action" || return 1

            local ifb_class_iface
            ifb_class_iface=$(echo "${stats_json}" | jq -r '.classStats[] | select(.classId=="1:100") | .interface')
            assert_equals "${ifb_class_iface}" "ifb-${TARGET_IFACE}" "IFB Ingress classStat Interface Label" || return 1

            local ifb_class_dir
            ifb_class_dir=$(echo "${stats_json}" | jq -r '.classStats[] | select(.classId=="1:100") | .direction')
            assert_equals "${ifb_class_dir}" "ingress" "IFB Ingress classStat Direction Label" || return 1
            ;;

        "S6_COMBINED_IFB")
            assert_equals "$(echo "${vtcclass_json}" | jq -r '.items[] | select(.spec.className=="vlan-100-high") | .spec.direction')" "ingress+egress" "VTCClass Direction (VLAN 100)" || return 1

            assert_equals "$(echo "${config_json}" | jq -r '.actual.classes[] | select(.classId=="1:100") | .egressRate')" "2Gbit" "Class 1:100 Egress Rate" || return 1
            assert_equals "$(echo "${config_json}" | jq -r '.actual.classes[] | select(.classId=="1:100") | .egressCeil')" "5Gbit" "Class 1:100 Egress Ceil" || return 1
            assert_equals "$(echo "${config_json}" | jq -r '.actual.classes[] | select(.classId=="1:100") | .ingressRate')" "3Gbit" "Class 1:100 Ingress Rate" || return 1
            assert_equals "$(echo "${config_json}" | jq -r '.actual.classes[] | select(.classId=="1:100") | .ingressCeil')" "10Gbit" "Class 1:100 Ingress Ceil" || return 1

            assert_equals "$(echo "${config_json}" | jq -r '.actual.ingressFilters[] | select(.type=="matchall") | .interface')" "${TARGET_IFACE}" "Redirect Filter Interface" || return 1
            assert_equals "$(echo "${config_json}" | jq -r '.actual.ingressFilters[] | select(.type=="matchall") | .action')" "mirred redirect dev ifb-enp1s0" "Redirect Filter Action" || return 1

            assert_equals "$(echo "${config_json}" | jq -r '.actual.ingressFilters[] | select(.vlanId==100 and .action=="htb classify") | .action')" "htb classify" "IFB Classify Action (VLAN 100)" || return 1

            local egress_class_iface
            egress_class_iface=$(echo "${stats_json}" | jq -r '.classStats[] | select(.classId=="1:100" and .direction=="egress") | .interface')
            assert_equals "${egress_class_iface}" "${TARGET_IFACE}" "Combined Egress Class Interface (${TARGET_IFACE})" || return 1

            local ingress_class_iface
            ingress_class_iface=$(echo "${stats_json}" | jq -r '.classStats[] | select(.classId=="1:100" and .direction=="ingress") | .interface')
            assert_equals "${ingress_class_iface}" "ifb-${TARGET_IFACE}" "Combined Ingress Class Interface (ifb-${TARGET_IFACE})" || return 1

            local raw_ingress_filter_iface
            raw_ingress_filter_iface=$(echo "${config_json}" | jq -r '.actual.ingressFilters[] | select(.vlanId==100 and .action=="htb classify") | .interface')
            local ingress_filter_iface
            ingress_filter_iface=$(resolve_target_interface "${raw_ingress_filter_iface}")
            assert_equals "${ingress_filter_iface}" "ifb-${TARGET_IFACE}" "Combined Ingress Filter Interface (ifb-${TARGET_IFACE})" || return 1
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

    log_info "3. Querying Agent /config endpoint..."
    local config_resp
    config_resp=$(curl -s "http://${AGENT_NODE_IP}:8080/config?interface=${TARGET_IFACE}")
    echo "${config_resp}" | jq . > "${LOG_DIR}/${scenario_id}_curl_config.json"

    log_info "4. Querying Agent /stats endpoint..."
    local stats_resp
    stats_resp=$(curl -s "http://${AGENT_NODE_IP}:8080/stats?interface=${TARGET_IFACE}")
    echo "${stats_resp}" | jq . > "${LOG_DIR}/${scenario_id}_curl_stats.json"

    log_info "5. Running Strict Attribute Assertion Suite..."
    if ! verify_scenario_attributes "${scenario_id}" "${config_resp}" "${stats_resp}"; then
        log_fail "Strict attribute assertion failed!"
        collect_failure_diagnostics "${scenario_id}"
        return 1
    fi

    log_info "6. Testing CR Deletion & Host Cleanup..."
    cleanup_resources

    local post_delete_config
    post_delete_config=$(curl -s "http://${AGENT_NODE_IP}:8080/config?interface=${TARGET_IFACE}")
    local htb_present
    htb_present=$(echo "${post_delete_config}" | jq -r '.actual.htbQdiscPresent')

    if [ "${htb_present}" == "true" ]; then
        log_fail "HTB qdisc remained active after CR deletion!"
        collect_failure_diagnostics "${scenario_id}"
        return 1
    fi

    log_pass "Scenario [${scenario_id}] completed and verified successfully!"
    return 0
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
    interface: ${TARGET_IFACE}
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
    interface: ${TARGET_IFACE}
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
    interface: ${TARGET_IFACE}
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
    interface: ${TARGET_IFACE}
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
    interface: ${TARGET_IFACE}
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
    interface: ${TARGET_IFACE}
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

echo ""
echo "======================================================================"
if [ ${#FAILED_SUITES[@]} -eq 0 ]; then
    log_pass "ALL 6 SCENARIOS PASSED STRICT ATTRIBUTE & INTERFACE VERIFICATION!"
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
