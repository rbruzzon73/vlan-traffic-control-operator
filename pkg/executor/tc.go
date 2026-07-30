package executor

import (
	"fmt"
	"os/exec"
	"strings"

	"github.com/go-logr/logr"
	networkingv1alpha1 "networking.med.io/vlan-traffic-control/api/v1alpha1"
)

// ResolveClassifier selects the appropriate matching arguments for tc flower
func ResolveClassifier(cls networkingv1alpha1.VlanClassSpec, rootHandle int) (protocol string, flowerMatch []string, desc string, err error) {
	vlanID := cls.VlanID
	classHandle := fmt.Sprintf("%d:%d", rootHandle, cls.ClassMinor)

	matchType := cls.MatchType
	if matchType == "" || matchType == networkingv1alpha1.ClassifierAuto {
		if vlanID > 0 {
			matchType = networkingv1alpha1.ClassifierVlan
		} else if cls.Subnet != "" {
			matchType = networkingv1alpha1.ClassifierSubnet
		} else if cls.Mark > 0 {
			matchType = networkingv1alpha1.ClassifierMark
		}
	}

	switch matchType {
	case networkingv1alpha1.ClassifierVlan:
		if vlanID <= 0 {
			return "", nil, "", fmt.Errorf("matchType 'vlan' requested for class %s but vlanId is missing", classHandle)
		}
		return "802.1Q", []string{"vlan_id", fmt.Sprintf("%d", vlanID)}, fmt.Sprintf("VLAN ID %d", vlanID), nil

	case networkingv1alpha1.ClassifierSubnet:
		if cls.Subnet == "" {
			return "", nil, "", fmt.Errorf("matchType 'subnet' requested for class %s but subnet is missing", classHandle)
		}
		return "ip", []string{"src_ip", cls.Subnet}, fmt.Sprintf("Subnet %s", cls.Subnet), nil

	case networkingv1alpha1.ClassifierMark:
		if cls.Mark == 0 {
			return "", nil, "", fmt.Errorf("matchType 'mark' requested for class %s but mark is missing", classHandle)
		}
		return "all", []string{"mark", fmt.Sprintf("%d", cls.Mark)}, fmt.Sprintf("SKB Mark %d", cls.Mark), nil

	default:
		return "", nil, "", fmt.Errorf("no valid classifier criteria found for class %s (must specify vlanId, subnet, or mark)", classHandle)
	}
}

// ApplyAdaptiveHtbHierarchy configures HTB qdisc, classes, and filters on the specified interface
func ApplyAdaptiveHtbHierarchy(root *networkingv1alpha1.HtbRootSpec, strategy networkingv1alpha1.TcStrategyType, log logr.Logger) (string, error) {
	if root == nil {
		return "", fmt.Errorf("root HTB spec is nil")
	}

	iface := root.Interface
	rootHandle := root.HtbID
	if rootHandle <= 0 {
		rootHandle = 1
	}

	htbHandle := fmt.Sprintf("%d:", rootHandle)
	rootClassHandle := fmt.Sprintf("%d:1", rootHandle)

	// 1. Ensure egress root qdisc exists
	if !isQdiscPresent(iface, "root", log) {
		defaultMinor := root.DefaultClassMinor
		if defaultMinor <= 0 {
			defaultMinor = 99
		}

		cmdQdisc := []string{"tc", "qdisc", "add", "dev", iface, "root", "handle", htbHandle, "htb", "default", fmt.Sprintf("%d", defaultMinor)}
		if err := execHostCmd(log, "tc", cmdQdisc...); err != nil {
			return string(strategy), fmt.Errorf("failed adding root htb qdisc on %s: %w", iface, err)
		}

		cmdRootClass := []string{"tc", "class", "add", "dev", iface, "parent", htbHandle, "classid", rootClassHandle, "htb", "rate", root.Rate}
		if err := execHostCmd(log, "tc", cmdRootClass...); err != nil {
			return string(strategy), fmt.Errorf("failed adding root class %s on %s: %w", rootClassHandle, iface, err)
		}
	}

	// 2. Ensure ingress qdisc exists for rate policing
	if !isQdiscPresent(iface, "ingress", log) {
		cmdIngress := []string{"tc", "qdisc", "add", "dev", iface, "handle", "ffff:", "ingress"}
		_ = execHostCmd(log, "tc", cmdIngress...)
	}

	// 3. Process child traffic classes
	for _, cls := range root.Classes {
		classHandle := fmt.Sprintf("%d:%d", rootHandle, cls.ClassMinor)
		minorHandle := fmt.Sprintf("%d:", cls.ClassMinor)

		// Create/Update HTB class
		cmdClass := []string{"tc", "class", "replace", "dev", iface, "parent", rootClassHandle, "classid", classHandle, "htb", "rate", cls.EgressRate}
		if cls.EgressCeil != "" {
			cmdClass = append(cmdClass, "ceil", cls.EgressCeil)
		}
		if cls.EgressBurst != "" {
			cmdClass = append(cmdClass, "burst", cls.EgressBurst)
		}
		if cls.Priority > 0 {
			cmdClass = append(cmdClass, "prio", fmt.Sprintf("%d", cls.Priority))
		}

		if err := execHostCmd(log, "tc", cmdClass...); err != nil {
			log.Error(err, "[TC] Failed applying HTB class", "interface", iface, "classHandle", classHandle)
			continue
		}

		// Attach fq_codel leaf qdisc if enabled
		if cls.EnableFqCodel {
			cmdLeaf := []string{"tc", "qdisc", "replace", "dev", iface, "parent", classHandle, "handle", minorHandle, "fq_codel"}
			_ = execHostCmd(log, "tc", cmdLeaf...)
		}

		// Egress Classification Filter
		_ = ApplyClassEgressFilter(iface, cls, rootHandle, log)

		// Ingress Rate Policing Filter
		_ = ApplyClassIngressPolice(iface, cls, rootHandle, log)
	}

	return string(strategy), nil
}

