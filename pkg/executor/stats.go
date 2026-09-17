package executor

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/vishvananda/netlink"
	networkingv1alpha1 "networking.med.io/vlan-traffic-control/api/v1alpha1"
)

// GetInterfaceStats queries host netlink for live TC statistics on physical and IFB interfaces.
func GetInterfaceStats(iface string, desired *networkingv1alpha1.HtbRootSpec) (*networkingv1alpha1.InterfaceStats, error) {
	nodeName := os.Getenv("NODE_NAME")
	if nodeName == "" {
		nodeName, _ = os.Hostname()
	}

	stats := &networkingv1alpha1.InterfaceStats{
		Interface:    iface,
		Node:         nodeName,
		ClassStats:   make([]networkingv1alpha1.ClassStat, 0),
		IngressStats: make([]networkingv1alpha1.IngressStat, 0),
	}

	link, err := netlink.LinkByName(iface)
	if err != nil {
		return stats, fmt.Errorf("failed to locate interface %s: %w", iface, err)
	}

	// Resolve Active HTB Major Handle (default to 1 if not set in spec)
	rootHandle := uint32(1)
	if desired != nil && desired.HtbID > 0 {
		rootHandle = uint32(desired.HtbID)
	}
	expectedRootHandle := netlink.MakeHandle(uint16(rootHandle), 0)

	// Check if corresponding IFB interface exists
	ifbDevName := fmt.Sprintf("ifb-%s", iface)
	if len(ifbDevName) > 15 {
		ifbDevName = ifbDevName[:15]
	}
	ifbLink, errIfb := netlink.LinkByName(ifbDevName)
	isIfbPresent := errIfb == nil && ifbLink != nil

	// Build lookup maps strictly based on rootHandle
	prioToSpec := make(map[uint16]networkingv1alpha1.ClassSpec)
	classIDToSpec := make(map[string]networkingv1alpha1.ClassSpec)
	dynamicDefaultClassID := fmt.Sprintf("%d:99", rootHandle)

	if desired != nil {
		if desired.DefaultClassID != "" {
			dynamicDefaultClassID = desired.DefaultClassID
		} else if desired.DefaultClassMinor > 0 {
			dynamicDefaultClassID = fmt.Sprintf("%d:%d", rootHandle, desired.DefaultClassMinor)
		}

		for _, cls := range desired.Classes {
			cid := cls.GetClassID(int(rootHandle))
			if cid != "" {
				classIDToSpec[cid] = cls
			}

			// Map priority to spec
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
						classIDToSpec[fmt.Sprintf("%d:%d", rootHandle, minor)] = cls
					}
				}
			}

			if cls.Priority == 0 || strings.Contains(strings.ToLower(cls.Name), "default") {
				dynamicDefaultClassID = cid
			}
		}
	}

	// 1. Collect HTB Class Statistics (Egress on Phys, Ingress on IFB)
	type classTarget struct {
		link      netlink.Link
		ifaceName string
		direction string
	}

	defaultDir := "egress"
	if strings.HasPrefix(iface, "ifb-") {
		defaultDir = "ingress"
	}

	classScanTargets := []classTarget{
		{link: link, ifaceName: iface, direction: defaultDir},
	}
	if isIfbPresent && !strings.HasPrefix(iface, "ifb-") {
		classScanTargets = append(classScanTargets, classTarget{
			link:      ifbLink,
			ifaceName: ifbDevName,
			direction: "ingress",
		})
	}

	for _, target := range classScanTargets {
		if classes, err := netlink.ClassList(target.link, 0); err == nil {
			for _, c := range classes {
				htb, ok := c.(*netlink.HtbClass)
				if !ok {
					continue
				}

				handleStr := netlink.HandleStr(htb.Attrs().Handle)
				if handleStr == fmt.Sprintf("%d:1", rootHandle) {
					continue // Skip root HTB class handle
				}

				cStat := networkingv1alpha1.ClassStat{
					Interface: target.ifaceName,
					ClassID:   handleStr,
					Direction: target.direction,
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

				// Resolve class name
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
	}

	// 2. Collect Ingress Filter Statistics across Physical and IFB devices
	type filterTarget struct {
		link      netlink.Link
		ifaceName string
		handles   []uint32
	}

	var filterScanTargets []filterTarget

	if isIfbPresent && !strings.HasPrefix(iface, "ifb-") {
		filterScanTargets = append(filterScanTargets, filterTarget{
			link:      link,
			ifaceName: iface,
			handles:   []uint32{netlink.HANDLE_INGRESS, netlink.HANDLE_MIN_INGRESS, netlink.MakeHandle(0xffff, 2), netlink.HANDLE_ROOT, 0},
		})
		filterScanTargets = append(filterScanTargets, filterTarget{
			link:      ifbLink,
			ifaceName: ifbDevName,
			handles:   []uint32{expectedRootHandle, 0},
		})
	} else {
		filterScanTargets = append(filterScanTargets, filterTarget{
			link:      link,
			ifaceName: iface,
			handles:   []uint32{netlink.HANDLE_INGRESS, netlink.HANDLE_MIN_INGRESS, netlink.MakeHandle(0xffff, 2), netlink.HANDLE_ROOT, expectedRootHandle, 0},
		})
	}

	seenStatsFilter := make(map[string]bool)

	for _, target := range filterScanTargets {
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

				prio := attrs.Priority
				dedupKey := fmt.Sprintf("%s-%d-%d-%d", target.ifaceName, prio, attrs.Handle, attrs.Protocol)
				if seenStatsFilter[dedupKey] {
					continue
				}
				seenStatsFilter[dedupKey] = true

				var bytesVal, pktsVal, dropsVal uint64
				var flowerVlanID int
				filterType := f.Type()

				// Matchall Ingress Redirect Rule on physical interface
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
						Interface: target.ifaceName,
						FilterID:  filterIDStr,
						Direction: "ingress",
						ClassID:   "1:1",
						ClassName: "ifb-ingress-redirect",
						Bytes:     bytesVal,
						Packets:   pktsVal,
						Drops:     dropsVal,
					})
					continue
				}

				if isIfbPresent && target.ifaceName == iface {
					continue
				}

				// Extract Flower stats
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
				} else if genericFilter, ok := f.(*netlink.GenericFilter); ok {
					_ = genericFilter
				}

				filterIDStr := fmt.Sprintf("pref-%d-handle-%d", prio, attrs.Handle)
				iStat := networkingv1alpha1.IngressStat{
					Interface: target.ifaceName,
					FilterID:  filterIDStr,
					Direction: "ingress",
					Bytes:     bytesVal,
					Packets:   pktsVal,
					Drops:     dropsVal,
				}

				// Priority-driven Spec Lookup
				if prio == 49152 || prio == 0 {
					iStat.ClassID = dynamicDefaultClassID
					iStat.ClassName = "default-fallback"
				} else if spec, found := prioToSpec[prio]; found {
					iStat.ClassID = spec.GetClassID(int(rootHandle))
					iStat.ClassName = spec.Name
					iStat.Subnet = spec.Subnet
					iStat.VlanID = spec.VlanID
				} else if desired != nil {
					for _, cls := range desired.Classes {
						clsHandle := cls.GetClassID(int(rootHandle))
						parts := strings.Split(clsHandle, ":")
						minorVal := 0
						if len(parts) == 2 {
							minorVal, _ = strconv.Atoi(parts[1])
						}

						if cls.Priority == int(prio) || cls.ClassMinor == int(prio) || minorVal == int(prio) {
							iStat.ClassID = clsHandle
							iStat.ClassName = cls.Name
							iStat.Subnet = cls.Subnet
							iStat.VlanID = cls.VlanID
							break
						}
					}
				}

				// Fallback formatting if ClassID not resolved
				if iStat.ClassID == "" {
					if flowerVlanID > 0 {
						iStat.ClassID = fmt.Sprintf("%d:%d", rootHandle, flowerVlanID)
						iStat.ClassName = fmt.Sprintf("vlan-%d-migration", flowerVlanID)
						iStat.VlanID = flowerVlanID
					} else {
						iStat.ClassID = fmt.Sprintf("%d:%d", rootHandle, prio)
						iStat.ClassName = fmt.Sprintf("prio-%d-filter", prio)
					}
				}

				stats.IngressStats = append(stats.IngressStats, iStat)
			}
		}
	}

	return stats, nil
}

// GetInterfaceStatsFiltered queries TC stats and filters results by targetVlan or targetClassID.
func GetInterfaceStatsFiltered(iface string, classMap map[string]string, targetVlan int, targetClassID string, rootHtbID int) (*networkingv1alpha1.InterfaceStats, error) {
	if rootHtbID <= 0 {
		rootHtbID = 1
	}

	desired := &networkingv1alpha1.HtbRootSpec{
		Interface: iface,
		HtbID:     rootHtbID,
		Classes:   make([]networkingv1alpha1.ClassSpec, 0),
	}

	for classID, name := range classMap {
		parts := strings.Split(classID, ":")
		minorVal := 0
		if len(parts) == 2 {
			minorVal, _ = strconv.Atoi(parts[1])
		}

		resolvedClassID := fmt.Sprintf("%d:%d", rootHtbID, minorVal)

		desired.Classes = append(desired.Classes, networkingv1alpha1.ClassSpec{
			ClassID:    resolvedClassID,
			Name:       name,
			ClassMinor: minorVal,
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
