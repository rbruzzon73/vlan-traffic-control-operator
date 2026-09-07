# `VlanTrafficControl` Operator Verification Suite
## Test Suite Specification (`test-vtc-strict-suite.sh`)

This document defines the 15 end-to-end scenarios executed by `test-vtc-strict-suite.sh` to validate CRD parsing, Linux kernel Traffic Control (`tc`) alignment, IFB redirection, multi-interface/multi-`htbId` isolation, self-healing, and resilience.

---

### Scenario Summary Matrix

| ID | Title | Primary Focus | Strategy | Targeted Interface(s) |
| :--- | :--- | :--- | :--- | :--- |
| **S1** | Egress Only (No IFB) | Pure egress shaping & `fq_codel` attached leaf qdiscs | `flower` | `enp1s0` |
| **S2** | Ingress Only (No IFB) | Stateless TC flower policing filters (`police rate ... conform-exceed drop`) | `flower` | `enp1s0` |
| **S3** | Combined Egress + Ingress | Simultaneous ingress policing and egress HTB shaping in a single VTC | `flower` | `enp1s0` |
| **S4** | IFB Egress Only | Egress HTB shaping with IFB strategy active | `ifb` | `enp1s0` |
| **S5** | IFB Ingress Only | Ingress redirect filter (`mirred redirect dev ifb-enp1s0`) and IFB device creation | `ifb` | `enp1s0` $\rightarrow$ `ifb-enp1s0` |
| **S6** | Combined Egress + Ingress IFB | Full dual-direction HTB shaping across physical and virtual IFB interfaces | `ifb` | `enp1s0` & `ifb-enp1s0` |
| **S7** | IFB Teardown & Device Cleanup | IFB device destruction and netlink teardown verification on VTC CR deletion | `ifb` | `enp1s0` & `ifb-enp1s0` |
| **S8** | Dual VTC Multi-`htbId` | Independent non-default root handles (`htbId: 1` vs `htbId: 2`) across physical and bridge links | `flower` | `enp1s0` (`1:`) & `br-vlan380` (`2:`) |
| **S9** | Full CRD Parameter Matrix | Complete parameter coverage (`htbId: 10`, `defaultClassId`, `mark` match, `fq_codel`, custom bursts) | `flower` | `enp1s0` (`10:`) |
| **S10** | Kernel Drift & Self-Healing | Operator recovery after manual host kernel corruption (`tc qdisc del dev enp1s0 root`) | `flower` | `enp1s0` |
| **S11** | Live CR Mutation | Real-time propagation of spec mutations (updating rates, adding new classes) | `flower` | `enp1s0` |
| **S12** | Control Plane Node Isolation | `nodeSelector` / taint isolation ensuring master nodes remain untouched | `flower` | Master (`hub-master01`) vs Worker |
| **S13** | Malformed Spec Safety | Crash-prevention when invalid CR specs (empty `subnet`) are submitted | `flower` | `enp1s0` |
| **S14** | Pod Restart Persistence | State persistence and zero-drift across agent pod container restarts | `flower` | `enp1s0` |
| **S15** | Concurrent Race Condition | Mutex & netlink safety during rapid asynchronous CR creation/deletion | `flower` | `enp1s0` |

---

### Detailed Test Case Specifications

#### **S1: Egress Only - No IFB (`S1_EGRESS_NOIFB`)**
* **Applied CR Spec Parameters:**
  * `tcStrategy`: `flower`
  * `htbRoot.interface`: `enp1s0`
  * `htbRoot.htbId`: `1`
  * `htbRoot.rate`: `10Gbit`
  * `htbRoot.defaultClassId`: `"1:99"`
  * `classes[]`: Egress rates (`100Mbit`–`2Gbit`), egress ceils (`2Gbit`–`8Gbit`), egress bursts (`20k`–`50k`), `enableFqCodel: true`, match types: `subnet` (`10.0.100.0/24`), `vlan` (`280`, `380`).
* **Assertions Checked:**
  * REST `/config`: `isAligned: true`, `driftDeltas: []`, `actual.htbQdiscPresent: true`.
  * REST `/stats`: Verifies all `classStats[]` have `direction: "egress"` and `interface: "enp1s0"`.
  * Kube API `VlanTrafficControlClass`: Verifies projected CRDs reflect `direction: egress`, `guaranteed: 2Gbit`, and `ceilBorrow: 8Gbit`.
  * Teardown: `tc qdisc del` removes root HTB `1:` on CR deletion.

