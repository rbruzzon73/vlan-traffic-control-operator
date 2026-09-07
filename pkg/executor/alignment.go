package executor

import (
	"fmt"
	"net"
	"strings"

	"github.com/vishvananda/netlink"
	networkingv1alpha1 "networking.med.io/vlan-traffic-control/api/v1alpha1"
)

// AlignmentReport defines the response structure for GET /config drift detection.
type AlignmentReport struct {
	Interface string   `json:"interface"`
	IsAligned bool     `json:"isAligned"`
	Deltas    []string `json:"deltas,omitempty"`
}

// InspectNodeAlignment inspects whether existing host netlink filters match the desired CR spec.
func InspectNodeAlignment(spec *networkingv1alpha1.HtbRootSpec, strategy networkingv1alpha1.TcStrategyType, targetClassID string) (*AlignmentReport, error) {
	report := &AlignmentReport{
		Interface: spec.Interface,
		IsAligned: true,
		Deltas:    []string{},
	}

	link, err := netlink.LinkByName(spec.Interface)
	if err != nil {
		report.IsAligned = false
		report.Deltas = append(report.Deltas, fmt.Sprintf("interface %s not found on host: %v", spec.Interface, err))
		return report, nil
	}

	// Strategy Check 1: IFB Strategy Alignment Check
	if strings.ToLower(string(strategy)) == "ifb" {
		ifbName := fmt.Sprintf("ifb-%s", spec.Interface)
		if len(ifbName) > 15 {
			ifbName = ifbName[:15]
		}

		ifbLink, err := netlink.LinkByName(ifbName)
		if err != nil {
			report.IsAligned = false
			report.Deltas = append(report.Deltas, fmt.Sprintf("IFB interface %s missing on host", ifbName))
			return report, nil
		}

		qdiscs, err := netlink.QdiscList(ifbLink)
		if err != nil || len(qdiscs) == 0 {
			report.IsAligned = false
			report.Deltas = append(report.Deltas, fmt.Sprintf("HTB root qdisc missing on %s", ifbName))
			return report, nil
		}

		return report, nil
	}

	// Strategy Check 2: Stateless Flower Strategy Alignment Check
	existingFilters, err := netlink.FilterList(link, netlink.HANDLE_INGRESS)
	if err != nil {
		report.IsAligned = false
		report.Deltas = append(report.Deltas, fmt.Sprintf("failed listing ingress filters on %s: %v", spec.Interface, err))
		return report, nil
	}

	rootHandle := spec.HtbID
	if rootHandle <= 0 {
		rootHandle = 1
	}

	for _, cls := range spec.Classes {
		cID := cls.GetClassID(rootHandle)
		if targetClassID != "" && cID != targetClassID {
			continue
		}

		if cls.IngressRate == "" {
			continue
		}

		desiredSubnet := cls.Subnet
		var desiredIPNet *net.IPNet
		if desiredSubnet != "" {
			_, desiredIPNet, _ = net.ParseCIDR(desiredSubnet)
		}

		matched := false
		for _, f := range existingFilters {
			flower, ok := f.(*netlink.Flower)
			if !ok {
				continue
			}

			if cls.Priority > 0 && flower.Priority != uint16(cls.Priority) {
				continue
			}

			if desiredIPNet != nil {
				if flower.DestIP == nil || !flower.DestIP.Equal(desiredIPNet.IP) {
					continue
				}
			}

			if cls.VlanID > 0 && flower.VlanId != uint16(cls.VlanID) {
				continue
			}

			matched = true
			break
		}

		if !matched {
			report.IsAligned = false
			report.Deltas = append(report.Deltas, fmt.Sprintf("Missing ingress filter for class %s (%s, subnet: %s, vlan: %d)", cID, cls.Name, cls.Subnet, cls.VlanID))
		}
	}

	return report, nil
}

