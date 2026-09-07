package v1alpha1

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// TcStrategyType defines the TC filter execution strategy (e.g. flower, u32, auto, ifb)
type TcStrategyType string

const (
	TcStrategyFlower TcStrategyType = "flower"
	TcStrategyU32    TcStrategyType = "u32"
	TcStrategyAuto   TcStrategyType = "auto"
	TcStrategyIFB    TcStrategyType = "ifb"
)

// IngressAction defines the action taken on packets exceeding the policed rate limit.
// +kubebuilder:validation:Enum=drop;pass
type IngressAction string

const (
	IngressActionDrop IngressAction = "drop"
	IngressActionPass IngressAction = "pass"
)

// TolerationSpec represents pod/CR policy scheduling tolerations.
type TolerationSpec struct {
	// +optional
	// +kubebuilder:validation:Description="Taint key that the toleration applies to."
	Key string `json:"key,omitempty"`

	// +optional
	// +kubebuilder:validation:Description="Operator represents a key's relationship to the value. Valid operators are Exists and Equal."
	Operator string `json:"operator,omitempty"`

	// +optional
	// +kubebuilder:validation:Description="Taint value the toleration matches to."
	Value string `json:"value,omitempty"`

	// +optional
	// +kubebuilder:validation:Description="Effect indicates the taint effect to match (e.g. NoSchedule, PreferNoSchedule, NoExecute)."
	Effect string `json:"effect,omitempty"`
}

func (t TolerationSpec) ToCoreV1() corev1.Toleration {
	return corev1.Toleration{
		Key:      t.Key,
		Operator: corev1.TolerationOperator(t.Operator),
		Value:    t.Value,
		Effect:   corev1.TaintEffect(t.Effect),
	}
}

// ClassSpec defines individual HTB class configuration parameters.
type ClassSpec struct {
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Description="Unique human-readable name for this traffic class rule (e.g., vlan-100-high-priority)."
	Name string `json:"name"`

	// +optional
	// +kubebuilder:validation:Description="HTB class handle identifier (e.g., 1:100). If omitted, classMinor is used to construct handle '1:<minor>'."
	ClassID string `json:"classId,omitempty"`

	// +optional
	// +kubebuilder:validation:Description="HTB class minor number (1-65535) used to derive the class handle if classId is omitted."
	ClassMinor int `json:"classMinor,omitempty"`

	// +optional
	// +kubebuilder:validation:Enum=vlan;subnet;mark;ip;port;dscp
	// +kubebuilder:validation:Description="Packet classification match type. Supported options: 'vlan', 'subnet', 'mark', 'ip', 'port', 'dscp'."
	MatchType string `json:"matchType,omitempty"`

	// +optional
	// +kubebuilder:validation:Description="VLAN Tag ID (1-4094) to match for traffic classification."
	VlanID int `json:"vlanId,omitempty"`

	// +optional
	// +kubebuilder:validation:Description="CIDR subnet block (e.g., 10.0.100.0/24) to match target packet destination/source IP addresses."
	Subnet string `json:"subnet,omitempty"`

	// +optional
	// +kubebuilder:validation:Description="FWMark 32-bit integer value to match packets pre-classified by iptables/nftables."
	Mark uint32 `json:"mark,omitempty"`

	// +optional
	// +kubebuilder:validation:Description="Specific IPv4 or IPv6 address to match for traffic classification."
	IP string `json:"ip,omitempty"`

	// +optional
	// +kubebuilder:validation:Description="TCP/UDP port number to match for traffic classification."
	Port int `json:"port,omitempty"`

	// +optional
	// +kubebuilder:validation:Description="DSCP Quality of Service code point value to match."
	Dscp int `json:"dscp,omitempty"`

	// Egress parameters
	// +optional
	// +kubebuilder:validation:Description="Guaranteed minimum egress bandwidth rate (e.g., 2Gbit, 500Mbit)."
	EgressRate string `json:"egressRate,omitempty"`

	// +optional
	// +kubebuilder:validation:Description="Maximum ceiling egress bandwidth rate for dynamic HTB borrowing (e.g., 10Gbit)."
	EgressCeil string `json:"egressCeil,omitempty"`

	// +optional
	// +kubebuilder:validation:Description="Egress token bucket burst buffer size (e.g., 16Mb, 64Mb) to absorb high-throughput TCP spikes."
	EgressBurst string `json:"egressBurst,omitempty"`

	// +optional
	// +kubebuilder:validation:Description="Enable FQ_CoDel Active Queue Management to prevent bufferbloat and optimize latency for small control-plane packets."
	EnableFqCodel bool `json:"enableFqCodel,omitempty"`

	// Ingress parameters
	// +optional
	// +kubebuilder:validation:Description="Guaranteed minimum ingress bandwidth rate (e.g., 2Gbit, 500Mbit)."
	IngressRate string `json:"ingressRate,omitempty"`

	// +optional
	// +kubebuilder:validation:Description="Maximum ceiling ingress bandwidth rate on IFB virtual devices (e.g., 10Gbit)."
	IngressCeil string `json:"ingressCeil,omitempty"`

	// +optional
	// +kubebuilder:validation:Description="Ingress token bucket burst buffer size (e.g., 16Mb, 64Mb) for TC policing/shaping."
	IngressBurst string `json:"ingressBurst,omitempty"`

	// +optional
	// +kubebuilder:validation:Description="Action taken on packets exceeding policed rate: 'drop' (hard cap) or 'pass' (allow overlimit)."
	IngressAction IngressAction `json:"ingressAction,omitempty"`

	// +optional
	// +kubebuilder:validation:Description="Strict evaluation priority (0-49152). Lower priority numbers are evaluated first in TC flower pipelines."
	Priority int `json:"priority,omitempty"`
}

