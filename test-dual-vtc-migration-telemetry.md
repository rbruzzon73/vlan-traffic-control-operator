# `test-dual-vtc-migration-telemetry.sh` Dual-Strategy Specification (IFB vs. Flower)

### **Overview & Architectural Intent**
The `test-dual-vtc-migration-telemetry.sh` suite validates two distinct Linux kernel Traffic Control (`tc`) operational modes used by the `vlan-traffic-control-operator` during KubeVirt VM Live Migrations:

1. **Stateless Flower Policing (`tcStrategy: flower`)**: Uses `cls_flower` filters directly on host interface ingress queues (`ffff:`) with `act_police` rate-limiting.
2. **Stateful IFB Redirection (`tcStrategy: ifb`)**: Redirects physical ingress packets (`act_mirred`) to an Intermediate Functional Block (`ifb-enp1s0`) virtual device to apply full Hierarchical Token Bucket (`htb`) class shaping on incoming migration streams.

---

### **Hardware Offload & Interface Targeting Mechanics**

#### **Why Ingress Appears Untagged on `enp1s0`**
* **NIC Hardware VLAN Offloading:** Modern physical NIC drivers (e.g., `rx-vlan-offload` on `enp1s0`) strip incoming 802.1Q tags at the hardware layer before handing socket buffers (`skb`) to the TC ingress hook (`ffff:`). Consequently, `cls_flower` filters targeting explicit `vlan_id` tags on `enp1s0` observe zero hits, while catch-all filters (`pref 49152`) capture the untagged IP payload frames.
* **Bridge Layer Demuxing:** In OpenShift/KubeVirt secondary networking setups, the physical interface (`enp1s0`) acts as an uplink to a bridge (`br-vlan380`). Ingress migration traffic targeting secondary VM interfaces must be evaluated at the bridge layer (`br-vlan380`), where VLAN tagging and subnet routing are resolved, rather than on the raw physical link.

#### **Why Two VTC Custom Resources (CRs) are Required**
1. **Separation of Interface Targets (Uplink vs. Bridge):**
   * **Egress VTC CR (Targeting `enp1s0`):** Outgoing migration traffic leaving the host must be shaped using an HTB class hierarchy directly on the physical uplink (`enp1s0`) to enforce outbound bandwidth limits before packets hit the physical switch.
   * **Ingress VTC CR (Targeting `br-vlan380` or `ifb-enp1s0`):** Incoming migration traffic entering the host cannot be reliably filtered by VLAN ID on `enp1s0` due to NIC hardware stripping. Placing the ingress Flower policing filter on `br-vlan380` allows the agent to evaluate traffic after the bridge demuxes it into the target VM's secondary network domain.
2. **Separation of Shaping Mechanics per Direction:**
   * **Egress (HTB Stateful Hierarchy):** Outbound traffic shaping requires stateful queue discipline (`sch_htb`) with parent/child class hierarchies (`1:1`, `1:380`) to guarantee migration bandwidth while allowing fallback borrowing (`1:99`).
   * **Ingress (Stateless Policing vs. IFB Redirection):** Inbound traffic cannot be queued directly on ingress without redirecting to an `ifb` device. Using a dedicated Ingress CR allows administrators to flexibly toggle between lightweight `act_police` directly on `br-vlan380` or full IFB ingress shaping on `ifb-enp1s0`.

---

### **Detailed Test Strategy Breakdown**

#### **1. Stateless Flower Policing Test Path (`tcStrategy: flower`)**
* **Kernel Mechanics:**
  * Attaches a `clsact` or `ingress` qdisc handle (`ffff:`) directly to physical or bridge interfaces (`enp1s0` or `br-vlan380`).
  * Applies `cls_flower` matching rules (matching `vlan_id`, `dst_ip` subnets, or `fwmark`).
  * Attaches an `act_police` action: `police rate <RATE> burst <BURST> conform-exceed pass` (or `drop`).
* **What is Tested & Checked:**
  * **Direct Netlink Attach:** Confirms filters are attached to `ffff:` without spawning virtual IFB devices.
  * **Non-Disruptive Migration Measurement:** Confirms the `ingressAction: pass` flag translates into `conform-exceed pass`, allowing live migration traffic to be accurately measured in byte/packet counters without dropping frames.
  * **Stateless Subnet Protocol Matching:** Verifies that untagged bridge filters automatically set `protocol ip` to prevent kernel iproute2 parse failures (`Illegal "dst_ip"`).

#### **2. Stateful IFB Redirection Test Path (`tcStrategy: ifb`)**
* **Kernel Mechanics:**
  * Dynamically creates an IFB virtual netdevice on the host (`ifb-enp1s0`) and brings its link state `UP`.
  * Attaches a `matchall` filter on physical `ffff:` redirecting all ingress frames (`action mirred egress redirect dev ifb-enp1s0`).
  * Instantiates a root HTB qdisc (`1:`) on `ifb-enp1s0` and builds an egress-like HTB class hierarchy for incoming traffic.
* **What is Tested & Checked:**
  * **Virtual Device Lifecycle:** Verifies `ifb-enp1s0` is dynamically instantiated, brought `UP`, and cleanly destroyed upon VTC CR deletion.
  * **Redirect Filter Integrity:** Checks that the physical interface features an active `mirred redirect` filter targeting `ifb-enp1s0`.
  * **Ingress Class Telemetry Mapping:** Confirms the agent REST API (`/stats`) queries `ifb-enp1s0` netlink class lists and correctly attributes ingress traffic counters to `classId: "1:380"`.
  * **Ghost Stat Suppression:** Ensures stats from the physical interface and the IFB interface are not double-counted or duplicated across endpoints.

---

### **Summary of Parameters Checked Across Strategies**

| Parameter / Feature | Stateless Flower Path (`flower`) | Stateful IFB Path (`ifb`) |
| :--- | :--- | :--- |
| **Interface Targeted** | Physical or Bridge Link (`enp1s0`, `br-vlan380`) | Physical Link $\rightarrow$ Virtual Device (`ifb-enp1s0`) |
| **Ingress Qdisc Handle** | `ffff:` (Direct Ingress) | `ffff:` (Redirect) + `1:` (Root HTB on IFB) |
| **Ingress Shaping Type** | Stateless Rate Policing (`act_police`) | Full HTB Class Hierarchy (`sch_htb`) |
| **Live Migration Action** | `police rate 10Gbit burst 256Mb conform-exceed pass` | `htb classify` into child class `1:380` |
| **Telemetry Source** | Netlink filter action statistics (`netlink.Flower`) | Netlink class statistics (`netlink.HtbClass`) |
| **Device Teardown Rule** | Deletes `ffff:` ingress qdisc | Deletes `ffff:`, flushes IFB root `1:`, deletes link `ifb-enp1s0` |
