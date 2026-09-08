# VLAN Traffic Control Operator - Solution Overview & Technical Architecture 

---

# VLAN Traffic Control Operator

- **VLAN Traffic Control Operator** delivers fine-grained, declarative Quality of Service (QoS), traffic policing, and bandwidth shaping for OpenShift and Kubernetes host interfaces.
- Standard CNI bandwidth plugins are typically limited to basic pod-level rate-limiting and cannot regulate non-pod host traffic, secondary Multus interfaces, OVS/Linux bridges, or hardware-stripped 802.1Q VLAN streams.
- This operator bridges the CNI gap by enabling cluster administrators to declaratively manage Linux Traffic Control (`tc`) qdiscs, filters, IFB virtual devices, and classifiers natively across worker nodes using OpenShift Custom Resources (CRs).

**References:**
- Original design and implementation by **Riccardo Bruzzone** (`rbruzzon@redhat.com`).

---

### Core Architecture & Component Workflow

The operator follows a dual-component architecture consisting of a cluster-wide **Controller Manager** and host-bound **Node Agents**:

```text
```text
+----------------------------------------------------------------------------------------+
|                                 OpenShift Cluster                                      |
|                                                                                        |
|  +----------------------------------------------------------------------------------+  |
|  | VlanTrafficControl Custom Resources (Single or Split Ingress/Egress Manifests)   |  |
|  +--------------------------------------+-------------------------------------------+  |
|                                         |                                              |
|                                         v                                              |
|  +----------------------------------------------------------------------------------+  |
|  |                   vlan-tc-operator-controller (Manager)                          |  |
|  |      (Watches VTC CRDs, evaluates nodeSelectors, orchestrates Agents)            |  |
|  +--------------------------------------+-------------------------------------------+  |
|                                         |                                              |
|                                         | Reconciles API state & triggers parallel     |
|                                         | agent REST HTTP endpoints (/reconcile)       |
|                                         v                                              |
|  +----------------------------------------------------------------------------------+  |
|  |                           Worker Node (DaemonSet)                                |  |
|  |                                                                                  |  |
|  |  +-------------------------+   chroot /host   +-------------------------------+  |  |
|  |  |     vlan-tc-agent Pod   | ---------------> | Host Network Namespace        |  |  |
|  |  | (REST API & Reconciler) |                  | (tc, netlink, kernel modules) |  |  |
|  |  +------------+------------+                  +------------+------------------+  |  |
|  |               |                                           |                      |  |
|  |               | Exposes /stats                            | Configures           |  |
|  |               v                                           v                      |  |
|  |  +-------------------------+                  +--------------------------+       |  |
|  |  | Aggregated JSON Metrics |                  | HTB Qdiscs, IFB Redirect,|       |  |
|  |  | (enp1s0 & br-vlan380)   |                  | Flower & act_police      |       |  |
|  |  +-------------------------+                  +--------------------------+       |  |
|  +----------------------------------------------------------------------------------+  |
+----------------------------------------------------------------------------------------+
```

---

## Package Directory Layout

```text
pkg/
├── controller
│   └── vlantrafficcontrol_controller.go  # Cluster-wide CRD reconciler & agent orchestrator
└── executor
    ├── alignment.go     # Parallel strategy alignment verifier & validation state engine
    ├── htb_executor.go  # Stateful HTB class hierarchy builder & egress filter manager
    ├── ifb_executor.go  # IFB virtual netdevice creation & act_mirred ingress redirect engine
    ├── modules.go       # Kernel module verifier (sch_htb, cls_flower, act_mirred, etc.)
    ├── node_filter.go   # Evaluates nodeSelector rules & matches cluster host topology
    ├── stats.go         # Dual-interface telemetry aggregator (physical links & bridges)
    └── tc.go            # Core low-level tc wrapper & chroot host command execution engine
cmd/
├── agent
│   └── main.go          # Node DaemonSet entrypoint, self-healing startup & REST HTTP API server
└── manager
    └── main.go          # Cluster Controller Manager entrypoint & operator bootstrap
```
---

## Component Architecture & Module Reference

### Dynamic Interface Targeting
The operator contains **zero hardcoded network interface names**. Every physical uplink, bridge, or virtual link target is dynamically resolved at runtime from the `spec.htbRoot.interface` field in the applied `VlanTrafficControl` Custom Resource (e.g., `enp1s0`, `eth0`, `bond0`, or secondary bridge devices like `br-vlan380`). When using `tcStrategy: ifb`, virtual IFB netdevices are dynamically created and named on the fly using the format `ifb-<interface>`.

---

### 1. Controller Manager (`cmd/manager` & `pkg/controller`)
The Controller Manager is the orchestrator running at the cluster control-plane level.

* **Scope:** Cluster-wide control plane component.
* **Responsibilities:**
  * **CRD Watching:** Watches `Create`, `Update`, and `Delete` lifecycle events on `VlanTrafficControl` CRs (`networking.med.io/v1alpha1`).
  * **Split-CR Support:** Processes single unified manifests or split CR configurations (e.g., dedicated egress on a physical interface and dedicated ingress on a secondary bridge interface).
  * **Node Targeting (`pkg/executor/node_filter.go`):** Evaluates `nodeSelector` fields against cluster nodes to target policy application.
  * **Parallel Agent Orchestration:** Triggers parallel `/reconcile` and `/cleanup` HTTP invocations across target Node Agents to minimize alignment latency.
  * **Status Aggregation:** Collects health and reconciliation status from node agents and updates the `.status` subresource of the `VlanTrafficControl` CR.

---

### 2. Host Node Agent (`cmd/agent`)
The Node Agent is the execution daemon on every targeted worker host.

* **Scope:** Host-bound DaemonSet pod running with `hostNetwork: true`, privileged SCC, and `/host` filesystem access.
* **Responsibilities:**
  * **Host Traffic Shaping:** Executes `tc` invocations inside the host network namespace via `chroot /host` to construct qdiscs, classes, filters, and IFB devices on dynamically specified interfaces.
  * **Startup Self-Healing:** Runs `reconcileLocalTc` upon startup to restore target host network interfaces (physical links, bridges, or IFB devices) to the desired CR state after node reboots or pod restarts.
  * **REST API Server (Port `8080`):**
    * `GET /stats`: Accepts an `interface` query parameter and returns aggregated byte, packet, drop, and queue metrics for any target interface.
    * `GET /config`: Accepts an `interface` query parameter and reports current interface alignment (`isAligned`) and configuration state.
    * `POST /reconcile`: Triggers an immediate local `tc` rule reconciliation pass.
    * `POST /cleanup` / `DELETE /cleanup`: Selectively purges operator-managed `tc` rules without impacting non-managed host qdiscs.
    * `GET /healthz`: Agent readiness and liveness check probe.

---

### 3. Kernel Module Loader (`pkg/executor/modules.go`)
Ensures the host kernel has necessary Traffic Control kernel modules loaded before applying rules.

* **Scope:** Internal agent package invoked during node agent initialization.
* **Responsibilities:**
  * Verifies module presence and dynamically executes `chroot /host modprobe <module>` if required.
* **Managed Modules:**
  * `sch_htb`: Hierarchical Token Bucket queueing discipline.
  * `ifb`: Intermediate Functional Block virtual network device driver.
  * `act_mirred`: Packet mirroring/redirecting action for IFB routing.
  * `cls_flower`: Multi-field classification engine (`protocol ip`, `protocol 802.1q`).
  * `act_police`: Rate policing and frame dropping/passing action engine.
  * `sch_fq_codel`: Fair Queueing Controlled Delay active queue management.

---

### 4. Stateful HTB Execution Engine (`pkg/executor/htb_executor.go` & `pkg/executor/tc.go`)
Builds and manages egress HTB bandwidth hierarchies on arbitrary target physical or virtual network links.

* **Scope:** Internal package used by `cmd/agent`.
* **Responsibilities:**
  * Configures the root HTB qdisc (`handle 1:`) and parent class (`1:1`) on the interface specified in `spec.htbRoot.interface`.
  * Establishes default fallback classes (e.g., `1:99`) for non-classified host traffic.
  * Instantiates child traffic classes (e.g., `1:100`, `1:380`) specifying `rate`, `ceil`, `burst`, and `priority`.
  * Optionally attaches `fq_codel` AQM leaf qdiscs to child classes.
  * Applies numeric handles (`handle <minor>`) to egress filters for atomic rule replacement.

---

### 5. Ingress Strategy Engine: IFB vs. Flower (`pkg/executor/ifb_executor.go`)
Executes the configured ingress strategy defined by `spec.tcStrategy` on the target interface.

* **Scope:** Internal package used by `cmd/agent`.
* **Responsibilities:**
  * **Stateful IFB Strategy (`tcStrategy: ifb`):**
    * Dynamically creates a virtual device named `ifb-<interface>` corresponding to the target interface specified in the CR.
    * Attaches an `ingress` qdisc (`ffff:`) on the target link with a `matchall` filter using `act_mirred` to redirect all inbound frames to `ifb-<interface>`.
    * Applies full HTB class shaping on the dynamically created `ifb-<interface>` device.
  * **Stateless Flower Strategy (`tcStrategy: flower`):**
    * Instantiates `cls_flower` filters directly on ingress queues (`ffff:`) of the target interface.
    * Applies `act_police` rate limiters with configurable exceed actions (`pass` or `drop`).
    * Supports targeting any secondary bridge interface (e.g., OVS/Linux bridges) to capture demuxed VLAN traffic when NIC hardware offloading (`rx-vlan-offload`) strips 802.1Q headers on physical uplinks.

---

### 6. Dual-Interface Telemetry Engine (`pkg/executor/stats.go`)
Aggregates real-time netlink performance metrics across physical, virtual, and bridge links.

* **Scope:** Internal package backing the `GET /stats` API endpoint.
* **Responsibilities:**
  * Extracts HTB class statistics (`classStats`) on requested physical or virtual (`ifb-*`) devices.
  * Extracts Flower filter statistics (`ingressStats`) on requested physical or bridge interfaces.
  * Isolates demuxed stream counters from untagged physical host noise (`pref-49152`).
  * Returns structured JSON telemetry payloads consumed by monitoring systems and validation pipelines.
  
---

## Component Interaction Summary

```text
[ VlanTrafficControl CR ]
           │
           ▼
[ Controller Manager (cmd/manager) ] ── (Watches CRDs & Calls HTTP API) ──┐
                                                                          │
