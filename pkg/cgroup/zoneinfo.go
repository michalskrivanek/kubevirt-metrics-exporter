package cgroup

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

const zonePageSize = 4096

var nodeZoneRE = regexp.MustCompile(`^Node\s+(\d+),\s+zone\s+(\S+)`)

// numaZoneMemory holds zone size and free pages from /proc/zoneinfo.
type numaZoneMemory struct {
	NUMA         string
	Zone         string
	PresentBytes uint64
	FreeBytes    uint64
}

func zoneBytesFromPages(pages uint64) uint64 {
	return pages * uint64(zonePageSize)
}

// readZoneinfo parses /proc/zoneinfo present and pages-free counts per NUMA node and zone.
func readZoneinfo(procPath string) ([]numaZoneMemory, error) {
	f, err := os.Open(filepath.Join(procPath, "zoneinfo"))
	if err != nil {
		return nil, fmt.Errorf("opening zoneinfo: %w", err)
	}
	defer f.Close()

	var (
		results []numaZoneMemory
		current *numaZoneMemory
	)
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if m := nodeZoneRE.FindStringSubmatch(line); m != nil {
			if current != nil && current.PresentBytes > 0 {
				results = append(results, *current)
			}
			current = &numaZoneMemory{
				NUMA: m[1],
				Zone: m[2],
			}
			continue
		}
		if current == nil {
			continue
		}

		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "per-node stats") {
			continue
		}
		parts := strings.Fields(trimmed)
		if len(parts) < 2 {
			continue
		}

		switch {
		case parts[0] == "present":
			pages, err := strconv.ParseUint(parts[1], 10, 64)
			if err != nil {
				return nil, fmt.Errorf("zoneinfo NUMA %s zone %s present: %w", current.NUMA, current.Zone, err)
			}
			current.PresentBytes = zoneBytesFromPages(pages)
		case parts[0] == "pages" && len(parts) >= 3 && parts[1] == "free":
			pages, err := strconv.ParseUint(parts[2], 10, 64)
			if err != nil {
				return nil, fmt.Errorf("zoneinfo NUMA %s zone %s pages free: %w", current.NUMA, current.Zone, err)
			}
			current.FreeBytes = zoneBytesFromPages(pages)
		case parts[0] == "nr_free_pages":
			pages, err := strconv.ParseUint(parts[1], 10, 64)
			if err != nil {
				return nil, fmt.Errorf("zoneinfo NUMA %s zone %s nr_free_pages: %w", current.NUMA, current.Zone, err)
			}
			current.FreeBytes = zoneBytesFromPages(pages)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("reading zoneinfo: %w", err)
	}
	if current != nil && current.PresentBytes > 0 {
		results = append(results, *current)
	}
	if len(results) == 0 {
		return nil, fmt.Errorf("zoneinfo: no zone entries found")
	}

	sort.Slice(results, func(i, j int) bool {
		if results[i].NUMA != results[j].NUMA {
			return results[i].NUMA < results[j].NUMA
		}
		return results[i].Zone < results[j].Zone
	})
	return results, nil
}
