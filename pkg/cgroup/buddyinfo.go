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

const buddyPageSize = 4096

const (
	buddyZoneNormal  = "Normal"
	buddyZoneMovable = "Movable"
)

// numaBuddyFree holds exact free buddy block counts for the THP zone per NUMA node.
type numaBuddyFree struct {
	NUMA           string
	Zone           string
	OrderGe9Bytes  uint64
	AllOrdersBytes uint64
}

// buddySnapshot is parsed from a single /proc/buddyinfo read.
type buddySnapshot struct {
	zonesByNUMA map[string]map[string]bool
	thpByNUMA   []numaBuddyFree
}

// buddyZonesByNUMA reports which buddy zones exist for each NUMA node.
func buddyZonesByNUMA(procPath string) (map[string]map[string]bool, error) {
	snap, err := readBuddySnapshot(procPath)
	if err != nil {
		return nil, err
	}
	return snap.zonesByNUMA, nil
}

func thpBuddyZone(zones map[string]bool) string {
	if zones[buddyZoneMovable] {
		return buddyZoneMovable
	}
	return buddyZoneNormal
}

func buddyBytesFromCounts(counts []string) (orderGe9, allOrders uint64, err error) {
	for order, countStr := range counts {
		blocks, err := strconv.ParseUint(countStr, 10, 64)
		if err != nil {
			return 0, 0, fmt.Errorf("parsing buddyinfo order %d: %w", order, err)
		}
		bytes := blocks * (uint64(buddyPageSize) << order)
		allOrders += bytes
		if order >= 9 {
			orderGe9 += bytes
		}
	}
	return orderGe9, allOrders, nil
}

// readBuddySnapshot parses /proc/buddyinfo once and returns zone layout plus
// THP-zone free block counts per NUMA node.
func readBuddySnapshot(procPath string) (buddySnapshot, error) {
	f, err := os.Open(filepath.Join(procPath, "buddyinfo"))
	if err != nil {
		return buddySnapshot{}, fmt.Errorf("opening buddyinfo: %w", err)
	}
	defer f.Close()

	zonesByNUMA := make(map[string]map[string]bool)
	freeByNUMAZone := make(map[string]map[string]numaBuddyFree)

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		parts := strings.Fields(line)
		if len(parts) < 5 || parts[0] != "Node" || parts[2] != "zone" {
			continue
		}
		numa := strings.TrimSuffix(parts[1], ",")
		zone := strings.TrimSuffix(parts[3], ",")
		if zonesByNUMA[numa] == nil {
			zonesByNUMA[numa] = make(map[string]bool)
		}
		zonesByNUMA[numa][zone] = true

		orderGe9, allOrders, err := buddyBytesFromCounts(parts[4:])
		if err != nil {
			return buddySnapshot{}, fmt.Errorf("buddyinfo NUMA %s: %w", numa, err)
		}
		if freeByNUMAZone[numa] == nil {
			freeByNUMAZone[numa] = make(map[string]numaBuddyFree)
		}
		freeByNUMAZone[numa][zone] = numaBuddyFree{
			NUMA:           numa,
			Zone:           zone,
			OrderGe9Bytes:  orderGe9,
			AllOrdersBytes: allOrders,
		}
	}
	if err := scanner.Err(); err != nil {
		return buddySnapshot{}, fmt.Errorf("reading buddyinfo: %w", err)
	}
	if len(zonesByNUMA) == 0 {
		return buddySnapshot{}, fmt.Errorf("buddyinfo: no NUMA entries found")
	}

	thpByNUMA := make([]numaBuddyFree, 0, len(zonesByNUMA))
	for numa, zones := range zonesByNUMA {
		thpZone := thpBuddyZone(zones)
		entry, ok := freeByNUMAZone[numa][thpZone]
		if !ok {
			return buddySnapshot{}, fmt.Errorf("buddyinfo: THP zone %s not found for NUMA %s", thpZone, numa)
		}
		thpByNUMA = append(thpByNUMA, entry)
	}
	if len(thpByNUMA) == 0 {
		return buddySnapshot{}, fmt.Errorf("buddyinfo: THP zone entries not found")
	}

	sort.Slice(thpByNUMA, func(i, j int) bool {
		return thpByNUMA[i].NUMA < thpByNUMA[j].NUMA
	})

	return buddySnapshot{
		zonesByNUMA: zonesByNUMA,
		thpByNUMA:   thpByNUMA,
	}, nil
}

// readBuddyTHPZone parses /proc/buddyinfo for the THP buddy zone per NUMA node:
// Movable when that zone exists on the node, otherwise Normal.
func readBuddyTHPZone(procPath string) ([]numaBuddyFree, error) {
	snap, err := readBuddySnapshot(procPath)
	if err != nil {
		return nil, err
	}
	return snap.thpByNUMA, nil
}

// readBuddyNormal parses /proc/buddyinfo free block counts for the Normal zone per NUMA node.
func readBuddyNormal(procPath string) ([]numaBuddyFree, error) {
	f, err := os.Open(filepath.Join(procPath, "buddyinfo"))
	if err != nil {
		return nil, fmt.Errorf("opening buddyinfo: %w", err)
	}
	defer f.Close()

	var results []numaBuddyFree
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		parts := strings.Fields(line)
		if len(parts) < 5 || parts[0] != "Node" || parts[2] != "zone" {
			continue
		}
		zone := strings.TrimSuffix(parts[3], ",")
		if zone != buddyZoneNormal {
			continue
		}

		numa := strings.TrimSuffix(parts[1], ",")
		orderGe9, allOrders, err := buddyBytesFromCounts(parts[4:])
		if err != nil {
			return nil, fmt.Errorf("buddyinfo NUMA %s: %w", numa, err)
		}
		if allOrders == 0 {
			continue
		}
		results = append(results, numaBuddyFree{
			NUMA:           numa,
			Zone:           buddyZoneNormal,
			OrderGe9Bytes:  orderGe9,
			AllOrdersBytes: allOrders,
		})
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("reading buddyinfo: %w", err)
	}
	if len(results) == 0 {
		return nil, fmt.Errorf("buddyinfo: Normal zone entries not found")
	}
	return results, nil
}
