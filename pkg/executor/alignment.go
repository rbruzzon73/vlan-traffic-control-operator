package executor

import (
	"fmt"
	"net"
	"os"
	"strings"

	"github.com/vishvananda/netlink"
	networkingv1alpha1 "networking.med.io/vlan-traffic-control/api/v1alpha1"
)

// Helper: Format raw bytes/sec from netlink into human-readable TC rate string
func formatRateBps(rateBytes uint64) string {
	if rateBytes == 0 {
		return ""
	}
	bits := rateBytes * 8
	if bits%1000000000 == 0 {
		return fmt.Sprintf("%dGbit", bits/1000000000)
	}
	if bits%1000000 == 0 {
		return fmt.Sprintf("%dMbit", bits/1000000)
	}
	if bits%1000 == 0 {
		return fmt.Sprintf("%dkbit", bits/1000)
	}
	return fmt.Sprintf("%dbps", rateBytes)
}

// InspectNodeAlignment inspects whether existing host netlink state matches the desired CR spec
// and builds a full NodeConfigReport required by GET /config endpoints.
func InspectNodeAlignment(desired *networkingv1alpha1.HtbRootSpec, activeStrategy networkingv1alpha1.TcStrategyType, targetClassID string) (*networkingv1alpha1.NodeConfigReport, error) {
	nodeName := os.Getenv("NODE_NAME")
	if nodeName == "" {
		nodeName, _ = os.Hostname()
	}

	if desired == nil {
		return nil, fmt.Errorf("desired spec is nil")
	}

	report := &networkingv1alpha1.NodeConfigReport{
		Node:        nodeName,
		Interface:   desired.Interface,
		IsAligned:   true,
		Desired:     *desired,
		DriftDeltas: make([]networkingv1alpha1.ConfigDriftDelta, 0),
	}

	report.Actual.Interface = desired.Interface
	report.Actual.Classes = make([]networkingv1alpha1.ClassSpec, 0)
	report.Actual.IngressFilters = make([]networkingv1alpha1.FilterMeta, 0)

	rootHandle := desired.HtbID
	if rootHandle <= 0 {
		rootHandle = 1
	}
	expectedRootHandle := netlink.MakeHandle(uint16(rootHandle), 0)

	physLink, err := netlink.LinkByName(desired.Interface)
	if err != nil {
		report.IsAligned = false
		report.DriftDeltas = append(report.DriftDeltas, networkingv1alpha1.ConfigDriftDelta{
			TargetHandle: fmt.Sprintf("interface %s", desired.Interface),
			Property:     "existence",
			Expected:     "present on host",
			Actual:       "missing device",
		})
		return report, nil
	}

	ifbDevName := fmt.Sprintf("ifb-%s", desired.Interface)
	if len(ifbDevName) > 15 {
		ifbDevName = ifbDevName[:15]
	}

	ifbLink, errIfb := netlink.LinkByName(ifbDevName)
	isIfbActive := strings.ToLower(string(activeStrategy)) == "ifb" && errIfb == nil && ifbLink != nil

	if isIfbActive {
		report.Actual.IfbInterface = ifbDevName
	}

	// 1. Inspect Qdiscs
	qdiscs, err := netlink.QdiscList(physLink)
	if err == nil {
		for _, q := range qdiscs {
			if q.Type() == "htb" && q.Attrs().Handle == expectedRootHandle {
				report.Actual.HtbQdiscPresent = true
			}
			if q.Type() == "clsact" || q.Type() == "ingress" {
				report.Actual.ClsactPresent = true
				report.Actual.IngressPresent = true
			}
		}
	}

	if isIfbActive {
		ifbQdiscs, err := netlink.QdiscList(ifbLink)
		if err == nil {
			for _, q := range ifbQdiscs {
				if q.Type() == "htb" {
					report.Actual.HtbQdiscPresent = true
				}
			}
		}
	}

	desiredClassMap := make(map[string]networkingv1alpha1.ClassSpec)
	desiredPrioMap := make(map[uint16]networkingv1alpha1.ClassSpec)

	for _, cls := range desired.Classes {
		handle := cls.GetClassID(rootHandle)
		desiredClassMap[handle] = cls

		prioVal := cls.Priority
		if prioVal <= 0 {
			prioVal = cls.ClassMinor
		}
		if prioVal > 0 {
			desiredPrioMap[uint16(prioVal)] = cls
		}
	}

	// 2. Inspect HTB Classes
	classSpecsMap := make(map[string]*networkingv1alpha1.ClassSpec)

	if classes, err := netlink.ClassList(physLink, expectedRootHandle); err == nil {
		for _, c := range classes {
			if htb, ok := c.(*netlink.HtbClass); ok {
				classID := netlink.HandleStr(htb.Attrs().Handle)
				if classID == fmt.Sprintf("%d:1", rootHandle) {
					continue
				}
				spec := &networkingv1alpha1.ClassSpec{
					ClassID:    classID,
					Priority:   int(htb.Prio),
					EgressRate: formatRateBps(htb.Rate),
					EgressCeil: formatRateBps(htb.Ceil),
				}
				classSpecsMap[classID] = spec
			}
		}
	}

	if isIfbActive {
		if classes, err := netlink.ClassList(ifbLink, expectedRootHandle); err == nil {
			for _, c := range classes {
				if htb, ok := c.(*netlink.HtbClass); ok {
					classID := netlink.HandleStr(htb.Attrs().Handle)
					if classID == fmt.Sprintf("%d:1", rootHandle) {
						continue
					}
					existing, found := classSpecsMap[classID]
					if !found {
						existing = &networkingv1alpha1.ClassSpec{ClassID: classID, Priority: int(htb.Prio)}
						classSpecsMap[classID] = existing
					}
					existing.IngressRate = formatRateBps(htb.Rate)
					existing.IngressCeil = formatRateBps(htb.Ceil)
				}
			}
		}
	}

	for classID, spec := range classSpecsMap {
		if desSpec, matched := desiredClassMap[classID]; matched {
			spec.Name = desSpec.Name
			spec.MatchType = desSpec.MatchType
			spec.VlanID = desSpec.VlanID
			spec.Subnet = desSpec.Subnet
			spec.Mark = desSpec.Mark
			spec.EgressBurst = desSpec.EgressBurst
			spec.EnableFqCodel = desSpec.EnableFqCodel
			spec.IngressBurst = desSpec.IngressBurst
			spec.IngressAction = desSpec.IngressAction

			if spec.EgressRate == "" {
				spec.EgressRate = desSpec.EgressRate
			}
			if spec.EgressCeil == "" {
				spec.EgressCeil = desSpec.EgressCeil
			}
			if spec.IngressRate == "" {
				spec.IngressRate = desSpec.IngressRate
			}
			if spec.IngressCeil == "" {
				spec.IngressCeil = desSpec.IngressCeil
			}
		}
		if spec.Name == "" && (classID == "1:99" || classID == fmt.Sprintf("%d:99", rootHandle)) {
			spec.Name = "default-fallback"
		}
		report.Actual.Classes = append(report.Actual.Classes, *spec)
	}

	// 3. Inspect Filters (Purge stale physical parent ffff: filters if IFB strategy active)
	type scanTarget struct {
		link    netlink.Link
		handles []uint32
	}

	var scanTargets []scanTarget

	if isIfbActive {
		// IFB Active: Scan ONLY ingress handle on Physical (mirred redirect), and ingress handle on IFB
		scanTargets = append(scanTargets, scanTarget{
			link:    physLink,
			handles: []uint32{netlink.HANDLE_INGRESS},
		})
		scanTargets = append(scanTargets, scanTarget{
			link:    ifbLink,
			handles: []uint32{netlink.HANDLE_INGRESS},
		})
	} else {
		// Flower Active: Scan ingress & legacy parent ffff: on physical interface
		clsactIngressHandle := netlink.MakeHandle(0xffff, 2)
		scanTargets = append(scanTargets, scanTarget{
			link:    physLink,
			handles: []uint32{netlink.HANDLE_INGRESS, netlink.HANDLE_MIN_INGRESS, clsactIngressHandle},
		})
	}

	seenFilters := make(map[string]bool)

	for _, target := range scanTargets {
		isVirtual := target.link.Attrs().Name == ifbDevName
		scanIfaceName := target.link.Attrs().Name

		for _, h := range target.handles {
			filters, err := netlink.FilterList(target.link, h)
			if err != nil {
				continue
			}

			for _, f := range filters {
				attrs := f.Attrs()
				if attrs == nil {
					continue
				}

				dedupKey := fmt.Sprintf("%s-%d-%d", scanIfaceName, attrs.Priority, attrs.Handle)
				if seenFilters[dedupKey] {
					continue
				}
				seenFilters[dedupKey] = true

				var chainVal uint32
				if attrs.Chain != nil {
					chainVal = *attrs.Chain
				}

				meta := networkingv1alpha1.FilterMeta{
					Priority:  attrs.Priority,
					Handle:    attrs.Handle,
					Chain:     chainVal,
					Interface: scanIfaceName,
					Type:      f.Type(),
					Protocol:  attrs.Protocol,
					Action:    "police drop",
					Matches:   make(map[string]string),
				}

				if desSpec, matched := desiredPrioMap[attrs.Priority]; matched {
					meta.Name = desSpec.Name
					meta.MatchType = desSpec.MatchType
					if meta.VlanID == 0 {
						meta.VlanID = desSpec.VlanID
					}
					if meta.Subnet == "" {
						meta.Subnet = desSpec.Subnet
					}
					meta.Mark = desSpec.Mark
					meta.IngressRate = desSpec.IngressRate
					meta.IngressBurst = desSpec.IngressBurst

					actStr := desSpec.GetIngressAction()
					if actStr != "" {
						meta.Action = fmt.Sprintf("police %s", actStr)
					}
				}

				if !isVirtual && f.Type() == "matchall" && isIfbActive {
					meta.Name = fmt.Sprintf("Ingress traffic %s", desired.Interface)
					meta.Action = fmt.Sprintf("mirred redirect dev %s", ifbDevName)
					meta.MatchType = ""
					meta.VlanID = 0
					meta.Subnet = ""
					report.Actual.IngressFilters = append(report.Actual.IngressFilters, meta)
					continue
				}

				if isVirtual {
					meta.Action = "htb classify"
				}

				if flower, ok := f.(*netlink.Flower); ok {
					if flower.VlanId != 0 {
						meta.VlanID = int(flower.VlanId)
						meta.Matches["vlan_id"] = fmt.Sprintf("%d", flower.VlanId)
						meta.Matches["eth_type"] = fmt.Sprintf("0x%x", flower.EthType)
					}
					if flower.DestIP != nil {
						ipStr := flower.DestIP.String()
						meta.Subnet = ipStr
						meta.Matches["dst_ip"] = ipStr
					}
				}

				if f.Type() == "fw" {
					meta.Type = "fw"
					meta.MatchType = "mark"
					meta.Mark = uint32(attrs.Handle)
					meta.Matches["mark"] = fmt.Sprintf("%d", attrs.Handle)
				}

				report.Actual.IngressFilters = append(report.Actual.IngressFilters, meta)
			}
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
