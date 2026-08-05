package executor

import (
	"fmt"
	"os"
	"strings"

	"github.com/vishvananda/netlink"
	networkingv1alpha1 "networking.med.io/vlan-traffic-control/api/v1alpha1"
)

// CollectInterfaceStats queries Netlink to collect live byte, packet, and drop metrics for a single interface spec.
func CollectInterfaceStats(ifaceName string, rootSpec *networkingv1alpha1.HtbRootSpec) (*networkingv1alpha1.InterfaceStats, error) {
	nodeName := os.Getenv("NODE_NAME")
	if nodeName == "" {
		nodeName, _ = os.Hostname()
	}

	stats := &networkingv1alpha1.InterfaceStats{
		Interface:    ifaceName,
		Node:         nodeName,
		ClassStats:   make([]networkingv1alpha1.ClassStat, 0),
		IngressStats: make([]networkingv1alpha1.IngressStat, 0),
	}

	link, err := netlink.LinkByName(ifaceName)
	if err != nil {
		return stats, nil
	}

	rootHtbID := 1
	if rootSpec != nil && rootSpec.HtbID > 0 {
		rootHtbID = rootSpec.HtbID
	}

	prioToClassID := make(map[uint16]string)
	if rootSpec != nil {
		for _, cls := range rootSpec.Classes {
			prio := uint16(cls.Priority)
			if prio == 0 {
				prio = 1
			}
			prioToClassID[prio] = cls.GetClassID(rootSpec.HtbID)
		}
	}

	// 1. Collect Egress HTB Class Statistics
	classes, err := netlink.ClassList(link, netlink.MakeHandle(uint16(rootHtbID), 0))
	if err == nil {
		for _, c := range classes {
			if htb, ok := c.(*netlink.HtbClass); ok {
				attrs := htb.Attrs()
				if attrs == nil {
					continue
				}

				classID := netlink.HandleStr(attrs.Handle)

				// Filter out internal HTB root parent class handle (e.g. 1:1)
				if classID == fmt.Sprintf("%d:1", rootHtbID) {
					continue
				}

				className := ""
				if rootSpec != nil {
					for _, plannedCls := range rootSpec.Classes {
						if plannedCls.GetClassID(rootSpec.HtbID) == classID {
							className = plannedCls.Name
							break
						}
					}
					defaultHandle := fmt.Sprintf("%d:%d", rootSpec.HtbID, rootSpec.DefaultClassMinor)
					if rootSpec.DefaultClassID != "" {
						defaultHandle = rootSpec.DefaultClassID
					}
					if className == "" && (classID == "1:99" || classID == defaultHandle) {
						className = "default-fallback"
					}
				}

				var bytes, pkts, overlimits, drops uint64
				if attrs.Statistics != nil {
					if attrs.Statistics.Basic != nil {
						bytes = attrs.Statistics.Basic.Bytes
						pkts = uint64(attrs.Statistics.Basic.Packets)
					}
					if attrs.Statistics.Queue != nil {
						overlimits = uint64(attrs.Statistics.Queue.Overlimits)
						drops = uint64(attrs.Statistics.Queue.Drops)
					}
				}

				stats.ClassStats = append(stats.ClassStats, networkingv1alpha1.ClassStat{
					ClassID:    classID,
					ClassName:  className,
					Priority:   int(htb.Prio),
					Bytes:      bytes,
					Packets:    pkts,
					Overlimits: uint32(overlimits),
					Drops:      uint32(drops),
					Borrowed:   0,
				})
			}
		}
	}

	// 2. Collect Ingress Filter Statistics
	collectIngressStats(link, rootHtbID, "", prioToClassID, stats)

	return stats, nil
}

