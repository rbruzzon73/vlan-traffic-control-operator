package executor

import (
	"fmt"
	"os"

	"github.com/vishvananda/netlink"
	networkingv1alpha1 "networking.med.io/vlan-traffic-control/api/v1alpha1"
)

// CollectInterfaceStats queries Netlink to collect live byte, packet, and drop metrics.
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
		// Return clean JSON struct with empty arrays if device is absent on this host
		return stats, nil
	}

	// 1. Collect Egress HTB Class Statistics
	classes, err := netlink.ClassList(link, netlink.MakeHandle(1, 0))
	if err == nil {
		for _, c := range classes {
			if htb, ok := c.(*netlink.HtbClass); ok {
				attrs := htb.Attrs()
				if attrs == nil {
					continue
				}

				classID := netlink.HandleStr(attrs.Handle)
				className := ""

				if rootSpec != nil {
					for _, plannedCls := range rootSpec.Classes {
						if plannedCls.GetClassID(rootSpec.HtbID) == classID {
							className = plannedCls.Name
							break
						}
					}
					if className == "" && (classID == "1:99" || classID == fmt.Sprintf("%d:99", rootSpec.HtbID)) {
						className = "default-fallback"
					}
				}

				var bytes, pkts, overlimits uint64
				if attrs.Statistics != nil {
					if attrs.Statistics.Basic != nil {
						bytes = attrs.Statistics.Basic.Bytes
						pkts = uint64(attrs.Statistics.Basic.Packets)
					}
					if attrs.Statistics.Queue != nil {
						overlimits = uint64(attrs.Statistics.Queue.Overlimits)
					}
				}

				stats.ClassStats = append(stats.ClassStats, networkingv1alpha1.ClassStat{
					ClassID:    classID,
					ClassName:  className,
					Bytes:      bytes,
					Packets:    pkts,
					Overlimits: uint32(overlimits),
				})
			}
		}
	}

	// 2. Collect Ingress Policing Filter Statistics Across ALL Classifier Types (fw, flower, u32)
	filters, err := netlink.FilterList(link, netlink.HANDLE_INGRESS)
	if err == nil {
		for _, f := range filters {
			attrs := f.Attrs()
			if attrs == nil {
				continue
			}

			filterID := fmt.Sprintf("pref %d", attrs.Priority)

			var bytes, pkts, drops uint64

			// Extract policing action statistics dynamically based on concrete filter type
			var actions []netlink.Action
			switch v := f.(type) {
			case *netlink.Flower:
				actions = v.Actions
			case *netlink.U32:
				actions = v.Actions
			case *netlink.GenericFilter:
				actions = nil
			}

			for _, action := range actions {
				if actAttrs := action.Attrs(); actAttrs != nil && actAttrs.Statistics != nil {
					if actAttrs.Statistics.Basic != nil {
						bytes += actAttrs.Statistics.Basic.Bytes
						pkts += uint64(actAttrs.Statistics.Basic.Packets)
					}
					if actAttrs.Statistics.Queue != nil {
						drops += uint64(actAttrs.Statistics.Queue.Drops)
					}
				}
			}

			stats.IngressStats = append(stats.IngressStats, networkingv1alpha1.IngressStat{
				FilterID: filterID,
				Bytes:    bytes,
				Packets:  pkts,
				Drops:    drops,
			})
		}
	}

	return stats, nil
}

// GetInterfaceStatsFiltered matches the 4-argument signature called by cmd/agent/main.go.
func GetInterfaceStatsFiltered(ifaceName string, filterClasses map[string]string, rootHtbID int, targetClassID string) (*networkingv1alpha1.InterfaceStats, error) {
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

	if rootHtbID <= 0 {
		rootHtbID = 1
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

				// Filter by target class ID if specified
				if targetClassID != "" && classID != targetClassID {
					continue
				}

				className := ""
				if filterClasses != nil {
					className = filterClasses[classID]
				}
				if className == "" && classID == fmt.Sprintf("%d:99", rootHtbID) {
					className = "default-fallback"
				}

				var bytes, pkts, overlimits uint64
				if attrs.Statistics != nil {
					if attrs.Statistics.Basic != nil {
						bytes = attrs.Statistics.Basic.Bytes
						pkts = uint64(attrs.Statistics.Basic.Packets)
					}
					if attrs.Statistics.Queue != nil {
						overlimits = uint64(attrs.Statistics.Queue.Overlimits)
					}
				}

				stats.ClassStats = append(stats.ClassStats, networkingv1alpha1.ClassStat{
					ClassID:    classID,
					ClassName:  className,
					Bytes:      bytes,
					Packets:    pkts,
					Overlimits: uint32(overlimits),
				})
			}
		}
	}

	// 2. Collect Ingress Policing Filter Statistics
	filters, err := netlink.FilterList(link, netlink.HANDLE_INGRESS)
	if err == nil {
		for _, f := range filters {
			attrs := f.Attrs()
			if attrs == nil {
				continue
			}

			filterID := fmt.Sprintf("pref %d", attrs.Priority)
			classID := fmt.Sprintf("%d:%d", rootHtbID, attrs.Priority)

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

			for _, action := range actions {
				if actAttrs := action.Attrs(); actAttrs != nil && actAttrs.Statistics != nil {
					if actAttrs.Statistics.Basic != nil {
						bytes += actAttrs.Statistics.Basic.Bytes
						pkts += uint64(actAttrs.Statistics.Basic.Packets)
					}
					if actAttrs.Statistics.Queue != nil {
						drops += uint64(actAttrs.Statistics.Queue.Drops)
					}
				}
			}

			stats.IngressStats = append(stats.IngressStats, networkingv1alpha1.IngressStat{
				FilterID: filterID,
				Bytes:    bytes,
				Packets:  pkts,
				Drops:    drops,
			})
		}
	}

	return stats, nil
}
