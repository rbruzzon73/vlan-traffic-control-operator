package executor

import (
	"fmt"
	"strconv"
	"strings"

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

	// Dynamic Root Handle Resolution from CRD Spec
	rootHandle := uint32(1)
	if desired != nil && desired.HtbID > 0 {
		rootHandle = uint32(desired.HtbID)
	}
	expectedRootHandle := netlink.MakeHandle(uint16(rootHandle), 0)

	// Build Priority / Minor / ClassID / VlanID lookup maps & detect Default Fallback Class dynamically
	prioToSpec := make(map[uint16]networkingv1alpha1.ClassSpec)
	classIDToSpec := make(map[string]networkingv1alpha1.ClassSpec)
	dynamicDefaultClassID := fmt.Sprintf("%d:99", rootHandle) // Default fallback if unspecified in spec

	if desired != nil {
		for _, cls := range desired.Classes {
			if cls.ClassID != "" {
				classIDToSpec[cls.ClassID] = cls
			}
			// Detect custom default/fallback class from spec (priority 0 or named default)
			if cls.Priority == 0 || strings.Contains(strings.ToLower(cls.Name), "default") {
				dynamicDefaultClassID = cls.GetClassID(int(rootHandle))
			}
			if cls.Priority > 0 {
				prioToSpec[uint16(cls.Priority)] = cls
			}
			if cls.ClassMinor > 0 {
				prioToSpec[uint16(cls.ClassMinor)] = cls
			}
			if cls.VlanID > 0 {
				prioToSpec[uint16(cls.VlanID)] = cls
			}
			if cls.ClassID != "" {
				parts := strings.Split(cls.ClassID, ":")
				if len(parts) == 2 {
					if minor, err := strconv.Atoi(parts[1]); err == nil && minor > 0 {
						prioToSpec[uint16(minor)] = cls
					}
				}
			}
		}
	}

	handlesToScan := []uint32{
		netlink.HANDLE_INGRESS,
		netlink.HANDLE_MIN_INGRESS,
		netlink.MakeHandle(0xffff, 2),
		netlink.HANDLE_ROOT,
		expectedRootHandle,
	}

	// 1. Collect HTB Class Statistics natively for the queried interface
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

			direction := "egress"
			if strings.HasPrefix(iface, "ifb-") {
				direction = "ingress"
			}

			cStat := networkingv1alpha1.ClassStat{
				Interface: iface,
				ClassID:   handleStr,
				Direction: direction,
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

			// Resolve human-readable name from spec
			if spec, found := classIDToSpec[handleStr]; found {
				cStat.ClassName = spec.Name
			} else if desired != nil {
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

	// 2. Collect Ingress Filter Statistics (Only on physical interfaces, skipping IFB devices)
	if !strings.HasPrefix(iface, "ifb-") {
		seenStatsFilter := make(map[string]bool)

		for _, h := range handlesToScan {
			filters, err := netlink.FilterList(link, h)
			if err != nil {
				continue
			}

			hasMatchAll := false

			for _, f := range filters {
				attrs := f.Attrs()
				if attrs == nil {
					continue
				}

				prio := attrs.Priority
				dedupKey := fmt.Sprintf("%s-%d-%d-%d", iface, prio, attrs.Handle, attrs.Protocol)
				if seenStatsFilter[dedupKey] {
					continue
				}
				seenStatsFilter[dedupKey] = true

				var bytesVal, pktsVal, dropsVal uint64
				var flowerVlanID int
				filterType := f.Type()

				// Rule: If matchall redirect is active, collect its stats and skip unreachable rules
				if matchall, ok := f.(*netlink.MatchAll); ok || filterType == "matchall" {
					if matchall != nil {
						for _, act := range matchall.Actions {
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
					stats.IngressStats = append(stats.IngressStats, networkingv1alpha1.IngressStat{
						Interface: iface,
						FilterID:  filterIDStr,
						Direction: "ingress",
						ClassID:   fmt.Sprintf("%d:1", rootHandle),
						ClassName: "ifb-ingress-redirect",
						Bytes:     bytesVal,
						Packets:   pktsVal,
						Drops:     dropsVal,
					})

					hasMatchAll = true
					break // Short-circuit ghost rules behind matchall on this scan
				}

				if flower, ok := f.(*netlink.Flower); ok {
					flowerVlanID = int(flower.VlanId)
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
					Interface: iface,
					FilterID:  filterIDStr,
					Direction: "ingress",
					Bytes:     bytesVal,
					Packets:   pktsVal,
					Drops:     dropsVal,
				}

				// Enrich IngressStat metadata using lookup map or direct class matching
				if spec, found := prioToSpec[prio]; found {
					iStat.ClassID = spec.GetClassID(int(rootHandle))
					iStat.ClassName = spec.Name
					iStat.Subnet = spec.Subnet
					iStat.VlanID = spec.VlanID
				} else if desired != nil {
					for _, cls := range desired.Classes {
						if cls.Priority == int(prio) || cls.ClassMinor == int(prio) || (flowerVlanID > 0 && cls.VlanID == flowerVlanID) {
							iStat.ClassID = cls.GetClassID(int(rootHandle))
							iStat.ClassName = cls.Name
							iStat.Subnet = cls.Subnet
							iStat.VlanID = cls.VlanID
							break
						}
					}
				}

				// Dynamic fallback naming: use dynamic default class ID and root handle
				if iStat.ClassName == "" {
					if prio == 49152 || prio == 0 {
						iStat.ClassName = "default-fallback"
						iStat.ClassID = dynamicDefaultClassID
					} else if flowerVlanID > 0 {
						iStat.ClassName = fmt.Sprintf("vlan-%d-migration", flowerVlanID)
						iStat.ClassID = fmt.Sprintf("%d:%d", rootHandle, flowerVlanID)
						iStat.VlanID = flowerVlanID
					} else {
						iStat.ClassName = fmt.Sprintf("prio-%d-filter", prio)
					}
				}

				stats.IngressStats = append(stats.IngressStats, iStat)
			}

			if hasMatchAll {
				break // Short-circuit further handle scans for physical interface ingress
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
