#!/usr/bin/env bash
set -euo pipefail

NODE="hub-worker01.ocp4-hub.test.com"
KEY_FILE="${HOME}/.ssh/id_rsa_vms"

GREEN='\033[0;32m'
CYAN='\033[0;36m'
YELLOW='\033[1;33m'
RED='\033[0;31m'
NC='\033[0m'

# Map of interfaces to test
declare -A IFACE_MAP=(
  ["100"]="enp1s0.100|1:100"
  ["280"]="enp1s0.280|1:280"
  ["380"]="enp1s0.380|1:380"
)

# Fetch current packet counters across ALL interfaces on the worker node
get_all_counters() {
  local tc_output
  tc_output=$(oc debug "node/${NODE}" --quiet=true -- chroot /host bash -c \
    "tc -s class show dev enp1s0.100; tc -s class show dev enp1s0.280; tc -s class show dev enp1s0.380" 2>/dev/null)

  for tag in 100 280 380; do
    IFS="|" read -r iface class_id <<< "${IFACE_MAP[$tag]}"
    local pkts
    pkts=$(echo "${tc_output}" | grep -A 2 "class htb ${class_id}" | grep "Sent" | awk '{print $4}' || echo "0")
    echo "${tag}:${pkts}"
  done
}

run_test() {
  local target_tag="$1"
  local vm_name="$2"
  local vm_ip="$3"
  local target_ip="$4"

  IFS="|" read -r target_iface target_class <<< "${IFACE_MAP[$target_tag]}"

  echo -e "\n========================================================================"
  echo -e "${CYAN}🧪 Testing Isolation on VLAN ${target_tag} (${target_iface} -> Class ${target_class})${NC}"
  echo "========================================================================"

  # 1. Capture BEFORE snapshot across ALL interfaces
  declare -A before_pkts
  while IFS=":" read -r tag count; do
    before_pkts["$tag"]="$count"
  done < <(get_all_counters)

  echo "--> Initial Packet Counters:"
  for tag in 100 280 380; do
    IFS="|" read -r iface class <<< "${IFACE_MAP[$tag]}"
    echo "    • ${iface} (Class ${class}): ${before_pkts[$tag]} pkts"
  done

  echo -e "\n--> Injecting 200 ICMP packets from ${vm_name} (${vm_ip}) to ${target_ip}..."
  ssh -i "${KEY_FILE}" \
    -o StrictHostKeyChecking=no \
    -o UserKnownHostsFile=/dev/null \
    -o ConnectTimeout=5 \
    "redhat@${vm_ip}" \
    "ping -I enp2s0 -c 200 -i 0.2 ${target_ip}" >/dev/null 2>&1 || true

  # 2. Capture AFTER snapshot across ALL interfaces
  declare -A after_pkts
  while IFS=":" read -r tag count; do
    after_pkts["$tag"]="$count"
  done < <(get_all_counters)

  echo -e "\n--> Final Packet Counters & Isolation Verification:"
  local test_failed=0

  for tag in 100 280 380; do
    IFS="|" read -r iface class <<< "${IFACE_MAP[$tag]}"
    local delta=$((after_pkts[$tag] - before_pkts[$tag]))

    if [ "$tag" == "$target_tag" ]; then
      # Target VLAN must increase
      if [ "$delta" -gt 0 ]; then
        echo -e "    ${GREEN}✅ ${iface} [TARGET]: +${delta} packets (Expected traffic increase)${NC}"
      else
        echo -e "    ${RED}❌ ${iface} [TARGET]: +${delta} packets (FAIL: Expected traffic, got 0)${NC}"
        test_failed=1
      fi
    else
      # Non-target VLANs must remain unchanged (+0)
      if [ "$delta" -eq 0 ]; then
        echo -e "    ${GREEN}✅ ${iface} [ISOLATED]: +${delta} packets (Perfect isolation)${NC}"
      else
        echo -e "    ${RED}❌ ${iface} [LEAK DETECTED]: +${delta} packets (FAIL: Traffic leaked onto non-target VLAN!)${NC}"
        test_failed=1
      fi
    fi
  done

  if [ "$test_failed" -eq 0 ]; then
    echo -e "\n${GREEN}🎉 RESULT: VLAN ${target_tag} PASSED isolation & TC verification!${NC}"
  else
    echo -e "\n${RED}🚨 RESULT: VLAN ${target_tag} FAILED validation!${NC}"
  fi
}

echo -e "\n${CYAN}========================================================================${NC}"
echo -e "${CYAN}🧪 EXECUTING FULL MULTI-VLAN ISOLATION & TC VALIDATION SUITE${NC}"
echo -e "${CYAN}========================================================================${NC}"

run_test "100" "vm-vlan100" "10.0.100.202" "10.0.100.21"
run_test "280" "vm-vlan280" "10.0.218.202" "10.0.218.21"
run_test "380" "vm-vlan380" "10.0.238.202" "10.0.238.21"

echo -e "\n========================================================================"
echo -e "${GREEN}🏁 ALL ISOLATION & TC TESTS COMPLETED${NC}"
echo "========================================================================"