┌─────────────────────────────────────────────────────────────────────────┘
│
▼
[ Host Node Agent (cmd/agent) ]
  ├── 1. Modules Loader (pkg/executor/modules.go)   ──> Loads sch_htb, ifb, act_mirred, cls_flower
  ├── 2. Strategy Resolver (pkg/executor/ifb_executor.go) ──> Resolves Strategy: flower vs ifb
  ├── 3. TC Engine (pkg/executor/tc.go & htb_executor.go)──> Executes chroot /host tc commands
  └── 4. Telemetry Collector (pkg/executor/stats.go)──> Exposes /stats aggregated JSON metrics
```

---

### Key Capabilities & Traffic Control Features

#### Dual Ingress Strategy Architecture (`ifb` vs. `flower`)
Traffic control on ingress streams supports two complementary operational modes configured via `spec.tcStrategy`:

* **Stateless Flower Policing (`tcStrategy: flower`):** Applies `cls_flower` filters directly on ingress queues (`ffff:`) with `act_police` rate limiters. Features flexible `ingressAction` behavior (`drop` for enforcement or `pass` for non-disruptive monitoring). Supports targeting secondary bridge interfaces (such as `br-vlan380`) to capture demuxed streams when NIC hardware offloading (`rx-vlan-offload`) strips 802.1Q tags on physical uplinks.
* **Stateful IFB Redirection (`tcStrategy: ifb`):** Dynamically provisions an Intermediate Functional Block virtual device (`ifb-<interface>`) and redirects incoming traffic via `act_mirred`. This enables full `sch_htb` class queueing and hierarchical shaping on ingress traffic, mirroring egress functionality.

#### Flexible Multi-Match Classification
Traffic identification goes beyond standard 802.1Q VLAN tags. The operator supports three primary classification backends:

* **802.1Q VLAN Tag (`matchType: vlan`):** Uses `cls_flower` for direct matching on 802.1Q tagged frames (`ethertype 0x8100`) traversing physical uplink or bond interfaces when hardware offloading is disabled.
* **IP Subnet / CIDR (`matchType: subnet`):** Uses `cls_flower` matching on source IP (`src_ip`) for egress and destination IP (`dst_ip`) for ingress. Ideal for OpenShift Virtualization (KubeVirt) or software bridge interfaces (`br-vlan380`) where VLAN tags are demuxed prior to reaching host Layer 3 sockets.
* **Socket Buffer Mark (`matchType: mark`):** Uses `cls_fw` (`handle <mark> fw`) to match on 32-bit `skbmark` values set upstream by Open vSwitch (OVS) flows, `iptables`, or `nftables`.
* **Auto-Detection (`matchType: auto`):** Dynamically inspects class attributes and automatically selects the optimal classifier (`flower` or `fw`).

#### Hierarchy Token Bucket (HTB) & Traffic Shaping
* **Guaranteed Egress Bandwidth (`egressRate`):** Guarantees minimum outbound bandwidth allocation per traffic class under network contention.
* **Burst Ceilings (`egressCeil`):** Limits maximum burst rate capacity when excess root interface bandwidth is available for borrowing.
* **Ingress Rate Policing (`ingressRate` & `ingressBurst`):** Enforces rate caps on incoming traffic using kernel `act_police` filters on the `ingress` (`ffff:`) qdisc or dedicated IFB classes.
* **Priority Queuing (`priority`):** Assigns HTB and filter priority bands (1–7) to ensure latency-sensitive control traffic or storage networks pre-empt bulk data flows.

#### Active Queue Management (AQM)
* **Bufferbloat Prevention (`enableFqCodel`):** Automatically attaches `fq_codel` (Fair Queueing Controlled Delay) leaf qdiscs beneath HTB classes to minimize queue latency and combat bufferbloat under high throughput.

---

### Target Use Cases

* **Live-Migration Traffic Isolation:** Live VM migrations in hyperconverged OpenShift Virtualization clusters can easily saturate host interfaces. By deploying split Custom Resources (shaping egress on physical uplinks while policing demuxed ingress on bridge interfaces like `br-vlan380`), migration streams (`~1GB+`) can be strictly bandwidth-capped and isolated from host background noise without impacting control plane communications.
* **Control Plane Protection:** On shared NICs carrying both OpenShift infrastructure and tenant workloads, assigning strict priority bands (`priority: 1`) protects critical services like **ETCD consensus traffic** and API server communication against starvation.
* **Multi-Tenant Storage Isolation:** Prioritize latency-sensitive Ceph, iSCSI, or NVMe-oF storage traffic over standard application pod egress traffic on shared 10G/25G/100G host NICs.
* **Edge & Far-Edge Deployments:** Manage tight bandwidth constraints on resource-constrained edge nodes communicating over limited backhaul or satellite links by strictly queueing bulk data behind real-time applications.

---

## Custom Resource Definition (CRD) Reference

The `VlanTrafficControl` Custom Resource (`networking.med.io/v1alpha1`) defines the desired traffic shaping state, target nodes, and scheduling tolerations.

### `VlanTrafficControlSpec` (`spec`)

| Field | Type | Required | Default | Description |
| :--- | :--- | :---: | :---: | :--- |
| `nodeSelector` | `map[string]string` | No | `{}` | Map of node labels used to target worker or infrastructure nodes (e.g., `node-role.kubernetes.io/worker: ""`). |
| `nodeLabelSelector` | `Object` | No | `[]` | Kubernetes label selector matching (`matchLabels` & `matchExpressions`). |
| `tolerations` | `[]Toleration` | No | `[]` | Pod/DaemonSet tolerations allowing execution on tainted nodes (e.g., master/control-plane). |
| `reconcileIntervalSeconds` | `integer` | No | `30` | Interval in seconds between node agent reconciliation loops. |
| `tcStrategy` | `string` | **Yes** | `"flower"` | Ingress execution strategy (`flower` for stateless policing or `ifb` for stateful queue shaping). |
| `htbRoot` | `HtbRootSpec` | **Yes** | — | Root HTB and target interface configuration. |

---

### Node Targeting & Taint Tolerations

The operator provides granular control over which nodes in the cluster receive traffic control rules:

* **`nodeSelector` Label Matching:** Restricts policy enforcement strictly to nodes matching specified key-value labels. If left empty (`{}`), all accessible nodes are evaluated.
* **`tolerations` Support:** Allows the host agent DaemonSet to schedule and execute on tainted nodes, such as master nodes (`node-role.kubernetes.io/master:NoSchedule`), control-plane nodes, or dedicated infrastructure hosts. Standard Kubernetes toleration fields (`key`, `operator`, `value`, `effect`, `tolerationSeconds`) are fully supported.

---

### `HtbRootSpec` (`spec.htbRoot`)

| Field | Type | Required | Default | Description |
| :--- | :--- | :---: | :---: | :--- |
| `interface` | `string` | **Yes** | — | Target physical link (`enp1s0`), bond (`bond0`), or bridge interface (`br-vlan380`). Dynamically resolved at runtime without hardcoding. |
| `rate` | `string` | **Yes** | — | Total root egress bandwidth capacity for the target interface (e.g., `10Gbit`). |
| `defaultClassId` | `string` | No | `"1:99"` | Default HTB minor class ID where unclassified egress traffic is routed. |
| `htbId` | `integer` | No | `1` | Major handle ID for the root HTB qdisc (defines the `major:` prefix, e.g., `1:`). |
| `classes` | `[]VlanClassSpec` | **Yes** | — | List of individual traffic class definitions configured under this root. |

#### Guidance on Interface Target Selection:

* **Bond Master Interface (`bond0`):** Use for bonded physical NICs. Applying HTB qdiscs to `bond0` allows unified bandwidth arbitration across all VLANs traversing the bond.
* **Physical Interface (`enp1s0` / `eth0`):** Use for non-bonded single-NIC setups to apply global egress shaping across physical uplinks.
* **Software Bridge Interface (`br-vlan380`):** Recommended for Flower ingress rules when NIC hardware offloading (`rx-vlan-offload`) strips 802.1Q tags on physical NICs. Attaching to the bridge targets demuxed IPv4 streams cleanly.

---

### `VlanClassSpec` (`spec.htbRoot.classes[]`)

| Field | Type | Required | Default | Description |
| :--- | :--- | :---: | :---: | :--- |
| `name` | `string` | **Yes** | — | Human-readable identifier for the class configuration. |
| `matchType` | `string` | No | `"auto"` | Classification strategy (`vlan`, `subnet`, `mark`, `auto`). |
| `classId` | `string` | **Yes** | — | Unique HTB class identifier on the interface (e.g., `1:380`). Format: `^1:[0-9]+$`. |
| `vlanId` | `integer` | Conditional | — | 802.1Q VLAN tag ID (1–4094). **Required** if `matchType` is `vlan`. |
| `subnet` | `string` | Conditional | — | IPv4 CIDR subnet (e.g., `10.200.0.0/24`). **Required** if `matchType` is `subnet`. |
| `mark` | `uint32` | Conditional | — | 32-bit SKB mark set by OVS or iptables (e.g., `16`). **Required** if `matchType` is `mark`. |
| `egressRate` | `string` | **Yes** | — | Guaranteed outbound bandwidth rate (e.g., `50Mbit`, `1Gbit`). |
| `egressCeil` | `string` | No | `egressRate` | Maximum allowed outbound burst bandwidth ceiling (e.g., `200Mbit`, `10Gbit`). |
| `egressBurst` | `string` | No | `"1250b"` | Outbound burst buffer size (e.g., `15k`, `30k`). |
| `ingressRate` | `string` | No | `""` | Rate limit for incoming traffic on this class (e.g., `30Mbit`, `256Mb`). |
| `ingressBurst` | `string` | No | `"100k"` | Incoming policing burst buffer size (e.g., `15k`, `256Mb`). |
| `ingressAction` | `string` | No | `"drop"` | Policing action when traffic exceeds thresholds: **`drop`** (hard rate limit) or **`pass`** (monitor mode / telemetry collection). |
| `priority` | `integer` | No | `0` | HTB priority and TC filter priority level (1 = Highest Priority, 7 = Lowest Priority). |
| `enableFqCodel` | `boolean` | No | `true` | Toggles attaching an `fq_codel` leaf qdisc to combat bufferbloat under heavy load. |

---

### How `matchType: auto` Works

When `matchType: auto` is specified (or if `matchType` is omitted), the operator automatically infers the correct classifier module (`flower` vs `fw`) by evaluating configured parameters top-down:

1. **802.1Q Tag (`vlanId > 0`):** Configures a `cls_flower` **L2 802.1Q filter** (`protocol 802.1q flower vlan_id <vlanId>`).
2. **IP Subnet (`subnet != ""`):** Configures a `cls_flower` **L3 IP filter** (`protocol ip flower src_ip/dst_ip <subnet>`).
3. **SKB Mark (`mark > 0`):** Configures a `cls_fw` **Firewall Mark filter** (`protocol all handle <mark> fw`).

> **Note:** If `matchType: auto` is set but none of `vlanId`, `subnet`, or `mark` are supplied, the node agent logs a validation warning and skips filter creation for that class without interrupting overall reconciliation.

---

## Full Manifest Example

### VLAN Traffic Control CR with Flower tcStrategy - VLAN 380 defined as Live Migration Network in Red Hat OpenShift Virtualization  

```yaml
# Egress Custom Resource: Physical Link Egress Bandwidth Shaping
apiVersion: networking.med.io/v1alpha1
kind: VlanTrafficControl
metadata:
  name: flower-vlan-tc-egress
  namespace: openshift-vlan-tc-operator
