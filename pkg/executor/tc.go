package executor

import (
	"fmt"
	"os/exec"
	"strings"

	"github.com/go-logr/logr"
	networkingv1alpha1 "networking.med.io/vlan-traffic-control/api/v1alpha1"
)

// Helper to resolve ClassMinor safely with fallback to parsing legacy ClassID string
func getClassMinor(cls networkingv1alpha1.VlanClassSpec) int {
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

// ResolveClassifier selects the appropriate matching arguments for tc flower / fw
func ResolveClassifier(cls networkingv1alpha1.VlanClassSpec, rootHandle int) (filterType string, protocol string, matchArgs []string, desc string, err error) {
	vlanID := cls.VlanID
	classMinor := getClassMinor(cls)
	classHandle := fmt.Sprintf("%d:%d", rootHandle, classMinor)

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
			return "", "", nil, "", fmt.Errorf("matchType 'vlan' requested for class %s but vlanId is missing", classHandle)
		}
		return "flower", "802.1Q", []string{"vlan_id", fmt.Sprintf("%d", vlanID)}, fmt.Sprintf("VLAN ID %d", vlanID), nil

	case networkingv1alpha1.ClassifierSubnet:
		if cls.Subnet == "" {
			return "", "", nil, "", fmt.Errorf("matchType 'subnet' requested for class %s but subnet is missing", classHandle)
		}
		return "flower", "ip", []string{"src_ip", cls.Subnet}, fmt.Sprintf("Subnet %s", cls.Subnet), nil

	case networkingv1alpha1.ClassifierMark:
		if cls.Mark == 0 {
			return "", "", nil, "", fmt.Errorf("matchType 'mark' requested for class %s but mark is missing", classHandle)
		}
		return "fw", "all", []string{"handle", fmt.Sprintf("%d", cls.Mark)}, fmt.Sprintf("SKB Mark %d", cls.Mark), nil

	default:
		return "", "", nil, "", fmt.Errorf("no valid classifier criteria found for class %s (must specify vlanId, subnet, or mark)", classHandle)
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

	// Resolve default fallback minor class handle
	defaultMinor := root.DefaultClassMinor
	if defaultMinor <= 0 && root.DefaultClassID != "" {
		if idx := strings.Index(root.DefaultClassID, ":"); idx != -1 {
			_, _ = fmt.Sscanf(root.DefaultClassID[idx+1:], "%d", &defaultMinor)
		}
	}
	if defaultMinor <= 0 {
		defaultMinor = 99
	}

	// 1. Ensure egress root HTB qdisc exists
	if !isQdiscPresent(iface, "htb", log) {
		// Delete any existing filters attached to root (1:) to clear protocol locks
		_, _ = execHostCmdSilent("filter", "del", "dev", iface, "parent", "1:")

		// Delete any non-HTB default root qdisc first (e.g. fq_codel, mq, pfifo_fast)
		_, _ = execHostCmdSilent("qdisc", "del", "dev", iface, "root")

		cmdQdisc := []string{"qdisc", "add", "dev", iface, "root", "handle", htbHandle, "htb", "default", fmt.Sprintf("%d", defaultMinor)}
		if _, err := execHostCmd(log, "EGRESS", cmdQdisc...); err != nil {
			return string(strategy), fmt.Errorf("failed adding root htb qdisc on %s: %w", iface, err)
		}
		log.Info("🟢 [TC EGRESS OK] Added root HTB qdisc", "direction", "EGRESS", "interface", iface, "handle", htbHandle)
	}

	// 1b. ALWAYS ensure parent class (1:1) exists
	cmdRootClass := []string{"class", "replace", "dev", iface, "parent", htbHandle, "classid", rootClassHandle, "htb", "rate", root.Rate}
	if _, err := execHostCmd(log, "EGRESS", cmdRootClass...); err != nil {
		return string(strategy), fmt.Errorf("failed adding root parent class %s on %s: %w", rootClassHandle, iface, err)
	}

	// 1c. ALWAYS ensure Default Fallback Class (1:99) exists with Priority 0
	defaultClassHandle := fmt.Sprintf("%d:%d", rootHandle, defaultMinor)
	cmdDefaultClass := []string{"class", "replace", "dev", iface, "parent", rootClassHandle, "classid", defaultClassHandle, "htb", "prio", "0", "rate", "1Mbit", "ceil", root.Rate}
	if _, err := execHostCmd(log, "EGRESS", cmdDefaultClass...); err != nil {
		log.Info("⚠️ [TC EGRESS WARNING] Failed creating default fallback class", "direction", "EGRESS", "classHandle", defaultClassHandle, "error", err.Error())
	}

	// 2. Ensure ingress qdisc exists for rate policing
	if !isQdiscPresent(iface, "ingress", log) {
		cmdIngress := []string{"qdisc", "add", "dev", iface, "handle", "ffff:", "ingress"}
		if _, err := execHostCmd(log, "INGRESS", cmdIngress...); err != nil {
			log.Info("⚠️ [TC INGRESS WARNING] Failed adding ingress qdisc", "direction", "INGRESS", "interface", iface, "error", err.Error())
		} else {
			log.Info("🟢 [TC INGRESS OK] Added ingress qdisc", "direction", "INGRESS", "interface", iface, "handle", "ffff:")
		}
	}

	// 3. Process child traffic classes
	for _, cls := range root.Classes {
		classMinor := getClassMinor(cls)
		classHandle := fmt.Sprintf("%d:%d", rootHandle, classMinor)
		minorHandle := fmt.Sprintf("%d:", classMinor)

		// Create/Update HTB class under parent 1:1
		cmdClass := []string{"class", "replace", "dev", iface, "parent", rootClassHandle, "classid", classHandle, "htb", "rate", cls.EgressRate}
		if cls.EgressCeil != "" {
			cmdClass = append(cmdClass, "ceil", cls.EgressCeil)
		}
		if cls.EgressBurst != "" {
			cmdClass = append(cmdClass, "burst", cls.EgressBurst)
		}
		if cls.Priority > 0 {
			cmdClass = append(cmdClass, "prio", fmt.Sprintf("%d", cls.Priority))
		}

		if _, err := execHostCmd(log, "EGRESS", cmdClass...); err != nil {
			log.Error(err, "🔴 [TC EGRESS ERROR] Failed applying HTB class", "direction", "EGRESS", "interface", iface, "classHandle", classHandle)
			return string(strategy), fmt.Errorf("failed applying HTB class %s: %w", classHandle, err)
		}

		// Attach fq_codel leaf qdisc if enabled
		if cls.EnableFqCodel {
			_, _ = execHostCmdSilent("qdisc", "replace", "dev", iface, "parent", classHandle, "handle", minorHandle, "fq_codel")
		}

		// Egress Classification Filter
		if err := ApplyClassEgressFilter(iface, cls, rootHandle, log); err != nil {
			log.Error(err, "🔴 [TC EGRESS ERROR] Failed applying egress filter", "direction", "EGRESS", "interface", iface, "classHandle", classHandle)
		}

		// Ingress Rate Policing Filter
		if err := ApplyClassIngressPolice(iface, cls, rootHandle, log); err != nil {
			log.Error(err, "🔴 [TC INGRESS ERROR] Failed applying ingress policing filter", "direction", "INGRESS", "interface", iface, "classHandle", classHandle)
		}
	}

	return string(strategy), nil
}

