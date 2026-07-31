## Solution Overview & Technical Architecture

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
  ├── 1. Modules Loader (pkg/executor/modules.go) ──> Loads sch_htb, cls_flower, cls_fw
  ├── 2. Classifier Resolver (ResolveClassifier)  ──> Maps vlan/subnet/mark criteria
  ├── 3. TC Engine (pkg/executor/tc.go)           ──> Executes chroot /host tc commands
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

The `VlanTrafficControl` Custom Resource (`networking.med.io/vlan-traffic-control`) defines the desired traffic shaping state.

### `VlanTrafficControlSpec` (`spec`)

| Field | Type | Required | Default | Description |
| :--- | :--- | :---: | :---: | :--- |
| `nodeSelector` | `map[string]string` | No | `{}` | Map of node labels used to select target worker nodes (e.g., `node-role.kubernetes.io/worker: ""`). |
| `reconcileIntervalSeconds` | `integer` | No | `30` | Interval in seconds between node agent reconciliation loops. |
| `tcStrategy` | `string` | **Yes** | `"flower"` | Default traffic control strategy execution mode. Valid values: `flower`, `u32`, `auto`. |
| `htbRoot` | `HtbRootSpec` | **Yes** | — | Root HTB and interface configuration. |

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
  nodeSelector:
    node-role.kubernetes.io/worker: ""
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
