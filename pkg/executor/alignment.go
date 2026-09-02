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
	return fmt.Sprintf("%dbps", bits)
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

	// Initialize slices to ensure clean JSON arrays ([]) rather than null values
	report.Actual.Classes = make([]networkingv1alpha1.ClassSpec, 0)
	report.Actual.IngressFilters = make([]networkingv1alpha1.FilterMeta, 0)

	// Resolve dynamic root handle ID (e.g. htbId: 6 -> handle 6:0)
	rootHandle := desired.HtbID
	if rootHandle <= 0 {
		rootHandle = 1
	}
	expectedRootHandle := netlink.MakeHandle(uint16(rootHandle), 0)

	// Resolve target network link via Netlink
	link, err := netlink.LinkByName(desired.Interface)
	if err != nil {
		report.IsAligned = false
		report.DriftDeltas = append(report.DriftDeltas, networkingv1alpha1.ConfigDriftDelta{
			TargetHandle: fmt.Sprintf("interface %s", desired.Interface),
			Property:     "existence",
			Expected:     "present on host",
			Actual:       "missing device",
		})

		for _, plannedCls := range desired.Classes {
			classHandle := plannedCls.GetClassID(desired.HtbID)
			report.DriftDeltas = append(report.DriftDeltas, networkingv1alpha1.ConfigDriftDelta{
				TargetHandle: fmt.Sprintf("class %s", classHandle),
				Property:     "existence",
				Expected:     "configured",
				Actual:       fmt.Sprintf("missing (interface %s absent)", desired.Interface),
			})
		}
		return report, nil
	}

	// 1. Inspect Attached Qdiscs (Root HTB & Ingress) using dynamic rootHandle
	qdiscs, err := netlink.QdiscList(link)
	if err == nil {
		for _, q := range qdiscs {
			if q.Type() == "htb" && q.Attrs().Handle == expectedRootHandle {
				report.Actual.HtbQdiscPresent = true
			}
			if q.Type() == "ingress" {
				report.Actual.IngressPresent = true
			}
		}
	}

	// Helper map to quickly locate desired specs by Class ID
	desiredClassMap := make(map[string]networkingv1alpha1.ClassSpec)
	// Helper map to locate desired specs by Priority / Preference
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

	// 2. Inspect Actual Live Netlink Egress Classes using dynamic rootHandle
	liveClasses := make(map[string]*netlink.HtbClass)
	classes, err := netlink.ClassList(link, expectedRootHandle)
	if err == nil {
		for _, c := range classes {
			if htb, ok := c.(*netlink.HtbClass); ok {
				classID := netlink.HandleStr(htb.Attrs().Handle)
				liveClasses[classID] = htb

				liveRate := formatRateBps(htb.Rate)
				liveCeil := formatRateBps(htb.Ceil)

				actualCls := networkingv1alpha1.ClassSpec{
					ClassID:    classID,
					Priority:   int(htb.Prio),
					EgressRate: liveRate,
					EgressCeil: liveCeil,
				}

				if desSpec, matched := desiredClassMap[classID]; matched {
					actualCls.Name = desSpec.Name
					actualCls.MatchType = desSpec.MatchType
					actualCls.VlanID = desSpec.VlanID
					actualCls.Subnet = desSpec.Subnet
					actualCls.Mark = desSpec.Mark

					if actualCls.EgressRate == "" {
						actualCls.EgressRate = desSpec.EgressRate
					}
					if actualCls.EgressCeil == "" {
						actualCls.EgressCeil = desSpec.EgressCeil
					}
				}

				report.Actual.Classes = append(report.Actual.Classes, actualCls)
			}
		}
	}

	// 3. Validate Planned Egress Classes Against Kernel Netlink State
	for _, plannedCls := range desired.Classes {
		classHandle := plannedCls.GetClassID(rootHandle)

		if targetClassID != "" && classHandle != targetClassID {
			continue
		}

		actualHtb, exists := liveClasses[classHandle]
		if !exists {
			report.IsAligned = false
			report.DriftDeltas = append(report.DriftDeltas, networkingv1alpha1.ConfigDriftDelta{
				TargetHandle: fmt.Sprintf("class %s", classHandle),
				Property:     "existence",
				Expected:     "configured",
				Actual:       "missing",
			})
			continue
		}

		if plannedCls.Priority > 0 && int(actualHtb.Prio) != plannedCls.Priority {
			report.IsAligned = false
			report.DriftDeltas = append(report.DriftDeltas, networkingv1alpha1.ConfigDriftDelta{
				TargetHandle: fmt.Sprintf("class %s", classHandle),
				Property:     "priority",
				Expected:     fmt.Sprintf("%d", plannedCls.Priority),
				Actual:       fmt.Sprintf("%d", actualHtb.Prio),
			})
		}
	}

	// 4. Inspect & Validate Ingress Policing Filters Across ALL Filter Types (Flower & FW)
	filters, err := netlink.FilterList(link, netlink.HANDLE_INGRESS)
	liveFilters := make(map[uint16]bool)
	if err == nil {
		for _, f := range filters {
			attrs := f.Attrs()
			if attrs == nil {
				continue
			}

			liveFilters[attrs.Priority] = true

			var chainVal uint32
			if attrs.Chain != nil {
				chainVal = *attrs.Chain
			}

			meta := networkingv1alpha1.FilterMeta{
				Priority: attrs.Priority,
				Handle:   attrs.Handle,
				Chain:    chainVal,
				Type:     f.Type(),
				Protocol: attrs.Protocol,
				Action:   "police drop",
				Matches:  make(map[string]string),
			}

			// Map Flower Classifier
			if flower, ok := f.(*netlink.Flower); ok {
				if flower.VlanId != 0 {
					meta.Matches["vlan_id"] = fmt.Sprintf("%d", flower.VlanId)
					meta.Matches["eth_type"] = fmt.Sprintf("0x%x", flower.EthType)
				}
			}

			// Map FW Classifier (handle = mark)
			if f.Type() == "fw" {
				meta.Type = "fw"
				meta.MatchType = "mark"
				meta.Mark = uint32(attrs.Handle)
				meta.Matches["mark"] = fmt.Sprintf("%d", attrs.Handle)
			}

			// Enrich with matching 1:1 class metadata & action
			if desSpec, matched := desiredPrioMap[attrs.Priority]; matched {
				meta.Name = desSpec.Name
				meta.MatchType = desSpec.MatchType
				meta.VlanID = desSpec.VlanID
				meta.Subnet = desSpec.Subnet
				meta.Mark = desSpec.Mark
				meta.IngressRate = desSpec.IngressRate
				meta.IngressBurst = desSpec.IngressBurst
				meta.Action = fmt.Sprintf("police %s", desSpec.GetIngressAction())
			}

			report.Actual.IngressFilters = append(report.Actual.IngressFilters, meta)
		}
	}

	for _, plannedCls := range desired.Classes {
		if plannedCls.IngressRate == "" {
			continue
		}

		prioVal := plannedCls.Priority
		if prioVal <= 0 {
			prioVal = plannedCls.ClassMinor
		}

		if targetClassID != "" && plannedCls.GetClassID(rootHandle) != targetClassID {
			continue
		}

		if !liveFilters[uint16(prioVal)] {
			report.IsAligned = false
			report.DriftDeltas = append(report.DriftDeltas, networkingv1alpha1.ConfigDriftDelta{
				TargetHandle: fmt.Sprintf("ingress filter pref %d", prioVal),
				Property:     "existence",
				Expected:     "configured",
				Actual:       "missing",
			})
		}
	}

	return report, nil
}