func (c *ClassSpec) GetClassID(rootID int) string {
	if c.ClassID != "" {
		return c.ClassID
	}
	if rootID <= 0 {
		rootID = 1
	}
	return formatClassHandle(rootID, c.ClassMinor)
}

func (c *ClassSpec) GetIngressAction() string {
	if c.IngressAction == IngressActionPass {
		return "pass"
	}
	return "drop"
}

func formatClassHandle(rootID, minorID int) string {
	return fmt.Sprintf("%d:%d", rootID, minorID)
}

// HtbRootSpec defines the root HTB qdisc parameters and attached class specs.
type HtbRootSpec struct {
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Description="Target host network interface name (e.g., enp1s0, eth0)."
	Interface string `json:"interface"`

	// +kubebuilder:default=1
	// +optional
	// +kubebuilder:validation:Description="Numeric handle ID for the root HTB qdisc (default is 1, creating handle '1:')."
	HtbID int `json:"htbId,omitempty"`

	// +optional
	// +kubebuilder:validation:Description="Default HTB class handle for unclassified fallback traffic (e.g., '1:99')."
	DefaultClassID string `json:"defaultClassId,omitempty"`

	// +kubebuilder:default=99
	// +optional
	// +kubebuilder:validation:Description="Minor ID for default class if defaultClassId is omitted (default is 99)."
	DefaultClassMinor int `json:"defaultClassMinor,omitempty"`

	// +optional
	// +kubebuilder:validation:Description="Total physical interface link capacity rate (e.g., 10Gbit, 1Gbit)."
	Rate string `json:"rate,omitempty"`

	// +listType=atomic
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Description="List of traffic class rules attached to this HTB root."
	Classes []ClassSpec `json:"classes"`
}