#### **S2: Ingress Only - No IFB (`S2_INGRESS_NOIFB`)**
* **Applied CR Spec Parameters:**
  * `tcStrategy`: `flower`
  * `classes[]`: `ingressRate` (`3Gbit`, `1.5Gbit`, `10Gbit`), `ingressBurst` (`40k`), `ingressAction`: `drop`, priority: `1`–`3`.
* **Assertions Checked:**
  * REST `/config`: Verifies stateless filter action matches `police rate 3Gbit burst 40k conform-exceed drop`.
  * Kernel `tc`: Confirms ingress qdisc `ffff:` is created on `enp1s0` with `cls_flower` filters attached directly to the physical interface.

#### **S3: Egress + Ingress Same VTC (`S3_COMBINED_NOIFB`)**
* **Applied CR Spec Parameters:**
  * Combined egress rates (`2Gbit`) and ingress rates (`3Gbit`) on individual class specifications (`vlan-100-high`).
* **Assertions Checked:**
  * Kube API `VlanTrafficControlClass`: Verifies projected class direction reads `direction: "ingress+egress"`.
  * REST `/stats`: Verifies ingress filters and egress classes evaluate simultaneously under handle `1:`.

#### **S4: IFB Strategy Egress Only (`S4_EGRESS_IFB`)**
* **Applied CR Spec Parameters:**
  * `tcStrategy`: `ifb`
  * Egress rates (`1.2Gbit`), egress ceils (`6Gbit`), egress bursts (`35k`).
* **Assertions Checked:**
  * REST `/config`: Validates physical interface `enp1s0` retains HTB root qdisc `1:` while IFB redirection remains inactive when ingress rules are absent.

#### **S5: IFB Strategy Ingress Only (`S5_INGRESS_IFB`)**
* **Applied CR Spec Parameters:**
  * `tcStrategy`: `ifb`
  * `ingressRate` (`1Gbit`), `ingressCeil` (`10Gbit`), `enableFqCodel: true`.
* **Assertions Checked:**
  * Kernel Link: Confirms virtual link `ifb-enp1s0` is created, brought `UP`, and attached.
  * REST `/config`: Checks catch-all redirect filter `mirred redirect dev ifb-enp1s0` on physical interface `ffff:`.
  * REST `/stats`: Verifies ingress `classStats[]` are correctly attributed to interface `ifb-enp1s0`.

#### **S6: IFB Strategy Combined Egress + Ingress (`S6_COMBINED_IFB`)**
* **Applied CR Spec Parameters:**
  * `tcStrategy`: `ifb`
  * Full dual-direction shaping: egress (`2Gbit`/`5Gbit`), ingress (`3Gbit`/`10Gbit`), ingress action `htb classify`.
* **Assertions Checked:**
  * REST `/stats`: Checks separation of stats labels—`direction: egress` mapped to `enp1s0`, `direction: ingress` mapped to `ifb-enp1s0`.
  * Purge Check: Verifies ghost physical ingress stats count is `0`.

#### **S7: IFB Teardown & Device Destruction (`S7_IFB_CLEANUP_TEARDOWN`)**
* **Applied CR Spec Parameters:**
  * Temporary IFB VTC CR created and immediately deleted.
* **Assertions Checked:**
  * Host Kernel: Executes `ip link show dev ifb-enp1s0` and asserts output returns `Device "ifb-enp1s0" does not exist`.

#### **S8: Dual VTC Multi-`htbId` (`S8_DUAL_VTC_MULTI_HTBID`)**
* **Applied CR Spec Parameters:**
  * CR 1 (`enp1s0`): `htbId: 1`, `rate: 10Gbit`, `classes`: `1:100`, `1:380`.
  * CR 2 (`br-vlan380`): `htbId: 2`, `classes`: `2:100` (`2Gbit`), `2:380` (`10Gbit`), `ingressAction: pass`.
* **Assertions Checked:**
  * Cross-Interface Scoping: Queries `/config?interface=br-vlan380` and confirms `desired.htbId == 2`.
  * Class ID Resolution: Verifies ingress class mapping for priority 3 on `br-vlan380` resolves strictly to `2:380` (preventing fallback collisions to `1:99`).
  * Action Check: Validates `police rate 10Gbit burst 256Mb conform-exceed pass`.

