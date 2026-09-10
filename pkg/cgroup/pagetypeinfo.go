// Copyright 2026 The KubeVirt Metrics Exporter Authors
// SPDX-License-Identifier: Apache-2.0

package cgroup

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

const (
	pagetypePageSize = 4096
	// pagetypeSaturatedPageCeiling is the per-order page count used when the kernel
	// prints a saturated field (e.g. ">100000"). True free pages at that order are
	// higher; we cap at this value so panels still render without overstating stock.
	pagetypeSaturatedPageCeiling = 100000
)

// pagetypeExcludedByZone maps zone name to per-NUMA Unmovable/Isolate freelist bytes.
type pagetypeExcludedByZone map[string]map[string]numaPagetypeExcluded

// numaPagetypeExcluded holds pagetypeinfo free memory excluded from movable-capable
// buddy stock (Unmovable and Isolate migratypes) for one NUMA node.
type numaPagetypeExcluded struct {
	NUMA                    string
	UnmovableOrderGe9Bytes  uint64
	UnmovableAllOrdersBytes uint64
	IsolateOrderGe9Bytes    uint64
	IsolateAllOrdersBytes   uint64
}

func (e numaPagetypeExcluded) excludedOrderGe9Bytes() uint64 {
	return e.UnmovableOrderGe9Bytes + e.IsolateOrderGe9Bytes
}

func (e numaPagetypeExcluded) excludedAllOrdersBytes() uint64 {
	return e.UnmovableAllOrdersBytes + e.IsolateAllOrdersBytes
}

func addPagetypeExcluded(dst, src numaPagetypeExcluded) numaPagetypeExcluded {
	return numaPagetypeExcluded{
		NUMA:                    dst.NUMA,
		UnmovableOrderGe9Bytes:  dst.UnmovableOrderGe9Bytes + src.UnmovableOrderGe9Bytes,
		UnmovableAllOrdersBytes: dst.UnmovableAllOrdersBytes + src.UnmovableAllOrdersBytes,
		IsolateOrderGe9Bytes:    dst.IsolateOrderGe9Bytes + src.IsolateOrderGe9Bytes,
		IsolateAllOrdersBytes:   dst.IsolateAllOrdersBytes + src.IsolateAllOrdersBytes,
	}
}

// readPagetypeExcludedNormal parses /proc/pagetypeinfo Unmovable and Isolate free
// page counts for the Normal zone per NUMA node.
func readPagetypeExcludedNormal(procPath string) ([]numaPagetypeExcluded, error) {
	return readPagetypeExcludedZone(procPath, buddyZoneNormal)
}

// readPagetypeBuddyStats reads /proc/pagetypeinfo once and derives THP-zone
// excluded migratypes and the Normal+Movable unmovable sum. zonesByNUMA must
// come from the same poll's buddyinfo read so buddyinfo is not opened again.
func readPagetypeBuddyStats(procPath string, zonesByNUMA map[string]map[string]bool) (excludedTHP []numaPagetypeExcluded, unmovableSum []numaPagetypeExcluded, err error) {
	byZone, err := readPagetypeExcludedAllZones(procPath)
	if err != nil {
		return nil, nil, err
	}

	excludedTHP, err = pagetypeExcludedTHPZone(byZone, zonesByNUMA)
	if err != nil {
		return nil, nil, err
	}
	return excludedTHP, pagetypeUnmovableSum(byZone), nil
}

func readPagetypeExcludedZone(procPath, zone string) ([]numaPagetypeExcluded, error) {
	byZone, err := readPagetypeExcludedAllZones(procPath)
	if err != nil {
		return nil, err
	}
	return pagetypeExcludedForZone(byZone, zone)
}

