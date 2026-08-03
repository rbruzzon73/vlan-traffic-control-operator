# VLAN Traffic Control Operator - Solution Overview & Technical Architecture

**VLAN Traffic Control Operator** delivers fine-grained, declarative Quality of Service (QoS) and host-level network traffic shaping for OpenShift/Kubernetes clusters.

Standard Kubernetes bandwidth CNI plugins are often limited to basic pod-level ingress/egress rate limiting and fail to address non-pod traffic, secondary Multus interfaces, Open vSwitch (OVS) bridges, or hardware-stripped VLAN tags. This operator bridges that gap by allowing cluster administrators to manage Linux Traffic Control (`tc`) queueing disciplines, classifiers, and rate limiters natively across worker nodes using standard OpenShift Custom Resources.

---

### Core Architecture & Component Workflow

The operator follows a dual-component architecture consisting of a cluster-wide **Controller Manager** and host-bound **Node Agents**:

```text
+-----------------------------------------------------------------------------------+
|                                 OpenShift Cluster                                 |
|                                                                                   |
|  +-------------------------------------+                                          |
|  |  VlanTrafficControl CR (YAML Spec)  |                                          |
|  +------------------+------------------+                                          |
|                     |                                                             |
|                     v                                                             |
|  +-------------------------------------+                                          |
|  |   vlan-tc-operator-controller       |  (Watches CRDs, validates specs,         |
|  |             (Manager)               |   updates cluster-wide status)           |
|  +------------------+------------------+                                          |
|                     |                                                             |
|                     | Reconciles via API & triggers Agent HTTP endpoints          |
|                     v                                                             |
|  +-----------------------------------------------------------------------------+  |
|  |                           Worker Node (DaemonSet)                           |  |
|  |                                                                             |  |
|  |  +------------------------+        chroot /host     +--------------------+  |  |
|  |  |   vlan-tc-agent Pod    | ----------------------> | Host OS Network    |  |  |
|  |  | (HTTP API & Reconciler)|                         | Namespace (tc)     |  |  |
|  |  +-----------+------------+                         +---------+----------+  |  |
|  |              |                                                |             |  |
|  |              | Fetches /stats                                 | Configures  |  |
|  |              v                                                v             |  |
|  |       +--------------+                              +--------------------+  |  |
|  |       | Structured   |                              | HTB Qdisc, Flower, |  |  |
|  |       | JSON Metrics |                              | FW & Ingress Police|  |  |
|  |       +--------------+                              +--------------------+  |  |
|  +-----------------------------------------------------------------------------+  |
+-----------------------------------------------------------------------------------+
```

### Component Architecture & Module Reference

This document provides a detailed breakdown of every module and component comprising the **VLAN Traffic Control Operator**, outlining their scope, responsibilities, and operational behavior across the cluster and worker nodes.

---

#### 1. Controller Manager (`cmd/manager` & `internal/controller`)

The Controller Manager is the central brain of the operator running at the cluster level. It acts as an orchestrator, handling resource lifecycle events and keeping cluster state aligned with Custom Resource definitions.

* **Scope:** Cluster-wide control plane component.
* **Responsibilities:**
  * **CRD Watching:** Listens for `Create`, `Update`, and `Delete` events on `VlanTrafficControl` Custom Resources (`networking.med.io/v1alpha1`).
  * **Node Targeting & Filtering:** Evaluates the `nodeSelector` block defined in the CRD to identify which worker nodes should receive traffic shaping policies.
  * **Status Aggregation:** Collects health and reconciliation status from all node agents and updates the `.status` subresource of the `VlanTrafficControl` CR.
  * **Agent Orchestration:** Calls the HTTP `/reconcile` or `/cleanup` REST endpoints on individual Node Agent pods to trigger immediate host-level updates whenever a CR is modified or deleted.
* **Behavior:** Operates as a standard Kubernetes controller loop using `controller-runtime`. It does not execute direct `tc` commands on the host itself; instead, it delegates all host networking manipulations to the Node Agents.

---

#### 2. Host Node Agent (`cmd/agent`)

The Host Node Agent is the execution engine running directly on every targeted worker node.

* **Scope:** Host-bound DaemonSet pod running with host networking, privileged SCC permissions, and `/host` directory mounts.
* **Responsibilities:**
  * **Host Traffic Shaping:** Executes `tc` commands inside the host network namespace via `chroot /host` to configure `htb` qdiscs, classes, and filters (`cls_flower`, `cls_fw`, `act_police`).
  * **Startup Self-Healing:** Performs an automatic startup reconciliation pass (`reconcileLocalTc`) when spawned, ensuring host network interfaces (`enp1s0`, `br-ex`, bonds) match the desired CR state immediately after a node reboot or agent restart.
  * **REST API Server:** Exposes an internal HTTP server on port `8080` providing:
    * `GET /stats`: Returns real-time byte/packet statistics and queue metrics.
    * `POST /reconcile`: Triggers an instant local `tc` rule sync.
    * `POST /cleanup` / `DELETE /cleanup`: Performs selective removal of rules managed by the operator without disturbing other host qdiscs.
    * `GET /healthz`: Health check probe endpoint.
* **Behavior:** Runs continuously on worker nodes. When triggered, it reads cluster CRDs via `client-go` and invokes `pkg/executor` to safely apply or update host rules using `tc class replace` and `tc filter replace`.

---

#### 3. Kernel Module Loader (`pkg/executor/modules.go`)

This module ensures the Linux kernel running on the host node has all required Traffic Control modules loaded before attempting rule execution.

* **Scope:** Internal package function invoked during agent boot.
* **Responsibilities:**
  * Checks for loaded kernel modules by reading host module paths via `/lib/modules`.
  * Automatically executes `modprobe` inside the host namespace (`chroot /host modprobe <module>`) if a module is missing.
* **Modules Verified:**
  * `sch_htb`: Hierarchy Token Bucket queueing discipline.
  * `cls_flower`: Advanced multi-field packet classifier.
  * `cls_fw`: Firewall mark filter module for `skbmark` matching.
  * `act_police`: Rate policing action module for ingress caps.
  * `sch_fq_codel`: Fair Queueing Controlled Delay AQM leaf qdisc.
* **Behavior:** Non-destructive and idempotent. If modules are already built into the kernel or loaded, it logs a debug entry and continues; if missing, it attempts to load them dynamically.

---

#### 4. TC Execution Engine (`pkg/executor/tc.go`)

This is the core low-level execution package that builds and executes deterministic Linux `tc` command sequences.

* **Scope:** Core Go package relied upon by `cmd/agent`.
* **Responsibilities:**
  * **Qdisc & Class Hierarchy Creation:**
    * Ensures the egress root HTB qdisc (`handle 1:`) and parent class (`1:1`) exist on the target interface.
    * Configures the default fallback class (`1:99`) with priority 0 for unclassified traffic.
    * Ensures the ingress qdisc (`handle ffff:`) is present for rate policing.
  * **Child Class Allocation:** Adds or replaces HTB classes (`1:100`, `1:280`, etc.) specifying `rate`, `ceil`, `burst`, and `prio`.
  * **AQM Leaf Attachment:** Optionally attaches `fq_codel` leaf qdiscs under HTB classes when `enableFqCodel: true`.
  * **Filter Generation & Execution:**
    * `ApplyClassEgressFilter`: Constructs egress classification filters attached to parent `1:`. Uses explicit numeric filter handles (`handle <minor>`) to allow atomic replacement.
    * `ApplyClassIngressPolice`: Constructs ingress policing drop rules attached to parent `ffff:` using kernel `act_police`.
* **Behavior:** Translates structured Go CRD specs into atomic command arrays (e.g., `chroot /host tc filter replace dev enp1s0 ...`) and handles Linux kernel execution output.

---

#### 5. Classifier Resolver (`ResolveClassifier` in `pkg/executor/tc.go`)

A dedicated decision-tree function responsible for selecting the correct Linux kernel classifier backend based on class criteria.

* **Scope:** Internal utility function in `pkg/executor`.
* **Responsibilities & Mapping:**
  * **`matchType: vlan`** -> Returns `filterType: "flower"`, `protocol: "802.1Q"`, matching `vlan_id`.
  * **`matchType: subnet`** -> Returns `filterType: "flower"`, `protocol: "ip"`, matching `src_ip` (egress) or `dst_ip` (ingress).
  * **`matchType: mark`** -> Returns `filterType: "fw"`, `protocol: "all"`, matching `handle <mark> fw`.
  * **`matchType: auto`** -> Evaluates specs top-down:
    1. If `vlanId > 0` -> Selects `vlan` (`cls_flower`).
    2. Else if `subnet != ""` -> Selects `subnet` (`cls_flower`).
    3. Else if `mark > 0` -> Selects `mark` (`cls_fw`).
* **Behavior:** Pure, side-effect-free evaluation logic that returns exact parameter strings and execution flags to the caller.

---

#### 6. Statistics Collector (`pkg/executor/htb_executor.go`)

This component queries the Linux kernel to retrieve real-time traffic shaping metrics for monitoring and API exposure.

* **Scope:** Internal package invoked by the agent `/stats` HTTP endpoint.
* **Responsibilities:**
  * Executes `tc -s class show dev <iface>` and `tc -s filter show dev <iface> ingress` via `chroot /host`.
  * Parses raw `tc` text outputs (bytes sent, packet counts, drops, rate overlimits, tokens, queue backlog) into structured Go structs and JSON objects.
