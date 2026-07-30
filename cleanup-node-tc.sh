#!/usr/bin/env bash
#
# Safe Traffic Control (TC) Node Cleanup Script
# Flushes HTB root qdiscs, ingress/clsact qdiscs, and associated eBPF/flower filters.

set -euo pipefail

# Ensure script is executed as root
if [ "$EUID" -ne 0 ]; then
    echo "[ERROR] This script must be run as root." >&2
    exit 1
fi

# Function to remove TC rules for a specific interface
cleanup_interface() {
    local iface="$1"

    if ! ip link show "$iface" >/dev/null 2>&1; then
        echo "[WARN] Interface '$iface' does not exist on this host. Skipping."
        return 0
    fi

    echo "[INFO] Starting TC cleanup on interface: ${iface}"

    # 1. Remove Root Qdisc (Clears HTB trees, classes, and egress filters)
    if tc qdisc show dev "$iface" | grep -q "qdisc htb"; then
        echo "  -> Deleting HTB root qdisc on ${iface}..."
        tc qdisc del dev "$iface" root || echo "  [!] Failed or already removed root qdisc on ${iface}"
    else
        echo "  -> No HTB root qdisc detected on ${iface}."
    fi

    # 2. Remove Ingress / clsact Qdisc (Clears eBPF policing & flower filters)
    if tc qdisc show dev "$iface" | grep -E -q "qdisc (ingress|clsact)"; then
        echo "  -> Deleting ingress/clsact qdisc on ${iface}..."
        tc qdisc del dev "$iface" ingress 2>/dev/null || true
        tc qdisc del dev "$iface" clsact 2>/dev/null || true
    else
        echo "  -> No ingress/clsact qdisc detected on ${iface}."
    fi

    echo "[SUCCESS] Finished TC cleanup on interface: ${iface}"
}

# --- Execution Logic ---

# Mode 1: Clean up a specific interface if passed as an argument
if [ $# -gt 0 ]; then
    cleanup_interface "$1"
    exit 0
fi

# Mode 2: Auto-discover interfaces with active HTB, ingress, or clsact qdiscs
echo "[INFO] Auto-discovering network interfaces with active TC rules..."

ACTIVE_IFACES=$(tc qdisc show | grep -E "qdisc (htb|ingress|clsact)" | awk '{print $5}' | sort -u)

if [ -z "$ACTIVE_IFACES" ]; then
    echo "[INFO] No active HTB, ingress, or clsact qdiscs found on any interface."
    exit 0
fi

for dev in $ACTIVE_IFACES; do
    cleanup_interface "$dev"
done

echo "[COMPLETE] All node TC configurations have been reset."
