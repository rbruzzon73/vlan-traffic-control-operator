package executor

import (
	"fmt"
	"net"
	"os/exec"
	"strconv"
	"strings"

	"github.com/go-logr/logr"
	"github.com/vishvananda/netlink"
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

// ResolveClassifier selects the appropriate matching arguments for tc flower / fw mark
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
		} else if cls.Mark > 0 {
			matchType = "mark"
		}
	}

	switch matchType {
	case "vlan":
		if vlanID <= 0 {
			return "", nil, "", prio, fmt.Errorf("vlan classifier specified but vlanId is missing for class %s", classHandle)
		}
		return "802.1q", []string{"vlan_id", fmt.Sprintf("%d", vlanID)}, fmt.Sprintf("VLAN %d", vlanID), prio, nil

	case "subnet":
		if cls.Subnet == "" {
			return "", nil, "", prio, fmt.Errorf("subnet classifier specified but subnet is missing for class %s", classHandle)
		}
		return "ip", []string{"src_ip", cls.Subnet}, fmt.Sprintf("Subnet %s", cls.Subnet), prio, nil

	case "mark":
		if cls.Mark == 0 {
			return "", nil, "", prio, fmt.Errorf("mark classifier specified but mark is missing for class %s", classHandle)
		}
		return "all", []string{"handle", fmt.Sprintf("%d", cls.Mark), "fw"}, fmt.Sprintf("SKB Mark %d", cls.Mark), prio, nil

	default:
		return "", nil, "", prio, fmt.Errorf("no classifier rule matched for class %s (vlanId=%d, subnet=%s, mark=%d)", classHandle, vlanID, cls.Subnet, cls.Mark)
	}
}

// ApplyAdaptiveHtbHierarchy delegates to ApplyHtbHierarchy and reconciles ingress policing rules
func ApplyAdaptiveHtbHierarchy(spec *networkingv1alpha1.HtbRootSpec, strategy networkingv1alpha1.TcStrategyType, log logr.Logger) (string, error) {
	// 1. Apply Egress HTB Hierarchy
	err := ApplyHtbHierarchy(spec, log)
	if err != nil {
		return string(strategy), err
	}

	// 2. Reconcile Ingress Policing Rules
	if spec != nil && spec.Interface != "" {
		link, err := netlink.LinkByName(spec.Interface)
		if err != nil {
			log.Error(err, "[TC] Failed to get link for ingress reconciliation", "interface", spec.Interface)
			return string(strategy), err
		}

		if err := ReconcileIngressRules(link, spec, log); err != nil {
			log.Error(err, "[TC] Failed reconciling ingress policing rules", "interface", spec.Interface)
			return string(strategy), err
		}
	}

	return string(strategy), nil
}