* **Behavior:** Read-only operation that provides high-frequency visibility into queue utilization and rate limit enforcement across nodes.

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
  ├── 1. Modules Loader (pkg/executor/modules.go)  ──> Loads sch_htb, cls_flower, cls_fw
  ├── 2. Classifier Resolver (ResolveClassifier)   ──> Maps vlan/subnet/mark criteria
  ├── 3. TC Engine (pkg/executor/tc.go)            ──> Executes chroot /host tc commands
  └── 4. Stats Collector (htb_executor.go)         ──> Exposes /stats JSON metrics
  ```

---

### Key Capabilities & Traffic Control Features

#### Flexible Multi-Match Classification
Traffic identification goes beyond standard 802.1Q VLAN tags. The operator supports three primary classification backends to handle complex container and virtual machine networking topologies:

* **802.1Q VLAN Tag (`matchType: vlan`):** Uses `cls_flower` for direct matching on 802.1Q tagged frames traversing physical trunk interfaces or bond devices.
* **IP Subnet / CIDR (`matchType: subnet`):** Uses `cls_flower` matching on source IP (`src_ip`) for egress and destination IP (`dst_ip`) for ingress. Ideal for OpenShift Virtualization (KubeVirt) or OVS bridge interfaces where VLAN tags are stripped prior to hitting the host Linux stack.
* **Socket Buffer Mark (`matchType: mark`):** Uses `cls_fw` (`handle <mark> fw`) to match on 32-bit `skbmark` values set upstream by Open vSwitch flows, `iptables`, or `nftables`.
* **Auto-Detection (`matchType: auto`):** Dynamically inspects class attributes and automatically selects the optimal classifier (`flower` or `fw`).

#### Hierarchy Token Bucket (HTB) & Traffic Shaping
* **Guaranteed Egress Bandwidth (`egressRate`):** Guarantees minimum outbound bandwidth allocation per traffic class under heavy contention.
* **Burst Ceilings (`egressCeil`):** Limits maximum burst rate capacity when excess root interface bandwidth is available.
* **Ingress Rate Policing (`ingressRate` & `ingressBurst`):** Enforces hard bandwidth caps on incoming interface traffic using kernel `act_police` drop filters on the `ingress` (`ffff:`) qdisc.
* **Priority Queuing (`priority`):** Assigns HTB and filter priority bands (1–7) to ensure latency-sensitive control traffic or storage networks pre-empt bulk data flows.

#### Active Queue Management (AQM)
* **Bufferbloat Prevention (`enableFqCodel`):** Automatically attaches `fq_codel` (Fair Queueing Controlled Delay) leaf qdiscs beneath HTB classes to minimize queue latency and prevent TCP bufferbloat under maximum throughput conditions.

---

### Target Use Cases

* **Shared Interface & Live-Migration Protection:** On hyperconverged host interfaces shared across OpenShift control plane services, OpenStack/KubeVirt VM networks, and storage VLANs, high-burst operations like **VM live migrations** can saturate physical links. By assigning strict priority bands (`priority: 1`) and rate ceilings, you ensure critical services like **ETCD heartbeat/consensus traffic** remain pre-empted and latency-protected.
* **OpenShift Virtualization / KubeVirt:** Enforce strict egress and ingress bandwidth caps on virtual machine secondary interfaces (SR-IOV, Multus, or OVS bridge ports) to prevent individual tenant VMs from monopolizing node-level network capacity.
* **Multi-Tenant Storage Isolation:** Prioritize latency-sensitive NVMe-oF, Ceph, or iSCSI storage traffic (e.g., VLAN 100/200) over standard pod egress traffic on shared 10G/25G/100G host NICs.
* **Edge & Far-Edge Deployments:** Manage tight bandwidth constraints on resource-constrained edge nodes communicating over limited backhaul or satellite links by strictly queueing bulk data behind real-time applications.

---

## Custom Resource Definition (CRD) Reference

The `VlanTrafficControl` Custom Resource (`networking.med.io/vlan-traffic-control`) defines the desired traffic shaping state, target nodes, and pod scheduling tolerations.

### `VlanTrafficControlSpec` (`spec`)

| Field | Type | Required | Default | Description |
| :--- | :--- | :---: | :---: | :--- |
| `nodeSelector` | `map[string]string` | No | `{}` | Map of node labels used to select target worker or infrastructure nodes (e.g., `node-role.kubernetes.io/worker: ""`). |
| `tolerations` | `[]Toleration` | No | `[]` | List of Kubernetes pod/daemonset tolerations allowing the agent to run on tainted nodes (e.g., master/control-plane or dedicated infra taints). |
| `reconcileIntervalSeconds` | `integer` | No | `30` | Interval in seconds between node agent reconciliation loops. |
| `tcStrategy` | `string` | **Yes** | `"flower"` | Default traffic control strategy execution mode. Valid values: `flower`, `u32`, `auto`. |
| `htbRoot` | `HtbRootSpec` | **Yes** | — | Root HTB and interface configuration. |

### Node Targeting & Taint Tolerations

The operator provides granular control over which nodes in the cluster receive traffic control rules:

* **`nodeSelector` Label Matching:** Restricts policy enforcement strictly to worker nodes matching specified key-value labels. If left empty (`{}`), all accessible nodes are evaluated.
* **`tolerations` Support:** Allows the host agent to schedule and execute on tainted nodes, such as master nodes (`node-role.kubernetes.io/master:NoSchedule`), control-plane nodes (`node-role.kubernetes.io/control-plane:NoSchedule`), or dedicated edge/storage infrastructure nodes. Standard Kubernetes toleration fields (`key`, `operator`, `value`, `effect`, `tolerationSeconds`) are supported.

---

### `HtbRootSpec` (`spec.htbRoot`)

| Field | Type | Required | Default | Description |
| :--- | :--- | :---: | :---: | :--- |
| `interface` | `string` | **Yes** | — | Target physical, bond, or bridge network interface name (e.g., `enp1s0`, `br-ex`). |
| `rate` | `string` | **Yes** | — | Total root egress bandwidth rate capacity for the interface (e.g., `10Gbit`). |
| `defaultClassId` | `string` | No | `"1:99"` | Default HTB class ID where unclassified traffic is routed. |
| `htbId` | `integer` | No | `1` | Custom HTB root handle ID. |
| `classes` | `[]VlanClassSpec` | **Yes** | — | List of individual traffic control class definitions configured under this root interface. |

### `VlanClassSpec` (`spec.htbRoot.classes[]`)

| Field | Type | Required | Default | Description |
| :--- | :--- | :---: | :---: | :--- |
| `name` | `string` | No | `""` | Human-readable name or descriptor for this traffic class. |
| `matchType` | `string` | No | `"auto"` | Classification strategy. Valid values: `vlan`, `subnet`, `mark`, `auto`. |
| `classId` | `string` | **Yes** | — | Unique HTB minor class identifier on the interface (e.g., `1:100` or `1:280`). Format: `^1:[0-9]+$`. |
| `vlanId` | `integer` | Conditional | — | 802.1Q VLAN tag ID (1–4094). **Required** if `matchType` is `vlan`. |
| `subnet` | `string` | Conditional | — | IPv4 CIDR subnet (e.g., `10.200.0.0/24`). **Required** if `matchType` is `subnet`. |
| `mark` | `uint32` | Conditional | — | 32-bit SKB buffer mark set by OVS or iptables (e.g., `16`). **Required** if `matchType` is `mark`. |
| `egressRate` | `string` | **Yes** | — | Guaranteed outbound bandwidth rate (e.g., `50Mbit`, `1Gbit`). |
| `egressCeil` | `string` | No | `egressRate` | Maximum allowed outbound burst bandwidth ceiling (e.g., `200Mbit`, `10Gbit`). |
| `egressBurst` | `string` | No | `"1250b"` | Outbound burst buffer size (e.g., `15k`, `30k`). |
| `ingressRate` | `string` | No | `""` | Hard policing bandwidth cap for incoming interface traffic (e.g., `25Mbit`). |
| `ingressBurst` | `string` | No | `"100k"` | Incoming policing burst buffer size (e.g., `50k`). |
| `priority` | `integer` | No | `0` | HTB priority and TC filter priority level (1 = Highest Priority, 7 = Lowest Priority). |
| `enableFqCodel` | `boolean` | No | `true` | Toggles attaching an `fq_codel` leaf qdisc to prevent bufferbloat under heavy load. |

### How `matchType: auto` Works

When `matchType: auto` is used (or if `matchType` is left blank), the operator automatically infers the correct classifier module (`flower` vs `fw`) and protocol by inspecting which fields are defined in your class specification.

It evaluates your configuration using a top-down priority cascade:

1. **802.1Q Tag (`vlanId > 0`):** Configures a `cls_flower` **L2 802.1Q VLAN tag filter** (`tc filter ... protocol 802.1Q flower vlan_id <vlanId>`).
2. **IP Subnet (`subnet != ""`):** Configures a `cls_flower` **L3 IP filter** (`tc filter ... protocol ip flower src_ip/dst_ip <subnet>`).
3. **SKB Mark (`mark > 0`):** Configures a `cls_fw` **Firewall Mark filter** (`tc filter ... protocol all handle <mark> fw`).

> **Note:** If `matchType: auto` is set but none of `vlanId`, `subnet`, or `mark` are defined, the agent safely logs a validation warning, skips filter creation for that specific class, and continues processing without crashing.

---

## Full Manifest Example

This complete example demonstrates all four classification strategies (`vlan`, `subnet`, `mark`, and `auto`) configured on a single physical host interface:

```yaml
apiVersion: networking.med.io/v1alpha1
kind: VlanTrafficControl
metadata:
  name: vlan-tc-workers
  namespace: openshift-vlan-tc-operator
spec:
  # Target specific worker nodes via label matching
  nodeSelector:
    node-role.kubernetes.io/worker: ""
  
  # Tolerations allowing host agent execution on master/control-plane nodes
  tolerations:
    - key: "node-role.kubernetes.io/master"
      operator: "Exists"
      effect: "NoSchedule"
    - key: "node-role.kubernetes.io/control-plane"
      operator: "Exists"
      effect: "NoSchedule"
    - key: "node.kubernetes.io/unreachable"
      operator: "Exists"
      effect: "NoExecute"
      tolerationSeconds: 600

  reconcileIntervalSeconds: 60
  tcStrategy: "flower"
  htbRoot:
    interface: "enp1s0"
    htbId: 1
    rate: "10Gbit"
    defaultClassId: "1:99"
    classes:
      # -----------------------------------------------------------------------
      # 1. Standard 802.1Q Tagged VLAN Matching (matchType: vlan)
      # Matches hardware 802.1Q tagged frames traversing the physical interface.
      # -----------------------------------------------------------------------
      - name: "storage-vlan-100"
        matchType: "vlan"
        vlanId: 100
        classId: "1:100"
        priority: 1
        egressRate: "50Mbit"
        egressCeil: "10Gbit"
        egressBurst: "15k"
        ingressRate: "25Mbit"
        ingressBurst: "50k"
        enableFqCodel: true

      # -----------------------------------------------------------------------
      # 2. IP Subnet Matching (matchType: subnet)
      # Essential for OpenShift Virtualization (KubeVirt) VM traffic where VLAN
      # tags are stripped by OVS bridge before reaching the host network interface.
      # -----------------------------------------------------------------------
      - name: "kubevirt-subnet-untagged"
        matchType: "subnet"
        subnet: "10.200.0.0/24"
        classId: "1:280"
        priority: 2
        egressRate: "200Mbit"
        egressCeil: "200Mbit"
        egressBurst: "30k"
        ingressRate: "200Mbit"
        ingressBurst: "30k"
        enableFqCodel: true

      # -----------------------------------------------------------------------
      # 3. Socket Buffer Mark Matching (matchType: mark)
      # Matches traffic marked upstream by OVS, iptables, or nftables flows via cls_fw.
      # -----------------------------------------------------------------------
      - name: "ovs-marked-flow"
        matchType: "mark"
        mark: 16
        classId: "1:380"
        priority: 3
        egressRate: "500Mbit"
        egressCeil: "500Mbit"
        egressBurst: "50k"
        ingressRate: "100Mbit"
        ingressBurst: "20k"
        enableFqCodel: true

      # -----------------------------------------------------------------------
      # 4. Automatic Classification Strategy Inference (matchType: auto)
      # Automatically infers the classification protocol based on specified fields.
      # -----------------------------------------------------------------------
      - name: "auto-detect-vlan-400"
        matchType: "auto"
        vlanId: 400
        classId: "1:400"
        priority: 4
        egressRate: "1Gbit"
        egressCeil: "2Gbit"
        ingressRate: "500Mbit"
        enableFqCodel: false
```

---

## VLAN Traffic Control & Bridge Architecture

This section describes the Traffic Control (TC) configuration, CNI setup, and kernel-level packet flow for managing VM traffic on **VLAN 100** via `br-vlan100` and the host sub-interface `enp1s0.100`.

---

### 1. Custom Resource Definition (CRD) Configuration

Apply the `VlanTrafficControl` CRD to attach HTB egress shaping and ingress policing to the sub-interface `enp1s0.100`:

```yaml
apiVersion: networking.med.io/v1alpha1
kind: VlanTrafficControl
metadata:
  name: vlan-tc-vlan100
spec:
  htbRoot:
    interface: enp1s0.100  # Target host sub-interface
    rate: 10Gbit
    defaultClassId: "1:99"
    htbId: 1
    classes:
    - classId: "1:100"
      name: storage-vlan-100
      priority: 1
      matchType: subnet
      subnet: 10.0.100.0/24
      egressRate: 50Mbit
      egressCeil: 10Gbit
      egressBurst: 15k
      ingressRate: 30Mbit
      ingressBurst: 50k
      enableFqCodel: true
  nodeSelector:
    node-role.kubernetes.io/worker: ""
  reconcileIntervalSeconds: 30
  tcStrategy: flower
