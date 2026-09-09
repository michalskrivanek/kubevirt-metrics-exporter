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

// readPagetypeExcludedTHPZone parses Unmovable and Isolate counts from the THP
// buddy zone per NUMA node (Movable when present, else Normal).
func readPagetypeExcludedTHPZone(procPath string) ([]numaPagetypeExcluded, error) {
	zonesByNUMA, err := buddyZonesByNUMA(procPath)
	if err != nil {
		return nil, err
	}

	zoneEntries := make(map[string]map[string]numaPagetypeExcluded)
	for _, zones := range zonesByNUMA {
		zone := thpBuddyZone(zones)
		if zoneEntries[zone] != nil {
			continue
		}
		entries, err := readPagetypeExcludedZone(procPath, zone)
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

// readPagetypeUnmovableSum parses Unmovable and Isolate free page counts summed
// across Normal and Movable zones per NUMA node.
func readPagetypeUnmovableSum(procPath string) ([]numaPagetypeExcluded, error) {
	normal, err := readPagetypeExcludedZone(procPath, buddyZoneNormal)
	if err != nil {
		normal = nil
	}
	movable, err := readPagetypeExcludedZone(procPath, buddyZoneMovable)
	if err != nil {
		movable = nil
	}
	if len(normal) == 0 && len(movable) == 0 {
		return nil, nil
	}

	byNUMA := make(map[string]numaPagetypeExcluded)
	for _, entry := range normal {
		byNUMA[entry.NUMA] = entry
	}
	for _, entry := range movable {
		if existing, ok := byNUMA[entry.NUMA]; ok {
			byNUMA[entry.NUMA] = addPagetypeExcluded(existing, entry)
		} else {
			byNUMA[entry.NUMA] = entry
		}
	}

	results := make([]numaPagetypeExcluded, 0, len(byNUMA))
	for _, entry := range byNUMA {
		results = append(results, entry)
	}

	sort.Slice(results, func(i, j int) bool {
		return results[i].NUMA < results[j].NUMA
	})
	return results, nil
}

func readPagetypeExcludedZone(procPath, zone string) ([]numaPagetypeExcluded, error) {
	f, err := os.Open(filepath.Join(procPath, "pagetypeinfo"))
	if err != nil {
		return nil, fmt.Errorf("opening pagetypeinfo: %w", err)
	}
	defer f.Close()

	byNUMA := make(map[string]*numaPagetypeExcluded)
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
		if lineZone != zone {
			continue
		}

		migratype := parts[5]
		var orderGe9Field, allOrdersField *uint64
		switch migratype {
		case "Unmovable":
			// continue below
		case "Isolate":
			// continue below
		default:
			continue
		}

		numa := strings.TrimSuffix(parts[1], ",")
		entry, ok := byNUMA[numa]
		if !ok {
			entry = &numaPagetypeExcluded{NUMA: numa}
			byNUMA[numa] = entry
		}

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
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("reading pagetypeinfo: %w", err)
	}
	if len(byNUMA) == 0 {
		return nil, fmt.Errorf("pagetypeinfo: %s zone Unmovable/Isolate entries not found", zone)
	}

	results := make([]numaPagetypeExcluded, 0, len(byNUMA))
	for _, entry := range byNUMA {
		results = append(results, *entry)
	}

	sort.Slice(results, func(i, j int) bool {
		return results[i].NUMA < results[j].NUMA
	})

	return results, nil
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
