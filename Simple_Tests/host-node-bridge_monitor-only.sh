#!/usr/bin/env bash

NODE="hub-worker01.ocp4-hub.test.com"
INTERFACES=("enp1s0.100" "enp1s0.280" "enp1s0.380")
CLASSES=("1:100" "1:280" "1:380")

# Store previous state for deltas
declare -A PREV_PKTS
declare -A PREV_BORROWED
declare -A PREV_OVERLIMITS

print_header() {
  printf "%-10s | %-12s | %-8s | %-10s | %-13s | %-10s | %-14s | %-10s | %-15s\n" \
    "TIMESTAMP" "INTERFACE" "CLASS" "TOTAL PKTS" "BORROWED PKTS" "BORROW %" "DELTA BORROWED" "OVERLIMITS" "DELTA OVERLIMITS"
  echo "-----------------------------------------------------------------------------------------------------------------------------------"
}

get_htb_stats() {
  local dev=$1
  local classid=$2

  # Fetch raw tc output for the specific interface on worker01
  raw_out=$(oc debug node/${NODE} --quiet=true -- chroot /host tc -s class show dev "${dev}" 2>/dev/null)

  # Use awk to parse ONLY the 'class htb <classid>' block
  echo "$raw_out" | awk -v cid="${classid}" '
    $0 ~ "class htb " cid {
      in_block=1
      next
    }
    in_block && $0 ~ "class htb " { in_block=0 }
    in_block && $0 ~ "class fq_codel" { in_block=0 }
    in_block {
      if ($0 ~ /Sent/ ) {
        for (i=1; i<=NF; i++) {
          if ($i == "pkt") pkts = $(i-1)
          if ($i ~ /^overlimits/) {
            gsub(/[^0-9]/, "", $(i+1))
            overlimits = $(i+1)
          }
        }
      }
      if ($0 ~ /borrowed:/) {
        for (i=1; i<=NF; i++) {
          if ($i == "borrowed:") borrowed = $(i+1)
        }
      }
    }
    END {
      print (pkts ? pkts : 0) " " (borrowed ? borrowed : 0) " " (overlimits ? overlimits : 0)
    }
  '
}

clear
print_header

while true; do
  TIMESTAMP=$(date +%H:%M:%S)

  for i in "${!INTERFACES[@]}"; do
    DEV="${INTERFACES[$i]}"
    CLASS="${CLASSES[$i]}"

    read -r PKTS BORROWED OVERLIMITS <<< "$(get_htb_stats "${DEV}" "${CLASS}")"

    # Fetch previous values
    P_PKTS=${PREV_PKTS[$DEV]:-$PKTS}
    P_BORROWED=${PREV_BORROWED[$DEV]:-$BORROWED}
    P_OVERLIMITS=${PREV_OVERLIMITS[$DEV]:-$OVERLIMITS}

    # Calculate deltas
    D_BORROWED=$((BORROWED - P_BORROWED))
    D_OVERLIMITS=$((OVERLIMITS - P_OVERLIMITS))

    # Calculate borrowed percentage cleanly using awk
    if [ "$PKTS" -gt 0 ]; then
      BORROW_PCT=$(awk "BEGIN { printf \"%.1f%%\", ($BORROWED / $PKTS) * 100 }")
    else
      BORROW_PCT="0.0%"
    fi

    # Format deltas with sign
    FORMATTED_D_BORROWED=$(printf "+%d" "$D_BORROWED")
    FORMATTED_D_OVERLIMITS=$(printf "+%d" "$D_OVERLIMITS")

    printf "%-10s | %-12s | %-8s | %-10d | %-13d | %-10s | %-14s | %-10d | %-15s\n" \
      "${TIMESTAMP}" "${DEV}" "${CLASS}" "${PKTS}" "${BORROWED}" "${BORROW_PCT}" "${FORMATTED_D_BORROWED}" "${OVERLIMITS}" "${FORMATTED_D_OVERLIMITS}"

    # Update previous state
    PREV_PKTS[$DEV]=$PKTS
    PREV_BORROWED[$DEV]=$BORROWED
    PREV_OVERLIMITS[$DEV]=$OVERLIMITS
  done

  echo "-----------------------------------------------------------------------------------------------------------------------------------"
  sleep 5
done
