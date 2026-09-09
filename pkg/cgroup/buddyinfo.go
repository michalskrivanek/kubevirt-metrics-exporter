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

// buddyZonesByNUMA reports which buddy zones exist for each NUMA node.
func buddyZonesByNUMA(procPath string) (map[string]map[string]bool, error) {
	f, err := os.Open(filepath.Join(procPath, "buddyinfo"))
	if err != nil {
		return nil, fmt.Errorf("opening buddyinfo: %w", err)
	}
	defer f.Close()

	byNUMA := make(map[string]map[string]bool)
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		parts := strings.Fields(line)
		if len(parts) < 5 || parts[0] != "Node" || parts[2] != "zone" {
			continue
		}
		numa := strings.TrimSuffix(parts[1], ",")
		zone := strings.TrimSuffix(parts[3], ",")
		if byNUMA[numa] == nil {
			byNUMA[numa] = make(map[string]bool)
		}
		byNUMA[numa][zone] = true
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("reading buddyinfo: %w", err)
	}
	if len(byNUMA) == 0 {
		return nil, fmt.Errorf("buddyinfo: no NUMA entries found")
	}
	return byNUMA, nil
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

// readBuddyTHPZone parses /proc/buddyinfo for the THP buddy zone per NUMA node:
// Movable when that zone exists on the node, otherwise Normal.
func readBuddyTHPZone(procPath string) ([]numaBuddyFree, error) {
	f, err := os.Open(filepath.Join(procPath, "buddyinfo"))
	if err != nil {
		return nil, fmt.Errorf("opening buddyinfo: %w", err)
	}
	defer f.Close()

	zonesByNUMA, err := buddyZonesByNUMA(procPath)
	if err != nil {
		return nil, err
	}

	byNUMAZone := make(map[string]map[string]numaBuddyFree)
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		parts := strings.Fields(line)
		if len(parts) < 5 || parts[0] != "Node" || parts[2] != "zone" {
			continue
		}
		numa := strings.TrimSuffix(parts[1], ",")
		zone := strings.TrimSuffix(parts[3], ",")
		thpZone := thpBuddyZone(zonesByNUMA[numa])
		if zone != thpZone {
			continue
		}

		orderGe9, allOrders, err := buddyBytesFromCounts(parts[4:])
		if err != nil {
			return nil, fmt.Errorf("buddyinfo NUMA %s: %w", numa, err)
		}
		if byNUMAZone[numa] == nil {
			byNUMAZone[numa] = make(map[string]numaBuddyFree)
		}
		byNUMAZone[numa][zone] = numaBuddyFree{
			NUMA:           numa,
			Zone:           zone,
			OrderGe9Bytes:  orderGe9,
			AllOrdersBytes: allOrders,
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("reading buddyinfo: %w", err)
	}

	results := make([]numaBuddyFree, 0, len(zonesByNUMA))
	for numa, zones := range zonesByNUMA {
		thpZone := thpBuddyZone(zones)
		entry, ok := byNUMAZone[numa][thpZone]
		if !ok {
			return nil, fmt.Errorf("buddyinfo: THP zone %s not found for NUMA %s", thpZone, numa)
		}
		results = append(results, entry)
	}
	if len(results) == 0 {
		return nil, fmt.Errorf("buddyinfo: THP zone entries not found")
	}

	sort.Slice(results, func(i, j int) bool {
		return results[i].NUMA < results[j].NUMA
	})

	return results, nil
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