// AlignIngressFilters reconciles ingress flower filters without resetting action statistics.
func AlignIngressFilters(spec *networkingv1alpha1.HtbRootSpec) error {
	link, err := netlink.LinkByName(spec.Interface)
	if err != nil {
		return fmt.Errorf("interface %s not found: %w", spec.Interface, err)
	}

	qdiscs, err := netlink.QdiscList(link)
	if err != nil {
		return fmt.Errorf("failed to list qdiscs on %s: %w", spec.Interface, err)
	}

	hasIngressQdisc := false
	for _, q := range qdiscs {
		if q.Attrs().Parent == netlink.HANDLE_INGRESS || q.Type() == "clsact" {
			hasIngressQdisc = true
			break
		}
	}

	if !hasIngressQdisc {
		clsact := &netlink.GenericQdisc{
			QdiscAttrs: netlink.QdiscAttrs{
				LinkIndex: link.Attrs().Index,
				Handle:    netlink.HANDLE_INGRESS,
				Parent:    netlink.HANDLE_ROOT,
			},
			QdiscType: "clsact",
		}
		if err := netlink.QdiscAdd(clsact); err != nil {
			return fmt.Errorf("failed to add clsact qdisc on %s: %w", spec.Interface, err)
		}
	}

	existingFilters, err := netlink.FilterList(link, netlink.HANDLE_INGRESS)
	if err != nil {
		return fmt.Errorf("failed to list ingress filters on %s: %w", spec.Interface, err)
	}

	for _, cls := range spec.Classes {
		if cls.IngressRate == "" {
			continue
		}

		var desiredIPNet *net.IPNet
		if cls.Subnet != "" {
			_, desiredIPNet, err = net.ParseCIDR(cls.Subnet)
			if err != nil {
				return fmt.Errorf("invalid subnet %s: %w", cls.Subnet, err)
			}
		}

		desiredPriority := uint16(cls.Priority)
		if desiredPriority == 0 {
			desiredPriority = 3
		}

		isAligned := false
		for _, existing := range existingFilters {
			existingFlower, ok := existing.(*netlink.Flower)
			if !ok {
				continue
			}

			if existingFlower.Priority != desiredPriority {
				continue
			}

			if desiredIPNet != nil {
				if existingFlower.DestIP == nil || !existingFlower.DestIP.Equal(desiredIPNet.IP) {
					continue
				}
			}

			if cls.VlanID > 0 && existingFlower.VlanId != uint16(cls.VlanID) {
				continue
			}

			isAligned = true
			break
		}

		if isAligned {
			continue
		}

		rateBytes := ParseRateToBytes(cls.IngressRate)
		burstBytes := uint32(268433750)
		if cls.IngressBurst != "" {
			burstBytes = ParseBurstToBytes(cls.IngressBurst)
		}

		policeAction := &netlink.PoliceAction{
			ActionAttrs: netlink.ActionAttrs{
				Action: netlink.TC_ACT_OK,
			},
			Rate:  rateBytes,
			Burst: burstBytes,
		}

		protocol := uint16(0x0800)
		if cls.VlanID > 0 {
			protocol = uint16(0x8100)
		}

		newFlower := &netlink.Flower{
			FilterAttrs: netlink.FilterAttrs{
				LinkIndex: link.Attrs().Index,
				Parent:    netlink.HANDLE_INGRESS,
				Priority:  desiredPriority,
				Protocol:  protocol,
			},
			Actions: []netlink.Action{policeAction},
		}

		if desiredIPNet != nil {
			newFlower.DestIP = desiredIPNet.IP
		}
		if cls.VlanID > 0 {
			newFlower.VlanId = uint16(cls.VlanID)
		}

		if err := netlink.FilterReplace(newFlower); err != nil {
			return fmt.Errorf("failed to replace flower filter on %s: %w", spec.Interface, err)
		}
	}

	return nil
}

// ParseRateToBytes converts human readable rate strings (e.g. 10Gbit, 100Mbit) to uint32 bytes/sec.
func ParseRateToBytes(rateStr string) uint32 {
	var val uint64
	var unit string
	_, _ = fmt.Sscanf(rateStr, "%d%s", &val, &unit)
	switch unit {
	case "Gbit", "gbps":
		return uint32((val * 1000 * 1000 * 1000) / 8)
	case "Mbit", "mbps":
		return uint32((val * 1000 * 1000) / 8)
	case "Kbit", "kbps":
		return uint32((val * 1000) / 8)
	default:
		return uint32(val)
	}
}

// ParseBurstToBytes converts human readable burst strings to uint32 bytes.
func ParseBurstToBytes(burstStr string) uint32 {
	var val uint32
	var unit string
	_, _ = fmt.Sscanf(burstStr, "%d%s", &val, &unit)
	switch unit {
	case "Mb", "MB":
		return val * 1024 * 1024
	case "Kb", "KB":
		return val * 1024
	default:
		return val
	}
}