// ReconcileIngressRules manages ingress qdisc and policing filters reliably via direct TC commands.
func ReconcileIngressRules(link netlink.Link, spec *networkingv1alpha1.HtbRootSpec, log logr.Logger) error {
	if spec == nil || spec.Interface == "" {
		return nil
	}

	iface := spec.Interface
	activeIngressClasses := make(map[uint16]networkingv1alpha1.ClassSpec)

	for _, cls := range spec.Classes {
		if cls.IngressRate != "" {
			prio := uint16(cls.Priority)
			if prio == 0 {
				prio = 1
			}
			activeIngressClasses[prio] = cls
		}
	}

	// 1. If no active ingress rules exist, clean up ingress qdisc
	if len(activeIngressClasses) == 0 {
		cmd := exec.Command("tc", "qdisc", "del", "dev", iface, "ingress")
		_ = cmd.Run() // Ignore error if ingress qdisc didn't exist
		return nil
	}

	// 2. Ensure ingress qdisc exists (ffff:)
	cmdAddQdisc := exec.Command("tc", "qdisc", "add", "dev", iface, "ingress")
	_ = cmdAddQdisc.Run() // Ignore error if qdisc already exists

	// 3. Remove all existing ingress filters to ensure a clean state
	cmdFlush := exec.Command("tc", "filter", "del", "dev", iface, "ingress")
	_ = cmdFlush.Run()

	// 4. Add fresh policing filters for each configured class
	for prio, cls := range activeIngressClasses {
		rateBytes, err := ParseRateToBytesPerSec(cls.IngressRate)
		if err != nil {
			log.Error(err, "[TC] Invalid ingressRate string", "classId", cls.GetClassID(spec.HtbID), "rate", cls.IngressRate)
			continue
		}

		burstBytes := uint64(50000)
		if cls.IngressBurst != "" {
			if b, err := ParseBurstToBytes(cls.IngressBurst); err == nil && b > 0 {
				burstBytes = b
			}
		} else if cls.EgressBurst != "" {
			if b, err := ParseBurstToBytes(cls.EgressBurst); err == nil && b > 0 {
				burstBytes = b
			}
		}

		// Use ResolveClassifier to determine protocol, match args, and filter type
		protocol, matchArgs, _, _, err := ResolveClassifier(cls, spec.HtbID)
		if err != nil {
			log.Error(err, "[TC] Failed resolving classifier for ingress rule", "classId", cls.GetClassID(spec.HtbID))
			continue
		}

		// Base args: tc filter add dev <iface> parent ffff:
		args := []string{"filter", "add", "dev", iface, "parent", "ffff:", "protocol", protocol, "prio", strconv.Itoa(int(prio))}

		// Handle 'fw' (Firewall Mark) vs 'flower' (VLAN/Subnet) classifier differences
		if len(matchArgs) >= 3 && matchArgs[0] == "handle" && matchArgs[2] == "fw" {
			// Constructs: tc filter add dev <iface> parent ffff: protocol all prio <prio> handle <mark> fw action police ...
			args = append(args, "handle", matchArgs[1], "fw")
		} else {
			// Constructs: tc filter add dev <iface> parent ffff: protocol <proto> prio <prio> flower ...
			args = append(args, "flower")

			if cls.VlanID > 0 {
				args = append(args, "vlan_id", strconv.Itoa(cls.VlanID))
			}

			if cls.Subnet != "" {
				_, cidr, err := net.ParseCIDR(cls.Subnet)
				if err == nil && cidr != nil {
					args = append(args, "dst_ip", cls.Subnet)
				}
			}
		}

		// Determine ingress action ("drop" or "pass")
		action := strings.ToLower(string(cls.IngressAction))
		if action == "" {
			action = "drop"
		}

		// Append police action parameters
		rateStr := fmt.Sprintf("%dbps", rateBytes)
		burstStr := fmt.Sprintf("%db", burstBytes)
		args = append(args, "action", "police", "rate", rateStr, "burst", burstStr, action)

		out, err := exec.Command("tc", args...).CombinedOutput()
		if err != nil {
			log.Error(err, "[TC] Failed adding ingress policing filter via tc CLI", "interface", iface, "prio", prio, "args", strings.Join(args, " "), "output", string(out))
			return fmt.Errorf("tc filter add failed: %w (output: %s)", err, string(out))
		}

		log.Info("✅ [TC] Reconciled ingress policing filter", "interface", iface, "classId", cls.GetClassID(spec.HtbID), "rate", cls.IngressRate)
	}

	return nil
}

// ParseRateToBytesPerSec converts TC human-readable rate strings (e.g. 50Mbit, 10Gbit, 100kbit) to bytes/sec
func ParseRateToBytesPerSec(rateStr string) (uint64, error) {
	s := strings.TrimSpace(strings.ToLower(rateStr))
	if s == "" {
		return 0, fmt.Errorf("empty rate string")
	}

	var multiplier uint64 = 1
	if strings.HasSuffix(s, "gbit") {
		multiplier = 1000 * 1000 * 1000 / 8
		s = strings.TrimSuffix(s, "gbit")
	} else if strings.HasSuffix(s, "mbit") {
		multiplier = 1000 * 1000 / 8
		s = strings.TrimSuffix(s, "mbit")
	} else if strings.HasSuffix(s, "kbit") {
		multiplier = 1000 / 8
		s = strings.TrimSuffix(s, "kbit")
	} else if strings.HasSuffix(s, "bps") {
		multiplier = 1 / 8
		s = strings.TrimSuffix(s, "bps")
	}

	val, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return 0, err
	}
	return val * multiplier, nil
}

// ParseBurstToBytes converts burst strings (e.g. 15k, 50k, 1M) to raw bytes
func ParseBurstToBytes(burstStr string) (uint64, error) {
	s := strings.TrimSpace(strings.ToLower(burstStr))
	if s == "" {
		return 0, fmt.Errorf("empty burst string")
	}

	var multiplier uint64 = 1
	if strings.HasSuffix(s, "m") {
		multiplier = 1024 * 1024
		s = strings.TrimSuffix(s, "m")
	} else if strings.HasSuffix(s, "k") {
		multiplier = 1024
		s = strings.TrimSuffix(s, "k")
	} else if strings.HasSuffix(s, "b") {
		multiplier = 1
		s = strings.TrimSuffix(s, "b")
	}

	val, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return 0, err
	}
	return val * multiplier, nil
}