// ApplyClassEgressFilter applies classification filter
func ApplyClassEgressFilter(iface string, cls networkingv1alpha1.VlanClassSpec, rootHandle int, log logr.Logger) error {
	classMinor := getClassMinor(cls)
	classHandle := fmt.Sprintf("%d:%d", rootHandle, classMinor)
	filterParentStr := fmt.Sprintf("%d:", rootHandle)

	filterType, proto, matchArgs, desc, err := ResolveClassifier(cls, rootHandle)
	if err != nil {
		return err
	}

	prioVal := cls.Priority
	if prioVal <= 0 {
		prioVal = classMinor
	}
	prioStr := fmt.Sprintf("%d", prioVal)

	// AUTO-CLEANUP STALE FILTERS:
	// Flush ANY filter at this priority across all protocol layers (802.1q, ip, all)
	// so protocol mismatches (e.g., switching from 802.1q to IP) never throw RTNETLINK errors.
	for _, p := range []string{proto, "802.1q", "ip", "all"} {
		_, _ = execHostCmdSilent("filter", "del", "dev", iface, "parent", filterParentStr, "prio", prioStr, "protocol", p)
	}

	var args []string
	if filterType == "fw" {
		args = []string{"filter", "add", "dev", iface, "parent", filterParentStr, "prio", prioStr, "protocol", proto}
		args = append(args, matchArgs...)
		args = append(args, "fw", "classid", classHandle)
	} else {
		args = []string{"filter", "add", "dev", iface, "parent", filterParentStr, "prio", prioStr, "protocol", proto, "flower"}
		args = append(args, matchArgs...)
		args = append(args, "classid", classHandle)
	}

	out, err := execHostCmd(log, "EGRESS", args...)
	if err != nil {
		return err
	}

	log.Info("🟢 [TC EGRESS OK] Applied egress filter successfully",
		"direction", "EGRESS",
		"interface", iface,
		"targetClass", classHandle,
		"strategy", desc,
		"priority", prioVal,
		"protocol", proto,
		"output", out,
	)
	return nil
}

