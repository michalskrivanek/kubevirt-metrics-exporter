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

// numaSlabReclaimable holds per-NUMA reclaimable slab from zoneinfo per-node stats.
type numaSlabReclaimable struct {
	NUMA             string
	ReclaimableBytes uint64
}

type zoneinfoSnapshot struct {
	Zones           []numaZoneMemory
	SlabReclaimable []numaSlabReclaimable
}

func zoneBytesFromPages(pages uint64) uint64 {
	return pages * uint64(zonePageSize)
}

// readZoneinfo parses /proc/zoneinfo present and pages-free counts per NUMA node
// and zone, plus per-node nr_slab_reclaimable (not zone-scoped).
func readZoneinfo(procPath string) (zoneinfoSnapshot, error) {
	f, err := os.Open(filepath.Join(procPath, "zoneinfo"))
	if err != nil {
		return zoneinfoSnapshot{}, fmt.Errorf("opening zoneinfo: %w", err)
	}
	defer f.Close()

	var (
		results     []numaZoneMemory
		current     *numaZoneMemory
		inNodeStats bool
		slabByNUMA  = map[string]uint64{}
	)
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if m := nodeZoneRE.FindStringSubmatch(line); m != nil {
			if current != nil && current.PresentBytes > 0 {
				results = append(results, *current)
			}
			inNodeStats = false
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
		if trimmed == "" {
			continue
		}
		if strings.HasPrefix(trimmed, "per-node stats") {
			inNodeStats = true
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
				return zoneinfoSnapshot{}, fmt.Errorf("zoneinfo NUMA %s zone %s present: %w", current.NUMA, current.Zone, err)
			}
			current.PresentBytes = zoneBytesFromPages(pages)
		case parts[0] == "pages" && len(parts) >= 3 && parts[1] == "free":
			inNodeStats = false
			pages, err := strconv.ParseUint(parts[2], 10, 64)
			if err != nil {
				return zoneinfoSnapshot{}, fmt.Errorf("zoneinfo NUMA %s zone %s pages free: %w", current.NUMA, current.Zone, err)
			}
			current.FreeBytes = zoneBytesFromPages(pages)
		case parts[0] == "nr_free_pages":
			pages, err := strconv.ParseUint(parts[1], 10, 64)
			if err != nil {
				return zoneinfoSnapshot{}, fmt.Errorf("zoneinfo NUMA %s zone %s nr_free_pages: %w", current.NUMA, current.Zone, err)
			}
			current.FreeBytes = zoneBytesFromPages(pages)
		case inNodeStats && parts[0] == "nr_slab_reclaimable":
			pages, err := strconv.ParseUint(parts[1], 10, 64)
			if err != nil {
				return zoneinfoSnapshot{}, fmt.Errorf("zoneinfo NUMA %s nr_slab_reclaimable: %w", current.NUMA, err)
			}
			slabByNUMA[current.NUMA] = zoneBytesFromPages(pages)
		}
	}
	if err := scanner.Err(); err != nil {
		return zoneinfoSnapshot{}, fmt.Errorf("reading zoneinfo: %w", err)
	}
	if current != nil && current.PresentBytes > 0 {
		results = append(results, *current)
	}
	if len(results) == 0 {
		return zoneinfoSnapshot{}, fmt.Errorf("zoneinfo: no zone entries found")
	}

	sort.Slice(results, func(i, j int) bool {
		if results[i].NUMA != results[j].NUMA {
			return results[i].NUMA < results[j].NUMA
		}
		return results[i].Zone < results[j].Zone
	})

	slab := make([]numaSlabReclaimable, 0, len(slabByNUMA))
	for numa, bytes := range slabByNUMA {
		slab = append(slab, numaSlabReclaimable{NUMA: numa, ReclaimableBytes: bytes})
	}
	sort.Slice(slab, func(i, j int) bool {
		return slab[i].NUMA < slab[j].NUMA
	})

	return zoneinfoSnapshot{Zones: results, SlabReclaimable: slab}, nil
}
