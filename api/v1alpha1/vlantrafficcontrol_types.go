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
	Key string `json:"key,omitempty"`
	// +optional
	Operator string `json:"operator,omitempty"`
	// +optional
	Value string `json:"value,omitempty"`
	// +optional
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
	Name       string `json:"name"`
	ClassID    string `json:"classId,omitempty"`
	ClassMinor int    `json:"classMinor,omitempty"`

	// +optional
	MatchType string `json:"matchType,omitempty"`
	// +optional
	VlanID int `json:"vlanId,omitempty"`
	// +optional
	Subnet string `json:"subnet,omitempty"`
	// +optional
	Mark uint32 `json:"mark,omitempty"`
	// +optional
	IP string `json:"ip,omitempty"`
	// +optional
	Port int `json:"port,omitempty"`
	// +optional
	Dscp int `json:"dscp,omitempty"`

	// Egress parameters
	// +optional
	EgressRate string `json:"egressRate,omitempty"`
	// +optional
	EgressCeil string `json:"egressCeil,omitempty"`
	// +optional
	EgressBurst string `json:"egressBurst,omitempty"`
	// +optional
	EnableFqCodel bool `json:"enableFqCodel,omitempty"`

	// Ingress parameters
	// +optional
	IngressRate string `json:"ingressRate,omitempty"`
	// +optional
	IngressCeil string `json:"ingressCeil,omitempty"`
	// +optional
	IngressBurst string `json:"ingressBurst,omitempty"`
	// +optional
	IngressAction IngressAction `json:"ingressAction,omitempty"`

	// +optional
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
	Interface string `json:"interface"`

	// +kubebuilder:default=1
	// +optional
	HtbID int `json:"htbId,omitempty"`

	// +optional
	DefaultClassID string `json:"defaultClassId,omitempty"`

	// +kubebuilder:default=99
	// +optional
	DefaultClassMinor int `json:"defaultClassMinor,omitempty"`

	// +optional
	Rate string `json:"rate,omitempty"`

	// +listType=atomic
	Classes []ClassSpec `json:"classes"`
}

// VlanTrafficControlSpec defines the desired state of VlanTrafficControl.
type VlanTrafficControlSpec struct {
	NodeSelector             map[string]string     `json:"nodeSelector,omitempty"`
	NodeLabelSelector        *metav1.LabelSelector `json:"nodeLabelSelector,omitempty"`
	Tolerations              []TolerationSpec      `json:"tolerations,omitempty"`
	ReconcileIntervalSeconds int                   `json:"reconcileIntervalSeconds,omitempty"`
	TcStrategy               TcStrategyType        `json:"tcStrategy"`
	HtbRoot                  HtbRootSpec           `json:"htbRoot"`
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
	Conditions         []metav1.Condition `json:"conditions,omitempty"`
	ObservedGeneration int64              `json:"observedGeneration,omitempty"`
	ActiveNodes        []string           `json:"activeNodes,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:path=vlantrafficcontrols,shortName=vtc;vtcs,scope=Cluster
// +kubebuilder:printcolumn:name="INTERFACE",type="string",JSONPath=".spec.htbRoot.interface"
// +kubebuilder:printcolumn:name="CAPACITY",type="string",JSONPath=".spec.htbRoot.rate"
// +kubebuilder:printcolumn:name="STRATEGY",type="string",JSONPath=".spec.tcStrategy"
// +kubebuilder:printcolumn:name="READY",type="string",JSONPath=".status.conditions[?(@.type=='Ready')].status"
// +kubebuilder:printcolumn:name="AGE",type="date",JSONPath=".metadata.creationTimestamp"

type VlanTrafficControl struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   VlanTrafficControlSpec   `json:"spec,omitempty"`
	Status VlanTrafficControlStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

type VlanTrafficControlList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []VlanTrafficControl `json:"items"`
}

type VlanTrafficControlClassSpec struct {
	ClassName     string        `json:"className"`
	Direction     string        `json:"direction,omitempty"` // "ingress", "egress", or "both"
	ClassID       string        `json:"classId"`
	MatchType     string        `json:"matchType,omitempty"`
	VlanID        int           `json:"vlanId,omitempty"`
	Subnet        string        `json:"subnet,omitempty"`
	Mark          uint32        `json:"mark,omitempty"`
	IP            string        `json:"ip,omitempty"`
	Port          int           `json:"port,omitempty"`
	Dscp          int           `json:"dscp,omitempty"`
	Guaranteed    string        `json:"guaranteed,omitempty"`
	CeilBorrow    string        `json:"ceilBorrow,omitempty"`
	EgressBurst   string        `json:"egressBurst,omitempty"`
	EnableFqCodel bool          `json:"enableFqCodel,omitempty"`
	IngressRate   string        `json:"ingressRate,omitempty"`
	IngressCeil   string        `json:"ingressCeil,omitempty"`
	IngressBurst  string        `json:"ingressBurst,omitempty"`
	IngressAction IngressAction `json:"ingressAction,omitempty"`
	Priority      int           `json:"priority,omitempty"`
	Aligned       string        `json:"aligned,omitempty"`
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