// ApplyClassEgressFilter applies flower egress classification filter
func ApplyClassEgressFilter(iface string, cls networkingv1alpha1.VlanClassSpec, rootHandle int, log logr.Logger) error {
	classHandle := fmt.Sprintf("%d:%d", rootHandle, cls.ClassMinor)
	filterParentStr := fmt.Sprintf("%d:", rootHandle)

	proto, matchArgs, desc, err := ResolveClassifier(cls, rootHandle)
	if err != nil {
		return err
	}

	log.Info("[TC] Applying egress filter", "interface", iface, "strategy", desc, "targetClass", classHandle)

	args := []string{"tc", "filter", "replace", "dev", iface, "parent", filterParentStr, "prio", "1", "protocol", proto, "flower"}
	args = append(args, matchArgs...)
	args = append(args, "action", "goto", "chain", "0", "classid", classHandle)

	return execHostCmd(log, "tc", args...)
}

// ApplyClassIngressPolice applies flower ingress policing filter
func ApplyClassIngressPolice(iface string, cls networkingv1alpha1.VlanClassSpec, rootHandle int, log logr.Logger) error {
	if cls.IngressRate == "" {
		return nil
	}

	proto, matchArgs, desc, err := ResolveClassifier(cls, rootHandle)
	if err != nil {
		return err
	}

	// For ingress subnet matching, match on dst_ip instead of src_ip
	if cls.MatchType == networkingv1alpha1.ClassifierSubnet || (cls.Subnet != "" && proto == "ip") {
		matchArgs = []string{"dst_ip", cls.Subnet}
	}

	log.Info("[TC] Applying ingress policing filter", "interface", iface, "strategy", desc, "rate", cls.IngressRate)

	args := []string{"tc", "filter", "replace", "dev", iface, "parent", "ffff:", "prio", "1", "protocol", proto, "flower"}
	args = append(args, matchArgs...)
	args = append(args, "action", "police", "rate", cls.IngressRate, "burst", "100k", "drop")

	return execHostCmd(log, "tc", args...)
}

// Helper: Check if a qdisc type exists on interface
func isQdiscPresent(iface string, qdiscType string, log logr.Logger) bool {
	cmd := exec.Command("chroot", "/host", "tc", "qdisc", "show", "dev", iface)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return false
	}
	return strings.Contains(string(out), qdiscType)
}

// Helper: Run command on host network namespace
func execHostCmd(log logr.Logger, name string, args ...string) error {
	fullArgs := append([]string{"/host", name}, args...)
	cmd := exec.Command("chroot", fullArgs...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		log.Error(err, "[EXEC-FAIL]", "cmd", strings.Join(append([]string{name}, args...), " "), "output", strings.TrimSpace(string(out)))
		return err
	}
	return nil
}

// FormatClassID formats a minor ID or string handle safely as rootHandle:minor
func FormatClassID(rootHandle int, classMinor int) string {
	return fmt.Sprintf("%d:%d", rootHandle, classMinor)
}
