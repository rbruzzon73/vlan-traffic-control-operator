package v1alpha1

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// TcStrategyType defines the TC filter execution strategy (e.g. flower, u32, auto)
type TcStrategyType string

const (
	TcStrategyFlower TcStrategyType = "flower"
	TcStrategyU32    TcStrategyType = "u32"
	TcStrategyAuto   TcStrategyType = "auto"
)

// IngressAction defines the action taken on packets exceeding the policed rate limit.
// +kubebuilder:validation:Enum=drop;pass
type IngressAction string

const (
	IngressActionDrop IngressAction = "drop"
	IngressActionPass IngressAction = "pass"
)

// TolerationSpec represents pod/CR policy scheduling tolerations (e.g., Master/Control-Plane nodes).
type TolerationSpec struct {
	// Key is the taint key that the toleration applies to.
	// +optional
	Key string `json:"key,omitempty"`

	// Operator represents a key's relationship to the value. Valid values are Exists and Equal.
	// +optional
	Operator string `json:"operator,omitempty"`

	// Value is the taint value the toleration matches to.
	// +optional
	Value string `json:"value,omitempty"`

	// Effect indicates the taint effect to match. Empty means match all taint effects.
	// +optional
	Effect string `json:"effect,omitempty"`
}

// ToCoreV1 converts TolerationSpec to standard corev1.Toleration
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
	// Name is a descriptive human-readable name for this traffic class.
	Name string `json:"name"`

	// ClassID is the full HTB class identifier (e.g. "1:100").
	// +optional
	ClassID string `json:"classId,omitempty"`

	// ClassMinor is the minor ID of the HTB class (e.g. 100 for handle 1:100).
	// +optional
	ClassMinor int `json:"classMinor,omitempty"`

	// MatchType defines how traffic is classified: "vlan", "subnet", "ip", "port", or "dscp".
	MatchType string `json:"matchType"`

	// VlanID is the 802.1Q VLAN tag ID to match (1-4094).
	// +optional
	VlanID int `json:"vlanId,omitempty"`

	// Subnet is the CIDR block to match for egress/ingress policing (e.g. "10.0.100.0/24").
	// +optional
	Subnet string `json:"subnet,omitempty"`

	// SKB Mark classifier value (e.g. 16 for OVS marked flows)
	// +optional
	Mark uint32 `json:"mark,omitempty"`

	// IP is a single IP address to match.
	// +optional
	IP string `json:"ip,omitempty"`

	// Port is a L4 TCP/UDP port number to match.
	// +optional
	Port int `json:"port,omitempty"`

	// Dscp is the Differentiated Services Code Point value to match (0-63).
	// +optional
	Dscp int `json:"dscp,omitempty"`

	// EgressRate is the guaranteed egress bandwidth rate (e.g. "100Mbit", "1Gbit").
	EgressRate string `json:"egressRate"`

	// EgressCeil is the maximum burstable egress bandwidth rate (e.g. "500Mbit").
	// +optional
	EgressCeil string `json:"egressCeil,omitempty"`

	// EgressBurst is the optional burst allowance size (e.g. "15k").
	// +optional
	EgressBurst string `json:"egressBurst,omitempty"`

	// EnableFqCodel controls whether fq_codel leaf qdisc is attached to this class.
	// +optional
	EnableFqCodel bool `json:"enableFqCodel,omitempty"`

	// IngressRate is the maximum ingress rate limit for traffic policing.
	// +optional
	IngressRate string `json:"ingressRate,omitempty"`

	// IngressBurst is the optional burst allowance size for ingress policing.
	// +optional
	IngressBurst string `json:"ingressBurst,omitempty"`

	// IngressAction defines the exceeding packet action ("drop" or "pass"). Default is "drop".
	// +kubebuilder:validation:Enum=drop;pass
	// +optional
	IngressAction IngressAction `json:"ingressAction,omitempty"`

	// Priority is the HTB class priority (0-7, lower numbers indicate higher priority).
	// +optional
	Priority int `json:"priority,omitempty"`
}

// GetClassID resolves the full "X:Y" class handle reliably.
func (c *ClassSpec) GetClassID(rootID int) string {
	if c.ClassID != "" {
		return c.ClassID
	}
	if rootID <= 0 {
		rootID = 1
	}
	return formatClassHandle(rootID, c.ClassMinor)
}

// GetIngressAction returns configured action or defaults to "drop".
func (c *ClassSpec) GetIngressAction() string {
	if c.IngressAction == IngressActionPass {
		return "pass"
	}
	return "drop"
}

func formatClassHandle(rootID, minorID int) string {
	return fmt.Sprintf("%d:%d", rootID, minorID)
}

// HtbRootSpec defines the root HTB qdisc and attached class specs.
type HtbRootSpec struct {
	// Interface is the target network interface on the host (e.g. "enp1s0.100").
	Interface string `json:"interface"`

	// HtbID is the root qdisc handle ID (e.g. 1 for handle "1:").
	// +kubebuilder:default=1
	// +optional
	HtbID int `json:"htbId,omitempty"`

	// DefaultClassID is the fallback HTB class handle for unmatched traffic.
	// +optional
	DefaultClassID string `json:"defaultClassId,omitempty"`

	// DefaultClassMinor is the minor ID of the default HTB class (e.g. 99 for handle "1:99").
	// +kubebuilder:default=99
	// +optional
	DefaultClassMinor int `json:"defaultClassMinor,omitempty"`

	// Rate is the total root qdisc capacity (e.g. "10Gbit").
	Rate string `json:"rate"`

	// Classes is the list of HTB leaf classes attached to this root.
	Classes []ClassSpec `json:"classes"`
}

