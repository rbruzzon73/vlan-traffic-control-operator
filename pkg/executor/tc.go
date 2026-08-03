package executor

import (
	"fmt"
	"strings"

	"github.com/go-logr/logr"
	networkingv1alpha1 "networking.med.io/vlan-traffic-control/api/v1alpha1"
)

// Helper to resolve ClassMinor safely with fallback to parsing ClassID string
func getClassMinor(cls networkingv1alpha1.ClassSpec) int {
	if cls.ClassMinor > 0 {
		return cls.ClassMinor
	}
	if cls.ClassID != "" {
		var minor int
		if idx := strings.Index(cls.ClassID, ":"); idx != -1 {
			_, _ = fmt.Sscanf(cls.ClassID[idx+1:], "%d", &minor)
		} else {
			_, _ = fmt.Sscanf(cls.ClassID, "%d", &minor)
		}
		if minor > 0 {
			return minor
		}
	}
	return 0
}

// ResolveClassifier selects the appropriate matching arguments for tc flower
func ResolveClassifier(cls networkingv1alpha1.ClassSpec, rootHandle int) (protocol string, flowerMatch []string, desc string, priority int, err error) {
	vlanID := cls.VlanID
	classMinor := getClassMinor(cls)
	classHandle := fmt.Sprintf("%d:%d", rootHandle, classMinor)
	prio := cls.Priority
	if prio <= 0 {
		prio = 100
	}

	matchType := strings.ToLower(string(cls.MatchType))

	if matchType == "" || matchType == "auto" {
		if vlanID > 0 {
			matchType = "vlan"
		} else if cls.Subnet != "" {
			matchType = "subnet"
		}
	}

	switch matchType {
	case "vlan":
		if vlanID <= 0 {
			return "", nil, "", 0, fmt.Errorf("vlan classifier specified but vlanId is missing for class %s", classHandle)
		}
		return "802.1q", []string{"vlan_id", fmt.Sprintf("%d", vlanID)}, fmt.Sprintf("VLAN %d", vlanID), prio, nil

	case "subnet":
		if cls.Subnet == "" {
			return "", nil, "", 0, fmt.Errorf("subnet classifier specified but subnet is missing for class %s", classHandle)
		}
		return "ip", []string{"src_ip", cls.Subnet}, fmt.Sprintf("Subnet %s", cls.Subnet), prio, nil

	default:
		return "", nil, "", 0, fmt.Errorf("no classifier rule matched for class %s (vlanId=%d, subnet=%s)", classHandle, vlanID, cls.Subnet)
	}
}

// ApplyAdaptiveHtbHierarchy delegates to ApplyHtbHierarchy
func ApplyAdaptiveHtbHierarchy(spec *networkingv1alpha1.HtbRootSpec, strategy networkingv1alpha1.TcStrategyType, log logr.Logger) (string, error) {
	err := ApplyHtbHierarchy(spec, log)
	return string(strategy), err
}