```

---

### 2. Multus NetworkAttachmentDefinition (NAD)

The VM connects its secondary interface (`eth1`) to the Linux bridge `br-vlan100` using the Multus Bridge CNI plugin:

```yaml
apiVersion: k8s.cni.cncf.io/v1
kind: NetworkAttachmentDefinition
metadata:
  name: storage-vlan100-nad
  namespace: default   # Set to your VM's target namespace
spec:
  config: |
    {
      "cniVersion": "0.3.1",
      "name": "storage-vlan100-nad",
      "type": "bridge",
      "bridge": "br-vlan100",
      "macspoofchk": false,
      "vlan": 0
    }
```

---

### 3. Verification & Metrics Monitoring

Check the active class counters on the host node using `oc debug` or direct host access:

```bash
# Query egress HTB class counters for class 1:100
oc debug node/<worker-node> -- bash -c 'chroot /host tc -s class show dev enp1s0.100 classid 1:100'

# Query ingress policing counters
oc debug node/<worker-node> -- bash -c 'chroot /host tc -s filter show dev enp1s0.100 parent ffff:'
```

#### Example Command Output

```text
class htb 1:100 parent 1:1 leaf 100: prio 1 rate 50Mbit ceil 10Gbit burst 15Kb cburst 0b
 Sent 109326 bytes 1269 pkt (dropped 0, overlimits 0 requeues 0) 
 backlog 0b 0p requeues 0
 lended: 1269 borrowed: 0 giants: 0
 tokens: 38215 ctokens: 14
```

---

### 4. Host Node Bridge Architecture

The diagram below illustrates how `br-vlan100` switches traffic internally and where Traffic Control hooks into `enp1s0.100`:

```text
+-----------------------------------------------------------------------------------+
| HOST NODE (Linux Kernel)                                                          |
|                                                                                   |
|  +-----------------------------------------------------------------------------+  |
|  | VM Pod (test-vm-vlan100)                                                    |  |
|  |   Interface: eth1 (10.0.100.50)                                             |  |
|  +-----------------------------------+-----------------------------------------+  |
|                                      |                                            |
|                                      | [VM OUTBOUND / HOST INBOUND]               |
|                                      v                                            |
|  +-----------------------------------------------------------------------------+  |
|  | LINUX BRIDGE: br-vlan100  [vlan_filtering=1]                                |  |
|  |                                                                             |  |
|  |   +---------------------------------------------------------------------+   |  |
|  |   | Port: veth1af88ce5 (VM Tap Port)                                    |   |  |
|  |   |   - PVID 100 / Egress Untagged                                      |   |  |
|  |   +----------------------------------+----------------------------------+   |  |
|  |                                      |                                      |  |
|  |                                      | (Kernel Bridge Direct L2 Forwarding) |  |
|  |                                      | * Bypasses br-vlan100 Root Qdisc *   |  |
|  |                                      v                                      |  |
|  |   +---------------------------------------------------------------------+   |  |
|  |   | Port / Device: enp1s0.100 (Host VLAN Sub-Interface)                 |   |  |
|  |   |   - PVID 100 / Egress Untagged                                      |   |  |
|  |   |                                                                     |   |  |
|  |   |   ===============================================================   |   |  |
|  |   |   |  🎯 TC ATTACHMENT POINT FOR OPERATOR                        |   |   |  |
|  |   |   |                                                             |   |   |  |
|  |   |   |  1. EGRESS (Root HTB Qdisc):                                |   |   |  |
|  |   |   |     - Catches VM -> Physical Network traffic                |   |   |  |
|  |   |   |     - Matches: protocol ip src_ip 10.0.100.0/24             |   |   |  |
|  |   |   |     - Shape/Rate-Limit: Class 1:100                         |   |   |  |
|  |   |   |                                                             |   |   |  |
|  |   |   |  2. INGRESS (Ingress Qdisc ffff:):                          |   |   |  |
|  |   |   |     - Catches Physical Network -> VM return traffic         |   |   |  |
|  |   |   |     - Matches: protocol ip dst_ip 10.0.100.0/24             |   |   |  |
|  |   |   |     - Police Action: Rate-limit return flow                 |   |   |  |
|  |   |   ===============================================================   |   |  |
|  |   +----------------------------------+----------------------------------+   |  |
|  +--------------------------------------+--------------------------------------+  |
|                                         |                                         |
|                                         | Frame tagged with 802.1Q (VID 100)      |
|                                         v                                         |
|  +-----------------------------------------------------------------------------+  |
|  | Physical NIC: enp1s0                                                        |  |
|  +-----------------------------------+-----------------------------------------+  |
+--------------------------------------|--------------------------------------------+
                                       |
                                       | 802.1Q Tagged Frame [VLAN 100]
                                       v
                     +-----------------------------------+
                     | Physical Switch / External Router |
                     | (10.0.100.1 Gateway)              |
                     +-----------------------------------+
```

---

### 5. Packet Transformation & Tagging Flow

Because `tc` evaluates packets on `enp1s0.100` before the driver attaches 802.1Q tags on egress (and after removing them on ingress), rules match **`protocol ip`** (`eth_type ipv4`) directly.

### Outbound Flow (VM ➔ Switch)

```text
  [1]  +----------------------------------------------------+
       |  VM POD: test-vm-vlan100 (eth1)                    |
       |  • Packet State: Raw IPv4 Payload                  |
       |  ------------------------------------------------  |
       |  STATUS: [NO TAG] ❌                               |
       +-------------------------+--------------------------+
                                 |
                                 v
  [2]  +----------------------------------------------------+
       |  BRIDGE INGRESS PORT: veth1af88ce5                 |
       |  • PVID 100 sets internal kernel metadata flag     |
       |  • No 802.1Q bytes inserted into frame             |
       |  ------------------------------------------------  |
       |  STATUS: [NO TAG] ❌                               |
       +-------------------------+--------------------------+
                                 |
                                 |  [Bridge forwards packet internally]
                                 v
  [3]  +----------------------------------------------------+
       |  BRIDGE PORT / SUB-INTERFACE: enp1s0.100           |
       |                                                    |
       |  🎯 3a. TC EGRESS ENGINE (Root HTB Qdisc)          |
       |      • Evaluates raw IP payload                    |
       |      • Filter Match: protocol ip src 10.0.100.0/24 |
       |                                                    |
       |  ------------------------------------------------  |
       |  STATUS: [NO TAG] ❌                               |
       +-------------------------+--------------------------+
                                 |
                                 |  [Packet handed down to sub-interface driver]
                                 v
  [4]  +----------------------------------------------------+
       |  SUB-INTERFACE DRIVER & PHYSICAL NIC: enp1s0       |
       |  • Driver injects 4-byte 802.1Q VLAN header        |
       |  • VLAN ID = 100 inserted into packet bytes        |
       |  ------------------------------------------------  |
       |  STATUS: [TAG PRESENT] 🏷️ (VID 100)                |
       +-------------------------+--------------------------+
                                 |
                                 |  [Wire: 802.1Q Tagged Frame]
                                 v
  [5]  +----------------------------------------------------+
       |  PHYSICAL SWITCH / GATEWAY (10.0.100.1)            |
       |  ------------------------------------------------  |
       |  STATUS: [TAG PRESENT] 🏷️ (VID 100)                |
       +----------------------------------------------------+
```

#### Inbound Flow (Switch ➔ VM)

```text
  [5]  +----------------------------------------------------+
       |  PHYSICAL SWITCH / GATEWAY (10.0.100.1)            |
       |  ------------------------------------------------  |
       |  STATUS: [TAG PRESENT] 🏷️ (VID 100)                |
       +-------------------------+--------------------------+
                                 |
                                 |  [Wire: 802.1Q Tagged Frame]
                                 v
  [4]  +----------------------------------------------------+
       |  PHYSICAL NIC: enp1s0                              |
       |  • Receives tagged 802.1Q frame from physical wire |
       |  ------------------------------------------------  |
       |  STATUS: [TAG PRESENT] 🏷️ (VID 100)                |
       +-------------------------+--------------------------+
                                 |
                                 v
  [3]  +----------------------------------------------------+
       |  SUB-INTERFACE DRIVER & SUB-INTERFACE: enp1s0.100  |
       |                                                    |
       |  🎯 3a. SUB-INTERFACE DRIVER                       |
       |      • Strips the 4-byte 802.1Q VLAN header        |
       |      • Hands raw IPv4 payload up to interface      |
       |                                                    |
       |  🎯 3b. TC INGRESS ENGINE (ffff: Police Qdisc)     |
       |      • Evaluates packet AFTER tag removal          |
       |      • Filter Match: protocol ip dst 10.0.100.0/24 |
       |      • Action: Police return bandwidth (30Mbit)    |
       |                                                    |
       |  ------------------------------------------------  |
       |  STATUS: [NO TAG] ❌                               |
       +-------------------------+--------------------------+
                                 |
                                 |  [Bridge forwards untagged packet to VM port]
                                 v
  [2]  +----------------------------------------------------+
       |  BRIDGE EGRESS PORT: veth1af88ce5                  |
       |  • Egress Untagged rule passes plain IP to VM      |
       |  ------------------------------------------------  |
       |  STATUS: [NO TAG] ❌                               |
       +-------------------------+--------------------------+
                                 |
                                 v
  [1]  +----------------------------------------------------+
       |  VM POD: test-vm-vlan100 (eth1)                    |
       |  • Receives plain ICMP reply / payload             |
       |  ------------------------------------------------  |
       |  STATUS: [NO TAG] ❌                               |
       +----------------------------------------------------+
```

---


## MACVLAN (Sub-iface) Traffic Control & Architecture (VLAN Tag Matching)

This section documents the Traffic Control (TC) configuration, Multus MACVLAN CNI setup, and kernel-level packet flow when using a MACVLAN interface where TC matches frames directly by **VLAN ID 100** (`matchType: vlan`).

---

### 1. Custom Resource Definition (CRD) Configuration

In this setup, your CRD uses `matchType: vlan` with `vlanId: 100` to intercept tagged 802.1Q traffic on the physical master interface (`enp1s0`):

```yaml
apiVersion: networking.med.io/v1alpha1
kind: VlanTrafficControl
metadata:
  name: vlan-tc-vlan100-macvlan
spec:
  htbRoot:
    interface: enp1s0  # Master trunk interface
    rate: 10Gbit
    defaultClassId: "1:99"
    htbId: 1
    classes:
    - classId: "1:100"
      name: storage-vlan-100
      priority: 1
      matchType: vlan
      vlanId: 100
      egressRate: 50Mbit
      egressCeil: 10Gbit
      egressBurst: 15k
      ingressRate: 30Mbit
      ingressBurst: 50k
      enableFqCodel: true
  nodeSelector:
    node-role.kubernetes.io/worker: ""
  reconcileIntervalSeconds: 30
  tcStrategy: flower
```

---

### 2. Multus NetworkAttachmentDefinition (NAD)

The VM pod connects its secondary interface directly to the sub-interface `enp1s0.100` using MACVLAN in `bridge` mode:

```yaml
apiVersion: k8s.cni.cncf.io/v1
kind: NetworkAttachmentDefinition
metadata:
  name: vlan100-network
  namespace: default
spec:
  config: '{
      "cniVersion": "0.3.1",
      "name": "vlan100-network",
      "type": "macvlan",
      "master": "enp1s0.100",
      "mode": "bridge",
      "ipam": {}
    }'
```

---

### 3. Verification & Metrics Monitoring

Check the active class counters on `enp1s0` for Class `1:100`:

```bash
# Query HTB class counters for VLAN 100 on enp1s0
oc debug node/<worker-node> -- bash -c 'chroot /host tc -s class show dev enp1s0 classid 1:100'
```

#### Example Command Output

```text
class htb 1:100 parent 1:1 leaf 100: prio 1 rate 50Mbit ceil 10Gbit burst 15Kb cburst 0b
 Sent 109326 bytes 1269 pkt (dropped 0, overlimits 0 requeues 0) 
 backlog 0b 0p requeues 0
 lended: 1269 borrowed: 0 giants: 0
 tokens: 38215 ctokens: 14