spec:
  tcStrategy: flower           # Stateless Flower classifier strategy; uses cls_flower filters on ingress/egress queues with act_police rate limiters
  reconcileIntervalSeconds: 60 # Node agent reconciliation interval to ensure host tc configuration alignment
  nodeSelector:
    node-role.kubernetes.io/worker: "" # Targets all standard OpenShift worker nodes in the cluster
  htbRoot:
    interface: enp1s0          # Physical network interface attached to the host uplink
    rate: 10Gbit               # Maximum physical line speed available to the root HTB qdisc
    htbId: 1                   # Major handle ID (1:) for the root HTB queuing discipline
    defaultClassId: "1:99"     # Directs all unclassified egress traffic into class 1:99
    defaultClassMinor: 99
    classes:
      # Default Control-Plane Fallback (etcd, API, Kubelet, Node Management)
      - name: default-fallback
        classId: "1:99"
        priority: 0            # Absolute highest priority (prio 0); HTB processes these queues before any tenant or migration queues
        egressRate: 10Gbit     # Guaranteed egress rate set to full wire speed; prevents host management traffic from ever bottlenecking
        enableFqCodel: false   # Disables fq_codel leaf qdisc to ensure simple FIFO queuing for fallback streams

      # High Priority Tenant Traffic (VLAN 100)
      - name: vlan-100-high-priority
        classId: "1:100"
        vlanId: 100
        matchType: subnet       # L3 classification backend; matches destination IP subnet using cls_flower
        subnet: 10.0.100.0/24   # Target IP range for high-priority tenant workloads
        priority: 1            # Priority 1 evaluation; served after control plane (prio 0) but before live migration (prio 3)
        egressRate: 2Gbit      # Guaranteed egress bandwidth floor of 2Gbps for outgoing tenant traffic
        egressBurst: 16Mb      # Maximum burst buffer size allowed for high-priority outbound spikes
        enableFqCodel: false

      # KubeVirt Live Migration (VLAN 380)
      - name: vlan-380-migration
        classId: "1:380"
        vlanId: 380
        matchType: vlan         # L2 classification backend; matches 802.1Q VLAN tag 380 directly using cls_flower
        priority: 3            # Lowest priority (prio 3); yields bandwidth to control plane (prio 0) and tenant traffic (prio 1)
        egressRate: 10Gbit     # Full wire-speed egress ceiling; allows migration payload to burst up to 10Gbps when link is idle
        egressBurst: 256Mb     # Large burst buffer to accommodate bursty live migration memory state sync
        enableFqCodel: false
---
# Ingress Custom Resource: Bridge Link Ingress Traffic Isolation & Policing
apiVersion: networking.med.io/v1alpha1
kind: VlanTrafficControl
metadata:
  name: flower-vlan-tc-ingress
  namespace: openshift-vlan-tc-operator
spec:
  tcStrategy: flower           # Stateless Flower classifier strategy for ingress rate policing
  reconcileIntervalSeconds: 60 # Node agent reconciliation interval
  nodeSelector:
    node-role.kubernetes.io/worker: "" # Targets all standard OpenShift worker nodes
  htbRoot:
    interface: br-vlan380      # Targets software bridge link to inspect demuxed VLAN 380 traffic after HW offload (rx-vlan-offload) strips 802.1Q tags on enp1s0
    rate: 10Gbit               # Maximum line rate capacity for the bridge interface
    htbId: 1                   # Major handle ID (1:) for the bridge HTB qdisc
    defaultClassId: "1:99"     # Directs unclassified bridge traffic into class 1:99
    defaultClassMinor: 99
    classes:
      # KubeVirt Live Migration Ingress Policing (VLAN 380)
      - name: vlan-380-migration
        classId: "1:380"
        matchType: subnet       # L3 classification backend; matches demuxed IPv4 destination subnet on the software bridge
        subnet: 10.0.238.0/24   # Migration subnet range delivering memory stream payloads
        priority: 3            # Ingress filter evaluation priority (pref 3)
        ingressRate: 10Gbit    # Maximum incoming bandwidth rate limit enforced via act_police
        ingressBurst: 256Mb    # Generous burst window allowing high-speed migration transfers without premature packet drops
        ingressAction: pass    # Monitor mode (conform-exceed pass); records telemetry byte counters (pref-3) on act_police without dropping migration frames
        enableFqCodel: false
```

### VLAN Traffic Control CR with IFB tcStrategy - VLAN 380 defined as Live Migration Network in Red Hat OpenShift Virtualization  

```yaml
apiVersion: networking.med.io/v1alpha1
kind: VlanTrafficControl
metadata:
  name: ifb-vlan-tc-combined
  namespace: openshift-vlan-tc-operator
spec:
  nodeSelector:
    node-role.kubernetes.io/worker: "" # Targets all standard OpenShift worker nodes in the cluster
  tcStrategy: ifb               # Stateful Intermediate Functional Block strategy; uses act_mirred redirection to dynamically construct stateful HTB class trees on ingress for rate ceilings and bandwidth borrowing
  htbRoot:
    interface: enp1s0           # Target physical host uplink interface (dynamically creates virtual netdevice 'ifb-enp1s0')
    rate: 10Gbit                # Maximum physical wire speed capacity available to the root HTB qdisc (handle 1:)
    defaultClassId: "1:99"      # Minor class ID handle where unclassified fallback traffic is routed
    defaultClassMinor: 99
    classes:
      # Default Control-Plane Fallback (etcd, API Server, Kubelet, Node Management)
      - name: default-fallback
        classId: "1:99"
        priority: 0             # Absolute highest evaluation priority (prio 0); HTB processes these queues before any tenant or live migration queues
        egressRate: 10Gbit      # Guaranteed egress bandwidth floor set to line rate; prevents host management traffic from ever bottlenecking
        egressCeil: 10Gbit      # Outbound burst ceiling set to 10Gbps full wire speed
        ingressRate: 10Gbit     # Guaranteed ingress bandwidth floor on IFB device set to line rate; ensures etcd/API ingress traffic is never artificially throttled
        ingressCeil: 10Gbit     # Stateful IFB ingress burst ceiling set to 10Gbps full wire speed
        enableFqCodel: true     # Enables Fair Queueing Controlled Delay AQM; isolates small etcd heartbeat flows and guarantees sub-millisecond latency [1]

      # High Priority Workloads (VLAN 100)
      - name: vlan-100-high-priority
        classId: "1:100"
        vlanId: 100
        matchType: subnet        # L3 classification backend; matches destination IP subnet using cls_flower
        subnet: 10.0.100.0/24    # Target IPv4 CIDR range for high-priority tenant workloads
        priority: 1             # Priority 1 evaluation; served after control plane (prio 0) but before live migration streams (prio 3)
        egressRate: 2Gbit       # Guaranteed egress bandwidth floor of 2Gbps for outgoing tenant traffic
        egressCeil: 10Gbit       # Outbound burst ceiling; allows VLAN 100 to borrow up to 10Gbps line rate if higher priority classes are idle
        ingressRate: 2Gbit      # Guaranteed ingress bandwidth floor of 2Gbps on virtual device 'ifb-enp1s0'
        ingressCeil: 10Gbit      # Stateful IFB ingress ceiling; enables dynamic bandwidth borrowing up to 10Gbps during inbound traffic spikes
        enableFqCodel: true     # Prevents bufferbloat and maintains low queuing latency across active tenant TCP flows [1]

      # KubeVirt Live Migration (VLAN 380) - Dynamic Bi-Directional Bandwidth Borrowing
      - name: vlan-380-migration
        classId: "1:380"
        vlanId: 380
        matchType: vlan          # L2 classification backend; matches 802.1Q VLAN tag 380 directly using cls_flower
        priority: 3             # Lowest evaluation priority (prio 3); automatically yields excess bandwidth to etcd (prio 0) and high-priority workloads (prio 1)
        egressRate: 500Mbit     # Minimum guaranteed egress bandwidth floor of 500Mbps for live migration streams
        egressCeil: 10Gbit      # Outbound burst ceiling; allows migration payload to borrow up to 10Gbps full wire speed when the uplink is unutilized
        ingressRate: 500Mbit    # Minimum guaranteed ingress bandwidth floor of 500Mbps on IFB device
        ingressCeil: 10Gbit     # Stateful IFB ingress ceiling; queue buffers allow inbound migration payload to borrow up to 10Gbps without dropping TCP packets
        enableFqCodel: true     # Active Queue Management keeps migration TCP streams smooth and prevents packet drop-retransmit cycles under load [1]

