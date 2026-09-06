package executor

import (
	"fmt"

	"github.com/vishvananda/netlink"
	networkingv1alpha1 "networking.med.io/vlan-traffic-control/api/v1alpha1"
)

// GetInterfaceStats queries the host netlink layer for live TC statistics on physical and IFB interfaces.
func GetInterfaceStats(iface string, desired *networkingv1alpha1.HtbRootSpec) (*networkingv1alpha1.InterfaceStats, error) {
	stats := &networkingv1alpha1.InterfaceStats{
		Interface:    iface,
		ClassStats:   make([]networkingv1alpha1.ClassStat, 0),
		IngressStats: make([]networkingv1alpha1.IngressStat, 0),
	}

	link, err := netlink.LinkByName(iface)
	if err != nil {
		return stats, fmt.Errorf("failed to locate interface %s: %w", iface, err)
	}

	ifbDevName := fmt.Sprintf("ifb-%s", iface)
	if len(ifbDevName) > 15 {
		ifbDevName = ifbDevName[:15]
	}
	ifbLink, _ := netlink.LinkByName(ifbDevName)

	rootHandle := uint32(1)
	if desired != nil && desired.HtbID > 0 {
		rootHandle = uint32(desired.HtbID)
	}
	expectedRootHandle := netlink.MakeHandle(uint16(rootHandle), 0)

	prioToSpec := make(map[uint16]networkingv1alpha1.ClassSpec)
	if desired != nil {
		for idx, cls := range desired.Classes {
			prio := uint16(cls.Priority)
			if prio == 0 {
				prio = uint16(idx + 1)
			}
			prioToSpec[prio] = cls
		}
	}

	// 1. Collect HTB Egress Class Statistics on Physical Interface
	if classes, err := netlink.ClassList(link, expectedRootHandle); err == nil {
		for _, c := range classes {
			htb, ok := c.(*netlink.HtbClass)
			if !ok {
				continue
			}

			handleStr := netlink.HandleStr(htb.Attrs().Handle)
			if handleStr == fmt.Sprintf("%d:1", rootHandle) {
				continue
			}

			cStat := networkingv1alpha1.ClassStat{
				Interface: iface,
				ClassID:   handleStr,
				Direction: "egress",
				Priority:  int(htb.Prio),
			}

			if htb.Attrs().Statistics != nil {
				if htb.Attrs().Statistics.Basic != nil {
					cStat.Bytes = htb.Attrs().Statistics.Basic.Bytes
					cStat.Packets = uint64(htb.Attrs().Statistics.Basic.Packets)
				}
				if htb.Attrs().Statistics.Queue != nil {
					cStat.Drops = htb.Attrs().Statistics.Queue.Drops
					cStat.Overlimits = htb.Attrs().Statistics.Queue.Overlimits
				}
			}

			if desired != nil {
				for _, cls := range desired.Classes {
					if cls.GetClassID(int(rootHandle)) == handleStr {
						cStat.ClassName = cls.Name
						if cls.IngressRate != "" {
							cStat.Direction = "ingress+egress"
						}
						break
					}
				}
			}

			stats.ClassStats = append(stats.ClassStats, cStat)
		}
	}

	// 2. Collect HTB Ingress Class Statistics on IFB Device (if active)
	if ifbLink != nil {
		if ifbClasses, err := netlink.ClassList(ifbLink, expectedRootHandle); err == nil {
			for _, c := range ifbClasses {
				htb, ok := c.(*netlink.HtbClass)
				if !ok {
					continue
				}

				handleStr := netlink.HandleStr(htb.Attrs().Handle)
				if handleStr == fmt.Sprintf("%d:1", rootHandle) {
					continue
				}

				cStat := networkingv1alpha1.ClassStat{
					Interface: ifbDevName,
					ClassID:   handleStr,
					Direction: "ingress",
					Priority:  int(htb.Prio),
				}

				if htb.Attrs().Statistics != nil {
					if htb.Attrs().Statistics.Basic != nil {
						cStat.Bytes = htb.Attrs().Statistics.Basic.Bytes
						cStat.Packets = uint64(htb.Attrs().Statistics.Basic.Packets)
					}
					if htb.Attrs().Statistics.Queue != nil {
						cStat.Drops = htb.Attrs().Statistics.Queue.Drops
						cStat.Overlimits = htb.Attrs().Statistics.Queue.Overlimits
					}
				}

				if desired != nil {
					for _, cls := range desired.Classes {
						if cls.GetClassID(int(rootHandle)) == handleStr {
							cStat.ClassName = cls.Name
							break
						}
					}
				}

				stats.ClassStats = append(stats.ClassStats, cStat)
			}
		}
	}

	// 3. Collect Ingress Filter Statistics (on physical link AND virtual IFB link)
	linksToScanStats := []netlink.Link{link}
	if ifbLink != nil {
		linksToScanStats = append(linksToScanStats, ifbLink)
	}

	handlesToScan := []uint32{netlink.HANDLE_INGRESS, netlink.HANDLE_MIN_INGRESS, netlink.MakeHandle(0xffff, 2), netlink.HANDLE_ROOT, expectedRootHandle}
	seenStatsFilter := make(map[string]bool)

	for _, scanL := range linksToScanStats {
		scanDevName := scanL.Attrs().Name

		for _, h := range handlesToScan {
			filters, err := netlink.FilterList(scanL, h)
			if err != nil {
				continue
			}

			for _, f := range filters {
				attrs := f.Attrs()
				if attrs == nil {
					continue
				}

				prio := attrs.Priority
				dedupKey := fmt.Sprintf("%s-%d-%d-%d", scanDevName, prio, attrs.Handle, attrs.Protocol)
				if seenStatsFilter[dedupKey] {
					continue
				}
				seenStatsFilter[dedupKey] = true

				var bytesVal, pktsVal, dropsVal uint64

				if flower, ok := f.(*netlink.Flower); ok {
					for _, act := range flower.Actions {
						if actInfo := act.Attrs(); actInfo != nil && actInfo.Statistics != nil {
							if actInfo.Statistics.Basic != nil {
								bytesVal += actInfo.Statistics.Basic.Bytes
								pktsVal += uint64(actInfo.Statistics.Basic.Packets)
							}
							if actInfo.Statistics.Queue != nil {
								dropsVal += uint64(actInfo.Statistics.Queue.Drops)
							}
						}
					}
				}

				filterIDStr := fmt.Sprintf("pref-%d-handle-%d", prio, attrs.Handle)
				iStat := networkingv1alpha1.IngressStat{
					Interface: scanDevName,
					FilterID:  filterIDStr,
					Direction: "ingress",
					Bytes:     bytesVal,
					Packets:   pktsVal,
					Drops:     dropsVal,
				}

				if spec, found := prioToSpec[prio]; found {
					iStat.ClassID = spec.GetClassID(int(rootHandle))
					iStat.Subnet = spec.Subnet
				}

				stats.IngressStats = append(stats.IngressStats, iStat)
			}
		}
	}

	return stats, nil
}