// ApplyClassIngressPolice applies ingress policing filter
func ApplyClassIngressPolice(iface string, cls networkingv1alpha1.VlanClassSpec, rootHandle int, log logr.Logger) error {
	if cls.IngressRate == "" {
		return nil
	}

	filterType, proto, matchArgs, desc, err := ResolveClassifier(cls, rootHandle)
	if err != nil {
		return err
	}

	if cls.MatchType == networkingv1alpha1.ClassifierSubnet || (cls.Subnet != "" && proto == "ip") {
		matchArgs = []string{"dst_ip", cls.Subnet}
	}

	prioVal := cls.Priority
	if prioVal <= 0 {
		prioVal = getClassMinor(cls)
	}
	prioStr := fmt.Sprintf("%d", prioVal)

	ingressBurst := cls.IngressBurst
	if ingressBurst == "" {
		ingressBurst = "100k"
	}

	// AUTO-CLEANUP STALE FILTERS:
	// Flush ANY ingress filter at this priority across all potential protocols
	for _, p := range []string{proto, "ip", "802.1q", "all"} {
		_, _ = execHostCmdSilent("filter", "del", "dev", iface, "parent", "ffff:", "prio", prioStr, "protocol", p)
	}

	var args []string
	if filterType == "fw" {
		args = []string{"filter", "add", "dev", iface, "parent", "ffff:", "prio", prioStr, "protocol", proto}
		args = append(args, matchArgs...)
		args = append(args, "fw", "action", "police", "rate", cls.IngressRate, "burst", ingressBurst, "drop")
	} else {
		args = []string{"filter", "add", "dev", iface, "parent", "ffff:", "prio", prioStr, "protocol", proto, "flower"}
		args = append(args, matchArgs...)
		args = append(args, "action", "police", "rate", cls.IngressRate, "burst", ingressBurst, "drop")
	}

	out, err := execHostCmd(log, "INGRESS", args...)
	if err != nil {
		return err
	}

	log.Info("🟢 [TC INGRESS OK] Applied ingress policing filter successfully",
		"direction", "INGRESS",
		"interface", iface,
		"strategy", desc,
		"rate", cls.IngressRate,
		"burst", ingressBurst,
		"priority", prioVal,
		"protocol", proto,
		"output", out,
	)
	return nil
}

// Helper: Check if a specific qdisc type (e.g. "htb", "ingress") exists on interface
func isQdiscPresent(iface string, qdiscType string, log logr.Logger) bool {
	cmd := exec.Command("chroot", "/host", "tc", "qdisc", "show", "dev", iface)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return false
	}
	return strings.Contains(string(out), qdiscType)
}

// Helper: Run command on host network namespace with error logging, stderr capture, and direction metadata
func execHostCmd(log logr.Logger, direction string, args ...string) (string, error) {
	fullCmdStr := "tc " + strings.Join(args, " ")
	fullArgs := append([]string{"/host", "tc"}, args...)
	cmd := exec.Command("chroot", fullArgs...)

	out, err := cmd.CombinedOutput()
	outputStr := strings.TrimSpace(string(out))

	if err != nil {
		log.Error(err, fmt.Sprintf("🔴 [TC %s ERROR] Command execution failed", direction),
			"direction", direction,
			"command", fullCmdStr,
			"stderr", outputStr,
		)
		return outputStr, fmt.Errorf("command '%s' failed (%w): %s", fullCmdStr, err, outputStr)
	}
	return outputStr, nil
}

// Helper: Run command silently without logging errors (for pre-cleanups)
func execHostCmdSilent(args ...string) (string, error) {
	fullArgs := append([]string{"/host", "tc"}, args...)
	cmd := exec.Command("chroot", fullArgs...)
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// FormatClassID formats a minor ID or string handle safely as rootHandle:minor
func FormatClassID(rootHandle int, classMinor int) string {
	return fmt.Sprintf("%d:%d", rootHandle, classMinor)
}
