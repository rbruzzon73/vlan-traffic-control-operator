package executor

import (
	"bufio"
	"fmt"
	"strconv"
	"strings"
)

// ClassStats holds structured traffic metrics for an HTB class
type ClassStats struct {
	ClassID    string `json:"classId"`
	Bytes      uint64 `json:"bytes"`
	Packets    uint64 `json:"packets"`
	Drops      uint64 `json:"drops,omitempty"`
	Overlimits uint64 `json:"overlimits,omitempty"`
	RateBytes  uint64 `json:"rateBytes,omitempty"`
	RatePkt    uint64 `json:"ratePkt,omitempty"`
}

// IngressFilterStats holds structured traffic metrics for an ingress policing filter
type IngressFilterStats struct {
	FilterID string `json:"filterId"`
	Bytes    uint64 `json:"bytes"`
	Packets  uint64 `json:"packets"`
	Drops    uint64 `json:"drops"`
}

// GetHtbClassStatsStructured parses `tc -s class show dev <iface>` output into structured Go structs
func GetHtbClassStatsStructured(iface string) ([]ClassStats, error) {
	cmd := execHostCommand("tc", "-s", "class", "show", "dev", iface)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("failed fetching HTB class stats on %s: %w (%s)", iface, err, strings.TrimSpace(string(out)))
	}

	var stats []ClassStats
	scanner := bufio.NewScanner(strings.NewReader(string(out)))
	var current *ClassStats

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())

		// Header line: class htb 1:10 parent 1:1 prio 0 rate 100Mbit ceil 500Mbit ...
		if strings.HasPrefix(line, "class htb") {
			if current != nil {
				stats = append(stats, *current)
			}
			parts := strings.Fields(line)
			classID := ""
			if len(parts) >= 3 {
				classID = parts[2]
			}
			current = &ClassStats{
				ClassID: classID,
			}
			continue
		}

		if current == nil {
			continue
		}

		// Line 2: Sent 123456 bytes 1234 pkt (dropped 0, overlimits 0 requeues 0)
		if strings.HasPrefix(line, "Sent") {
			parts := strings.Fields(line)
			for i := 0; i < len(parts); i++ {
				if parts[i] == "bytes" && i > 0 {
					current.Bytes, _ = strconv.ParseUint(parts[i-1], 10, 64)
				}
				if parts[i] == "pkt" && i > 0 {
					current.Packets, _ = strconv.ParseUint(parts[i-1], 10, 64)
				}
				if strings.HasPrefix(parts[i], "dropped") && i+1 < len(parts) {
					cleanVal := strings.TrimRight(parts[i+1], ",")
					current.Drops, _ = strconv.ParseUint(cleanVal, 10, 64)
				}
				if strings.HasPrefix(parts[i], "overlimits") && i+1 < len(parts) {
					cleanVal := strings.TrimRight(parts[i+1], ",")
					current.Overlimits, _ = strconv.ParseUint(cleanVal, 10, 64)
				}
			}
		}

		// Line 3: rate 10Mbit 1000pps backlog 0b 0p requeues 0
		if strings.HasPrefix(line, "rate") {
			parts := strings.Fields(line)
			if len(parts) >= 2 {
				current.RateBytes = parseRateToBytes(parts[1])
			}
		}
	}

	if current != nil {
		stats = append(stats, *current)
	}

	return stats, nil
}

// GetIngressFilterStatsStructured parses `tc -s filter show dev <iface> ingress` output
func GetIngressFilterStatsStructured(iface string) ([]IngressFilterStats, error) {
	cmd := execHostCommand("tc", "-s", "filter", "show", "dev", iface, "ingress")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("failed fetching ingress filter stats on %s: %w (%s)", iface, err, strings.TrimSpace(string(out)))
	}

	var stats []IngressFilterStats
	scanner := bufio.NewScanner(strings.NewReader(string(out)))
	var current *IngressFilterStats

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())

		if strings.HasPrefix(line, "filter") {
			if current != nil {
				stats = append(stats, *current)
			}
			parts := strings.Fields(line)
			filterID := ""
			if len(parts) >= 3 {
				filterID = parts[2]
			}
			current = &IngressFilterStats{
				FilterID: filterID,
			}
			continue
		}

		if current == nil {
			continue
		}

		// Policing action counter line: Action 1: police ... Sent 12345 bytes 100 pkt (dropped 5 ...)
		if strings.Contains(line, "Sent") {
			parts := strings.Fields(line)
			for i := 0; i < len(parts); i++ {
				if parts[i] == "bytes" && i > 0 {
					current.Bytes, _ = strconv.ParseUint(parts[i-1], 10, 64)
				}
				if parts[i] == "pkt" && i > 0 {
					current.Packets, _ = strconv.ParseUint(parts[i-1], 10, 64)
				}
				if strings.HasPrefix(parts[i], "dropped") && i+1 < len(parts) {
					cleanVal := strings.TrimRight(parts[i+1], ",")
					current.Drops, _ = strconv.ParseUint(cleanVal, 10, 64)
				}
			}
		}
	}

	if current != nil {
		stats = append(stats, *current)
	}

	return stats, nil
}

func parseRateToBytes(rateStr string) uint64 {
	rateStr = strings.ToLower(strings.TrimSpace(rateStr))
	var multiplier uint64 = 1

	switch {
	case strings.HasSuffix(rateStr, "gbit"):
		multiplier = 1000 * 1000 * 1000 / 8
		rateStr = strings.TrimSuffix(rateStr, "gbit")
	case strings.HasSuffix(rateStr, "mbit"):
		multiplier = 1000 * 1000 / 8
		rateStr = strings.TrimSuffix(rateStr, "mbit")
	case strings.HasSuffix(rateStr, "kbit"):
		multiplier = 1000 / 8
		rateStr = strings.TrimSuffix(rateStr, "kbit")
	case strings.HasSuffix(rateStr, "bps"):
		multiplier = 1
		rateStr = strings.TrimSuffix(rateStr, "bps")
	}

	val, err := strconv.ParseUint(rateStr, 10, 64)
	if err != nil {
		return 0
	}
	return val * multiplier
}