# [1] FQ_CoDel (Fair Queueing Controlled Delay) active queue management combines two complementary techniques:
#     - Fair Queueing (FQ): Distributes traffic into dynamic flow buckets to prevent bulk transfers from starving latency-sensitive control packets.
#     - Controlled Delay (CoDel): Monitors buffer dwell time and drops packets if latency exceeds 5 ms, triggering TCP congestion control before queues bloat.
```

---
## Traffic Control & Telemetry Execution Matrices

The following matrices describe packet path mechanics, classification filter matching, and netlink telemetry counter sources across both ingress policing and egress bandwidth shaping modes.

### 1. Ingress Traffic Matrix (Ingress / `ffff:`)

| Strategy | Attachment Location | Monitored Ingress Interface | Hardware Offload (`rx-vlan-offload`) | Packet Header at TC Ingress | Rule / Filter Matching Behavior | Telemetry Counter Source |
| :--- | :--- | :--- | :--- | :--- | :--- | :--- |
| **Flower** | **Physical Link** (`enp1s0`) | `enp1s0` | **Enabled** (Default) | Untagged `ETH_P_IP` (`0x0800`) | Falls through to default `protocol ip` / `pref 49152` catch-all | Netlink Action Stats (`act_police` under `pref 49152`) |
| **Flower** | **Physical Link** (`enp1s0`) | `enp1s0` | **Disabled** | Tagged `ETH_P_8021Q` (`0x8100`) | `protocol 802.1q vlan_id 380` | Netlink Action Stats (`act_police` under `pref 1`/`pref 3`) |
| **Flower** | **Bridge Link** (`br-vlan380`) | `br-vlan380` | **N/A** (Software Bridge) | Demuxed per-VLAN stream | `protocol ip` + `dst_ip` subnet match | Netlink Action Stats (`act_police` on `br-vlan380`) |
| **IFB** | **Physical Link** (`enp1s0`) $\rightarrow$ `ifb-enp1s0` | `ifb-enp1s0` | **Enabled / Disabled** | Redirected via `act_mirred` | `matchall` filter redirects all ingress frames to `ifb-enp1s0` | Netlink Class Stats (`sch_htb` classes `1:380` on `ifb-enp1s0`) |

---

### 2. Egress Traffic Matrix (Egress / `root x:`)

> **Note:** `x` represents the root `htbId` (Hierarchical Token Bucket Identifier, major handle handle `x:`).

| Strategy | Attachment Location | Monitored Egress Interface | Hardware Offload (`tx-vlan-offload`) | Packet Header at TC Egress | Rule / Filter Matching Behavior | Telemetry Counter Source |
| :--- | :--- | :--- | :--- | :--- | :--- | :--- |
| **Flower** | **Physical Link** (`enp1s0`) | `enp1s0` | **Enabled** (Default) | Untagged `ETH_P_IP` (Tag inserted by HW later) | HTB filter matching mark/skb priority or Class ID (`1:380`) | Netlink Class Stats (`sch_htb` class `1:380` on `enp1s0`) |
| **Flower** | **Physical Link** (`enp1s0`) | `enp1s0` | **Disabled** | Tagged `ETH_P_8021Q` (`0x8100`) | `protocol 802.1q vlan_id 380` mapped to Class ID (`1:380`) | Netlink Class Stats (`sch_htb` class `1:380` on `enp1s0`) |
| **IFB** | **Physical Link** (`enp1s0`) | `enp1s0` | **Enabled / Disabled** | Native Host Egress Stream | Direct HTB class hierarchy (`1:1` root parent, `1:380` leaf child) | Netlink Class Stats (`sch_htb` class `1:380` on `enp1s0`) |


## VLAN Traffic Control Strategy and Architecture

### IFB Architecture (Stateful HTB Queueing & Multi-Class Flow Logic)

The Intermediate Functional Block (IFB) architecture provides stateful, bi-directional bandwidth management across host network interfaces. Standard Linux Traffic Control (`tc`) only supports classful queuing disciplines (`sch_htb`) on egress interfaces. To apply stateful class hierarchies, dynamic bandwidth borrowing, and buffer queueing to inbound traffic, the operator redirects physical ingress streams into a virtual netdevice (`ifb-<interface>`).

---

#### Ingress Packet Processing Pipeline

The decision tree below details how inbound frames arriving on a physical port are redirected to the virtual IFB device, evaluated against priority-ordered `cls_flower` classifiers, and scheduled through HTB class queues before entering the host/pod network stack:

```text
┌─────────────────────────────────────────────────────────────────────────────────────────────────────────────────┐
│                                                                                                                 │
│   Inbound Packet Arrives at Physical Port                                                                       │
│             │                                                                                                   │
│             ▼                                                                                                   │
│   ┌───────────────────────────────────────────────┐                                                             │
│   │ Action: mirred redirect dev ifb-enp1s0        │                                                             │
│   └──────────────────────┬────────────────────────┘                                                             │
└──────────────────────────┼──────────────────────────────────────────────────────────────────────────────────────┘
                           │
                           ▼ Redirected into Intermediate Functional Block Device (ifb-enp1s0)
┌─────────────────────────────────────────────────────────────────────────────────────────────────────────────────┐
│ IFB Device Processing (ifb-enp1s0)                                                                              │
│                                                                                                                 │
│   ┌───────────────────────────────────────────────┐                                                             │
│   │ Priority 1 Filter: Match VLAN 100 / Subnet?   │                                                             │
│   └──────────────────────┬────────────────────────┘                                                             │
│                          │                                                                                      │
│             ┌────────────┴────────────┐                                                                         │
│          YES│                       NO│                                                                         │
│             ▼                         ▼                                                                         │
│   ┌───────────────────┐   ┌───────────────────────────────────────────────┐                                     │
│   │ Class 1:100       │   │ Priority 3 Filter: Match VLAN 380?            │                                     │
│   │ (prio 1)          │   └───────────────────────┬───────────────────────┘                                     │
│   └────────┬──────────┘                           │                                                             │
│            │                          ┌───────────┴───────────┐                                                 │
│            │                       YES│                     NO│                                                 │
│            │                          ▼                       ▼                                                 │
│            │               ┌───────────────────┐   ┌───────────────────────────────────────────────┐            │
│            │               │ Class 1:380       │   │ Priority 49152 Filter: Match All (Fallback)?  │            │
│            │               │ (prio 3)          │   └───────────────────────┬───────────────────────┘            │
│            │               └────────┬──────────┘                           │                                    │
│            │                        │                          ┌───────────┴───────────┐                        │
│            │                        │                       YES│                     NO│                        │
│            │                        │                          ▼                       ▼                        │
│            │                        │               ┌───────────────────┐   ┌─────────────────────────┐ │
│            │                        │               │ Class 1:99        │   │ Drop / Unmatched Kernel │ │
│            │                        │               │ (prio 0 / etcd)   │   │ Exception               │ │
│            │                        │               └─────────┬─────────┘   └─────────────────────────┘ │
│            │                        │                         │                                         │
│            ▼                        ▼                         ▼                                         │
│   ┌───────────────────────────────────────────────────────────────────────────────────────────────────────────┐ │
│   │ Hierarchical Token Bucket (HTB) Engine & Active Queue Management                                          │ │
│   │                                                                                                           │ │
│   │   • Class 1:99  (prio 0): Min 10G, Ceil 10G + FQ_CoDel ──► First served, sub-ms latency (etcd/control)    │ │
│   │   • Class 1:100 (prio 1): Min 2G,  Ceil 10G + FQ_CoDel ──► Guaranteed 2G, borrows idle bandwidth          │ │
│   │   • Class 1:380 (prio 3): Min 500M, Ceil 10G + FQ_CoDel ──► Paced buffer queueing (prevents TCP drops)    │ │
│   └──────────────────────────────────────────────────┬────────────────────────────────────────────────────────┘ │
│                                                      │                                                          │
│                                                      ▼                                                          │
│                                             To Host / Pod / VM Stack                                            │
└─────────────────────────────────────────────────────────────────────────────────────────────────────────────────┘
```
---

#### Egress Packet Processing Pipeline

The following diagram details how outbound frames originated by local host services, Kubernetes pods, or virtual machines are classified, shaped through the Hierarchical Token Bucket (HTB) class tree, and scheduled onto the physical network wire (`enp1s0`):

```text
┌─────────────────────────────────────────────────────────────────────────────────────────────────────────────────┐
│                                                                                                                 │
│   Outbound Packet Generated by Host / Pod / VM Stack                                                            │
│             │                                                                                                   │
│             ▼                                                                                                   │
│   ┌───────────────────────────────────────────────┐                                                             │
│   │ Enters Physical / Uplink Interface (enp1s0)   │                                                             │
│   └──────────────────────┬────────────────────────┘                                                             │
└──────────────────────────┼──────────────────────────────────────────────────────────────────────────────────────┘
                           │
                           ▼ Root HTB Qdisc Classifier Evaluation (handle 1:)
┌─────────────────────────────────────────────────────────────────────────────────────────────────────────────────┐
│ Class Filter Matching (Priority Cascade)                                                                        │
│                                                                                                                 │
│   ┌───────────────────────────────────────────────┐                                                             │
│   │ Priority 1 Filter: Match VLAN 100 / Subnet?   │                                                             │
│   └──────────────────────┬────────────────────────┘                                                             │
│                          │                                                                                      │
│             ┌────────────┴────────────┐                                                                         │
│          YES│                       NO│                                                                         │
│             ▼                         ▼                                                                         │
│   ┌───────────────────┐   ┌───────────────────────────────────────────────┐                                     │
│   │ Class 1:100       │   │ Priority 3 Filter: Match VLAN 380 Tag / Subnet?│                                    │
│   │ (prio 1)          │   └───────────────────────┬───────────────────────┘                                     │
│   └────────┬──────────┘                           │                                                             │
│            │                          ┌───────────┴───────────┐                                                 │
│            │                       YES│                     NO│                                                 │
│            │                          ▼                       ▼                                                 │
│            │               ┌───────────────────┐   ┌───────────────────────────────────────────────┐            │
│            │               │ Class 1:380       │   │ Priority 49152 Filter: Default Catch-All?     │            │
│            │               │ (prio 3)          │   └───────────────────────┬───────────────────────┘            │
│            │               └────────┬──────────┘                           │                                    │
│            │                        │                          ┌───────────┴───────────┐                        │
│            │                        │                       YES│                     NO│                        │
│            │                        │                          ▼                       ▼                        │
│            │                        │               ┌───────────────────┐   ┌─────────────────────────┐ │
│            │                        │               │ Class 1:99        │   │ Direct Fallback Queue   │ │
│            │                        │               │ (prio 0 / etcd)   │   │ (unclassified)          │ │
│            │                        │               └─────────┬─────────┘   └─────────────────────────┘ │
│            │                        │                         │                                         │
│            ▼                        ▼                         ▼                                         │
│   ┌───────────────────────────────────────────────────────────────────────────────────────────────────────────┐ │
│   │ Egress Hierarchical Token Bucket (HTB) Engine & Active Queue Management                                  │ │
│   │                                                                                                           │ │
│   │   • Class 1:99  (prio 0): Guaranteed Rate 10G, Ceil 10G ──► Pre-empts all traffic (sub-ms etcd/control)   │ │
│   │   • Class 1:100 (prio 1): Guaranteed Rate 2G,  Ceil 10G ──► Tenant bandwidth floor, borrows idle capacity  │ │
│   │   • Class 1:380 (prio 3): Guaranteed Rate 500M, Ceil 10G ──► Migration queue (yields under network load)   │ │
│   └──────────────────────────────────────────────────┬────────────────────────────────────────────────────────┘ │
│                                                      │                                                          │
│                                                      ▼                                                          │
│                                           Transmitted on Physical Wire                                          │
└─────────────────────────────────────────────────────────────────────────────────────────────────────────────────┘
```

---

### Live Migration Packet Path under `tcStrategy: ifb`

During live migration events (e.g., KubeVirt VM transitions), traffic traverses both egress shaping and IFB ingress redirection loops across the source and destination worker nodes:

```text
    FROM SOURCE NODE: hub-worker01                                         TO  DESTINATION NODE: hub-worker02
 ┌────────────────────────────────────┐                                ┌────────────────────────────────────┐
 │  Pod / VM (vm-vlan100 / migration) │                                │  Pod / VM (Target Instance)        │
 └─────────────────┬──────────────────┘                                └─────────────────▲──────────────────┘
                   │                                                                     │
                   │ (Outbound Migration Stream)                                         │ (Inbound Post-Shaped)
                   ▼                                                                     │
 ┌────────────────────────────────────┐                                ┌─────────────────┴──────────────────┐
 │ VIRTUAL DEVICE: ifb-enp1s0         │                                │ VIRTUAL DEVICE: ifb-enp1s0         │
 │                                    │                                │                                    │
 │ [Point 3] Flower Classifier        │                                │ [Point 3] Flower Classifier        │
 │  └─► pref 3: match vlan 380        │                                │  └─► pref 3: match vlan 380        │
 │                                    │                                │                                    │
 │ [Point 4] Ingress HTB Classes      │                                │ [Point 4] Ingress HTB Classes      │
 │  └─► Class 1:380 (500Mbit)         │                                │  └─► Class 1:380 (500Mbit)         │
 └─────────────────▲──────────────────┘                                └─────────────────▲──────────────────┘
                   │                                                                     │
                   │ (Redirect Ingress)                                                  │ (Redirect Ingress)
 ┌─────────────────┴──────────────────┐                                ┌─────────────────┴──────────────────┐
 │ PHYSICAL DEVICE: enp1s0            │                                │ PHYSICAL DEVICE: enp1s0            │
 │                                    │                                │                                    │
 │ [Point 1] Ingress Redirection      │                                │ [Point 1] Ingress Redirection      │
 │  └─► matchall -> redirect ifb-enp1s0                                │  └─► matchall -> redirect ifb-enp1s0
 │                                    │                                │                                    │
 │ [Point 2] Egress HTB Classes       │                                │ [Point 2] Egress HTB Classes       │
 │  └─► Class 1:380 (500Mbit Limit)   │                                │  └─► Class 1:380 (500Mbit Limit)   │
 └─────────────────┬──────────────────┘                                └─────────────────▲──────────────────┘
                   │                                                                     │
                   │                                                                     │
                   └───────────────────────────────► LIVE MIGRATION ─────────────────────┘
				                           (Physical Network / VLAN 380 Wire)