#### **S9: Full Parameter Matrix (`S9_FULL_PARAMETER_MATRIX`)**
* **Applied CR Spec Parameters:**
  * `reconcileIntervalSeconds`: `15`
  * `htbRoot.htbId`: `10`
  * `htbRoot.defaultClassId`: `"10:88"`
  * `classes[]`: `matchType: mark` (`mark: 105`), `matchType: subnet`, `enableFqCodel: true`, egress rate `1Gbit`, egress ceil `4Gbit`, egress burst `32Mb`, ingress rate `2Gbit`, ingress action `pass`.
* **Assertions Checked:**
  * REST `/config`: Verifies root handle `10:` is queried in netlink.
  * Kernel `tc`: Validates `qdisc htb 10:`, `qdisc fq_codel 88:` attached to leaf class `10:88`, and `fw` mark filter handle `105`.

#### **S10: Kernel Drift & Self-Healing (`S10_KERNEL_DRIFT_SELF_HEALING`)**
* **Applied CR Spec Parameters:**
  * `reconcileIntervalSeconds`: `10`
  * Host Mutation: Wipes root HTB qdisc directly via host debug pod (`tc qdisc del dev enp1s0 root`).
* **Assertions Checked:**
  * Self-Healing: Calls POST `/reconcile` and asserts that root HTB qdisc `1:` and leaf class `1:100` (`2Gbit`) are automatically re-created in kernel memory.
  * Alignment: Verifies `/config` returns `isAligned: true`.

#### **S11: Dynamic CR Mutation (`S11_LIVE_CR_MUTATION`)**
* **Applied CR Spec Parameters:**
  * Mutation Pass: Updates active CR class `1:100` egress rate from `2Gbit` $\rightarrow$ `5Gbit`, adds new class `1:400` (`3Gbit`).
* **Assertions Checked:**
  * Real-Time Propagation: Queries REST `/config` and verifies `actual.classes[]` reflects `1:100` rate = `5Gbit` and confirms existence of class `1:400`.

#### **S12: Control Plane Node Isolation (`S12_MULTI_NODE_ISOLATION`)**
* **Applied CR Spec Parameters:**
  * `nodeSelector`: `node-role.kubernetes.io/worker: ""`
* **Assertions Checked:**
  * Master Isolation: Queries agent REST endpoint on master node (`192.168.100.11:8080`) and asserts `actual.htbQdiscPresent == false`.
  * Worker Inclusion: Queries worker node (`192.168.100.21:8080`) and asserts `isAligned == true`.

#### **S13: Malformed Spec Safety (`S13_INVALID_SPEC_SAFETY`)**
* **Applied CR Spec Parameters:**
  * Malformed Input: `matchType: subnet` with `subnet: ""` (empty CIDR string).
* **Assertions Checked:**
  * Process Stability: Asserts that agent REST endpoint `http://192.168.100.21:8080/healthz` continues returning `HTTP 200 OK` without binary crash or panic.

#### **S14: Agent Pod Restart Resilience (`S14_POD_RESTART_RESILIENCE`)**
* **Applied CR Spec Parameters:**
  * Active VTC CR `vtc-test-s1` deployed.
  * Action: `oc delete pod -l app=vlan-traffic-control-agent` on target worker node.
* **Assertions Checked:**
  * Zero-Downtime State: Upon pod spin-up, agent inspects existing host TC state, attaches netlink listeners, and reports `htbQdiscPresent: true` and `isAligned: true`.

#### **S15: Concurrent Race Condition Safety (`S15_CONCURRENT_RACE_SAFETY`)**
* **Applied CR Spec Parameters:**
  * Stress Action: Fires rapid parallel loops of `oc apply` and `oc delete vtc --all` in background jobs.
* **Assertions Checked:**
  * Netlink Consistency: Applies final clean spec and verifies that no orphaned HTB classes, lock deadlocks, or stale ingress policing filters remain.

---

### Execution Instructions

```bash
# 1. Ensure cluster permissions and node environment variables are set
export NAMESPACE="openshift-vlan-tc-operator"

# 2. Grant executable permissions to test runner
chmod +x test-vtc-strict-suite.sh

# 3. Execute full 15-scenario verification suite
./test-vtc-strict-suite.sh
```

* Upon completion, detailed JSON responses, netlink dumps, and kernel state logs are stored under `./vtc-strict-test-logs-YYYYMMDD_HHMMSS/` .
