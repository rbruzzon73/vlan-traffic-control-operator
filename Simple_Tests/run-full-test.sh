#!/usr/bin/env bash

# Target Node & Cluster Scope
export TARGET_WORKER="hub-worker02.ocp4-hub.test.com"
export NAMESPACE="openshift-vlan-tc-operator"
export CR_NAME="verify-all-fields-custom-tc"
export MANAGER_DEPLOY="deploy/vlan-traffic-control-manager"
export AGENT_LABEL="app=vlan-traffic-control-agent"

# Root HTB Hierarchy Configuration (htbRoot)
export TEST_IFACE="enp1s0"
export TEST_ROOT_RATE="10Gbit"
export TEST_ROOT_HTB_ID=6
export TEST_DEFAULT_CLASS_ID="6:99"
export TEST_RECONCILE_INTERVAL=15
export TEST_STRATEGY="flower"

# Class 1: 802.1Q VLAN Match (matchType: vlan)
export VLAN_CLASS_NAME="vlan150-prod-class"
export VLAN_CLASS_ID="6:100"
export VLAN_ID=150
export VLAN_PRIO=1
export VLAN_EGRESS_RATE="100Mbit"
export VLAN_EGRESS_CEIL="200Mbit"
export VLAN_EGRESS_BURST="20k"
export VLAN_INGRESS_RATE="50Mbit"
export VLAN_INGRESS_BURST="100k"
export VLAN_INGRESS_ACTION="drop"
export VLAN_ENABLE_FQCODEL="true"

# Class 2: IP Subnet Match (matchType: subnet)
export SUBNET_CLASS_NAME="app-subnet-class"
export SUBNET_CLASS_ID="6:200"
export SUBNET_CIDR="172.16.50.0/24"
export SUBNET_PRIO=2
export SUBNET_EGRESS_RATE="300Mbit"
export SUBNET_EGRESS_CEIL="600Mbit"
export SUBNET_EGRESS_BURST="40k"
export SUBNET_INGRESS_RATE="150Mbit"
export SUBNET_INGRESS_BURST="80k"

# Class 3: SKB Mark Match (matchType: mark)
export MARK_CLASS_NAME="fwmark-sec-class"
export MARK_CLASS_ID="6:300"
export MARK_VAL=16
export MARK_PRIO=3
export MARK_EGRESS_RATE="500Mbit"
export MARK_EGRESS_CEIL="500Mbit"
export MARK_EGRESS_BURST="50k"
export MARK_INGRESS_RATE="100Mbit"
export MARK_INGRESS_BURST="50k"
export MARK_INGRESS_ACTION="drop"
export MARK_ENABLE_FQCODEL="true"

# Class 4: Auto-detect Match (matchType: auto)
export AUTO_CLASS_NAME="auto-vlan250-class"
export AUTO_CLASS_ID="6:400"
export AUTO_VLAN_ID=250
export AUTO_PRIO=4
export AUTO_EGRESS_RATE="1Gbit"
export AUTO_EGRESS_CEIL="2Gbit"
export AUTO_EGRESS_BURST="60k"
export AUTO_INGRESS_RATE="200Mbit"
export AUTO_INGRESS_BURST="100k"
export AUTO_INGRESS_ACTION="drop"
export AUTO_ENABLE_FQCODEL="false"

# Execute test suite
./verify-tc-cr-fields.sh
