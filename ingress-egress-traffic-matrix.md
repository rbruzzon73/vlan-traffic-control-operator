### 1. Ingress Traffic Matrix (Ingress / `ffff:`)

| Strategy | Attachment Location | Monitored Ingress Interface | Hardware Offload (`rx-vlan-offload`) | Packet Header at TC Ingress | Rule / Filter Matching Behavior | Telemetry Counter Source |
| :--- | :--- | :--- | :--- | :--- | :--- | :--- |
| **Flower** | **Physical Link** (`enp1s0`) | `enp1s0` | **Enabled** (Default) | Untagged `ETH_P_IP` (0x0800) | Falls through to default `protocol ip` / `pref 49152` catch-all | Netlink Action Stats (`act_police` under `pref 49152`) |
| **Flower** | **Physical Link** (`enp1s0`) | `enp1s0` | **Disabled** | Tagged `ETH_P_8021Q` (0x8100) | `protocol 802.1q vlan_id 380` | Netlink Action Stats (`act_police` under `pref 1`/`pref 3`) |
| **Flower** | **Bridge Link** (`br-vlan380`) | `br-vlan380` | **N/A** (Software Bridge) | Demuxed per-VLAN stream | `protocol ip` + `dst_ip` subnet match | Netlink Action Stats (`act_police` on `br-vlan380`) |
| **IFB** | **Physical Link** (`enp1s0`) $\rightarrow$ `ifb-enp1s0` | `ifb-enp1s0` | **Enabled / Disabled** | Redirected via `act_mirred` | `matchall` filter redirects all ingress frames to `ifb-enp1s0` | Netlink Class Stats (`sch_htb` classes `1:380` on `ifb-enp1s0`) |

---

### 2. Egress Traffic Matrix (Egress / `root 1:`)

| Strategy | Attachment Location | Monitored Egress Interface | Hardware Offload (`tx-vlan-offload`) | Packet Header at TC Egress | Rule / Filter Matching Behavior | Telemetry Counter Source |
| :--- | :--- | :--- | :--- | :--- | :--- | :--- |
| **Flower** | **Physical Link** (`enp1s0`) | `enp1s0` | **Enabled** (Default) | Untagged `ETH_P_IP` (Tag inserted by HW later) | HTB filter matching mark/skb priority or Class ID (`1:380`) | Netlink Class Stats (`sch_htb` class `1:380` on `enp1s0`) |
| **Flower** | **Physical Link** (`enp1s0`) | `enp1s0` | **Disabled** | Tagged `ETH_P_8021Q` (0x8100) | `protocol 802.1q vlan_id 380` mapped to Class ID (`1:380`) | Netlink Class Stats (`sch_htb` class `1:380` on `enp1s0`) |
| **IFB** | **Physical Link** (`enp1s0`) | `enp1s0` | **Enabled / Disabled** | Native Host Egress Stream | Direct HTB class hierarchy (`1:1` root parent, `1:380` leaf child) | Netlink Class Stats (`sch_htb` class `1:380` on `enp1s0`) |