// GetInterfaceStatsFiltered queries TC stats and filters results by targetVlan or targetClassID.
func GetInterfaceStatsFiltered(iface string, classMap map[string]string, targetVlan int, targetClassID string) (*networkingv1alpha1.InterfaceStats, error) {
	desired := &networkingv1alpha1.HtbRootSpec{
		Interface: iface,
		HtbID:     1,
		Classes:   make([]networkingv1alpha1.ClassSpec, 0),
	}

	for classID, name := range classMap {
		desired.Classes = append(desired.Classes, networkingv1alpha1.ClassSpec{
			ClassID: classID,
			Name:    name,
		})
	}

	fullStats, err := GetInterfaceStats(iface, desired)
	if err != nil || (targetClassID == "" && targetVlan <= 0) {
		return fullStats, err
	}

	filtered := &networkingv1alpha1.InterfaceStats{
		Interface:    fullStats.Interface,
		Node:         fullStats.Node,
		ClassStats:   make([]networkingv1alpha1.ClassStat, 0),
		IngressStats: make([]networkingv1alpha1.IngressStat, 0),
	}

	for _, c := range fullStats.ClassStats {
		if (targetClassID != "" && c.ClassID == targetClassID) || targetClassID == "" {
			filtered.ClassStats = append(filtered.ClassStats, c)
		}
	}

	for _, i := range fullStats.IngressStats {
		if (targetClassID != "" && i.ClassID == targetClassID) || targetClassID == "" {
			filtered.IngressStats = append(filtered.IngressStats, i)
		}
	}

	return filtered, nil
}
