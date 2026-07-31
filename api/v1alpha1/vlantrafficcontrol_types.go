package v1alpha1

import (
	"fmt"
	"strconv"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// TcStrategyType defines the strategy for Traffic Control execution
// +kubebuilder:validation:Enum=flower;u32;auto
type TcStrategyType string

const (
	TcStrategyAuto   TcStrategyType = "auto"
	TcStrategyFlower TcStrategyType = "flower"
	TcStrategyU32    TcStrategyType = "u32"
)

// ClassifierType defines how traffic is identified on the host interface.
// +kubebuilder:validation:Enum=vlan;subnet;mark;auto
type ClassifierType string

const (
	ClassifierVlan   ClassifierType = "vlan"
	ClassifierSubnet ClassifierType = "subnet"
	ClassifierMark   ClassifierType = "mark"
	ClassifierAuto   ClassifierType = "auto"
)

// VlanClassSpec defines rules for a single traffic class (VLAN, Subnet, or SKB Mark).
type VlanClassSpec struct {
	// Descriptive name for this traffic class. [OPTIONAL]
	// +optional
	Name string `json:"name,omitempty"`

	// HTB minor class identifier (e.g., 10 for handle X:10). [OPTIONAL]
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	// +optional
	ClassMinor int `json:"classMinor,omitempty"`

	// Unique HTB class identifier (e.g., "1:10", "1:100"). [OPTIONAL]
	// +kubebuilder:validation:Pattern=`^1:[0-9]+$`
	// +optional
	ClassID string `json:"classId,omitempty"`

	// MatchType explicitly selects the classification strategy:
	// - "vlan": Matches 802.1Q tagged traffic using vlanId (Range: 1-4094).
	// - "subnet": Matches untagged/stripped traffic based on IP network CIDR using subnet.
	// - "mark": Matches packets marked by OVS or iptables using mark (skbmark).
	// - "auto" (default): Automatically selects classification based on defined fields. [OPTIONAL]
	// +kubebuilder:default=auto
	// +kubebuilder:validation:Enum=vlan;subnet;mark;auto
	// +optional
	MatchType ClassifierType `json:"matchType,omitempty"`

	// 802.1Q VLAN tag ID to match (Range: 1 to 4094). Required if matchType is "vlan". [OPTIONAL]
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=4094
	// +optional
	VlanID int `json:"vlanId,omitempty"`

	// Subnet (CIDR) to match untagged or stripped traffic (e.g., "10.200.0.0/24"). Required if matchType is "subnet". [OPTIONAL]
	// +optional
	Subnet string `json:"subnet,omitempty"`

	// Socket buffer mark (skbmark) set by OVS/iptables (e.g., 100 or 16). Required if matchType is "mark". [OPTIONAL]
	// +optional
	Mark uint32 `json:"mark,omitempty"`

	// Guaranteed outbound bandwidth rate (e.g., "50Mbit", "1Gbit"). [REQUIRED]
	// +kubebuilder:validation:Required
	EgressRate string `json:"egressRate"`

	// Maximum allowed outbound burst bandwidth ceiling (e.g., "100Mbit", "2Gbit"). [OPTIONAL]
	// +optional
	EgressCeil string `json:"egressCeil,omitempty"`

	// Outbound burst buffer size (Units: KB or Bytes, e.g., "15k"). [OPTIONAL]
	// +optional
	EgressBurst string `json:"egressBurst,omitempty"`

	// Hard policing bandwidth cap for incoming traffic (e.g., "20Mbit"). [OPTIONAL]
	// +optional
	IngressRate string `json:"ingressRate,omitempty"`

	// Incoming policing burst buffer size (Units: KB or Bytes, e.g., "10k"). [OPTIONAL]
	// +optional
	IngressBurst string `json:"ingressBurst,omitempty"`

	// HTB Priority level (0=Highest Priority, 7=Lowest Priority). [OPTIONAL]
	// +kubebuilder:default=0
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=7
	// +optional
	Priority int `json:"priority,omitempty"`

	// Toggles attaching fq_codel leaf qdisc to prevent bufferbloat. [OPTIONAL]
	// +kubebuilder:default=true
	// +optional
	EnableFqCodel bool `json:"enableFqCodel"`
}

// GetClassMinor returns the integer minor class ID, parsing ClassID ("1:10") if ClassMinor isn't directly specified.
func (c *VlanClassSpec) GetClassMinor() int {
	if c.ClassMinor > 0 {
		return c.ClassMinor
	}
	if c.ClassID != "" {
		parts := strings.Split(c.ClassID, ":")
		if len(parts) == 2 {
			if val, err := strconv.Atoi(parts[1]); err == nil {
				return val
			}
		}
	}
	return 0
}

// GetClassID returns the formatted class ID string (e.g., "1:10").
func (c *VlanClassSpec) GetClassID(rootHtbID int) string {
	if c.ClassID != "" {
		return c.ClassID
	}
	if rootHtbID <= 0 {
		rootHtbID = 1
	}
	return fmt.Sprintf("%d:%d", rootHtbID, c.GetClassMinor())
}

// HtbClassSpec defines HTB class execution parameters used internally by executor engines.
type HtbClassSpec struct {
	ClassID       string         `json:"classId"`
	ClassMinor    int            `json:"classMinor,omitempty"`
	VlanID        int            `json:"vlanId,omitempty"`
	Subnet        string         `json:"subnet,omitempty"`
	Mark          uint32         `json:"mark,omitempty"`
	Match         ClassifierType `json:"matchType,omitempty"`
	Priority      int            `json:"priority,omitempty"`
	EgressRate    string         `json:"egressRate,omitempty"`
	EgressCeil    string         `json:"egressCeil,omitempty"`
	EgressBurst   string         `json:"egressBurst,omitempty"`
	IngressRate   string         `json:"ingressRate,omitempty"`
	IngressBurst  string         `json:"ingressBurst,omitempty"`
	EnableFqCodel bool           `json:"enableFqCodel,omitempty"`
}

// HtbRootSpec defines root HTB settings and target host interface.
type HtbRootSpec struct {
	// Target physical, bond, or bridge network interface name (e.g., "enp1s0", "br-ex"). [REQUIRED]
	// +kubebuilder:validation:Required
	Interface string `json:"interface"`

	// Total root egress bandwidth rate capacity (e.g., "10Gbit"). [REQUIRED]
	// +kubebuilder:validation:Required
	Rate string `json:"rate"`

	// Default class minor ID where unclassified traffic is routed (e.g., 99 for handle X:99). [OPTIONAL]
	// +kubebuilder:default=99
	// +optional
	DefaultClassMinor int `json:"defaultClassMinor,omitempty"`

	// Default class ID where unclassified traffic is routed (e.g., "1:99"). [OPTIONAL]
	// +kubebuilder:default="1:99"
	// +optional
	DefaultClassID string `json:"defaultClassId,omitempty"`

	// Custom HTB root handle ID (e.g., 1 for handle 1:0). [OPTIONAL]
	// +kubebuilder:default=1
	// +optional
	HtbID int `json:"htbId,omitempty"`

	// List of traffic control classes configured on this interface. [REQUIRED]
	// +kubebuilder:validation:Required
	Classes []VlanClassSpec `json:"classes"`
}

// GetDefaultClassMinor returns DefaultClassMinor, parsing DefaultClassID ("1:99") if DefaultClassMinor isn't directly specified.
func (r *HtbRootSpec) GetDefaultClassMinor() int {
	if r.DefaultClassMinor > 0 {
		return r.DefaultClassMinor
	}
	if r.DefaultClassID != "" {
		parts := strings.Split(r.DefaultClassID, ":")
		if len(parts) == 2 {
			if val, err := strconv.Atoi(parts[1]); err == nil {
				return val
			}
		}
	}
	return 99
}

// VlanTrafficControlSpec defines the desired state of VlanTrafficControl.
type VlanTrafficControlSpec struct {
	// Map of node labels used to select target worker nodes. [OPTIONAL]
	// +optional
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`

	// Interval in seconds between node agent reconciliation loops. [OPTIONAL]
	// +kubebuilder:default=30
	// +optional
	ReconcileIntervalSeconds int `json:"reconcileIntervalSeconds,omitempty"`

	// Strategy used to classify traffic: "flower", "u32", or "auto". [REQUIRED]
	// +kubebuilder:validation:Enum=flower;u32;auto
	// +kubebuilder:default="flower"
	TcStrategy TcStrategyType `json:"tcStrategy"`

	// Root HTB configuration parameters. [REQUIRED]
	HtbRoot HtbRootSpec `json:"htbRoot"`
}

// VlanTrafficControlStatus defines the observed runtime status and conditions aggregated across worker node agents.
type VlanTrafficControlStatus struct {
	ObservedGeneration int64              `json:"observedGeneration,omitempty"`
	Conditions         []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster

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