```

---

### 4. Host Node MACVLAN Architecture (VLAN 100 Tagged)

```text
+-----------------------------------------------------------------------------------+
| HOST NODE (Linux Kernel)                                                          |
|                                                                                   |
|  +-----------------------------------------------------------------------------+  |
|  | VM Pod (test-vm-vlan100)                                                    |  |
|  |   Interface: eth1 (10.0.100.50) / macvlan0                                  |  |
|  +-----------------------------------+-----------------------------------------+  |
|                                      |                                            |
|                                      | [Direct MACVLAN Untagged Payload]          |
|                                      v                                            |
|  +-----------------------------------------------------------------------------+  |
|  | Host Sub-Interface: enp1s0.100 (Sub-interface driver attaches 802.1Q tag)   |  |
|  +-----------------------------------+-----------------------------------------+  |
|                                      |                                            |
|                                      | Frame tagged with 802.1Q (VID 100)         |
|                                      v                                            |
|  +-----------------------------------------------------------------------------+  |
|  | Physical Trunk NIC: enp1s0                                                  |  |
|  |                                                                             |  |
|  |   ===============================================================           |  |
|  |   |  🎯 TC ATTACHMENT POINT FOR OPERATOR                        |           |  |
|  |   |                                                             |           |  |
|  |   |  1. EGRESS (Root HTB Qdisc):                                |           |  |
|  |   |     - Catches VLAN 100 tagged frames on enp1s0              |           |  |
|  |   |     - Matches: protocol 802.1Q vlan_id 100                  |           |  |
|  |   |     - Shape/Rate-Limit: Class 1:100                         |           |  |
|  |   |                                                             |           |  |
|  |   |  2. INGRESS (Ingress Qdisc ffff:):                          |           |  |
|  |   |     - Catches incoming VLAN 100 tagged frames on enp1s0     |           |  |
|  |   |     - Matches: protocol 802.1Q vlan_id 100                  |           |  |
|  |   |     - Police Action: Rate-limit return flow                 |           |  |
|  |   ===============================================================           |  |
|  +-----------------------------------+-----------------------------------------+  |
+--------------------------------------|--------------------------------------------+
                                       |
                                       | 802.1Q Tagged Frame [VLAN 100]
                                       v
                     +-----------------------------------+
                     | Physical Switch / External Router |
                     | (10.0.100.1 Gateway)              |
                     +-----------------------------------+
```

---

### 5. Packet Flow & Tag Status

#### Outbound Flow (VM ➔ Switch)

```text
  [1]  +----------------------------------------------------+
       |  VM POD: test-vm-vlan100 (macvlan0 / eth1)         |
       |  • Packet State: Raw Untagged IPv4 Payload         |
       |  ------------------------------------------------  |
       |  STATUS: [NO TAG] ❌                               |
       +-------------------------+--------------------------+
                                 |
                                 v
  [2]  +----------------------------------------------------+
       |  MACVLAN SUB-INTERFACE: enp1s0.100                 |
       |  • Driver injects 4-byte 802.1Q header             |
       |  • Encapsulates payload with VLAN ID = 100         |
       |  ------------------------------------------------  |
       |  STATUS: [TAG PRESENT] 🏷️ (VID 100)                |
       +-------------------------+--------------------------+
                                 |
                                 v
  [3]  +----------------------------------------------------+
       |  PHYSICAL TRUNK NIC: enp1s0                        |
       |                                                    |
       |  🎯 3a. TC EGRESS ENGINE (Root HTB Qdisc)          |
       |      • Evaluates 802.1Q tagged frame               |
       |      • Filter Match: protocol 802.1Q vlan_id 100   |
       |      • Action: Rate-limit to Class 1:100           |
       |  ------------------------------------------------  |
       |  STATUS: [TAG PRESENT] 🏷️ (VID 100)                |
       +-------------------------+--------------------------+
                                 |
                                 |  [Wire: 802.1Q Tagged Frame]
                                 v
  [4]  +----------------------------------------------------+
       |  PHYSICAL SWITCH / GATEWAY (10.0.100.1)            |
       |  ------------------------------------------------  |
       |  STATUS: [TAG PRESENT] 🏷️ (VID 100)                |
       +----------------------------------------------------+
```

#### Inbound Flow (Switch ➔ VM)

```text
  [4]  +----------------------------------------------------+
       |  PHYSICAL SWITCH / GATEWAY (10.0.100.1)            |
       |  ------------------------------------------------  |
       |  STATUS: [TAG PRESENT] 🏷️ (VID 100)                |
       +-------------------------+--------------------------+
                                 |
                                 |  [Wire: 802.1Q Tagged Frame]
                                 v
  [3]  +----------------------------------------------------+
       |  PHYSICAL TRUNK NIC: enp1s0                        |
       |                                                    |
       |  🎯 3a. TC INGRESS ENGINE (ffff: Police Qdisc)     |
       |      • Evaluates incoming 802.1Q tagged frame      |
       |      • Filter Match: protocol 802.1Q vlan_id 100   |
       |      • Action: Police return bandwidth (30Mbit)    |
       |  ------------------------------------------------  |
       |  STATUS: [TAG PRESENT] 🏷️ (VID 100)                |
       +-------------------------+--------------------------+
                                 |
                                 v
  [2]  +----------------------------------------------------+
       |  MACVLAN SUB-INTERFACE: enp1s0.100                 |
       |  • Strips 4-byte 802.1Q header (VID 100)           |
       |  • Passes untagged payload up to MACVLAN slave     |
       |  ------------------------------------------------  |
       |  STATUS: [NO TAG] ❌                               |
       +-------------------------+--------------------------+
                                 |
                                 v
  [1]  +----------------------------------------------------+
       |  VM POD: test-vm-vlan100 (macvlan0 / eth1)         |
       |  • Receives plain ICMP reply / payload             |
       |  ------------------------------------------------  |
       |  STATUS: [NO TAG] ❌                               |
       +----------------------------------------------------+
```

---

## MACVLAN (Physical) & Architecture [ PENDING ]

---

## Open vSwitch (OVS) / OVN-Kubernetes Integration & Architecture [ PENDING ]

---

## IPvLAN (L2 / L3 Mode) & Architecture [ PENDING ]

---

## SR-IOV (Single Root I/O Virtualization) & Architecture [ HW REQUIRED ]

- HW reconfiguration required:

   ~~~
   [root@rbruzzon-platinum vlan-traffic-control-operator]# for iface in /sys/class/net/*; do
    if [ -f "$iface/device/sriov_totalvfs" ]; then
        echo "$(basename $iface) SUPPORTS SR-IOV! Max VFs: $(cat $iface/device/sriov_totalvfs)"
    fi
   done
   
   eno1 SUPPORTS SR-IOV! Max VFs: 32
   eno2 SUPPORTS SR-IOV! Max VFs: 32
   ~~~

---

## OpenShift Cluster Metrics & Ingress Filter Observability

This section details how the `vlan-traffic-control-agent` DaemonSet collects real-time Traffic Control (TC) telemetry across all OpenShift worker nodes, exposes structured egress (including HTB priority levels, default class statistics, and bandwidth borrowing) and ingress bandwidth metrics, and maps kernel netlink filter stats directly back to `VlanTrafficControl` Custom Resources.

---

### Key Capabilities

* **Native Netlink Engine:** Replaces shell subprocess calls with direct kernel socket inspection (`vishvananda/netlink`) for high-performance telemetry collection without CLI execution overhead.
* **Unified Telemetry Schema:** Merges egress bandwidth queue statistics (`prio`, `bytes`, `packets`, `overlimits`, `borrowed`) and ingress rate-policing drop counters (`bytes`, `packets`, `drops`) into a single API payload.
* **Default Class Telemetry:** Automatically reports metrics for the default fallback class (e.g. `1:99` / `default-fallback`), capturing unclassified host traffic.
* **HTB Priority & Borrowing Tracking:** Surfaces class priority (`prio`) and `borrowed` token counters when a class exceeds its guaranteed `rate` and consumes spare root capacity up to its `ceil`.
* **CRD Metadata Mapping:** Automatically correlates kernel handles (`classId` `1:100`, `filterId` `pref 100`) with custom human-readable class names defined in the `VlanTrafficControl` CRD.
* **Granular Filtering:** Supports target filtering by specific **VLAN Tag ID** (`?vlan=100`) or **TC Class Handle** (`?classId=1:100`) to isolate specific tenant or application traffic.

---

### Agent Observability Endpoints

Each agent pod exposes an HTTP telemetry interface on port `8080`:

| Endpoint | Method | Query Parameters | Description |
| :--- | :--- | :--- | :--- |
| `/stats` | `GET` | `interface` *(required)*, `vlan` *(optional)*, `classId` *(optional)* | Fetches structured egress (with prio, default class & borrowing) and ingress TC metrics via Netlink sockets. |
| `/reconcile` | `POST` | *None* | Triggers an immediate local TC rule reconciliation pass on the node. |
| `/cleanup` | `DELETE` / `POST` | `interface` *(required)* | Flushes root HTB and ingress policing qdiscs on the node. |
| `/healthz` | `GET` | *None* | Liveness probe endpoint. |

---

### Querying Telemetry

#### 1. Query All Worker Node Telemetry (via Operator Manager)
Run this command from inside the cluster manager pod to inspect telemetry across all worker nodes:

```bash
for pod_ip in $(oc get pods -n openshift-vlan-tc-operator -l app=vlan-traffic-control-agent -o jsonpath='{.items[*].status.podIP}'); do
  oc exec -n openshift-vlan-tc-operator deploy/vlan-traffic-control-manager -- \
    curl -s "http://${pod_ip}:8080/stats?interface=enp1s0" | jq .
done
```

#### 2. Query Single VLAN or TC Rule
Isolate telemetry for a specific VLAN ID (e.g., VLAN 100) or class ID (e.g., `1:100`):

```bash
# Filter stats for VLAN 100
curl -s "http://${agent_pod_ip}:8080/stats?interface=enp1s0&vlan=100" | jq .