```

#### Bi-Directional Flow Mechanics & Reverse Path Shaping

* **Forward Stream (Source to Destination):**
  1. **Source Egress Shaping:** Outbound VM memory payloads are classified on `hub-worker01` via `enp1s0` class `1:380` [Point 2], constraining outbound bandwidth according to `egressRate` and `egressCeil`.
  2. **Destination Ingress Shaping:** Arriving frames on `hub-worker02` are caught by the `matchall` rule on `enp1s0` [Point 1] and redirected to `ifb-enp1s0`. They are matched by `cls_flower` [Point 3] and scheduled through IFB class `1:380` [Point 4] before entering the target VM instance.

* **Return Control Flow (Destination to Source):**
  1. **Destination Egress Shaping:** TCP ACKs, window updates, and synchronization handshakes generated by the destination node (`hub-worker02`) exit back through its physical uplink `enp1s0` [Point 2]. These return packets are evaluated under the destination's local egress HTB tree (`class 1:380`).
  2. **Source Ingress Shaping:** As return ACKs arrive at `hub-worker01`, they enter the physical port `enp1s0` [Point 1], get redirected via `act_mirred` to `ifb-enp1s0`, and pass through the source node's ingress HTB queue hierarchy [Point 4].
  3. **ACK Pre-emption via `fq_codel`:** Because return flow TCP ACKs are small packets, enabling `enableFqCodel: true` ensures that returning ACKs bypass queued bulk migration blocks without delay, preventing artificial sender window throttling and maintaining maximal TCP throughput across the migration pipeline.

---

### Flower Architecture (Stateless Policing - Priority Logic Flow)

The Flower Architecture provides high-performance, stateless traffic control directly on network ingress and egress queues. Unlike IFB, which redirects frames into virtual devices for stateful class queueing, `tcStrategy: flower` instantiates lightweight `cls_flower` filters directly on interface qdiscs. On ingress queues (`ffff:`), it pairs classification rules with `act_police` token buckets to enforce bandwidth caps or monitor streams at line rate with minimal CPU overhead.

---

#### Ingress Packet Processing Pipeline

Inbound frames arriving on an interface configured with `tcStrategy: flower` (e.g., physical uplink `enp1s0` or bridge link `br-vlan380`) are evaluated top-down against priority-ordered `cls_flower` filters. Packets matching a filter are checked against an `act_police` rate limiter: compliant packets pass through, while exceeding packets are dropped or allowed based on `ingressAction`:

```text
┌───────────────────────────────────────────────────────────────────────────────────┐
│                                                                                   │
│   Inbound Packet Arrives                                                          │
│             │                                                                     │
│             ▼                                                                     │
│   ┌───────────────────────────────────────────────┐                               │
│   │ Priority 1 Filter: Match VLAN 100 / Subnet?   │                               │
│   └──────────────────────┬────────────────────────┘                               │
│                          │                                                        │
│             ┌────────────┴────────────┐                                           │
│          YES│                       NO│                                           │
│             ▼                         ▼                                           │
│   ┌───────────────────┐   ┌───────────────────────────────────────────────┐       │
│   │ Police Rate = 2G  │   │ Priority 3 Filter: Match VLAN 380?            │       │
│   └────────┬──────────┘   └───────────────────────┬───────────────────────┘       │
│            │                                      │                               │
│       ┌────┴─────┐                   ┌────────────┴────────────┐                  │
│    OK │       NO │                YES│                       NO│                  │
│       ▼          ▼                   ▼                         ▼                  │
│   Forward     HARD DROP   ┌───────────────────┐   ┌──────────────────────┐        │
│   to VM       (Drop In    │ Police Rate = 500M│   │ Priority 49152 Filter│        │
│  (VLAN 100)   Silicon)    └────────┬──────────┘   │ (Default Fallback)   │        │
│                                    │              └───────────┬──────────┘        │
│                               ┌────┴─────┐                    │                   │
│                            OK │       NO │                    ▼                   │
│                               ▼          ▼                 Forward to             │
│                            Forward    HARD DROP            Host Stack [1]         │
│                            to VM      (Drop In                                    │
│                           (VLAN 380)  Silicon)                                    │
│                                                                                   │
│  [1] If the packet has an invalid destination, closed port, or fails firewall     │
│      rules (Netfilter DROP), it is silently dropped by standard OS networking     │
│      rules (incrementing system ipSystemStatsInDiscards or firewall drop          │
│      counters).                                                                   │
└───────────────────────────────────────────────────────────────────────────────────┘ 
```

---

#### Egress Packet Processing Pipeline

On the egress path, `tcStrategy: flower` uses standard HTB class hierarchies for outbound shaping while leveraging `cls_flower` filters attached to the root qdisc (`1:`) to classify outgoing frames into child HTB classes (`1:100`, `1:380`, etc.):

```text
┌─────────────────────────────────────────────────────────────────────────────────────────────────────────────────┐
│                                                                                                                 │
│   Outbound Packet Generated by Host / Pod / VM Stack                                                            │
│             │                                                                                                   │
│             ▼                                                                                                   │
│   ┌───────────────────────────────────────────────┐                                                             │
│   │ Enters Physical Interface Egress Queue        │                                                             │
│   └──────────────────────┬────────────────────────┘                                                             │
└──────────────────────────┼──────────────────────────────────────────────────────────────────────────────────────┘
                           │
                           ▼ Root HTB Qdisc Classifier Evaluation (handle 1:)
┌─────────────────────────────────────────────────────────────────────────────────────────────────────────────────┐
│ Priority-Ordered cls_flower Filter Matching                                                                     │
│                                                                                                                 │
│   ┌───────────────────────────────────────────────┐                                                             │
│   │ Priority 1 Filter: Match VLAN 100 / Subnet?   │                                                             │
│   └──────────────────────┬────────────────────────┘                                                             │
│                          │                                                                                      │
│             ┌────────────┴────────────┐                                                                         │
│          YES│                       NO│                                                                         │
│             ▼                         ▼                                                                         │
│   ┌───────────────────┐   ┌───────────────────────────────────────────────┐                                     │
│   │ Directs to        │   │ Priority 3 Filter: Match VLAN 380 / Subnet?   │                                     │
│   │ HTB Class 1:100   │   └───────────────────────┬───────────────────────┘                                     │
│   └────────┬──────────┘                           │                                                             │
│            │                          ┌───────────┴───────────┐                                                 │
│            │                       YES│                     NO│                                                 │
│            │                          ▼                       ▼                                                 │
│            │               ┌───────────────────┐   ┌───────────────────────────────────────────────┐            │
│            │               │ Directs to        │   │ Priority 49152 Filter: Default Catch-All?     │            │
│            │               │ HTB Class 1:380   │   └───────────────────────┬───────────────────────┘            │
│            │               └────────┬──────────┘                           │                                    │
│            │                        │                          ┌───────────┴───────────┐                        │
│            │                        │                       YES│                     NO│                        │
│            │                        │                          ▼                       ▼                        │
│            │                        │               ┌───────────────────┐   ┌─────────────────────────┐ │
│            │                        │               │ Directs to        │   │ Fallback Class 1:99     │ │
│            │                        │               │ Fallback 1:99     │   │ (Unclassified Egress)   │ │
│            │                        │               └─────────┬─────────┘   └─────────────────────────┘ │
│            │                        │                         │                                         │
│            ▼                        ▼                         ▼                                         │
│   ┌───────────────────────────────────────────────────────────────────────────────────────────────────────────┐ │
│   │ HTB Egress Queue Execution & Transmission                                                                 │ │
│   │                                                                                                           │ │
│   │   • Class 1:99  (prio 0): Min 10G, Ceil 10G ──► Highest priority transmission (etcd / control plane)      │ │
│   │   • Class 1:100 (prio 1): Min 2G,  Ceil 10G ──► Tenant egress floor, borrows unallocated link bandwidth   │ │
│   │   • Class 1:380 (prio 3): Min 500M, Ceil 10G ──► Migration egress floor, bursts up to wire rate           │ │
│   └──────────────────────────────────────────────────┬────────────────────────────────────────────────────────┘ │
│                                                      │                                                          │
│                                                      ▼                                                          │
│                                           Transmitted on Physical Wire                                          │
└─────────────────────────────────────────────────────────────────────────────────────────────────────────────────┘
```

---

#### Token Bucket Mechanism

Stateless ingress rate limiting under Flower relies on kernel `act_police` token buckets. Refill tokens accumulate at the configured `ingressRate` up to the maximum `ingressBurst` capacity. Incoming packet bursts consume tokens equal to their byte length:

```text
 ┌───────────────────────────────────────────────────────────────────────────┐
 │                        TOKEN BUCKET MECHANISM                             │
 │                                                                           │
 │   Tokens Refill at Rate (e.g., 500Mbit)                                   │
 │                │                                                          │
 │                ▼                                                          │
 │   ┌───────────────────────┐                                               │
 │   │ Bucket Capacity       │ ◄─── BURST PARAMETER                          │
 │   │ (e.g., 64MB Buffer)   │      Allows transient line-rate (10G) bursts   │
 │   └───────────┬───────────┘      without packet loss.                     │
 │                │                                                          │
 │   Packets      │ Consume Tokens                                           │
 │   Pass ────────┼────────────────────────► Forwarded to VM / Migration      │
 │                │                                                          │
 │                │ Tokens Exhausted                                         │
 │                └────────────────────────► HARD DROP (Police Drop)         │
 |                                                                           |
 |---------------------------------------------------------------------------|
 |                                                                           |
 | Burst Size ≥ Wire Speed (10Gbit) x Round Trip Time (RTT)                  |
 |                                                                           |
 | For standard data center networks (RTT ~ 0.5–1 ms), set burst to at       |
 | least 8MB to 64MB for bulk migration classes.                             |
 └───────────────────────────────────────────────────────────────────────────┘ 
