package executor

import (
	"fmt"
	"os"

	"github.com/vishvananda/netlink"
	networkingv1alpha1 "networking.med.io/vlan-traffic-control/api/v1alpha1"
)

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
	report.Actual.Classes = make([]networkingv1alpha1.HtbClassSpec, 0)
	report.Actual.IngressFilters = make([]networkingv1alpha1.FilterMeta, 0)

	// Resolve target network link via Netlink
	link, err := netlink.LinkByName(desired.Interface)
	if err != nil {
		// If interface is missing on this node, mark as misaligned and report the delta!
		report.IsAligned = false
		report.DriftDeltas = append(report.DriftDeltas, networkingv1alpha1.ConfigDriftDelta{
			TargetHandle: fmt.Sprintf("interface %s", desired.Interface),
			Property:     "existence",
			Expected:     "present on host",
			Actual:       "missing device",
		})

		// Also mark all targeted classes as missing due to host interface absence
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

	// 1. Inspect Attached Qdiscs (Root HTB & Ingress)
	qdiscs, err := netlink.QdiscList(link)
	if err == nil {
		for _, q := range qdiscs {
			if q.Type() == "htb" && q.Attrs().Handle == netlink.MakeHandle(1, 0) {
				report.Actual.HtbQdiscPresent = true
			}
			if q.Type() == "ingress" {
				report.Actual.IngressPresent = true
			}
		}
	}

	// 2. Inspect Actual Live Netlink Egress Classes
	liveClasses := make(map[string]*netlink.HtbClass)
	classes, err := netlink.ClassList(link, netlink.MakeHandle(1, 0))
	if err == nil {
		for _, c := range classes {
			if htb, ok := c.(*netlink.HtbClass); ok {
				classID := netlink.HandleStr(htb.Attrs().Handle)
				liveClasses[classID] = htb
				report.Actual.Classes = append(report.Actual.Classes, networkingv1alpha1.HtbClassSpec{
					ClassID:  classID,
					Priority: int(htb.Prio),
				})
			}
		}
	}

	rootHandle := desired.HtbID
	if rootHandle <= 0 {
		rootHandle = 1
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

	// 4. Inspect & Validate Ingress Policing Filters Across ALL Filter Types (Flower, U32, Fw, etc.)
	filters, err := netlink.FilterList(link, netlink.HANDLE_INGRESS)
	liveFilters := make(map[uint16]bool)
	if err == nil {
		for _, f := range filters {
			attrs := f.Attrs()
			if attrs == nil {
				continue
			}

			liveFilters[attrs.Priority] = true
			filterType := f.Type()

			report.Actual.IngressFilters = append(report.Actual.IngressFilters, networkingv1alpha1.FilterMeta{
				Priority: attrs.Priority,
				Type:     filterType,
				Protocol: attrs.Protocol,
			})
		}
	}

	for _, plannedCls := range desired.Classes {
		if plannedCls.IngressRate == "" {
			continue
		}

		prioVal := plannedCls.Priority
		if prioVal <= 0 {
			prioVal = plannedCls.GetClassMinor()
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