# Filter stats by Class ID handle
curl -s "http://${agent_pod_ip}:8080/stats?interface=enp1s0&classId=1:100" | jq .
```

---

### Sample Telemetry Payload (`/stats`)

```json
{
  "interface": "enp1s0",
  "node": "hub-worker03.ocp4-hub.test.com",
  "classStats": [
    {
      "classId": "1:99",
      "name": "default-fallback",
      "prio": 0,
      "bytes": 54210,
      "packets": 412,
      "overlimits": 0,
      "borrowed": 0
    },
    {
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
      "classId": "1:100",
      "filterId": "pref 100",
      "bytes": 842100,
      "packets": 5930,
      "drops": 0
    }
  ]
}
```

---

yes !

Here is the updated Node Configuration & Alignment Engine section for your README.md, now updated to include the missing host interface drift scenario alongside the fully aligned state example.

Markdown
## Node Configuration & Alignment Engine

This section details how the `vlan-traffic-control-agent` DaemonSet performs real-time drift detection and configuration auditing across OpenShift worker nodes. By comparing live kernel qdisc, class, and filter states retrieved via Netlink sockets (`vishvananda/netlink`) against the aggregated target specifications from `VlanTrafficControl` Custom Resources, the engine provides immediate visibility into node configuration alignment and pinpoints specific parameter discrepancies.

---

### Key Capabilities

* **Deterministic Drift Analysis:** Computes a strict boolean alignment state (`isAligned: true|false`) by matching live kernel socket parameters against the expected CRD specification matrix.
* **Missing Host Interface Detection:** Automatically flags targeted network interfaces (`br-vlan100`, `enp1s0.100`) that are absent on specific worker nodes, generating clear drift deltas rather than crashing or returning false positives.
* **Polymorphic Filter Evaluation:** Dynamically evaluates all active kernel classifier types (`Flower`, `U32`, `fw` skb-mark filters, and `GenericFilter`) via Netlink priority handles to eliminate false-negative drift reports when matching skb marks vs VLAN IDs.
* **Filter Engine Transparency:** Reports the exact kernel classifier module (`fw`, `flower`, `u32`) and protocol ID fulfilling each active ingress policy (`ingressFilters`).
* **Qdisc Existence Audit:** Verifies the presence of both the root HTB qdisc (`1:`) and ingress policing qdisc (`ffff:`) on the target host interface (`htbQdiscPresent`, `ingressPresent`).
* **Delta Discrepancy Reporting:** Returns a detailed list of configuration deltas (`driftDeltas`) identifying missing host devices, missing egress classes, orphan qdiscs, missing ingress policing filters, or mismatched TC priorities (`priority`).
* **Multi-CRD Spec Aggregation:** Dynamically merges all active `VlanTrafficControl` resources targeting a given node interface based on `nodeSelector` matching.
* **Targeted Partial Auditing:** Supports querying alignment for a single class handle (`?classId=1:380`) or VLAN ID to isolate tenant configuration drift without auditing the entire interface hierarchy.
* **Non-Disruptive Inspection:** Evaluates alignment in-memory using lightweight Netlink socket calls without mutating existing kernel TC structures or blocking data-path traffic.

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
# Audit alignment for TC Class 1:380
curl -s "http://${agent_pod_ip}:8080/config?interface=enp1s0&classId=1:380" | jq .
```

---

### Sample Configuration Alignment Payload (`/config`)

#### 1. Fully Aligned State Example:

```json
{
  "node": "hub-worker01.ocp4-hub.test.com",
  "interface": "enp1s0",
  "isAligned": true,
  "desired": {
    "interface": "enp1s0",
    "rate": "10Gbit",
    "classes": [
      {
        "name": "ovs-marked-flow",
        "classId": "1:380",
        "matchType": "mark",
        "mark": 16,
        "egressRate": "500Mbit",
        "egressCeil": "500Mbit",
        "egressBurst": "50k",
        "ingressRate": "100Mbit",
        "ingressBurst": "20k",
        "priority": 3,
        "enableFqCodel": true
      },
      {
        "name": "raw-htb-no-fqcodel",
        "classId": "1:400",
        "matchType": "auto",
        "vlanId": 400,
        "egressRate": "1Gbit",
        "egressCeil": "2Gbit",
        "ingressRate": "500Mbit",
        "priority": 4,
        "enableFqCodel": false
      }
    ]
  },
  "actual": {
    "htbQdiscPresent": true,
    "ingressPresent": true,
    "classes": [
      { "classId": "1:99" },
      { "classId": "1:1" },
      { "classId": "1:380", "priority": 3 },
      { "classId": "1:400", "priority": 4 }
    ],
    "ingressFilters": [
      {
        "priority": 3,
        "type": "fw",
        "protocol": 3
      },
      {
        "priority": 4,
        "type": "flower",
        "protocol": 33024
      }
    ]
  },
  "driftDeltas": []
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

### Understanding Alignment Drift Statuses (`driftDeltas`)

When a node is misaligned (`isAligned: false`), the `driftDeltas` array provides a granular audit log breaking down the exact mismatch between the `VlanTrafficControl` CRD specification and the live Linux kernel Netlink state.

Each entry in `driftDeltas` follows a structured 4-field schema:

```json
{
  "targetHandle": "interface br-vlan100",
  "property": "existence",
  "expected": "present on host",
  "actual": "missing device"
}
```

* **`targetHandle`**: Identifies the specific host interface, TC class ID, or filter handle being evaluated.
* **`property`**: The exact parameter or state check under inspection (`existence`, `priority`, `rate`, `ceil`).
* **`expected`**: The state or value dictated by the `VlanTrafficControl` CRD specification.
* **`actual`**: The live state or value retrieved directly from the kernel Netlink socket.

---

### Reference Matrix: Drift Delta Combinations & Causes

The table below describes all possible status combinations emitted by the alignment engine during cluster audits:

#### 1. Interface & Qdisc Existence Errors

| Target Handle | Property | Expected | Actual | Description & Root Cause |
| :--- | :--- | :--- | :--- | :--- |
| `interface <iface>` | `existence` | `present on host` | `missing device` | Netlink returned `LinkNotFoundError`. The host network bridge or sub-interface does not exist on this worker node. |
| `qdisc root` | `existence` | `htb` | `missing` | The HTB root qdisc (`1:`) is missing from the interface on the host. |
| `qdisc ingress` | `existence` | `ingress` | `missing` | The ingress policing qdisc (`ffff:`) was flushed or omitted on the target host interface. |

#### 2. Egress HTB Class Errors

| Target Handle | Property | Expected | Actual | Description & Root Cause |
| :--- | :--- | :--- | :--- | :--- |
| `class <handle>` *(e.g. `class 1:100`)* | `existence` | `configured` | `missing` | The egress HTB class handle exists in the CRD spec but was not created in the kernel. |
| `class <handle>` | `existence` | `configured` | `missing (interface <iface> absent)` | Cascading failure reported when an HTB class cannot be verified because the host interface itself is absent. |
| `class <handle>` | `priority` | `<expected_prio>` *(e.g. `1`)* | `<actual_prio>` *(e.g. `3`)* | The class exists in the kernel, but its priority (`prio`) diverges from the CRD spec. |
| `class <handle>` | `rate` | `<expected_rate>` *(e.g. `50Mbit`)* | `<actual_rate>` *(e.g. `10Mbit`)* | The configured egress committed rate differs from the live Netlink state. |
| `class <handle>` | `ceil` | `<expected_ceil>` *(e.g. `10Gbit`)* | `<actual_ceil>` *(e.g. `1Gbit`)* | The configured maximum ceiling rate differs from the live Netlink state. |

#### 3. Ingress Filter & Classifier Errors

| Target Handle | Property | Expected | Actual | Description & Root Cause |
| :--- | :--- | :--- | :--- | :--- |
| `ingress filter pref <prio>` *(e.g. `pref 3`)* | `existence` | `configured` | `missing` | The ingress policing filter (`fw`, `flower`, or `u32`) associated with this priority handle is missing. |
| `egress filter pref <prio>` | `existence` | `configured` | `missing` | The egress classifier steering traffic into the HTB class handle is missing in the kernel. |
| `ingress filter pref <prio>` | `rate` | `<expected_police>` *(e.g. `30Mbit`)* | `<actual_police>` *(e.g. `10Mbit`)* | The policing action drop threshold in the kernel does not match the CRD `ingressRate`. |

---

### Recommended Remediation Actions

1. **If an interface is localized (not present on all workers):**  
   Restrict the CRD `nodeSelector` so the operator targets only nodes where the physical interface or bridge is provisioned:

```yaml
spec:
  nodeSelector:
    node-role.kubernetes.io/storage-node: ""
```

2. **If an interface should exist cluster-wide:**  
   Inspect your OpenShift **NMState** or **NodeNetworkConfigurationPolicy (NNCP)** manifests to bring up the missing interface across all worker nodes.

3. **If a class/filter is missing on an active interface:**  
   Trigger an instant agent re-reconciliation to re-apply the desired Linux kernel TC qdiscs and filters without waiting for the periodic reconcile loop:

   * **Trigger Re-reconciliation across ALL Agent Pods in the Cluster:**
     ```bash
     for pod_ip in $(oc get pods -n openshift-vlan-tc-operator -l app=vlan-traffic-control-agent -o jsonpath='{.items[*].status.podIP}'); do
       echo "Triggering reconcile on Agent IP: ${pod_ip}"
       oc exec -n openshift-vlan-tc-operator deploy/vlan-traffic-control-manager -- \
         curl -s -X POST "http://${pod_ip}:8080/reconcile?interface=enp1s0" | jq .
     done
     ```

   * **Trigger Re-reconciliation on a Specific Node/Agent:**
     ```bash
     # Optionally specify ?interface=<iface> to target a single interface rebuild
     curl -X POST "http://${agent_pod_ip}:8080/reconcile?interface=enp1s0"
     ```
---

## Troubleshooting & Error Code Analysis

This section provides diagnostic procedures, common error code resolutions, and step-by-step troubleshooting workflows for the `VlanTrafficControl` operator, agent DaemonSet, and host-level Traffic Control (TC) subsystem.

---

### 1. Diagnostics Flowchart

When bandwidth shaping or ingress policing fails to apply, follow this diagnostic progression:

```text
+-------------------------------------------------------------+
| 1. Check Custom Resource Status                             |
|    oc get vlantrafficcontrol -o yaml                        |
+------------------------------+------------------------------+
                               |
            +------------------+------------------+
            |                                     |
            v                                     v
   [ Status: Ready ]                     [ Status: Degraded/Failed ]
            |                                     |
            v                                     v
+-----------------------+             +-----------------------+
| 2. Inspect Node Agent |             | Read Condition Message|
|    oc logs -n ...     |             | Check Netlink Error   |
|    -l app=agent       |             | (See Table Below)     |
+-----------+-----------+             +-----------------------+
            |
            v
+-------------------------------------------------------------+
| 3. Direct Host Kernel Verification                          |
|    oc debug node/<node> -- chroot /host tc -s qdisc show ...|
+-------------------------------------------------------------+
```

---

### 2. Common TC Netlink & Kernel Error Codes

When the `vlan-traffic-control-agent` attempts to program HTB qdiscs or `tc-flower` filters via netlink sockets, kernel errors return standard Linux `errno` codes.

| Error Code | Kernel Symbol | Root Cause | Resolution Strategy |
| :--- | :--- | :--- | :--- |
| **`exit status 2` / `ENOENT`** | `No such file or directory` | Target network interface (e.g., `enp1s0.100`) does not exist on the node. | Verify that NMState or NetworkManager has created the VLAN sub-interface before applying the CRD. |
| **`EEXIST` (-17)** | `File exists` | Attempting to create an HTB root qdisc (`1:`) or filter handle that is already bound to the interface. | The operator must perform an idempotent `replace` operation (`RTM_NEWQDISC` with `NLM_F_REPLACE`) instead of `create`. |
| **`EINVAL` (-22)** | `Invalid argument` | Invalid TC parameter combination (e.g., `rate` exceeds physical link maximum, or misconfigured quantum/burst). | Validate CR values. Ensure `egressBurst` is non-zero and `htbRoot.rate` is a valid `tc` unit string (`Mbit`, `Gbit`). |
| **`ENOMEM` (-12)** | `Cannot allocate memory` | PCIe MMIO memory BAR allocation failed when attempting to spawn SR-IOV Virtual Functions. | Add `pci=realloc` and `intel_iommu=on` to the host GRUB parameters and reboot the host node. |
| **`EOPNOTSUPP` (-95)**| `Operation not supported` | Hardware offload (`tcStrategy: flower` with `hw_offload`) requested on a NIC driver without switchdev support. | Set `tcStrategy` to software mode or verify SmartNIC driver capabilities (`ethtool -k <iface> \| grep hw-tc-offload`). |
| **`EBUSY` (-16)** | `Device or resource busy` | Attempting to delete a root HTB qdisc while secondary class queues are active or locked by another CNI. | Flush filters (`tc filter del ...`) before deleting parent class IDs (`tc class del ...`). |

---

### 3. Common Failure Scenarios & Troubleshooting Steps

#### Scenario A: Ingress Metrics Show 0 Bytes / Zero Packets

* **Symptom:** Egress metrics update correctly in `/stats`, but `ingressStats` counters remain at `0`.
* **Root Cause:** Ingress filters applied to a VLAN sub-interface (`enp1s0.100`) are incorrectly matching `protocol 802.1Q vlan_id 100` instead of `protocol ip`.
* **Verification:**
  ```bash
  oc debug node/<worker-node> -- bash -c 'chroot /host tc -s filter show dev enp1s0.100 parent ffff:'
  ```
* **Fix:** Because the kernel VLAN driver strips 802.1Q tags before hitting the sub-interface ingress qdisc, update the ingress rule to match plain IP (`matchType: subnet` / `protocol ip`).

---

#### Scenario B: Ingress Filters Unidentifiable in API (`filterId: "ffff:"`)

* **Symptom:** `/stats` output returns duplicate `filterId: "ffff:"` strings without mapping back to CRD classes.
* **Root Cause:** The agent reads the qdisc parent ID (`ffff:`) instead of parsing the netlink filter cookie or preference ID (`pref`).
* **Fix:** Ensure the operator attaches a unique netlink cookie (`TCA_COOKIE`) corresponding to the `classId` (e.g., `1:100`) when creating the filter rule.

---

#### Scenario C: Traffic Bypassing HTB Shaper on Linux Bridge

* **Symptom:** VM egress bandwidth exceeds the configured `egressRate` limit.
* **Root Cause:** TC root qdisc is attached to `br-vlan100` instead of `enp1s0.100`. Linux bridge internal L2 switching bypasses the bridge interface root qdisc.
* **Verification:** Check where the root HTB qdisc is attached:
  ```bash
  oc debug node/<worker-node> -- bash -c 'chroot /host tc qdisc show dev br-vlan100'
  ```
* **Fix:** Update `VlanTrafficControl` spec to target the egress port device (`interface: enp1s0.100`).

---

### 4. Useful Debug Commands Reference

```bash
# 1. View active HTB class hierarchy and operational rates
oc debug node/<node> -- bash -c 'chroot /host tc -s class show dev enp1s0.100'

