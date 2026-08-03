#!/usr/bin/env bash
set -eo pipefail

NAMESPACE="tc-virt-validation"

RED='\033[0;31m'
YELLOW='\033[1;33m'
GREEN='\033[0;32m'
NC='\033[0m'

echo -e "${RED}========================================================================${NC}"
echo -e "${RED}💣 STARTING COMPLETE TEARDOWN & CLEANUP OF TRAFFIC CONTROL TEST SETUP${NC}"
echo -e "${RED}========================================================================${NC}"

# 1. Delete VirtualMachine Instances
echo -e "\n${YELLOW}1. Deleting VirtualMachines (vm-vlan100, vm-vlan280, vm-vlan380)...${NC}"
oc delete vm vm-vlan100 vm-vlan280 vm-vlan380 -n "${NAMESPACE}" --ignore-not-found=true --wait=true

# 2. Delete VlanTrafficControl Policies
echo -e "\n${YELLOW}2. Deleting VlanTrafficControl CRs...${NC}"
oc delete vlantrafficcontrol vlan-tc-vlan100 vlan-tc-vlan280 vlan-tc-vlan380 --ignore-not-found=true

# 3. Delete NetworkAttachmentDefinitions
echo -e "\n${YELLOW}3. Deleting NetworkAttachmentDefinitions...${NC}"
oc delete nad nad-vlan100 nad-vlan280 nad-vlan380 -n "${NAMESPACE}" --ignore-not-found=true

# 4. Delete NMState Policy
echo -e "\n${YELLOW}4. Deleting NodeNetworkConfigurationPolicy (nncp)...${NC}"
oc delete nncp worker-vlan-bridges-policy br-vlan380-worker-policy --ignore-not-found=true

# 5. Remove Test Namespace
echo -e "\n${YELLOW}5. Deleting Namespace '${NAMESPACE}'...${NC}"
oc delete namespace "${NAMESPACE}" --ignore-not-found=true

# 6. Flush TC Qdiscs and Remove Linux Bridges on Worker Node
echo -e "\n${YELLOW}6. Flushing host interfaces & removing bridges on hub-worker01...${NC}"
oc debug node/hub-worker01.ocp4-hub.test.com --quiet=true -- chroot /host bash -c "
  # Remove TC qdiscs attached to VLAN sub-interfaces
  tc qdisc del dev enp1s0.100 root 2>/dev/null || true
  tc qdisc del dev enp1s0.280 root 2>/dev/null || true
  tc qdisc del dev enp1s0.380 root 2>/dev/null || true

  # Bring down and delete Linux bridges
  ip link set dev br-vlan100 down 2>/dev/null || true
  ip link set dev br-vlan280 down 2>/dev/null || true
  ip link set dev br-vlan380 down 2>/dev/null || true

  ip link del name br-vlan100 2>/dev/null || true
  ip link del name br-vlan280 2>/dev/null || true
  ip link del name br-vlan380 2>/dev/null || true

  # Remove VLAN sub-interfaces
  ip link del dev enp1s0.100 2>/dev/null || true
  ip link del dev enp1s0.280 2>/dev/null || true
  ip link del dev enp1s0.380 2>/dev/null || true
" || true

# 7. Remove local generated scripts
echo -e "\n${YELLOW}7. Cleaning up local bash test scripts...${NC}"
rm -f run-tc-validation.sh auto-tc-test.sh test-perf-tc.sh test-htb-borrowing.sh test-tc-suite.sh nncp-vlans.yaml nads.yaml vlan-tc-rules.yaml vms.yaml

echo -e "\n${GREEN}========================================================================${NC}"
echo -e "${GREEN}✨ CLEANUP COMPLETE: All VMs, TC rules, NADs, and host interfaces wiped!${NC}"
echo -e "${GREEN}========================================================================${NC}"
