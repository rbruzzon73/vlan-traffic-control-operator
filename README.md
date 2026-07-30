## Solution Overview & Technical Architecture

**VLAN Traffic Control Operator** is an operator built to deliver fine-grained, declarative Quality of Service (QoS) and host-level network traffic shaping. 

Standard Kubernetes bandwidth plugins are often limited to basic pod-level ingress/egress rate limiting and fail to address non-pod traffic, secondary Multus interfaces, Open vSwitch (OVS) bridges, or hardware-stripped VLAN tags. This operator bridges that gap by allowing cluster administrators to manage Linux Traffic Control (`tc`) queueing disciplines, classifiers, and rate limiters natively across worker nodes using standard OpenShift Custom Resources.

---

### Core Architecture & Component Workflow

The operator follows a dual-component architecture consisting of a cluster-wide **Controller Manager** and host-bound **Node Agents**:

~~~
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
|  |                          Worker Node (DaemonSet)                            |  |
|  |                                                                             |  |
|  |  +------------------------+        chroot /host     +--------------------+  |  |
|  |  |   vlan-tc-agent Pod    | ----------------------> | Host OS Network    |  |  |
|  |  | (HTTP API & Reconciler)|                         | Namespace (tc)     |  |  |
|  |  +-----------+------------+                         +---------+----------+  |  |
|  |              |                                                |             |  |
|  |              | Fetches /stats                                 | Configures  |  |
|  |              v                                                v             |  |
|  |       +--------------+                              +--------------------+  |  |
|  |       | Structured   |                              | HTB Qdisc, Flower  |  |  |
|  |       | JSON Metrics |                              | & Ingress Police   |  |  |
|  |       +--------------+                              +--------------------+  |  |
|  +-----------------------------------------------------------------------------+  |
+-----------------------------------------------------------------------------------+
~~~

#### 1. Controller Manager (`cmd/manager`)
* Watches `VlanTrafficControl` Custom Resources cluster-wide.
* Filters target worker nodes using configurable `nodeSelector` rules.
* Coordinates reconciliation loops across node agents and aggregates cluster-wide operational conditions into the resource `status`.

#### 2. Host Node Agent (`cmd/agent`)
* Runs as a privileged `DaemonSet` across targeted worker nodes.
* Executes in the host network namespace (`chroot /host`) to manipulate host network interfaces directly (`enp1s0`, `br-ex`, bond interfaces).
* Verifies and auto-loads required Linux kernel modules (`sch_htb`, `cls_flower`, `act_police`).
* Exposes a structured REST API (`/stats`, `/reconcile`, `/cleanup`, `/healthz`) for health probes, manual triggers, and real-time packet/byte metric collection.

---

### Key Capabilities & Traffic Control Features

#### Flexible Multi-Match Classification
Traffic identification goes beyond standard 802.1Q VLAN tags. The operator supports four distinct classification strategies to handle complex container and virtual machine networking topologies:

* **802.1Q VLAN Tag (`matchType: vlan`):** Direct matching on 802.1Q tagged frames traversing physical trunk interfaces or bond devices.
* **IP Subnet / CIDR (`matchType: subnet`):** Essential for OpenShift Virtualization (KubeVirt) or OVS bridge interfaces where VLAN tags are stripped prior to hitting the host Linux stack. Matches on source IP (`src_ip`) for egress and destination IP (`dst_ip`) for ingress.
* **Socket Buffer Mark (`matchType: mark`):** Matches on 32-bit `skbmark` values set upstream by Open vSwitch flows, `iptables`, or `nftables`.
* **Auto-Detection (`matchType: auto`):** Dynamically inspects class attributes and automatically selects the optimal classification protocol.

#### Hierarchy Token Bucket (HTB) & Traffic Shaping
* **Guaranteed Egress Bandwidth (`egressRate`):** Guarantees minimum outbound bandwidth allocation per traffic class under heavy contention.
* **Burst Ceilings (`egressCeil`):** Limits maximum burst rate capacity when excess root interface bandwidth is available.
* **Ingress Rate Policing (`ingressRate` & `ingressBurst`):** Enforces hard bandwidth caps on incoming interface traffic using kernel `act_police` drop filters on the `ingress` (`ffff:`) qdisc.
* **Priority Queuing (`priority`):** Assigns HTB priority bands (0–7) to ensure latency-sensitive control traffic or storage networks pre-empt bulk data flows.

#### Active Queue Management (AQM)
* **Bufferbloat Prevention (`enableFqCodel`):** Automatically attaches `fq_codel` (Fair Queueing Controlled Delay) leaf qdiscs beneath HTB classes to minimize queue latency and prevent TCP bufferbloat under maximum throughput conditions.

---

### Target Use Cases

* **Shared Interface & Live-Migration Protection:** On hyperconverged host interfaces shared across OpenShift control plane services, OpenStack/KubeVirt VM networks, and storage VLANs, high-burst operations like **VM live migrations** can saturate physical links and starve sensitive control traffic. By assigning strict priority bands (`priority: 0`) and rate ceilings, you ensure critical services like **ETCD heartbeat/consensus traffic** remain pre-empted and latency-protected during heavy virtual machine migrations.
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
| `tcStrategy` | `string` | **Yes** | `"flower"` | Traffic control execution strategy. Valid values: `flower`, `u32`, `auto`. |
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
| `ingressBurst` | `string` | No | `""` | Incoming policing burst buffer size (e.g., `50k`). |
| `priority` | `integer` | No | `0` | HTB priority level (0 = Highest Priority, 7 = Lowest Priority). |
| `enableFqCodel` | `boolean` | No | `true` | Toggles attaching an `fq_codel` leaf qdisc to prevent bufferbloat under heavy load. |

### How `matchType: auto` Works

When `matchType: auto` is used (or if `matchType` is left blank), the operator automatically infers the correct packet filtering method by inspecting which fields are defined in your class specification. 

It evaluates your configuration using a top-down priority cascade:

1. **802.1Q Tag (`vlanId > 0`):** If a `vlanId` is present, it configures an **L2 802.1Q VLAN tag filter** (`tc filter ... protocol 802.1Q flower vlan_id <vlanId>`).
2. **IP Subnet (`subnet != ""`):** If no `vlanId` exists but a `subnet` CIDR is set, it configures an **L3 IP filter** (`tc filter ... protocol ip flower src_ip/dst_ip <subnet>`). This is ideal for OpenShift Virtualization (KubeVirt) VM bridged networks where VLAN tags are stripped prior to hitting the host interface.
3. **SKB Mark (`mark > 0`):** If neither `vlanId` nor `subnet` is set, it matches on the **Socket Buffer mark** (`tc filter ... protocol all flower mark <mark>`) set upstream by OVS or firewall rules.

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
      # Matches traffic marked upstream by OVS, iptables, or nftables flows.
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