# 2. View all ingress policing filters and packet drop counters
oc debug node/<node> -- bash -c 'chroot /host tc -s filter show dev enp1s0.100 parent ffff:'

# 3. Stream real-time netlink TC events from kernel
oc debug node/<node> -- bash -c 'chroot /host tc monitor'

# 4. Check operator agent logs for netlink reconciliation errors
oc logs -n openshift-vlan-tc-operator -l app=vlan-traffic-control-agent --tail=100
```

---

## Basic test run results

Notes:
   - DaemonSet Spec Hash Change: a DaemonSet relies on a single workload template (.spec.template). Editing this specification (such as adding a toleration for master node scheduling) causes the cluster to update the underlying PodTemplateSpec generation hash.
   - Rolling Update Strategy: The controller identifies that every running pod configuration no longer matches the newly generated PodTemplateSpec. Even if a worker node pod remains functionally unaffected by the new toleration, the corresponding spec hash is marked as outdated.
   - Sequential Rollout: A rolling restart is triggered across every node in the cluster. Pods are terminated and recreated one node at a time—beginning on control plane nodes and continuing sequentially across worker nodes—until every instance aligns with the updated template version.

### Selector & Toleration Verification Suite [ PASSED ✅ ]

```bash
$ ./test-selectors-and-tolerations.sh 
========================================================================
🧪 Starting Dedicated Selector & Toleration Verification Suite
Target Interface: enp1s0.100
Master Verification Node: hub-master01.ocp4-hub.test.com
Worker Verification Node: hub-worker01.ocp4-hub.test.com
========================================================================
🧹 Deleting all existing cluster VlanTrafficControl CRs...
vlantrafficcontrol.networking.med.io "test-with-toleration" deleted
ext Flushing host TC interface enp1s0.100 across all active agent pods...

▶ TEST 1: Testing 'nodeSelector' map targeting (Workers only)...
vlantrafficcontrol.networking.med.io/test-nodeselector-worker created

------------------------------------------------------------------------
🔍 [TEST CASE] Test 1: nodeSelector map (Workers only)
Expected User TC Classes -> Workers: 1 | Masters: 0
------------------------------------------------------------------------
--> Waiting for agent DaemonSet to synchronize...
Waiting for daemon set "vlan-traffic-control-agent" rollout to finish: 0 of 6 updated pods are available...
Waiting for daemon set "vlan-traffic-control-agent" rollout to finish: 1 of 6 updated pods are available...
Waiting for daemon set "vlan-traffic-control-agent" rollout to finish: 2 of 6 updated pods are available...
Waiting for daemon set "vlan-traffic-control-agent" rollout to finish: 3 of 6 updated pods are available...
Waiting for daemon set "vlan-traffic-control-agent" rollout to finish: 4 of 6 updated pods are available...
Waiting for daemon set "vlan-traffic-control-agent" rollout to finish: 5 of 6 updated pods are available...
daemon set "vlan-traffic-control-agent" successfully rolled out
--> Querying Agent API /stats endpoint on target nodes...
    - Agent [hub-worker01.ocp4-hub.test.com]: 1 user class(es) active
    - Agent [hub-master01.ocp4-hub.test.com]: 0 user class(es) active
--> Verifying Host Linux Kernel ('tc class show dev enp1s0.100')...
    - Host Kernel [hub-master01.ocp4-hub.test.com]: 00 user HTB class(es)
    - Host Kernel [hub-worker01.ocp4-hub.test.com]: 1 user HTB class(es)
✅ TEST PASSED: State matches expectations!
vlantrafficcontrol.networking.med.io "test-nodeselector-worker" deleted

▶ TEST 2: Testing missing 'tolerations' on tainted Master nodes...
vlantrafficcontrol.networking.med.io/test-missing-toleration created

------------------------------------------------------------------------
🔍 [TEST CASE] Test 2: Missing tolerations (Master nodes skip CR)
Expected User TC Classes -> Workers: 0 | Masters: 0
------------------------------------------------------------------------
--> Waiting for agent DaemonSet to synchronize...
Waiting for daemon set "vlan-traffic-control-agent" rollout to finish: 0 of 6 updated pods are available...
Waiting for daemon set "vlan-traffic-control-agent" rollout to finish: 1 of 6 updated pods are available...
Waiting for daemon set "vlan-traffic-control-agent" rollout to finish: 2 of 6 updated pods are available...
Waiting for daemon set "vlan-traffic-control-agent" rollout to finish: 3 of 6 updated pods are available...
Waiting for daemon set "vlan-traffic-control-agent" rollout to finish: 4 of 6 updated pods are available...
Waiting for daemon set "vlan-traffic-control-agent" rollout to finish: 5 of 6 updated pods are available...
daemon set "vlan-traffic-control-agent" successfully rolled out
--> Querying Agent API /stats endpoint on target nodes...
    - Agent [hub-worker01.ocp4-hub.test.com]: 0 user class(es) active
    - Agent [hub-master01.ocp4-hub.test.com]: 0 user class(es) active
--> Verifying Host Linux Kernel ('tc class show dev enp1s0.100')...
    - Host Kernel [hub-master01.ocp4-hub.test.com]: 00 user HTB class(es)
    - Host Kernel [hub-worker01.ocp4-hub.test.com]: 00 user HTB class(es)
✅ TEST PASSED: State matches expectations!
vlantrafficcontrol.networking.med.io "test-missing-toleration" deleted

▶ TEST 3: Testing 'nodeSelector' WITH correct 'tolerations' on Master nodes...
vlantrafficcontrol.networking.med.io/test-with-toleration created

------------------------------------------------------------------------
🔍 [TEST CASE] Test 3: Correct toleration applied (Master nodes active)
Expected User TC Classes -> Workers: 0 | Masters: 1
------------------------------------------------------------------------
--> Waiting for agent DaemonSet to synchronize...
Waiting for daemon set "vlan-traffic-control-agent" rollout to finish: 4 out of 6 new pods have been updated...
Waiting for daemon set "vlan-traffic-control-agent" rollout to finish: 0 of 6 updated pods are available...
Waiting for daemon set "vlan-traffic-control-agent" rollout to finish: 1 of 6 updated pods are available...
Waiting for daemon set "vlan-traffic-control-agent" rollout to finish: 2 of 6 updated pods are available...
Waiting for daemon set "vlan-traffic-control-agent" rollout to finish: 3 of 6 updated pods are available...
Waiting for daemon set "vlan-traffic-control-agent" rollout to finish: 4 of 6 updated pods are available...
Waiting for daemon set "vlan-traffic-control-agent" rollout to finish: 5 of 6 updated pods are available...
daemon set "vlan-traffic-control-agent" successfully rolled out
--> Querying Agent API /stats endpoint on target nodes...
    - Agent [hub-worker01.ocp4-hub.test.com]: 0 user class(es) active
    - Agent [hub-master01.ocp4-hub.test.com]: 1 user class(es) active
--> Verifying Host Linux Kernel ('tc class show dev enp1s0.100')...
    - Host Kernel [hub-master01.ocp4-hub.test.com]: 1 user HTB class(es)
    - Host Kernel [hub-worker01.ocp4-hub.test.com]: 00 user HTB class(es)
✅ TEST PASSED: State matches expectations!
vlantrafficcontrol.networking.med.io "test-with-toleration" deleted

▶ TEST 4: Testing 'NodeLabelSelector.matchLabels'...
vlantrafficcontrol.networking.med.io/test-matchlabels created

------------------------------------------------------------------------
🔍 [TEST CASE] Test 4: NodeLabelSelector matchLabels (Worker nodes active)
Expected User TC Classes -> Workers: 1 | Masters: 0
------------------------------------------------------------------------
--> Waiting for agent DaemonSet to synchronize...
Waiting for daemon set "vlan-traffic-control-agent" rollout to finish: 0 of 6 updated pods are available...
Waiting for daemon set "vlan-traffic-control-agent" rollout to finish: 1 of 6 updated pods are available...
Waiting for daemon set "vlan-traffic-control-agent" rollout to finish: 2 of 6 updated pods are available...
Waiting for daemon set "vlan-traffic-control-agent" rollout to finish: 3 of 6 updated pods are available...
Waiting for daemon set "vlan-traffic-control-agent" rollout to finish: 4 of 6 updated pods are available...
Waiting for daemon set "vlan-traffic-control-agent" rollout to finish: 5 of 6 updated pods are available...
daemon set "vlan-traffic-control-agent" successfully rolled out
--> Querying Agent API /stats endpoint on target nodes...
    - Agent [hub-worker01.ocp4-hub.test.com]: 1 user class(es) active
    - Agent [hub-master01.ocp4-hub.test.com]: 0 user class(es) active
--> Verifying Host Linux Kernel ('tc class show dev enp1s0.100')...
    - Host Kernel [hub-master01.ocp4-hub.test.com]: 00 user HTB class(es)
    - Host Kernel [hub-worker01.ocp4-hub.test.com]: 1 user HTB class(es)
✅ TEST PASSED: State matches expectations!
vlantrafficcontrol.networking.med.io "test-matchlabels" deleted

▶ TEST 5: Testing 'NodeLabelSelector.matchExpressions' (Operator: Exists)...
vlantrafficcontrol.networking.med.io/test-matchexpressions created