// GetInterfaceStatsFiltered matches the parameter signature called in cmd/agent/main.go.
func GetInterfaceStatsFiltered(ifaceName string, filterClasses map[string]string, targetVlan int, targetClassID string) (*networkingv1alpha1.InterfaceStats, error) {
	nodeName := os.Getenv("NODE_NAME")
	if nodeName == "" {
		nodeName, _ = os.Hostname()
	}

	stats := &networkingv1alpha1.InterfaceStats{
		Interface:    ifaceName,
		Node:         nodeName,
		ClassStats:   make([]networkingv1alpha1.ClassStat, 0),
		IngressStats: make([]networkingv1alpha1.IngressStat, 0),
	}

	link, err := netlink.LinkByName(ifaceName)
	if err != nil {
		return stats, nil
	}

	rootHtbID := 1
	prioToClassID := make(map[uint16]string)

	if filterClasses != nil {
		for cid := range filterClasses {
			var minor int
			if idx := strings.Index(cid, ":"); idx != -1 {
				_, _ = fmt.Sscanf(cid[idx+1:], "%d", &minor)
			}
			if minor > 0 {
				prioToClassID[uint16(minor)] = cid
			}
		}
	}

	// 1. Collect Egress HTB Class Statistics
	classes, err := netlink.ClassList(link, netlink.MakeHandle(uint16(rootHtbID), 0))
	if err == nil {
		for _, c := range classes {
			if htb, ok := c.(*netlink.HtbClass); ok {
				attrs := htb.Attrs()
				if attrs == nil {
					continue
				}

				classID := netlink.HandleStr(attrs.Handle)

				// Filter out internal HTB root parent class handle (e.g. 1:1)
				if classID == fmt.Sprintf("%d:1", rootHtbID) {
					continue
				}

				// Filter by target class ID if specified
				if targetClassID != "" && classID != targetClassID {
					continue
				}

				className := ""
				if filterClasses != nil {
					className = filterClasses[classID]
				}
				if className == "" && (classID == "1:99" || classID == fmt.Sprintf("%d:99", rootHtbID)) {
					className = "default-fallback"
				}

				var bytes, pkts, overlimits, drops uint64
				if attrs.Statistics != nil {
					if attrs.Statistics.Basic != nil {
						bytes = attrs.Statistics.Basic.Bytes
						pkts = uint64(attrs.Statistics.Basic.Packets)
					}
					if attrs.Statistics.Queue != nil {
						overlimits = uint64(attrs.Statistics.Queue.Overlimits)
						drops = uint64(attrs.Statistics.Queue.Drops)
					}
				}

				stats.ClassStats = append(stats.ClassStats, networkingv1alpha1.ClassStat{
					ClassID:    classID,
					ClassName:  className,
					Priority:   int(htb.Prio),
					Bytes:      bytes,
					Packets:    pkts,
					Overlimits: uint32(overlimits),
					Drops:      uint32(drops),
					Borrowed:   0,
				})
			}
		}
	}

	// 2. Collect Ingress Filter Statistics across all potential handles
	collectIngressStats(link, rootHtbID, targetClassID, prioToClassID, stats)

	return stats, nil
}

// Internal helper to sweep HANDLE_INGRESS, HANDLE_MIN_INGRESS, clsact, and HANDLE_ROOT safely
func collectIngressStats(link netlink.Link, rootHtbID int, targetClassID string, prioToClassID map[uint16]string, stats *networkingv1alpha1.InterfaceStats) {
	clsactIngressHandle := netlink.MakeHandle(0xffff, 2)
	handlesToScan := []uint32{netlink.HANDLE_INGRESS, netlink.HANDLE_MIN_INGRESS, clsactIngressHandle, netlink.HANDLE_ROOT}
	seenFilters := make(map[string]bool)

	for _, parentHandle := range handlesToScan {
		filters, err := netlink.FilterList(link, parentHandle)
		if err != nil {
			continue
		}

		for _, f := range filters {
			attrs := f.Attrs()
			if attrs == nil {
				continue
			}

			filterID := fmt.Sprintf("pref %d", attrs.Priority)

			// Resolve actual ClassID dynamically from priority mapping
			classID := ""
			if cid, exists := prioToClassID[attrs.Priority]; exists {
				classID = cid
			} else {
				// Fallback matching logic for standard minor handles
				switch attrs.Priority {
				case 1:
					classID = fmt.Sprintf("%d:100", rootHtbID)
				case 2:
					classID = fmt.Sprintf("%d:280", rootHtbID)
				case 3:
					classID = fmt.Sprintf("%d:380", rootHtbID)
				case 4:
					classID = fmt.Sprintf("%d:400", rootHtbID)
				default:
					classID = fmt.Sprintf("%d:%d", rootHtbID, attrs.Priority)
				}
			}

			// De-duplicate filters across scanned handles
			dedupKey := fmt.Sprintf("%s-%s", filterID, classID)
			if seenFilters[dedupKey] {
				continue
			}

			if targetClassID != "" && classID != targetClassID {
				continue
			}

			var bytes, pkts, drops uint64
			var actions []netlink.Action

			switch v := f.(type) {
			case *netlink.Flower:
				actions = v.Actions
			case *netlink.U32:
				actions = v.Actions
			case *netlink.GenericFilter:
				actions = nil
			}

			// Extract statistics from attached actions (e.g., police/act_police)
			for _, action := range actions {
				if actAttrs := action.Attrs(); actAttrs != nil && actAttrs.Statistics != nil {
					if actAttrs.Statistics.Basic != nil {
						if actAttrs.Statistics.Basic.Bytes > bytes {
							bytes = actAttrs.Statistics.Basic.Bytes
						}
						if uint64(actAttrs.Statistics.Basic.Packets) > pkts {
							pkts = uint64(actAttrs.Statistics.Basic.Packets)
						}
					}
					if actAttrs.Statistics.Queue != nil {
						if uint64(actAttrs.Statistics.Queue.Drops) > drops {
							drops = uint64(actAttrs.Statistics.Queue.Drops)
						}
					}
				}
			}

			seenFilters[dedupKey] = true
			stats.IngressStats = append(stats.IngressStats, networkingv1alpha1.IngressStat{
				ClassID:  classID,
				FilterID: filterID,
				Bytes:    bytes,
				Packets:  pkts,
				Drops:    drops,
			})
		}
	}
}
