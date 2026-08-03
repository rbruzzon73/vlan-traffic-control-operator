#!/usr/bin/env bash
set -euo pipefail

NODE="hub-worker01.ocp4-hub.test.com"
KEY_FILE="${HOME}/.ssh/id_rsa_vms"

# Targets (adjust if using hub-worker02 .22 addresses)
TARGET_100="10.0.100.21"
TARGET_380="10.0.238.21"

GREEN='\033[0;32m'
CYAN='\033[0;36m'
YELLOW='\033[1;33m'
RED='\033[0;31m'
NC='\033[0m'

get_class_stats() {
  local iface="$1"
  local class_id="$2"
  oc debug "node/${NODE}" --quiet=true -- chroot /host tc -s class show dev "${iface}" 2>/dev/null \
    | grep -A 3 "class htb ${class_id}"
}

echo -e "${CYAN}========================================================================${NC}"
echo -e "${CYAN}🚀 PHASE 1: SINGLE-VLAN HTB BORROWING TEST (VLAN 100)${NC}"
echo -e "${CYAN}========================================================================${NC}"

echo -e "\n--> Baseline TC stats for enp1s0.100 (Class 1:100):"
get_class_stats "enp1s0.100" "1:100"

echo -e "\n--> Running 10s iperf3 stream on VLAN 100 (Port 5201)..."
ssh -i "${KEY_FILE}" -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
  redhat@10.0.100.202 "iperf3 -c ${TARGET_100} -p 5201 -I enp2s0 -t 10"

echo -e "\n--> Post-test TC stats for enp1s0.100 (Class 1:100):"
get_class_stats "enp1s0.100" "1:100"

echo -e "\n${CYAN}========================================================================${NC}"
echo -e "${CYAN}⚔️ PHASE 2: PARALLEL PRIORITY CONTENTION TEST${NC}"
echo -e "${CYAN}   (VLAN 100 [Priority 1, Port 5201] vs VLAN 380 [Priority 3, Port 5203])${NC}"
echo -e "${CYAN}========================================================================${NC}"

echo -e "\n--> Launching parallel iperf3 streams from VM-100 (P1) and VM-380 (P3)..."

ssh -i "${KEY_FILE}" -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
  redhat@10.0.100.202 "iperf3 -c ${TARGET_100} -p 5201 -I enp2s0 -t 15" > vlan100_iperf.log 2>&1 &
PID_VLAN100=$!

ssh -i "${KEY_FILE}" -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
  redhat@10.0.238.202 "iperf3 -c ${TARGET_380} -p 5203 -I enp2s0 -t 15" > vlan380_iperf.log 2>&1 &
PID_VLAN380=$!

echo -e "--> Waiting for parallel contention tests to complete..."
wait $PID_VLAN100
wait $PID_VLAN380

echo -e "\n------------------------------------------------------------------------"
echo -e "${GREEN}📊 PARALLEL THROUGHPUT RESULTS:${NC}"
echo -e "------------------------------------------------------------------------"

echo -e "\n🔹 ${CYAN}VLAN 100 (Priority 1) Throughput:${NC}"
grep -E "sender|receiver" vlan100_iperf.log | tail -n 2 || cat vlan100_iperf.log

echo -e "\n🔹 ${YELLOW}VLAN 380 (Priority 3) Throughput:${NC}"
grep -E "sender|receiver" vlan380_iperf.log | tail -n 2 || cat vlan380_iperf.log

echo -e "\n========================================================================"
echo -e "${GREEN}🏁 HTB BORROWING & CONTENTION TEST COMPLETE${NC}"
echo "========================================================================"