------------------------------------------------------------------------
🔍 [TEST CASE] Test 5: NodeLabelSelector matchExpressions (Master nodes active)
Expected User TC Classes -> Workers: 0 | Masters: 1
------------------------------------------------------------------------
--> Waiting for agent DaemonSet to synchronize...
Waiting for daemon set spec update to be observed...
Waiting for daemon set "vlan-traffic-control-agent" rollout to finish: 0 of 6 updated pods are available...
Waiting for daemon set "vlan-traffic-control-agent" rollout to finish: 1 of 6 updated pods are available...
Waiting for daemon set "vlan-traffic-control-agent" rollout to finish: 2 of 6 updated pods are available...
Waiting for daemon set "vlan-traffic-control-agent" rollout to finish: 3 of 6 updated pods are available...
Waiting for daemon set "vlan-traffic-control-agent" rollout to finish: 4 of 6 updated pods are available...
Waiting for daemon set "vlan-traffic-control-agent" rollout to finish: 5 of 6 updated pods are available...
daemon set "vlan-traffic-control-agent" successfully rolled out
--> Querying Agent API /stats endpoint on target nodes...
    - Agent [hub-worker01.ocp4-hub.test.com]: 0 user class(es) active
    - Agent [hub-master01.ocp4-hub.test.com]: 1 user class(es) active
--> Verifying Host Linux Kernel ('tc class show dev enp1s0.100')...
    - Host Kernel [hub-master01.ocp4-hub.test.com]: 1 user HTB class(es)
    - Host Kernel [hub-worker01.ocp4-hub.test.com]: 00 user HTB class(es)
✅ TEST PASSED: State matches expectations!
vlantrafficcontrol.networking.med.io "test-matchexpressions" deleted

▶ TEST 6: Testing custom node label matching ('group=traffic-test')...
node/hub-worker01.ocp4-hub.test.com labeled
vlantrafficcontrol.networking.med.io/test-custom-label created

------------------------------------------------------------------------
🔍 [TEST CASE] Test 6: Custom label match (group=traffic-test on single worker)
Expected User TC Classes -> Workers: 1 | Masters: 0
------------------------------------------------------------------------
--> Waiting for agent DaemonSet to synchronize...
Waiting for daemon set spec update to be observed...
Waiting for daemon set "vlan-traffic-control-agent" rollout to finish: 5 out of 6 new pods have been updated...
Waiting for daemon set "vlan-traffic-control-agent" rollout to finish: 0 of 6 updated pods are available...
Waiting for daemon set "vlan-traffic-control-agent" rollout to finish: 1 of 6 updated pods are available...
Waiting for daemon set "vlan-traffic-control-agent" rollout to finish: 2 of 6 updated pods are available...
Waiting for daemon set "vlan-traffic-control-agent" rollout to finish: 3 of 6 updated pods are available...
Waiting for daemon set "vlan-traffic-control-agent" rollout to finish: 4 of 6 updated pods are available...
Waiting for daemon set "vlan-traffic-control-agent" rollout to finish: 5 of 6 updated pods are available...
daemon set "vlan-traffic-control-agent" successfully rolled out
--> Querying Agent API /stats endpoint on target nodes...
    - Agent [hub-worker01.ocp4-hub.test.com]: 1 user class(es) active
    - Agent [hub-master01.ocp4-hub.test.com]: 0 user class(es) active
--> Verifying Host Linux Kernel ('tc class show dev enp1s0.100')...
    - Host Kernel [hub-master01.ocp4-hub.test.com]: 00 user HTB class(es)
    - Host Kernel [hub-worker01.ocp4-hub.test.com]: 1 user HTB class(es)
✅ TEST PASSED: State matches expectations!
node/hub-worker01.ocp4-hub.test.com unlabeled
vlantrafficcontrol.networking.med.io "test-custom-label" deleted

========================================================================
🎉 SUCCESS: All capability tests passed verification cleanly!
========================================================================
```

---

### TC targeting sequence [ FIX IN PROGRESS ❌ ]
```bash
[rbruzzon@rbruzzon-platinum Simple_Tests]$ ./test-tc-targeting-sequence.sh 
========================================================================
🚀 Starting Comprehensive 9-Step TC Rule Lifecycle Verification Suite
Interface Target: enp1s0.100
Master Verification Node: hub-master01.ocp4-hub.test.com
Worker Verification Node: hub-worker01.ocp4-hub.test.com
========================================================================
🧹 Deleting all existing cluster VlanTrafficControl CRs...
vlantrafficcontrol.networking.med.io "rule-all-1" deleted
vlantrafficcontrol.networking.med.io "rule-all-2" deleted
vlantrafficcontrol.networking.med.io "rule-master-3" deleted

▶ STEP 1: Applying 'rule-all-1' (No node restrictions, targeting ALL nodes)...
vlantrafficcontrol.networking.med.io/rule-all-1 created

------------------------------------------------------------------------
🔍 [VERIFICATION] Step 1: First TC rule added to ALL nodes
Expected User TC Classes -> Workers: 1 | Masters: 1
------------------------------------------------------------------------
--> Querying Agent API /stats endpoint...
    - Node [hub-worker02.ocp4-hub.test.com]: 1 user class(es) active
    - Node [hub-worker01.ocp4-hub.test.com]: 1 user class(es) active
    - Node [hub-worker03.ocp4-hub.test.com]: 1 user class(es) active
    - Node [hub-master02.ocp4-hub.test.com]: 1 user class(es) active
    - Node [hub-master03.ocp4-hub.test.com]: 1 user class(es) active
    - Node [hub-master01.ocp4-hub.test.com]: 1 user class(es) active
--> Kernel Verification ('tc class show dev enp1s0.100')...
    - Host Kernel [hub-worker01.ocp4-hub.test.com]: 1 user HTB class(es)
    - Host Kernel [hub-master01.ocp4-hub.test.com]: 1 user HTB class(es)
✅ TEST STEP PASSED!

▶ STEP 2: Applying 'rule-all-2' (NodeLabelSelector.matchLabels targeting all)...
vlantrafficcontrol.networking.med.io/rule-all-2 created

------------------------------------------------------------------------
🔍 [VERIFICATION] Step 2: Second TC rule added to ALL nodes
Expected User TC Classes -> Workers: 2 | Masters: 2
------------------------------------------------------------------------
--> Querying Agent API /stats endpoint...
    - Node [hub-worker02.ocp4-hub.test.com]: 2 user class(es) active
    - Node [hub-worker01.ocp4-hub.test.com]: 2 user class(es) active
    - Node [hub-worker03.ocp4-hub.test.com]: 2 user class(es) active
    - Node [hub-master02.ocp4-hub.test.com]: 2 user class(es) active
    - Node [hub-master03.ocp4-hub.test.com]: 2 user class(es) active
    - Node [hub-master01.ocp4-hub.test.com]: 2 user class(es) active
--> Kernel Verification ('tc class show dev enp1s0.100')...
    - Host Kernel [hub-worker01.ocp4-hub.test.com]: 2 user HTB class(es)
    - Host Kernel [hub-master01.ocp4-hub.test.com]: 2 user HTB class(es)
✅ TEST STEP PASSED!

▶ STEP 3: Applying 'rule-master-3' (nodeSelector & tolerations for Masters)...
vlantrafficcontrol.networking.med.io/rule-master-3 created

------------------------------------------------------------------------
🔍 [VERIFICATION] Step 3: Third TC rule added to MASTER nodes only
Expected User TC Classes -> Workers: 2 | Masters: 3
------------------------------------------------------------------------
--> Querying Agent API /stats endpoint...
    - Node [hub-worker02.ocp4-hub.test.com]: 2 user class(es) active
    - Node [hub-worker01.ocp4-hub.test.com]: 2 user class(es) active
    - Node [hub-worker03.ocp4-hub.test.com]: 2 user class(es) active
    - Node [hub-master02.ocp4-hub.test.com]: 3 user class(es) active
    - Node [hub-master03.ocp4-hub.test.com]: 3 user class(es) active
    - Node [hub-master01.ocp4-hub.test.com]: 3 user class(es) active
--> Kernel Verification ('tc class show dev enp1s0.100')...
    - Host Kernel [hub-worker01.ocp4-hub.test.com]: 2 user HTB class(es)
    - Host Kernel [hub-master01.ocp4-hub.test.com]: 3 user HTB class(es)
✅ TEST STEP PASSED!

▶ STEP 4: Restricting 'rule-all-1' to target Master nodes exclusively...
vlantrafficcontrol.networking.med.io/rule-all-1 configured

------------------------------------------------------------------------
🔍 [VERIFICATION] Step 4: First TC rule removed from WORKER nodes only
Expected User TC Classes -> Workers: 1 | Masters: 3
------------------------------------------------------------------------
--> Querying Agent API /stats endpoint...
    - Node [hub-worker02.ocp4-hub.test.com]: 1 user class(es) active
    - Node [hub-worker01.ocp4-hub.test.com]: 1 user class(es) active
    - Node [hub-worker03.ocp4-hub.test.com]: 1 user class(es) active
    - Node [hub-master02.ocp4-hub.test.com]: 3 user class(es) active
    - Node [hub-master03.ocp4-hub.test.com]: 3 user class(es) active
    - Node [hub-master01.ocp4-hub.test.com]: 3 user class(es) active
--> Kernel Verification ('tc class show dev enp1s0.100')...
    - Host Kernel [hub-worker01.ocp4-hub.test.com]: 1 user HTB class(es)
    - Host Kernel [hub-master01.ocp4-hub.test.com]: 3 user HTB class(es)
✅ TEST STEP PASSED!

▶ STEP 5: Restricting 'rule-all-2' nodeLabelSelector to target Master nodes exclusively...
vlantrafficcontrol.networking.med.io/rule-all-2 configured

------------------------------------------------------------------------
🔍 [VERIFICATION] Step 5: Second TC rule removed from WORKERS (Worker interface flushed)
Expected User TC Classes -> Workers: 0 | Masters: 3
------------------------------------------------------------------------
--> Querying Agent API /stats endpoint...
    - Node [hub-worker02.ocp4-hub.test.com]: 0 user class(es) active
    - Node [hub-worker01.ocp4-hub.test.com]: 0 user class(es) active
    - Node [hub-worker03.ocp4-hub.test.com]: 0 user class(es) active
    - Node [hub-master02.ocp4-hub.test.com]: 3 user class(es) active
    - Node [hub-master03.ocp4-hub.test.com]: 3 user class(es) active
    - Node [hub-master01.ocp4-hub.test.com]: 3 user class(es) active
--> Kernel Verification ('tc class show dev enp1s0.100')...
    - Host Kernel [hub-worker01.ocp4-hub.test.com]: 0 user HTB class(es)
    - Host Kernel [hub-master01.ocp4-hub.test.com]: 3 user HTB class(es)
--> Verifying Root HTB Qdisc ('1:') deletion on WORKER nodes...
    ✓ Root HTB qdisc '1:' successfully FLUSHED from hub-worker01.ocp4-hub.test.com
✅ TEST STEP PASSED!

▶ STEP 6: Re-applying 'rule-all-1' to target ALL nodes...
vlantrafficcontrol.networking.med.io/rule-all-1 configured

------------------------------------------------------------------------
🔍 [VERIFICATION] Step 6: First TC rule re-added to ALL nodes
Expected User TC Classes -> Workers: 1 | Masters: 3
------------------------------------------------------------------------
--> Querying Agent API /stats endpoint...
    - Node [hub-worker02.ocp4-hub.test.com]: 1 user class(es) active
    - Node [hub-worker01.ocp4-hub.test.com]: 1 user class(es) active
    - Node [hub-worker03.ocp4-hub.test.com]: 1 user class(es) active
    - Node [hub-master02.ocp4-hub.test.com]: 3 user class(es) active
    - Node [hub-master03.ocp4-hub.test.com]: 3 user class(es) active
    - Node [hub-master01.ocp4-hub.test.com]: 3 user class(es) active
--> Kernel Verification ('tc class show dev enp1s0.100')...
    - Host Kernel [hub-worker01.ocp4-hub.test.com]: 1 user HTB class(es)
    - Host Kernel [hub-master01.ocp4-hub.test.com]: 3 user HTB class(es)