```

#### Key Token Bucket Rules:
* **Conforming Traffic (`Tokens Available`):** Packets consume tokens equal to their size in bytes and are immediately forwarded into the networking stack.
* **Non-Conforming Traffic (`Tokens Exhausted`):**
  * When `ingressAction: drop` (default), non-conforming packets are dropped at the driver/kernel boundary (`HARD DROP`), enforcing a strict bandwidth ceiling.
  * When `ingressAction: pass` (monitor mode), packets pass through unthrottled while netlink exceed counters continue incrementing for telemetry collection.
* **Burst Tuning:** Setting an adequate `ingressBurst` buffer prevents TCP packet drop spikes when high-speed line-rate bursts (10Gbps+) arrive in short bursts before the sender TCP window adapts to the configured policing rate.

---

#### Live Migration Packet Path under `tcStrategy: flower`

In split-CR or dual-interface configurations, `tcStrategy: flower` isolates migration traffic by shaping egress on the physical uplink (`enp1s0`) while policing ingress streams on the secondary bridge interface (`br-vlan380`) where hardware-stripped VLAN tags are demuxed:

```text
       SOURCE NODE: hub-worker01                                                DESTINATION NODE: hub-worker02
 ┌────────────────────────────────────┐                                ┌────────────────────────────────────┐
 │  Pod / VM (vm-vlan100 / migration) │                                │  Pod / VM (Target Instance)        │
 └─────────────────┬──────────────────┘                                └─────────────────▲──────────────────┘
                   │                                                                     │
                   │ (Outbound Migration Stream)                                         │ (Inbound Demuxed Stream)
                   ▼                                                                     │
 ┌────────────────────────────────────┐                                ┌─────────────────┴──────────────────┐
 │ PHYSICAL DEVICE: enp1s0            │                                │ BRIDGE DEVICE: br-vlan380          │
 │                                    │                                │                                    │
 │ [Point 1] Egress HTB Classes       │                                │ [Point 2] Ingress Flower Police    │
 │  └─► Class 1:380 (10G Ceil)        │                                │  └─► pref 3: subnet match + police │
 └─────────────────┬──────────────────┘                                └─────────────────▲──────────────────┘
                   │                                                                     │
                   │                                                                     │ (Software Bridge Demux)
                   │                                                   ┌─────────────────┴──────────────────┐
                   │                                                   │ PHYSICAL DEVICE: enp1s0            │
                   │                                                   │                                    │
                   │                                                   │ [HW Offload: Strips 802.1Q Tag]     │
                   │                                                   └─────────────────▲──────────────────┘
                   │                                                                     │
                   │ ════════════════► FORWARD MIGRATION STREAM ══════════════════════►  │
                   │                    (Memory Pages & VM State)                        │
                   │                                                                     │
                   │  ◄═══════════════ RETURN CONTROL FLOW ════════════════════════════  │
                   │                 (TCP ACKs & Protocol Handshakes)                    │
                   └─────────────────────────────────────────────────────────────────────┘
                                          (Physical Network / VLAN 380 Wire)
```

#### Bi-Directional Migration Flow under Flower:
1. **Outbound Egress (Source Node):** VM migration payloads leaving `hub-worker01` are classified into HTB class `1:380` on `enp1s0` [Point 1], allowing memory transfers to burst up to 10Gbps when link capacity is available.
2. **Inbound Ingress (Destination Node):** When migration packets arrive on `hub-worker02` via physical link `enp1s0`, hardware offloading (`rx-vlan-offload`) strips the outer 802.1Q header. The frame passes untagged up to `br-vlan380`, where a Flower filter (`pref 3`) matches the IP subnet (`10.0.238.0/24`) [Point 2] and enforces the `act_police` token bucket without background host noise pollution.
3. **Return Control Flow:** Return TCP ACKs generated by `hub-worker02` exit through its local physical interface (`enp1s0`) under HTB egress class `1:380` and arrive back at `hub-worker01`.

---

#### Telemetry Measurement Points & Netlink Metrics

The Flower architecture exposes netlink filter and qdisc performance counters via the REST API server (`/config` and `/stats`):

| Measurement Point | TC Location & Qdisc | Exposed REST Endpoint & JSON Field | Collected Metrics & Telemetry Focus |
| :--- | :--- | :--- | :--- |
| **[Point 1]** | `enp1s0` / `sch_htb` | `/stats` $\rightarrow$ `.classStats[]` (`direction: egress`) | Physical outbound byte/packet counts, drops, overlimits, and HTB class borrowing statistics for egress class `1:380`. |
| **[Point 2]** | `br-vlan380` / `clsact` | `/stats` $\rightarrow$ `.ingressStats[]` | Per-filter match byte counters, packet counts, and `act_police` drop/exceed counters for demuxed migration streams. |
| **Filter Config** | `enp1s0` & `br-vlan380` | `/config` $\rightarrow$ `.actual.ingressFilters[]` | Reports configured filter priority (`pref 1`, `pref 3`), handle IDs, classification match criteria, and policing action (`drop` or `pass`). |

---

## Telemetry Measurement Points & Netlink Metrics

The operator captures statistics at discrete points along physical and virtual packet paths, exposing them via the REST API endpoints (`/config` and `/stats`). Output payloads explicitly include the `direction` field (`ingress` or `egress`) and the target `interface` name to simplify metric aggregation across multi-interface worker topologies.

| Measurement Point | TC Location & Qdisc | Exposed REST Endpoint & JSON Field | Collected Metrics & Telemetry Focus |
| :--- | :--- | :--- | :--- |
| **[Point 1]** | `enp1s0` / `clsact` | `/config` $\rightarrow$ `.actual.ingressFilters[]` | Action parameters (`mirred redirect dev ifb-enp1s0`), filter priority, handle ID, and attachment interface. |
| **[Point 2]** | `enp1s0` / `sch_htb` | `/stats` $\rightarrow$ `.classStats[]` (`direction: egress`, `interface: enp1s0`) | Physical outbound byte/packet counts, drops, rate overlimits, and HTB class borrowing statistics on `enp1s0`. |
| **[Point 3]** | `ifb-enp1s0` / `cls_flower` | `/stats` $\rightarrow$ `.ingressStats[]` (`direction: ingress`, `interface: ifb-enp1s0`) | Per-filter classification match counters (bytes, packets, drops) associated with VLAN 380 rules on `ifb-enp1s0`. |
| **[Point 4]** | `ifb-enp1s0` / `sch_htb` | `/stats` $\rightarrow$ `.classStats[]` (`direction: ingress`, `interface: ifb-enp1s0`) | Stateful inbound byte/packet counters, drops, overlimits, and borrowing statistics on the `ifb-enp1s0` device. |

---

### OpenShift Cluster Metrics & Ingress Filter Observability

This section details how the `vlan-traffic-control-agent` DaemonSet collects real-time Traffic Control (TC) telemetry across all OpenShift worker nodes, exposes structured egress (including HTB priority levels, default class statistics, and bandwidth borrowing) and ingress bandwidth metrics, and maps kernel netlink filter stats directly back to `VlanTrafficControl` Custom Resources.

---

### Key Capabilities

* **Native Netlink Engine:** Replaces shell subprocess calls with direct kernel socket inspection (`vishvananda/netlink`) for high-performance telemetry collection without CLI execution overhead.
* **Unified Telemetry Schema:** Merges egress bandwidth queue statistics (`prio`, `bytes`, `packets`, `overlimits`, `borrowed`) and ingress rate-policing drop counters (`bytes`, `packets`, `drops`) into a single API payload tagged with `direction` and `interface`.
* **Default Class Telemetry:** Automatically reports metrics for the default fallback class (e.g. `1:99` / `default-fallback`), capturing unclassified host traffic.
* **HTB Priority & Borrowing Tracking:** Surfaces class priority (`prio`) and `borrowed` token counters when a class exceeds its guaranteed `rate` and consumes spare root capacity up to its `ceil`.
* **CRD Metadata Mapping:** Automatically correlates kernel handles (`classId` `1:100`, `filterId` `pref 100`) with custom human-readable class names defined in the `VlanTrafficControl` CRD.
* **Granular Filtering:** Supports target filtering by specific **VLAN Tag ID** (`?vlan=100`), **TC Class Handle** (`?classId=1:100`), or **Interface Name** (`?interface=br-vlan380`) to isolate specific tenant or application traffic.

---

### Agent Observability Endpoints

Each agent pod exposes an HTTP telemetry interface on port `8080`:

| Endpoint | Method | Query Parameters | Description |
| :--- | :--- | :--- | :--- |
| `/config` | `GET` | `interface` *(required)*, `classId` *(optional)* | Audits live host kernel TC state against desired CRD specifications and returns a structured drift report. |
| `/stats` | `GET` | `interface` *(optional)*, `vlan` *(optional)*, `classId` *(optional)* | Fetches structured egress (with prio, default class & borrowing) and ingress TC metrics via Netlink sockets. |
| `/reconcile` | `POST` | `interface` *(optional)* | Triggers an immediate local TC rule reconciliation pass on the node. |
| `/cleanup` | `DELETE` / `POST` | `interface` *(required)* | Flushes root HTB and ingress policing qdiscs on the node. |
| `/healthz` | `GET` | *None* | Liveness probe endpoint. |

---

### Ingress Action Reference

| Action | API Display (`/config`) | Netlink Code | Behavior & Use Case |
| :--- | :--- | :--- | :--- |
| **`drop`** | `"police drop"` | `netlink.TC_ACT_SHOT` | **Default / Enforce Mode.** Hard drops exceeding packets immediately to prevent bandwidth exhaustion. |
| **`pass`** | `"police pass"` | `netlink.TC_ACT_OK` | **Monitor Mode.** Allows exceeding traffic to pass through uninterrupted while recording packet/byte counters. |

---

### TC Component Reference

| Direction | Component | TC Type | Purpose in Operator |
| :--- | :--- | :--- | :--- |
| **Ingress (RX)** | **Qdisc** | `ingress` (`ffff:`) / `clsact` | Hooks directly into the kernel RX pipeline prior to protocol handling. |
| | **Filter** | `flower` / `fw` / `u32` | Classifies incoming frames by **802.1Q VLAN ID**, **IP Subnet**, or **SKB Mark**. |
| | **Action** | `act_police` / `act_mirred` | Enforces rate caps via policing drops (`TC_ACT_SHOT`) or redirects ingress streams to IFB devices. |
| **Egress (TX)** | **Qdisc** | `htb` (`1:0`) | Hierarchical Token Bucket shaper managing outgoing traffic queues. |
| | **Classes** | `htb class` | Guarantees minimum throughput (`egressRate`) and defines max burst ceilings (`egressCeil`). |
| | **Leaf Qdiscs**| `fq_codel` | Fair queuing attached to active leaf classes to prevent bufferbloat and optimize latency. |

---

### Querying Telemetry

The Node Agent exposes an HTTP server on port `8080` to provide live Netlink kernel telemetry for egress HTB classes and ingress policing rules.

#### 1. Multi-Interface Generic Sweep (No Parameters)

Querying `/stats` without parameters automatically discovers all active managed host interfaces (physical, bonds, bridges, and IFB devices) while filtering out unmanaged bridges (`br-ex`) and container `veth` pairs:

```bash
AGENT_IP=$(oc get pod -n openshift-vlan-tc-operator -l app=vlan-traffic-control-agent -o jsonpath='{.items[0].status.podIP}')