// VlanTrafficControlSpec defines the desired state of VlanTrafficControl.
type VlanTrafficControlSpec struct {
	// NodeSelector is an optional map of key-value pairs to target specific nodes.
	// +optional
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`

	// NodeLabelSelector provides full Kubernetes label selector matching (matchLabels & matchExpressions).
	// +optional
	NodeLabelSelector *metav1.LabelSelector `json:"nodeLabelSelector,omitempty"`

	// Tolerations allows optional policy scheduling tolerations (e.g. to enforce policies on Master nodes).
	// +optional
	Tolerations []TolerationSpec `json:"tolerations,omitempty"`

	// ReconcileIntervalSeconds defines the interval in seconds between node agent reconciliation loops.
	// +kubebuilder:default=30
	// +optional
	ReconcileIntervalSeconds int `json:"reconcileIntervalSeconds,omitempty"`

	// TcStrategy specifies the strategy used to classify traffic: "flower", "u32", or "auto".
	// +kubebuilder:validation:Enum=flower;u32;auto
	// +kubebuilder:default="flower"
	TcStrategy TcStrategyType `json:"tcStrategy"`

	// HtbRoot defines the root HTB qdisc and class tree configuration.
	HtbRoot HtbRootSpec `json:"htbRoot"`
}

// ClassStat defines performance metrics for a single HTB class.
type ClassStat struct {
	ClassID    string `json:"classId"`
	ClassName  string `json:"name,omitempty"`
	Priority   int    `json:"prio"`
	Bytes      uint64 `json:"bytes"`
	Packets    uint64 `json:"packets"`
	RateBps    uint64 `json:"rateBps,omitempty"`
	Pps        uint64 `json:"pps,omitempty"`
	Drops      uint32 `json:"drops"`
	Overlimits uint32 `json:"overlimits"`
	Borrowed   uint64 `json:"borrowed"`
}

// IngressStat defines performance metrics for an ingress policing filter.
type IngressStat struct {
	ClassID  string `json:"classId,omitempty"`
	FilterID string `json:"filterId"`
	Subnet   string `json:"subnet,omitempty"`
	Bytes    uint64 `json:"bytes"`
	Packets  uint64 `json:"packets"`
	Drops    uint64 `json:"drops"`
}

// InterfaceStats represents aggregated traffic stats for a host interface.
type InterfaceStats struct {
	Interface    string        `json:"interface"`
	Node         string        `json:"node,omitempty"`
	ClassStats   []ClassStat   `json:"classStats"`
	IngressStats []IngressStat `json:"ingressStats"`
}

// ConfigDriftDelta represents individual class or qdisc drift details.
type ConfigDriftDelta struct {
	TargetHandle string `json:"targetHandle"`
	Property     string `json:"property"`
	Expected     string `json:"expected"`
	Actual       string `json:"actual"`
}

// FilterMeta represents kernel filter metadata discovered via Netlink.
type FilterMeta struct {
	Priority     uint16            `json:"priority"`
	Handle       uint32            `json:"handle,omitempty"`       // Netlink filter handle
	Chain        uint32            `json:"chain,omitempty"`        // Filter chain ID
	Type         string            `json:"type"`                   // flower, u32, etc.
	Protocol     uint16            `json:"protocol"`
	Name         string            `json:"name,omitempty"`         // Associated Class Name (1:1)
	MatchType    string            `json:"matchType,omitempty"`    // vlan, subnet, mark, auto
	VlanID       int               `json:"vlanId,omitempty"`       // Matched VLAN tag
	Subnet       string            `json:"subnet,omitempty"`       // Matched IP CIDR
	Mark         uint32            `json:"mark,omitempty"`         // Matched mark handle
	IngressRate  string            `json:"ingressRate,omitempty"`  // Target policed rate
	PeakRate     string            `json:"peakRate,omitempty"`     // Peak policing rate
	IngressBurst string            `json:"ingressBurst,omitempty"` // Policed burst buffer
	Action       string            `json:"action,omitempty"`       // "police drop" or "police pass"
	Matches      map[string]string `json:"matches,omitempty"`      // Key-value raw filter selectors
}

// ActualNodeState contains observed live kernel netlink TC state.
type ActualNodeState struct {
	HtbQdiscPresent bool         `json:"htbQdiscPresent"`
	IngressPresent  bool         `json:"ingressPresent"`
	Classes         []ClassSpec  `json:"classes"`
	IngressFilters  []FilterMeta `json:"ingressFilters"`
}

// NodeConfigReport tracks policy alignment state across cluster nodes.
type NodeConfigReport struct {
	Node        string             `json:"node"`
	Interface   string             `json:"interface"`
	IsAligned   bool               `json:"isAligned"`
	Desired     HtbRootSpec        `json:"desired"`
	Actual      ActualNodeState    `json:"actual"`
	DriftDeltas []ConfigDriftDelta `json:"driftDeltas"`
}

// VlanTrafficControlStatus defines the observed state of VlanTrafficControl.
type VlanTrafficControlStatus struct {
	// Conditions track the reconciliation state and health of the traffic control policy.
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// ObservedGeneration represents the last generation reconciled by the operator.
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// ActiveNodes lists host nodes currently enforcing this policy.
	ActiveNodes []string `json:"activeNodes,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:path=vlantrafficcontrols,shortName=vtc;vtcs,scope=Cluster

// VlanTrafficControl is the Schema for the vlantrafficcontrols API.
type VlanTrafficControl struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   VlanTrafficControlSpec   `json:"spec,omitempty"`
	Status VlanTrafficControlStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// VlanTrafficControlList contains a list of VlanTrafficControl.
type VlanTrafficControlList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []VlanTrafficControl `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(GroupVersion,
			&VlanTrafficControl{},
			&VlanTrafficControlList{},
		)
		metav1.AddToGroupVersion(s, GroupVersion)
		return nil
	})
}