// VlanTrafficControlSpec defines the desired state of VlanTrafficControl.
type VlanTrafficControlSpec struct {
	// +optional
	// +kubebuilder:validation:Description="Simple key-value node selector targeting specific OpenShift worker nodes."
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`

	// +optional
	// +kubebuilder:validation:Description="Standard Kubernetes label selector supporting matchLabels and matchExpressions for targeting nodes."
	NodeLabelSelector *metav1.LabelSelector `json:"nodeLabelSelector,omitempty"`

	// +optional
	// +kubebuilder:validation:Description="List of pod/CR policy scheduling tolerations required to run node agent pods on tainted worker/control-plane nodes."
	Tolerations []TolerationSpec `json:"tolerations,omitempty"`

	// +optional
	// +kubebuilder:default=60
	// +kubebuilder:validation:Description="Reconciliation loop interval in seconds for periodic drift checks (default is 60s)."
	ReconcileIntervalSeconds int `json:"reconcileIntervalSeconds,omitempty"`

	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Enum=ifb;flower;u32;auto
	// +kubebuilder:validation:Description="Traffic control strategy: 'ifb' (stateful HTB queueing/borrowing), 'flower' (stateless policing), 'u32', or 'auto'."
	TcStrategy TcStrategyType `json:"tcStrategy"`

	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Description="Root HTB qdisc parameters and attached class specifications."
	HtbRoot HtbRootSpec `json:"htbRoot"`
}

type ClassStat struct {
	Interface  string `json:"interface,omitempty"`
	ClassID    string `json:"classId"`
	ClassName  string `json:"name,omitempty"`
	Direction  string `json:"direction,omitempty"` // "ingress", "egress", "ingress+egress"
	Priority   int    `json:"prio"`
	Bytes      uint64 `json:"bytes"`
	Packets    uint64 `json:"packets"`
	RateBps    uint64 `json:"rateBps,omitempty"`
	Pps        uint64 `json:"pps,omitempty"`
	Drops      uint32 `json:"drops"`
	Overlimits uint32 `json:"overlimits"`
	Borrowed   uint64 `json:"borrowed"`
}

type IngressStat struct {
	Interface string `json:"interface,omitempty"`
	ClassID   string `json:"classId,omitempty"`
	FilterID  string `json:"filterId"`
	Direction string `json:"direction,omitempty"` // "ingress", "egress"
	Subnet    string `json:"subnet,omitempty"`
	Bytes     uint64 `json:"bytes"`
	Packets   uint64 `json:"packets"`
	Drops     uint64 `json:"drops"`
}

type InterfaceStats struct {
	Interface    string        `json:"interface"`
	Node         string        `json:"node,omitempty"`
	ClassStats   []ClassStat   `json:"classStats"`
	IngressStats []IngressStat `json:"ingressStats"`
}

type ConfigDriftDelta struct {
	TargetHandle string `json:"targetHandle"`
	Property     string `json:"property"`
	Expected     string `json:"expected"`
	Actual       string `json:"actual"`
}

type FilterMeta struct {
	Priority     uint16            `json:"priority"`
	Handle       uint32            `json:"handle,omitempty"`
	Chain        uint32            `json:"chain,omitempty"`
	Interface    string            `json:"interface,omitempty"` // "enp1s0" or "ifb-enp1s0"
	Type         string            `json:"type"`
	Protocol     uint16            `json:"protocol"`
	Name         string            `json:"name,omitempty"`
	Direction    string            `json:"direction,omitempty"` // "ingress", "egress"
	MatchType    string            `json:"matchType,omitempty"`
	VlanID       int               `json:"vlanId,omitempty"`
	Subnet       string            `json:"subnet,omitempty"`
	Mark         uint32            `json:"mark,omitempty"`
	IngressRate  string            `json:"ingressRate,omitempty"`
	PeakRate     string            `json:"peakRate,omitempty"`
	IngressBurst string            `json:"ingressBurst,omitempty"`
	Action       string            `json:"action,omitempty"`
	Matches      map[string]string `json:"matches,omitempty"`
}

type ActualNodeState struct {
	Interface       string       `json:"interface,omitempty"`
	IfbInterface    string       `json:"ifbInterface,omitempty"`
	HtbQdiscPresent bool         `json:"htbQdiscPresent"`
	IngressPresent  bool         `json:"ingressPresent"`
	ClsactPresent   bool         `json:"clsactPresent"`
	Classes         []ClassSpec  `json:"classes"`
	IngressFilters  []FilterMeta `json:"ingressFilters"`
}

type NodeConfigReport struct {
	Node        string             `json:"node"`
	Interface   string             `json:"interface"`
	IsAligned   bool               `json:"isAligned"`
	Desired     HtbRootSpec        `json:"desired"`
	Actual      ActualNodeState    `json:"actual"`
	DriftDeltas []ConfigDriftDelta `json:"driftDeltas"`
}

type VlanTrafficControlStatus struct {
	// +optional
	// +kubebuilder:validation:Description="Current status conditions reflecting operator and agent synchronization states."
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// +optional
	// +kubebuilder:validation:Description="Most recent generation observed and reconciled by the controller."
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// +optional
	// +kubebuilder:validation:Description="List of worker node names where the agent has applied and verified TC rules."
	ActiveNodes []string `json:"activeNodes,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:path=vlantrafficcontrols,shortName=vtc;vtcs,scope=Cluster
// +kubebuilder:printcolumn:name="INTERFACE",type="string",JSONPath=".spec.htbRoot.interface"
// +kubebuilder:printcolumn:name="CAPACITY",type="string",JSONPath=".spec.htbRoot.rate"
// +kubebuilder:printcolumn:name="STRATEGY",type="string",JSONPath=".spec.tcStrategy"
// +kubebuilder:printcolumn:name="READY",type="string",JSONPath=".status.conditions[?(@.type=='Ready')].status"
// +kubebuilder:printcolumn:name="AGE",type="date",JSONPath=".metadata.creationTimestamp"

// VlanTrafficControl is the Schema for the vlantrafficcontrols API
type VlanTrafficControl struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   VlanTrafficControlSpec   `json:"spec,omitempty"`
	Status VlanTrafficControlStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// VlanTrafficControlList contains a list of VlanTrafficControl
type VlanTrafficControlList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []VlanTrafficControl `json:"items"`
}