oc exec -n openshift-vlan-tc-operator deploy/vlan-traffic-control-manager -- \
  curl -s "http://${AGENT_IP}:8080/stats" | jq .
```

---

#### 2. Sweep All Agent Nodes in the Cluster

Audit telemetry across all running Agent Pod IPs sequentially from the Manager execution context:

```bash
oc get pods -n openshift-vlan-tc-operator -l app=vlan-traffic-control-agent \
  -o jsonpath='{range .items[*]}{.spec.nodeName}{"\t"}{.status.podIP}{"\n"}{end}' | \
  while read -r node ip; do
    echo "=== Node: ${node} (${ip}) ==="
    oc exec -n openshift-vlan-tc-operator deploy/vlan-traffic-control-manager -- \
      curl -s "http://${ip}:8080/stats?interface=enp1s0" | jq .
  done
```

---

#### 3. Target Specific Interfaces, Classes, or Fallback Rules

Isolate telemetry per interface, class handle, or class name:

```bash
# Query a specific bridge interface
curl -s "http://${AGENT_IP}:8080/stats?interface=br-vlan380" | jq .

# Filter by human-readable class name
curl -s "http://${AGENT_IP}:8080/stats?className=default-fallback" | jq .

# Filter by specific HTB Class ID handle
curl -s "http://${AGENT_IP}:8080/stats?interface=enp1s0&classId=1:100" | jq .
```

---

#### Sample Telemetry Payload (`GET /stats`)

```json
{
  "interface": "enp1s0",
  "node": "hub-worker03.ocp4-hub.test.com",
  "classStats": [
    {
      "interface": "enp1s0",
      "direction": "egress",
      "classId": "1:99",
      "name": "default-fallback",
      "prio": 0,
      "bytes": 54210,
      "packets": 412,
      "overlimits": 0,
      "borrowed": 0
    },
    {
      "interface": "enp1s0",
      "direction": "egress",
      "classId": "1:100",
      "name": "storage-vlan-100",
      "prio": 1,
      "bytes": 109802,
      "packets": 1279,
      "overlimits": 0,
      "borrowed": 42
    }
  ],
  "ingressStats": [
    {
      "interface": "br-vlan380",
      "direction": "ingress",
      "classId": "1:380",
      "filterId": "pref 3",
      "bytes": 842100,
      "packets": 5930,
      "drops": 0
    }
  ]
}
```

---

## Direct Host Verification via `oc debug node`

To directly verify Linux kernel Traffic Control configurations and debug low-level netlink object state independently of the operator API, execute debugging commands inside the host network namespace using `oc debug node/<worker-node>`:

### 1. Verify Egress HTB Class Hierarchies & Statistics
Inspect HTB class definitions, committed rates (`rate`), bursting ceilings (`ceil`), and byte/packet counters directly on the physical interface or virtual bridge:

```bash
# View active HTB class hierarchy and operational rates
oc debug node/hub-worker01.ocp4-hub.test.com -- \
  chroot /host tc -s class show dev enp1s0

# Inspect HTB classes on an IFB virtual device
oc debug node/hub-worker01.ocp4-hub.test.com -- \
  chroot /host tc -s class show dev ifb-enp1s0
```

---

### 2. Verify Ingress Policing Rules & Actions (Flower / IFB)
Audit active ingress classification filters, redirection actions (`act_mirred`), and rate policing counters (`act_police` drop/exceed counts):

```bash
# View ingress filters on the physical interface (e.g., IFB mirred redirection or Flower policing)
oc debug node/hub-worker01.ocp4-hub.test.com -- \
  chroot /host tc -s filter show dev enp1s0 parent ffff:

# View ingress Flower filters on a software bridge link
oc debug node/hub-worker01.ocp4-hub.test.com -- \
  chroot /host tc -s filter show dev br-vlan380 parent ffff:
```

---

### 3. Verify Active Qdiscs & Leaf Schedulers
Confirm that the root HTB qdisc (`handle 1:`) and ingress qdiscs (`handle ffff:`) are attached to host interfaces alongside active leaf queue disciplines (`fq_codel`):

```bash
# Show all qdiscs attached to host network interfaces
oc debug node/hub-worker01.ocp4-hub.test.com -- \
  chroot /host tc qdisc show dev enp1s0

# Check if IFB netdevices exist and are UP in the host kernel
oc debug node/hub-worker01.ocp4-hub.test.com -- \
  chroot /host ip link show dev ifb-enp1s0
```

---

### 4. Live Netlink Event Stream Monitoring
Monitor real-time kernel traffic control modifications (filter creations, qdisc updates, class deletions) as they happen on the host node:

```bash
oc debug node/hub-worker01.ocp4-hub.test.com -- \
  chroot /host tc monitor
```

---

## Node Configuration & Alignment Engine

This section details how the `vlan-traffic-control-agent` DaemonSet performs real-time drift detection and configuration auditing across OpenShift worker nodes. By comparing live kernel qdisc, class, and filter states retrieved via Netlink sockets (`vishvananda/netlink`) against the aggregated target specifications from `VlanTrafficControl` Custom Resources, the engine provides immediate visibility into node configuration alignment and pinpoints specific parameter discrepancies.

---

### Key Capabilities

* **Deterministic Drift Analysis:** Computes a strict boolean alignment state (`isAligned: true|false`) by matching live kernel socket parameters against the expected CRD specification matrix.
* **Missing Host Interface Detection:** Automatically flags targeted network interfaces (`br-vlan100`, `enp1s0`) that are absent on specific worker nodes, generating clear drift deltas rather than crashing or returning false positives.
* **Polymorphic Filter Evaluation:** Dynamically evaluates all active kernel classifier types (`Flower`, `U32`, `fw` skb-mark filters, and `GenericFilter`) via Netlink priority handles to eliminate false-negative drift reports when matching skb marks vs VLAN IDs.
* **Filter Engine Transparency:** Reports the exact kernel classifier module (`fw`, `flower`, `u32`) and protocol ID fulfilling each active ingress policy (`ingressFilters`).
* **Qdisc Existence Audit:** Verifies the presence of both the root HTB qdisc (`1:`) and ingress policing qdisc (`ffff:`) on the target host interface (`htbQdiscPresent`, `ingressPresent`).
* **Delta Discrepancy Reporting:** Returns a detailed list of configuration deltas (`driftDeltas`) identifying missing host devices, missing egress classes, orphan qdiscs, missing ingress policing filters, or mismatched TC priorities (`priority`).

---

### Alignment Engine Endpoint (`/config`)

The agent pod exposes the following HTTP configuration auditing interface on port `8080`:

| Endpoint | Method | Query Parameters | Description |
| :--- | :--- | :--- | :--- |
| `/config` | `GET` | `interface` *(required)*, `classId` *(optional)* | Audits live host kernel TC state against desired CRD specifications and returns a structured drift report. |

---

### Auditing Configuration Alignment

#### 1. Audit Full Node Configuration Alignment Across Worker Cluster
Run this command from inside the cluster manager pod to check alignment across all worker nodes:

```bash
for pod_ip in $(oc get pods -n openshift-vlan-tc-operator -l app=vlan-traffic-control-agent -o jsonpath='{.items[*].status.podIP}'); do
  oc exec -n openshift-vlan-tc-operator deploy/vlan-traffic-control-manager -- \
    curl -s "http://${pod_ip}:8080/config?interface=enp1s0" | jq .
done
```

#### 2. Audit Single VLAN or TC Rule Alignment
Isolate alignment status for a specific class ID handle (e.g., `1:380` / VLAN 380):

```bash
CLASS_ID="1:380"
curl -s "http://${agent_pod_ip}:8080/config?interface=enp1s0" | jq --arg cid "$CLASS_ID" '
  (.desired.classes[] | select(.classId == $cid).name) as $targetName |
  {
    node: .node,
    interface: .interface,
    isAligned: .isAligned,
    classId: $cid,
    matched_name: $targetName,
    desired_class: (.desired.classes[] | select(.classId == $cid)),
    actual_egress_class: (.actual.classes[] | select(.classId == $cid)),
    actual_ingress_filter: (.actual.ingressFilters[] | select(.name == $targetName)),
    drift_deltas: [.driftDeltas[]? | select(.classId == $cid or .name == $targetName or .target == $targetName)]
  }'