func readPagetypeExcludedAllZones(procPath string) (pagetypeExcludedByZone, error) {
	f, err := os.Open(filepath.Join(procPath, "pagetypeinfo"))
	if err != nil {
		return nil, fmt.Errorf("opening pagetypeinfo: %w", err)
	}
	defer f.Close()

	byZone := make(pagetypeExcludedByZone)
	inFreeSection := false

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "Free pages count per migrate type") {
			inFreeSection = true
			continue
		}
		if !inFreeSection {
			continue
		}
		if strings.HasPrefix(line, "Number of blocks") {
			inFreeSection = false
			continue
		}
		if line == "" {
			continue
		}

		parts := strings.Fields(line)
		if len(parts) < 7 || parts[0] != "Node" || parts[2] != "zone" || parts[4] != "type" {
			continue
		}
		lineZone := strings.TrimSuffix(parts[3], ",")
		migratype := parts[5]
		switch migratype {
		case "Unmovable", "Isolate":
		default:
			continue
		}

		numa := strings.TrimSuffix(parts[1], ",")
		zoneNUMA := byZone[lineZone]
		if zoneNUMA == nil {
			zoneNUMA = make(map[string]numaPagetypeExcluded)
			byZone[lineZone] = zoneNUMA
		}
		entry := zoneNUMA[numa]
		if entry.NUMA == "" {
			entry.NUMA = numa
		}

		var orderGe9Field, allOrdersField *uint64
		switch migratype {
		case "Unmovable":
			orderGe9Field = &entry.UnmovableOrderGe9Bytes
			allOrdersField = &entry.UnmovableAllOrdersBytes
		case "Isolate":
			orderGe9Field = &entry.IsolateOrderGe9Bytes
			allOrdersField = &entry.IsolateAllOrdersBytes
		}

		var orderGe9, allOrders uint64
		for order, countStr := range parts[6:] {
			pages, err := parsePagetypePageCount(countStr)
			if err != nil {
				return nil, fmt.Errorf("parsing pagetypeinfo NUMA %s order %d: %w", numa, order, err)
			}
			bytes := pages * (uint64(pagetypePageSize) << order)
			allOrders += bytes
			if order >= 9 {
				orderGe9 += bytes
			}
		}

		*orderGe9Field = orderGe9
		*allOrdersField = allOrders
		zoneNUMA[numa] = entry
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("reading pagetypeinfo: %w", err)
	}
	if len(byZone) == 0 {
		return nil, fmt.Errorf("pagetypeinfo: no Unmovable/Isolate entries found")
	}
	return byZone, nil
}

func pagetypeExcludedForZone(byZone pagetypeExcludedByZone, zone string) ([]numaPagetypeExcluded, error) {
	zoneNUMA := byZone[zone]
	if len(zoneNUMA) == 0 {
		return nil, fmt.Errorf("pagetypeinfo: %s zone Unmovable/Isolate entries not found", zone)
	}

	results := make([]numaPagetypeExcluded, 0, len(zoneNUMA))
	for _, entry := range zoneNUMA {
		results = append(results, entry)
	}
	sort.Slice(results, func(i, j int) bool {
		return results[i].NUMA < results[j].NUMA
	})
	return results, nil
}

func pagetypeExcludedTHPZone(byZone pagetypeExcludedByZone, zonesByNUMA map[string]map[string]bool) ([]numaPagetypeExcluded, error) {
	zoneEntries := make(map[string]map[string]numaPagetypeExcluded)
	for _, zones := range zonesByNUMA {
		zone := thpBuddyZone(zones)
		if zoneEntries[zone] != nil {
			continue
		}
		entries, err := pagetypeExcludedForZone(byZone, zone)
		if err != nil {
			return nil, err
		}
		zoneEntries[zone] = excludedByNUMAMap(entries)
	}

	results := make([]numaPagetypeExcluded, 0, len(zonesByNUMA))
	for numa, zones := range zonesByNUMA {
		zone := thpBuddyZone(zones)
		entry, ok := zoneEntries[zone][numa]
		if !ok {
			entry = numaPagetypeExcluded{NUMA: numa}
		}
		results = append(results, entry)
	}
	sort.Slice(results, func(i, j int) bool {
		return results[i].NUMA < results[j].NUMA
	})
	return results, nil
}

func pagetypeUnmovableSum(byZone pagetypeExcludedByZone) []numaPagetypeExcluded {
	normal := byZone[buddyZoneNormal]
	movable := byZone[buddyZoneMovable]
	if len(normal) == 0 && len(movable) == 0 {
		return nil
	}

	byNUMA := make(map[string]numaPagetypeExcluded)
	for numa, entry := range normal {
		byNUMA[numa] = entry
	}
	for numa, entry := range movable {
		if existing, ok := byNUMA[numa]; ok {
			byNUMA[numa] = addPagetypeExcluded(existing, entry)
		} else {
			byNUMA[numa] = entry
		}
	}

	results := make([]numaPagetypeExcluded, 0, len(byNUMA))
	for _, entry := range byNUMA {
		results = append(results, entry)
	}
	sort.Slice(results, func(i, j int) bool {
		return results[i].NUMA < results[j].NUMA
	})
	return results
}

func excludedByNUMAMap(excluded []numaPagetypeExcluded) map[string]numaPagetypeExcluded {
	byNUMA := make(map[string]numaPagetypeExcluded, len(excluded))
	for _, e := range excluded {
		byNUMA[e.NUMA] = e
	}
	return byNUMA
}

// parsePagetypePageCount parses a free-page count from pagetypeinfo.
func parsePagetypePageCount(s string) (uint64, error) {
	if strings.HasPrefix(s, ">") {
		return pagetypeSaturatedPageCeiling, nil
	}
	return strconv.ParseUint(s, 10, 64)
}

// subtractExcludedBytes returns buddy minus excluded stock, clamped at zero for inconsistent snapshots.
func subtractExcludedBytes(buddy, excluded uint64) uint64 {
	if buddy <= excluded {
		return 0
	}
	return buddy - excluded
}