✅ TEST STEP PASSED!

▶ STEP 7: Re-applying 'rule-all-2' to target ALL nodes...
vlantrafficcontrol.networking.med.io/rule-all-2 configured

------------------------------------------------------------------------
🔍 [VERIFICATION] Step 7: Second TC rule re-added to ALL nodes
Expected User TC Classes -> Workers: 2 | Masters: 3
------------------------------------------------------------------------
--> Querying Agent API /stats endpoint...
    - Node [hub-worker02.ocp4-hub.test.com]: 2 user class(es) active
    - Node [hub-worker01.ocp4-hub.test.com]: 2 user class(es) active
    - Node [hub-worker03.ocp4-hub.test.com]: 2 user class(es) active
    - Node [hub-master02.ocp4-hub.test.com]: 3 user class(es) active
    - Node [hub-master03.ocp4-hub.test.com]: 3 user class(es) active
    - Node [hub-master01.ocp4-hub.test.com]: 3 user class(es) active
--> Kernel Verification ('tc class show dev enp1s0.100')...
    - Host Kernel [hub-worker01.ocp4-hub.test.com]: 2 user HTB class(es)
    - Host Kernel [hub-master01.ocp4-hub.test.com]: 3 user HTB class(es)
✅ TEST STEP PASSED!

▶ STEP 8: Applying 'rule-worker-3' (NodeLabelSelector.matchExpressions Exists for Workers)...
vlantrafficcontrol.networking.med.io/rule-worker-3 created

------------------------------------------------------------------------
🔍 [VERIFICATION] Step 8: Third TC rule added to WORKER nodes only
Expected User TC Classes -> Workers: 3 | Masters: 3
------------------------------------------------------------------------
--> Querying Agent API /stats endpoint...
    - Node [hub-worker02.ocp4-hub.test.com]: 3 user class(es) active
    - Node [hub-worker01.ocp4-hub.test.com]: 3 user class(es) active
    - Node [hub-worker03.ocp4-hub.test.com]: 3 user class(es) active
    - Node [hub-master02.ocp4-hub.test.com]: 3 user class(es) active
    - Node [hub-master03.ocp4-hub.test.com]: 3 user class(es) active
    - Node [hub-master01.ocp4-hub.test.com]: 3 user class(es) active
--> Kernel Verification ('tc class show dev enp1s0.100')...
    - Host Kernel [hub-worker01.ocp4-hub.test.com]: 3 user HTB class(es)
    - Host Kernel [hub-master01.ocp4-hub.test.com]: 3 user HTB class(es)
✅ TEST STEP PASSED!

▶ STEP 9: Removing all TC rules from MASTER nodes...
vlantrafficcontrol.networking.med.io "rule-master-3" deleted
vlantrafficcontrol.networking.med.io/rule-all-1 configured
vlantrafficcontrol.networking.med.io/rule-all-2 configured

------------------------------------------------------------------------
🔍 [VERIFICATION] Step 9: All TC rules removed from MASTER nodes (Master interface flushed)
Expected User TC Classes -> Workers: 3 | Masters: 0
------------------------------------------------------------------------
--> Querying Agent API /stats endpoint...
    - Node [hub-worker02.ocp4-hub.test.com]: 3 user class(es) active
    - Node [hub-worker01.ocp4-hub.test.com]: 3 user class(es) active
    - Node [hub-worker03.ocp4-hub.test.com]: 3 user class(es) active
    - Node [hub-master02.ocp4-hub.test.com]: 0 user class(es) active
    - Node [hub-master03.ocp4-hub.test.com]: 0 user class(es) active
    - Node [hub-master01.ocp4-hub.test.com]: 0 user class(es) active
--> Kernel Verification ('tc class show dev enp1s0.100')...
    - Host Kernel [hub-worker01.ocp4-hub.test.com]: 3 user HTB class(es)
    - Host Kernel [hub-master01.ocp4-hub.test.com]: 0 user HTB class(es)
--> Verifying Root HTB Qdisc ('1:') deletion on MASTER nodes...
    ✓ Root HTB qdisc '1:' successfully FLUSHED from hub-master01.ocp4-hub.test.com
✅ TEST STEP PASSED!

🧹 Final Cleanup: Deleting all created test CRs...
vlantrafficcontrol.networking.med.io "rule-all-1" deleted
vlantrafficcontrol.networking.med.io "rule-all-2" deleted
vlantrafficcontrol.networking.med.io "rule-worker-3" deleted

========================================================================
🎉 SUCCESS: All 9 targeting and deletion steps passed perfectly!
========================================================================
```

---

### Host Node Bridge - TC rules validations with OCP VMs

- Environment: 
   - host-node-bridge_nncp-worker01.yaml
   - host-node-bridge_nncp-worker02.yaml
   - host-node-bridge_nncp-worker03.yaml
   - host-node-bridge_nads.yaml
   - host-node-bridge_vlan-tc-rules.yaml
   - host-node-bridge_vms.yaml (access via ssh key)

- Basic Script:
   - run-tc-validation.sh

```bash
========================================================================
🧪 EXECUTING FULL MULTI-VLAN ISOLATION & TC VALIDATION SUITE
========================================================================

========================================================================
🧪 Testing Isolation on VLAN 100 (enp1s0.100 -> Class 1:100)
========================================================================
--> Initial Packet Counters:
    • enp1s0.100 (Class 1:100): 380 pkts
    • enp1s0.280 (Class 1:280): 263 pkts
    • enp1s0.380 (Class 1:380): 289 pkts

--> Injecting 200 ICMP packets from vm-vlan100 (10.0.100.202) to 10.0.100.21...

--> Final Packet Counters & Isolation Verification:
    ✅ enp1s0.100 [TARGET]: +221 packets (Expected traffic increase)
    ✅ enp1s0.280 [ISOLATED]: +0 packets (Perfect isolation)
    ✅ enp1s0.380 [ISOLATED]: +0 packets (Perfect isolation)

🎉 RESULT: VLAN 100 PASSED isolation & TC verification!

========================================================================
🧪 Testing Isolation on VLAN 280 (enp1s0.280 -> Class 1:280)
========================================================================
--> Initial Packet Counters:
    • enp1s0.100 (Class 1:100): 601 pkts
    • enp1s0.280 (Class 1:280): 263 pkts
    • enp1s0.380 (Class 1:380): 289 pkts

--> Injecting 200 ICMP packets from vm-vlan280 (10.0.218.202) to 10.0.218.21...

--> Final Packet Counters & Isolation Verification:
    ✅ enp1s0.100 [ISOLATED]: +0 packets (Perfect isolation)
    ✅ enp1s0.280 [TARGET]: +221 packets (Expected traffic increase)
    ✅ enp1s0.380 [ISOLATED]: +0 packets (Perfect isolation)

🎉 RESULT: VLAN 280 PASSED isolation & TC verification!

========================================================================
🧪 Testing Isolation on VLAN 380 (enp1s0.380 -> Class 1:380)
========================================================================
--> Initial Packet Counters:
    • enp1s0.100 (Class 1:100): 601 pkts
    • enp1s0.280 (Class 1:280): 484 pkts
    • enp1s0.380 (Class 1:380): 289 pkts

--> Injecting 200 ICMP packets from vm-vlan380 (10.0.238.202) to 10.0.238.21...

--> Final Packet Counters & Isolation Verification:
    ✅ enp1s0.100 [ISOLATED]: +0 packets (Perfect isolation)
    ✅ enp1s0.280 [ISOLATED]: +0 packets (Perfect isolation)
    ✅ enp1s0.380 [TARGET]: +221 packets (Expected traffic increase)

🎉 RESULT: VLAN 380 PASSED isolation & TC verification!

========================================================================
🏁 ALL ISOLATION & TC TESTS COMPLETED
========================================================================
```

### Host Node Bridge - Borrowed PKTS

- Environment: 
   - host-node-bridge_nncp-worker01.yaml
   - host-node-bridge_nncp-worker02.yaml
   - host-node-bridge_nncp-worker03.yaml
   - host-node-bridge_nads.yaml
   - host-node-bridge_borrow_vlan-tc-rules.yaml
   - host-node-bridge_vms.yaml (access via ssh key)

- Basic Script:
   - host-node-bridge_monitor-only.sh


HTB regulates traffic using two primary thresholds per class:

* **`egressRate`**: The guaranteed base bandwidth allocated to the VLAN class.
* **`egressCeil`**: The maximum upper limit (ceiling) the VLAN class is allowed to reach.

- Parent 1:1 root class rate: 5Mbit
   -  ClassId: "1:100": egressRate: 1Mbit - egressCeil: 5Mbit [ Borrowing enabled ]
   -  ClassId: "1:280": egressRate: 1Mbit - egressCeil: 1Mbit [ No borrowing permitted ]
   -  ClassId: "1:380": egressRate: 1Mbit - egressCeil: 1Mbit [ No borrowing permitted ]


#### Key Operational Rules

| Condition | Token Borrowing Status | Kernel Behavior |
| :--- | :--- | :--- |
| **`Traffic > egressRate`** | **Active (`borrowed > 0`)** | The class borrows unused tokens from the root parent (`htbRoot.rate`) to scale above its guaranteed rate. |
| **`Traffic > egressCeil`** | **Throttled (`overlimits > 0`)** | Packets exceeding the ceiling rate are delayed/queued by the kernel. Overlimits counters increment rapidly. |
| **`egressRate == egressCeil`** | **Disabled (`borrowed = 0`)** | The class cannot borrow from the parent root under any circumstances. Traffic above `egressRate` is immediately rate-limited. |

---

### 📊 Real-Time Metric Monitoring

```bash
-----------------------------------------------------------------------------------------------------------------------------------
TIMESTAMP  | INTERFACE    | CLASS    | TOTAL PKTS | BORROWED PKTS | BORROW %   | DELTA BORROWED | OVERLIMITS | DELTA OVERLIMITS
-----------------------------------------------------------------------------------------------------------------------------------
21:09:18   | enp1s0.100   | 1:100    | 95969      | 70312         | 73.3%      | +3291          | 89395      | +4179          
21:09:18   | enp1s0.280   | 1:280    | 20630      | 0             | 0.0%       | +0             | 17855      | +895           
21:09:18   | enp1s0.380   | 1:380    | 20494      | 0             | 0.0%       | +0             | 16825      | +674           
-----------------------------------------------------------------------------------------------------------------------------------
21:09:26   | enp1s0.100   | 1:100    | 99059      | 72661         | 73.4%      | +2349          | 92382      | +2987          
21:09:26   | enp1s0.280   | 1:280    | 21290      | 0             | 0.0%       | +0             | 18508      | +653           
21:09:26   | enp1s0.380   | 1:380    | 21139      | 0             | 0.0%       | +0             | 17454      | +629           
-----------------------------------------------------------------------------------------------------------------------------------
21:09:35   | enp1s0.100   | 1:100    | 102509     | 75268         | 73.4%      | +2607          | 95714      | +3332          
21:09:35   | enp1s0.280   | 1:280    | 22031      | 0             | 0.0%       | +0             | 19238      | +730           
21:09:35   | enp1s0.380   | 1:380    | 21844      | 0             | 0.0%       | +0             | 18136      | +682           
-----------------------------------------------------------------------------------------------------------------------------------
21:09:44   | enp1s0.100   | 1:100    | 106023     | 77952         | 73.5%      | +2684          | 99125      | +3411          
21:09:44   | enp1s0.280   | 1:280    | 22781      | 0             | 0.0%       | +0             | 19978      | +740           
21:09:44   | enp1s0.380   | 1:380    | 22664      | 0             | 0.0%       | +0             | 18942      | +806           
-----------------------------------------------------------------------------------------------------------------------------------
```