```

---

### Sample Configuration Alignment Payload (`/config`)

#### 1. Fully Aligned State Example:

```json
{
  "node": "hub-worker01.ocp4-hub.test.com",
  "interface": "enp1s0",
  "isAligned": true,
  "classId": "1:380",
  "matched_name": "vlan-380-low-priority-migration",
  "desired_class": {
    "name": "vlan-380-low-priority-migration",
    "classId": "1:380",
    "matchType": "subnet",
    "subnet": "10.0.238.0/24",
    "egressRate": "500Mbit",
    "egressCeil": "2Gbit",
    "egressBurst": "20k",
    "enableFqCodel": true,
    "ingressRate": "500Mbit",
    "ingressBurst": "20k",
    "ingressAction": "drop",
    "priority": 3
  },
  "actual_egress_class": {
    "name": "vlan-380-low-priority-migration",
    "classId": "1:380",
    "matchType": "subnet",
    "subnet": "10.0.238.0/24",
    "egressRate": "500Mbit",
    "egressCeil": "2Gbit",
    "priority": 3
  },
  "actual_ingress_filter": {
    "priority": 3,
    "handle": 1,
    "type": "flower",
    "protocol": 2048,
    "name": "vlan-380-low-priority-migration",
    "matchType": "subnet",
    "subnet": "10.0.238.0/24",
    "ingressRate": "500Mbit",
    "ingressBurst": "20k",
    "action": "police drop"
  },
  "drift_deltas": []
}
```

#### 2. Misaligned State - Missing Host Interface (`br-vlan100` absent on worker):

```json
{
  "node": "hub-worker01.ocp4-hub.test.com",
  "interface": "br-vlan100",
  "isAligned": false,
  "desired": {
    "interface": "br-vlan100",
    "rate": "10Gbit",
    "classes": [
      {
        "name": "storage-vlan-100",
        "classId": "1:100",
        "matchType": "subnet",
        "subnet": "10.0.100.0/24",
        "egressRate": "50Mbit",
        "egressCeil": "10Gbit",
        "ingressRate": "30Mbit",
        "ingressBurst": "15k",
        "ingressAction": "drop",
        "priority": 1,
        "enableFqCodel": true
      }
    ]
  },
  "actual": {
    "htbQdiscPresent": false,
    "ingressPresent": false,
    "classes": [],
    "ingressFilters": []
  },
  "driftDeltas": [
    {
      "targetHandle": "interface br-vlan100",
      "property": "existence",
      "expected": "present on host",
      "actual": "missing device"
    },
    {
      "targetHandle": "class 1:100",
      "property": "existence",
      "expected": "configured",
      "actual": "missing (interface br-vlan100 absent)"
    }
  ]
}
```

---

### Reference Matrix: Drift Delta Combinations & Causes

#### 1. Interface & Qdisc Existence Errors

| Target Handle | Property | Expected | Actual | Description & Root Cause |
| :--- | :--- | :--- | :--- | :--- |
| `interface <iface>` | `existence` | `present on host` | `missing device` | Netlink returned `LinkNotFoundError`. The host network bridge or interface does not exist on this worker node. |
| `qdisc root` | `existence` | `htb` | `missing` | The HTB root qdisc (`1:`) is missing from the interface on the host. |
| `qdisc ingress` | `existence` | `ingress` | `missing` | The ingress policing qdisc (`ffff:`) was flushed or omitted on the target host interface. |

#### 2. Egress HTB Class Errors

| Target Handle | Property | Expected | Actual | Description & Root Cause |
| :--- | :--- | :--- | :--- | :--- |
| `class <handle>` *(e.g. `class 1:100`)* | `existence` | `configured` | `missing` | The egress HTB class handle exists in the CRD spec but was not created in the kernel. |
| `class <handle>` | `existence` | `configured` | `missing (interface <iface> absent)` | Cascading failure reported when an HTB class cannot be verified because the host interface itself is absent. |
| `class <handle>` | `priority` | `<expected_prio>` | `<actual_prio>` | The class exists in the kernel, but its priority (`prio`) diverges from the CRD spec. |
| `class <handle>` | `rate` | `<expected_rate>` | `<actual_rate>` | The configured egress committed rate differs from the live Netlink state. |
| `class <handle>` | `ceil` | `<expected_ceil>` | `<actual_ceil>` | The configured maximum ceiling rate differs from the live Netlink state. |

#### 3. Ingress Filter & Classifier Errors

| Target Handle | Property | Expected | Actual | Description & Root Cause |
| :--- | :--- | :--- | :--- | :--- |
| `ingress filter pref <prio>` | `existence` | `configured` | `missing` | The ingress policing filter (`fw`, `flower`, or `u32`) associated with this priority handle is missing. |
| `ingress filter pref <prio>` | `rate` | `<expected_police>` | `<actual_police>` | The policing action drop threshold in the kernel does not match the CRD `ingressRate`. |
| `ingress filter pref <prio>` | `action` | `<expected_action>` | `<actual_action>` | The kernel action (`police drop` vs `police pass`) differs from the CRD `ingressAction`. |

---

## Performance Profile & Resource Efficiency Comparisons Across TC Strategies

Selecting the optimal Traffic Control execution strategy (`spec.tcStrategy`) depends on line rate, host CPU availability, SmartNIC capabilities, and whether ingress traffic requires stateful class queueing or stateless rate policing.

```text
===================================================================================================
CPU CORE OVERHEAD & SAVINGS COMPARISON BY TC STRATEGY
===================================================================================================

  10G Link
    - ifb (Stateful SW)  │ [████████] 2-4 Cores Consumed (Baseline)
    - flower (SW Mode)   │ [████] 1-2 Cores Consumed (~50% Reduction)
    - flower (HW Offload)│ [] 0 Cores Consumed (2-4 Cores Reclaimed / 100% Offload)
                         │
  25G Link
    - ifb (Stateful SW)  │ [████████████████] 5-10 Cores Consumed (Baseline)
    - flower (SW Mode)   │ [████████] 2-5 Cores Consumed (~50% Reduction)
    - flower (HW Offload)│ [] 0 Cores Consumed (5-10 Cores Reclaimed / 100% Offload)
                         │
  40G Link
    - ifb (Stateful SW)  │ [████████████████████████] 8-15 Cores Consumed (Baseline)
    - flower (SW Mode)   │ [████████████] 4-8 Cores Consumed (~50% Reduction)
    - flower (HW Offload)│ [] 0 Cores Consumed (8-15 Cores Reclaimed / 100% Offload)
                         │
 100G+ Link
    - ifb (Stateful SW)  │ [████████████████████████████████] 20+ Cores Consumed (Baseline / Bottleneck)
    - flower (SW Mode)   │ [████████████████] 10-14 Cores Consumed (~50% Reduction)
    - flower (HW Offload)│ [] 0 Cores Consumed (20+ Cores Reclaimed / 100% Offload)
                         └─────────────────────────────────────────────────────────────────────────
                           0 Cores Consumed                                   20+ Cores Overhead

===================================================================================================
LATENCY & PROCESSING OVERHEAD COMPARISON BY TC STRATEGY
===================================================================================================

  10G Link
    - ifb (Stateful SW)  │ [████████████████████] 15.0 - 50.0 µs (Queueing & IFB Redirect)
    - flower (SW Mode)   │ [██████████] 5.0 - 15.0 µs (3x Lower / SW Police)
    - flower (HW Offload)│ [██] 1.5 - 3.0 µs (10x - 15x Lower / Silicon In-Line)
                         │
  25G Link
    - ifb (Stateful SW)  │ [████████████████████] 10.0 - 30.0 µs (Queueing & IFB Redirect)
    - flower (SW Mode)   │ [██████████] 3.5 - 10.0 µs (3x Lower / SW Police)
    - flower (HW Offload)│ [█] 1.0 - 2.0 µs (10x - 15x Lower / Silicon In-Line)
                         │
  40G Link
    - ifb (Stateful SW)  │ [████████████████████] 8.0 - 20.0 µs (Queueing & IFB Redirect)
    - flower (SW Mode)   │ [██████████] 2.5 - 7.0 µs (3x Lower / SW Police)
    - flower (HW Offload)│ [█] 0.8 - 1.5 µs (10x - 13x Lower / Silicon In-Line)
                         │
 100G+ Link
    - ifb (Stateful SW)  │ [████████████████████] 5.0 - 15.0 µs (Queueing & IFB Redirect)
    - flower (SW Mode)   │ [██████████] 1.8 - 5.0 µs (3x Lower / SW Police)
    - flower (HW Offload)│ [█] 0.5 - 1.0 µs (10x - 15x Lower / Silicon In-Line)
                         └─────────────────────────────────────────────────────────────────────────
                           Sub-Microsecond (0.5 µs)                         High Latency (50.0 µs)
```

---

### Strategy Comparison, Advantages & Disadvantages

| Strategy Mode | Execution Location | CPU Overhead | Latency Overhead | Key Strengths | Limitations | Optimal Use Case |
| :--- | :--- | :--- | :--- | :--- | :--- | :--- |
| **IFB (Stateful SW)** | Kernel (`act_mirred` + `ifb` netdevice) | **High** (2–20+ Cores) | **15–50 µs** | • Full HTB class trees on ingress.<br>• Dynamic bandwidth borrowing.<br>• Supports rate ceilings (`ceil`). | • Software redirect loop overhead.<br>• High CPU utilization at 25G/100G.<br>• Latency penalties from buffer queueing. | Strict ingress SLA enforcement requiring dynamic bandwidth borrowing on 10G links. |
| **Flower SW Mode** | Kernel (`clsact` / `act_police`) | **Medium** (1–14 Cores) | **1.8–15 µs** | • ~50% CPU savings vs IFB.<br>• Direct ingress policing without redirects.<br>• Supports monitor mode (`ingressAction: pass`). | • Stateless policing only (hard capping, no HTB borrowing on ingress). | High-speed links (25G–100G) without TC hardware offload support or software bridge targets (`br-vlan380`). |
| **Flower HW Offload** | NIC ASIC (`switchdev` / TC Offload) | **Zero** (0 Cores) | **0.5–3 µs** | • 100% host CPU offload.<br>• Sub-microsecond wire-speed latency.<br>• In-silicon rate enforcement. | • Requires SmartNIC / SRIOV switchdev hardware support.<br>• Limited ASIC table capacity for complex rules. | Enterprise high-throughput clusters (25G/40G/100G+) with TC-offload capable SmartNICs. |

---

### Architectural Recommendation Summary

1. **Use `tcStrategy: flower` (SW Mode) for General Deployments:** Provides the best balance of efficiency, compatibility, and low host CPU consumption across standard 10G/25G physical NICs and software bridges (`br-vlan380`).
2. **Use `tcStrategy: flower` (HW Offload Mode) for High-Speed Infrastructure:** Essential on 40G/100G+ SmartNIC uplinks where software packet processing introduces severe CPU bottlenecks. Offloads `cls_flower` filters and policing directly into NIC silicon.
3. **Use `tcStrategy: ifb` Only When Stateful Ingress Queueing Is Mandatory:** Deploy IFB when inbound traffic streams explicitly require full HTB queuing trees, leaf AQM (`fq_codel`), and dynamic bandwidth borrowing rather than stateless token-bucket policing caps.



