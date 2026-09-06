package executor

import (
	"fmt"
	"os"

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

// InspectNodeAlignment checks if the kernel TC state matches the aggregated planned CRD specs.
func InspectNodeAlignment(desired *networkingv1alpha1.HtbRootSpec, targetClassID string) (*networkingv1alpha1.NodeConfigReport, error) {
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

	// 1. Inspect Qdiscs on Physical Link and detect active redirect filter
	hasRedirectFilter := false
	clsactIngressHandle := netlink.MakeHandle(0xffff, 2)
	handlesToScan := []uint32{netlink.HANDLE_INGRESS, netlink.HANDLE_MIN_INGRESS, clsactIngressHandle, netlink.HANDLE_ROOT, expectedRootHandle}

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

	// Check if a matchall mirred redirect filter is attached to physical link
	for _, h := range handlesToScan {
		filters, err := netlink.FilterList(physLink, h)
		if err != nil {
			continue
		}
		for _, f := range filters {
			if f.Type() == "matchall" {
				hasRedirectFilter = true
				break
			}
		}
		if hasRedirectFilter {
			break
		}
	}

	isIfbActive := errIfb == nil && ifbLink != nil && hasRedirectFilter

	if isIfbActive {
		report.Actual.IfbInterface = ifbDevName
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

	for idx, cls := range desired.Classes {
		handle := cls.GetClassID(rootHandle)
		desiredClassMap[handle] = cls

		prioVal := cls.Priority
		if prioVal <= 0 {
			prioVal = cls.ClassMinor
		}
		if prioVal <= 0 {
			prioVal = idx + 1
		}
		desiredPrioMap[uint16(prioVal)] = cls
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

	// 3. Inspect Filters
	linksToScan := []netlink.Link{physLink}
	if isIfbActive {
		linksToScan = append(linksToScan, ifbLink)
	}

	seenFilters := make(map[string]bool)

	for _, scanLink := range linksToScan {
		isVirtual := scanLink.Attrs().Name == ifbDevName
		scanIfaceName := scanLink.Attrs().Name

		for _, h := range handlesToScan {
			filters, err := netlink.FilterList(scanLink, h)
			if err != nil {
				continue
			}

			for _, f := range filters {
				attrs := f.Attrs()
				if attrs == nil {
					continue
				}

				dedupKey := fmt.Sprintf("%s-%d-%d-%d", scanIfaceName, attrs.Priority, attrs.Handle, attrs.Protocol)
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
					Name:      scanIfaceName,
					Type:      f.Type(),
					Protocol:  attrs.Protocol,
					Action:    "police drop",
					Matches:   make(map[string]string),
				}

				if !isVirtual && hasRedirectFilter {
					if f.Type() == "matchall" {
						meta.Interface = desired.Interface
						meta.Name = fmt.Sprintf("Ingress traffic %s", desired.Interface)
						meta.Action = fmt.Sprintf("mirred redirect dev %s", ifbDevName)
						report.Actual.IngressFilters = append(report.Actual.IngressFilters, meta)
					}
					continue
				}

				if isVirtual {
					meta.Interface = ifbDevName
					meta.Action = "htb classify"
				}

				if flower, ok := f.(*netlink.Flower); ok {
					if flower.VlanId != 0 {
						meta.VlanID = int(flower.VlanId)
						meta.Matches["vlan_id"] = fmt.Sprintf("%d", flower.VlanId)
						meta.Matches["eth_type"] = fmt.Sprintf("0x%x", flower.EthType)
					}
					if flower.DestIP != nil {
						meta.Subnet = flower.DestIP.String()
						meta.Matches["dst_ip"] = flower.DestIP.String()
					}
				}

				if f.Type() == "fw" {
					meta.Type = "fw"
					meta.MatchType = "mark"
					meta.Mark = uint32(attrs.Handle)
					meta.Matches["mark"] = fmt.Sprintf("%d", attrs.Handle)
				}

				if desSpec, matched := desiredPrioMap[attrs.Priority]; matched {
					if desSpec.Name != "" {
						meta.Name = desSpec.Name
					}
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

					if !hasRedirectFilter {
						actionStr := desSpec.GetIngressAction()
						if actionStr == "" {
							actionStr = "drop"
						}
						meta.Action = fmt.Sprintf("police %s", actionStr)
					}
				}

				meta.Interface = scanIfaceName
				report.Actual.IngressFilters = append(report.Actual.IngressFilters, meta)
			}
		}
	}

	// 4. POST-PROCESSING DEDUPLICATION PASS
	// When IFB redirection is active, purge any physical interface filters that have a non-zero vlanId
	if isIfbActive {
		cleanFilters := make([]networkingv1alpha1.FilterMeta, 0, len(report.Actual.IngressFilters))
		for _, filter := range report.Actual.IngressFilters {
			if filter.Interface == desired.Interface && filter.VlanID > 0 {
				continue // Strip physical interface per-VLAN filter entry
			}
			cleanFilters = append(cleanFilters, filter)
		}
		report.Actual.IngressFilters = cleanFilters
	}

	return report, nil
}
