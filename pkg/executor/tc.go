package executor

import (
	"fmt"
	"os/exec"

	"github.com/go-logr/logr"
	networkingv1alpha1 "networking.med.io/vlan-traffic-control/api/v1alpha1"
)

// ResolveClassifier parses matching parameters for a class spec.
func ResolveClassifier(cls networkingv1alpha1.ClassSpec, defaultHtbID int) (string, []string, string, int, error) {
	prio := cls.Priority
	if prio <= 0 {
		prio = cls.ClassMinor
	}

	matchType := cls.MatchType
	if matchType == "" {
		if cls.Subnet != "" {
			matchType = "subnet"
		} else if cls.VlanID > 0 {
			matchType = "vlan"
		} else if cls.Mark > 0 {
			matchType = "mark"
		} else {
			matchType = "flower"
		}
	}

	switch matchType {
	case "subnet":
		if cls.Subnet == "" {
			return "", nil, "", prio, fmt.Errorf("subnet match type requested but subnet field is empty")
		}
		if cls.VlanID > 0 {
			return "802.1Q", []string{"vlan_id", fmt.Sprintf("%d", cls.VlanID), "vlan_ethtype", "ip", "dst_ip", cls.Subnet}, "subnet", prio, nil
		}
		return "ip", []string{"dst_ip", cls.Subnet}, "subnet", prio, nil

	case "vlan":
		if cls.VlanID <= 0 {
			return "", nil, "", prio, fmt.Errorf("vlan match type requested but vlanId field is invalid")
		}
		return "802.1Q", []string{"vlan_id", fmt.Sprintf("%d", cls.VlanID)}, "vlan", prio, nil

	case "mark":
		if cls.Mark <= 0 {
			return "", nil, "", prio, fmt.Errorf("mark match type requested but mark field is invalid")
		}
		return "all", []string{"handle", fmt.Sprintf("%d", cls.Mark), "fw"}, "mark", prio, nil

	default:
		if cls.VlanID > 0 {
			return "802.1Q", []string{"vlan_id", fmt.Sprintf("%d", cls.VlanID)}, "vlan", prio, nil
		}
		return "all", []string{}, "flower", prio, nil
	}
}

// ReconcileStatelessIngress configures tc flower ingress policing directly on physical or bridge interfaces.
func ReconcileStatelessIngress(spec *networkingv1alpha1.HtbRootSpec, log logr.Logger) error {
	if spec == nil || spec.Interface == "" {
		return nil
	}

	iface := spec.Interface

	// 1. Ensure ingress qdisc exists
	cmdQdisc := execHostCommand("tc", "qdisc", "add", "dev", iface, "handle", "ffff:", "ingress")
	_ = cmdQdisc.Run()

	// 2. Apply stateless ingress policing rules
	for _, cls := range spec.Classes {
		prio := cls.Priority
		if prio <= 0 {
			prio = cls.ClassMinor
		}

		// IF INGRESS RATE IS NOT SET, EXPLICITLY PURGE INGRESS FILTER FOR THIS PRIORITY
		if cls.IngressRate == "" {
			cmdDel := execHostCommand("tc", "filter", "del", "dev", iface, "parent", "ffff:", "prio", fmt.Sprintf("%d", prio))
			_ = cmdDel.Run()
			continue
		}

		burst := cls.IngressBurst
		if burst == "" {
			burst = "16Mb"
		}

		action := cls.GetIngressAction()
		if action == "" {
			action = "pass"
		}

		// Flush prior rule at this priority level before adding
		cmdDel := execHostCommand("tc", "filter", "del", "dev", iface, "parent", "ffff:", "prio", fmt.Sprintf("%d", prio))
		_ = cmdDel.Run()

		var cmd *exec.Cmd
		if cls.MatchType == "subnet" && cls.Subnet != "" {
			if cls.VlanID > 0 {
				cmd = execHostCommand("tc", "filter", "add", "dev", iface, "parent", "ffff:",
					"protocol", "802.1Q", "prio", fmt.Sprintf("%d", prio), "flower",
					"vlan_id", fmt.Sprintf("%d", cls.VlanID),
					"vlan_ethtype", "ip",
					"dst_ip", cls.Subnet,
					"action", "police", "rate", cls.IngressRate, "burst", burst, "conform-exceed", action)
			} else {
				cmd = execHostCommand("tc", "filter", "add", "dev", iface, "parent", "ffff:",
					"protocol", "ip", "prio", fmt.Sprintf("%d", prio), "flower",
					"dst_ip", cls.Subnet,
					"action", "police", "rate", cls.IngressRate, "burst", burst, "conform-exceed", action)
			}
		} else if cls.VlanID > 0 {
			cmd = execHostCommand("tc", "filter", "add", "dev", iface, "parent", "ffff:",
				"protocol", "802.1Q", "prio", fmt.Sprintf("%d", prio), "flower",
				"vlan_id", fmt.Sprintf("%d", cls.VlanID),
				"action", "police", "rate", cls.IngressRate, "burst", burst, "conform-exceed", action)
		} else {
			cmd = execHostCommand("tc", "filter", "add", "dev", iface, "parent", "ffff:",
				"protocol", "all", "prio", fmt.Sprintf("%d", prio), "flower",
				"action", "police", "rate", cls.IngressRate, "burst", burst, "conform-exceed", action)
		}

		if cmd != nil {
			if out, err := cmd.CombinedOutput(); err != nil {
				log.Error(err, "[STATELESS-INGRESS] Failed adding ingress policing filter", "interface", iface, "class", cls.Name, "output", string(out))
			} else {
				log.Info("✓ [STATELESS-INGRESS] Applied ingress policing rule", "interface", iface, "prio", prio, "rate", cls.IngressRate)
			}
		}
	}

	return nil
}