type VlanTrafficControlClassSpec struct {
	// +kubebuilder:validation:Description="Human-readable class name referenced from parent VlanTrafficControl CR."
	ClassName string `json:"className"`

	// +optional
	// +kubebuilder:validation:Description="Traffic flow direction for this class ('ingress', 'egress', or 'ingress+egress')."
	Direction string `json:"direction,omitempty"`

	// +kubebuilder:validation:Description="Resolved HTB class ID handle (e.g., '1:100', '1:380')."
	ClassID string `json:"classId"`

	// +optional
	// +kubebuilder:validation:Description="Traffic classification rule type used for matching."
	MatchType string `json:"matchType,omitempty"`

	// +optional
	// +kubebuilder:validation:Description="VLAN ID associated with this class projection."
	VlanID int `json:"vlanId,omitempty"`

	// +optional
	// +kubebuilder:validation:Description="IP Subnet CIDR associated with this class projection."
	Subnet string `json:"subnet,omitempty"`

	// +optional
	// +kubebuilder:validation:Description="FWMark value associated with this class projection."
	Mark uint32 `json:"mark,omitempty"`

	// +optional
	// +kubebuilder:validation:Description="Target IP address associated with this class projection."
	IP string `json:"ip,omitempty"`

	// +optional
	// +kubebuilder:validation:Description="Target TCP/UDP port associated with this class projection."
	Port int `json:"port,omitempty"`

	// +optional
	// +kubebuilder:validation:Description="Target DSCP code point associated with this class projection."
	Dscp int `json:"dscp,omitempty"`

	// +optional
	// +kubebuilder:validation:Description="Guaranteed bandwidth rate allocated for egress traffic."
	Guaranteed string `json:"guaranteed,omitempty"`

	// +optional
	// +kubebuilder:validation:Description="Maximum ceiling rate allowed for dynamic egress bandwidth borrowing."
	CeilBorrow string `json:"ceilBorrow,omitempty"`

	// +optional
	// +kubebuilder:validation:Description="Configured egress burst size buffer."
	EgressBurst string `json:"egressBurst,omitempty"`

	// +optional
	// +kubebuilder:validation:Description="Indicates if FQ_CoDel Active Queue Management is enabled on this class."
	EnableFqCodel bool `json:"enableFqCodel,omitempty"`

	// +optional
	// +kubebuilder:validation:Description="Guaranteed bandwidth rate allocated for ingress traffic."
	IngressRate string `json:"ingressRate,omitempty"`

	// +optional
	// +kubebuilder:validation:Description="Maximum ceiling rate allowed for ingress IFB traffic."
	IngressCeil string `json:"ingressCeil,omitempty"`

	// +optional
	// +kubebuilder:validation:Description="Configured ingress burst size buffer."
	IngressBurst string `json:"ingressBurst,omitempty"`

	// +optional
	// +kubebuilder:validation:Description="Action taken when ingress rate is exceeded."
	IngressAction IngressAction `json:"ingressAction,omitempty"`

	// +optional
	// +kubebuilder:validation:Description="Filter evaluation priority assigned to this class rule."
	Priority int `json:"priority,omitempty"`

	// +optional
	// +kubebuilder:validation:Description="Alignment status confirming runtime kernel state matches this spec ('True' or 'False')."
	Aligned string `json:"aligned,omitempty"`
}

// VlanTrafficControlClass represents a projected secondary resource exposing status, alignment, and traffic shaping metrics for individual HTB/IFB classes.
// +kubebuilder:object:root=true
// +kubebuilder:resource:path=vlantrafficcontrolsclasses,shortName=vtcclass;vtcclasses,scope=Cluster
// +kubebuilder:printcolumn:name="CLASS_NAME",type="string",JSONPath=".spec.className"
// +kubebuilder:printcolumn:name="DIRECTION",type="string",JSONPath=".spec.direction"
// +kubebuilder:printcolumn:name="CLASS_ID",type="string",JSONPath=".spec.classId"
// +kubebuilder:printcolumn:name="VLAN_ID",type="integer",JSONPath=".spec.vlanId"
// +kubebuilder:printcolumn:name="GUARANTEED",type="string",JSONPath=".spec.guaranteed"
// +kubebuilder:printcolumn:name="CEIL_BORROW",type="string",JSONPath=".spec.ceilBorrow"
// +kubebuilder:printcolumn:name="INGRESS_RATE",type="string",JSONPath=".spec.ingressRate"
// +kubebuilder:printcolumn:name="INGRESS_CEIL",type="string",JSONPath=".spec.ingressCeil"
// +kubebuilder:printcolumn:name="ALIGNED",type="string",JSONPath=".spec.aligned"
// +kubebuilder:printcolumn:name="AGE",type="date",JSONPath=".metadata.creationTimestamp"

type VlanTrafficControlClass struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec VlanTrafficControlClassSpec `json:"spec,omitempty"`
}

// +kubebuilder:object:root=true

type VlanTrafficControlClassList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []VlanTrafficControlClass `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(GroupVersion,
			&VlanTrafficControl{},
			&VlanTrafficControlList{},
			&VlanTrafficControlClass{},
			&VlanTrafficControlClassList{},
		)
		metav1.AddToGroupVersion(s, GroupVersion)
		return nil
	})
}
