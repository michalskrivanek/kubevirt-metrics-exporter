// Copyright 2026 The KubeVirt Metrics Exporter Authors
// SPDX-License-Identifier: Apache-2.0

package cgroup

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/cache"

	"github.com/openshift-virtualization/kubevirt-metrics-exporter/pkg/cri"
)

// Per-VMI memory metric descriptors (aligned with CRI-O PR #10143).
var (
	activeAnonDesc = prometheus.NewDesc(
		"container_memory_active_anon_bytes",
		"Active anonymous memory in bytes",
		[]string{"namespace", "name", "node", "pod"},
		nil,
	)

	inactiveAnonDesc = prometheus.NewDesc(
		"container_memory_inactive_anon_bytes",
		"Inactive anonymous memory in bytes",
		[]string{"namespace", "name", "node", "pod"},
		nil,
	)

	anonTHPDesc = prometheus.NewDesc(
		"container_memory_anon_thp_bytes",
		"Anonymous memory backed by transparent hugepages in bytes",
		[]string{"namespace", "name", "node", "pod"},
		nil,
	)

	shmemTHPDesc = prometheus.NewDesc(
		"container_memory_shmem_thp_bytes",
		"Shared memory backed by transparent hugepages in bytes. Requires kernel 6.8+ with shmem THP enabled.",
		[]string{"namespace", "name", "node", "pod"},
		nil,
	)

	fileTHPDesc = prometheus.NewDesc(
		"container_memory_file_thp_bytes",
		"File-backed memory backed by transparent hugepages in bytes. Requires file THP enabled.",
		[]string{"namespace", "name", "node", "pod"},
		nil,
	)
)

// Per-node metric descriptors.
var (
	khugepageCPUDesc = prometheus.NewDesc(
		"kme_cgroup_khugepaged_cpu_seconds_total",
		"Cumulative CPU time in seconds consumed by the khugepaged kernel thread",
		[]string{"node"},
		nil,
	)

	ksmdCPUDesc = prometheus.NewDesc(
		"kme_cgroup_ksmd_cpu_seconds_total",
		"Cumulative CPU time in seconds consumed by the ksmd kernel thread",
		[]string{"node"},
		nil,
	)

	// Aligned with node_exporter PR #3778.
	ksmProfitDesc = prometheus.NewDesc(
		"node_ksmd_general_profit_bytes",
		"Net memory saved by KSM after subtracting rmap_item tracking overhead in bytes. Negative means KSM overhead exceeds savings.",
		nil, nil,
	)

	thpSplitPMDDesc = prometheus.NewDesc(
		"kme_node_thp_split_pmd_total",
		"Total number of THP page table downgrades (thp_split_pmd from /proc/vmstat)",
		[]string{"node"},
		nil,
	)

	thpCollapseAllocDesc = prometheus.NewDesc(
		"kme_node_thp_collapse_alloc_total",
		"Total number of successful THP collapses by khugepaged (thp_collapse_alloc from /proc/vmstat)",
		[]string{"node"},
		nil,
	)

	movableOrderGe9Desc = prometheus.NewDesc(
		"kme_node_movable_bytes_order_ge_9",
		"Movable-capable free buddy memory at page order 9 and above in bytes (Movable zone if present, else Normal; buddy minus pagetype Unmovable and Isolate)",
		[]string{"node", "numa"},
		nil,
	)

	movableAllOrdersDesc = prometheus.NewDesc(
		"kme_node_movable_bytes_all_orders",
		"Movable-capable free buddy memory across all page orders in bytes (Movable zone if present, else Normal; buddy minus pagetype Unmovable and Isolate)",
		[]string{"node", "numa"},
		nil,
	)

	buddyOrderGe9Desc = prometheus.NewDesc(
		"kme_node_buddy_bytes_order_ge_9",
		"Total free buddy memory at page order 9 and above in bytes (Movable zone if present, else Normal; exact from /proc/buddyinfo)",
		[]string{"node", "numa"},
		nil,
	)

	buddyAllOrdersDesc = prometheus.NewDesc(
		"kme_node_buddy_bytes_all_orders",
		"Total free buddy memory across all page orders in bytes (Movable zone if present, else Normal; exact from /proc/buddyinfo)",
		[]string{"node", "numa"},
		nil,
	)

	unmovableOrderGe9Desc = prometheus.NewDesc(
		"kme_node_unmovable_bytes_order_ge_9",
		"Unmovable free buddy memory at page order 9 and above in bytes (Normal + Movable zones, from /proc/pagetypinfo)",
		[]string{"node", "numa"},
		nil,
	)

	unmovableAllOrdersDesc = prometheus.NewDesc(
		"kme_node_unmovable_bytes_all_orders",
		"Unmovable free buddy memory across all page orders in bytes (Normal + Movable zones, from /proc/pagetypinfo)",
		[]string{"node", "numa"},
		nil,
	)

	zonePresentBytesDesc = prometheus.NewDesc(
		"kme_node_zone_present_bytes",
		"Zone size in bytes from /proc/zoneinfo present pages (structural layout; DMA+DMA32+Normal reflect kernelcore pool)",
		[]string{"node", "numa", "zone"},
		nil,
	)

	zoneFreeBytesDesc = prometheus.NewDesc(
		"kme_node_zone_free_bytes",
		"Zone free pages in bytes from /proc/zoneinfo pages free (buddy freelist within the zone)",
		[]string{"node", "numa", "zone"},
		nil,
	)
)

// Operational metric descriptors.
var (
	scrapeErrorsDesc = prometheus.NewDesc(
		"kme_cgroup_scrape_errors_total",
		"Total errors encountered during cgroup subsystem poll cycles",
		nil, nil,
	)

	lastPollDesc = prometheus.NewDesc(
		"kme_cgroup_last_poll_timestamp_seconds",
		"Unix timestamp of the last cgroup subsystem poll cycle",
		nil, nil,
	)
)

type Config struct {
	NodeName     string
	PollInterval time.Duration
	CgroupRoot   string
	ProcPath     string
	SysPath      string
}

type vmiMemStats struct {
	namespace    string
	name         string
	pod          string
	activeAnon   uint64
	inactiveAnon uint64
	anonTHP      uint64
	shmemTHP     uint64
	fileTHP      uint64
}

type nodeStats struct {
	khugepageCPU       float64
	khugepageAvailable bool
	ksmdCPU            float64
	ksmdAvailable      bool
	ksmProfit          int64
	ksmProfitAvailable bool
	thpSplitPMD        uint64
	thpCollapseAlloc   uint64
	thpVMStatAvailable bool
	buddyByNuma        []numaBuddyFree
	buddyAvailable     bool
	excludedByNuma     []numaPagetypeExcluded
	unmovableByNuma    []numaPagetypeExcluded
	pagetypeAvailable  bool
	zoneByNuma         []numaZoneMemory
	zoneinfoAvailable  bool
}

type Collector struct {
	cfg       Config
	podStore  cache.Store
	criClient *cri.Client
	log       *slog.Logger

	mu           sync.RWMutex
	vmiResults   []vmiMemStats
	node         nodeStats
	scrapeErrors float64
	lastPollTS   float64

	// Absolute cgroup path of this process, used to resolve namespace-relative
	// paths from /proc/<pid>/cgroup. Empty if no cgroup namespace is active.
	cgroupNSRoot string

	// Cached kernel thread PIDs (discovered once, re-discovered on failure).
	khugepagePID int
	ksmdPID      int
}

func NewCollector(cfg Config, podStore cache.Store, criClient *cri.Client, log *slog.Logger) *Collector {
	if cfg.SysPath == "" {
		cfg.SysPath = "/sys"
	}
	return &Collector{
		cfg:       cfg,
		podStore:  podStore,
		criClient: criClient,
		log:       log,
	}
}

func (c *Collector) Run(ctx context.Context) {
	nsRoot, err := findSelfCgroup(c.cfg.CgroupRoot)
	if err != nil {
		c.log.Warn("cgroup: could not discover own cgroup, namespace-relative paths may fail", "error", err)
	} else {
		c.cgroupNSRoot = nsRoot
		c.log.Info("cgroup: discovered namespace root", "path", nsRoot)
	}

	c.discoverKernelThreads()
	c.poll(ctx)

	ticker := time.NewTicker(c.cfg.PollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.poll(ctx)
		}
	}
}

func (c *Collector) Describe(ch chan<- *prometheus.Desc) {
	ch <- activeAnonDesc
	ch <- inactiveAnonDesc
	ch <- anonTHPDesc
	ch <- shmemTHPDesc
	ch <- fileTHPDesc
	ch <- khugepageCPUDesc
	ch <- ksmdCPUDesc
	ch <- ksmProfitDesc
	ch <- thpSplitPMDDesc
	ch <- thpCollapseAllocDesc
	ch <- movableOrderGe9Desc
	ch <- movableAllOrdersDesc
	ch <- buddyOrderGe9Desc
	ch <- buddyAllOrdersDesc
	ch <- unmovableOrderGe9Desc
	ch <- unmovableAllOrdersDesc
	ch <- zonePresentBytesDesc
	ch <- zoneFreeBytesDesc
	ch <- scrapeErrorsDesc
	ch <- lastPollDesc
}

func (c *Collector) Collect(ch chan<- prometheus.Metric) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	ch <- prometheus.MustNewConstMetric(scrapeErrorsDesc, prometheus.CounterValue, c.scrapeErrors)
	ch <- prometheus.MustNewConstMetric(lastPollDesc, prometheus.GaugeValue, c.lastPollTS)

	for _, s := range c.vmiResults {
		labels := []string{s.namespace, s.name, c.cfg.NodeName, s.pod}
		ch <- prometheus.MustNewConstMetric(activeAnonDesc, prometheus.GaugeValue, float64(s.activeAnon), labels...)
		ch <- prometheus.MustNewConstMetric(inactiveAnonDesc, prometheus.GaugeValue, float64(s.inactiveAnon), labels...)
		ch <- prometheus.MustNewConstMetric(anonTHPDesc, prometheus.GaugeValue, float64(s.anonTHP), labels...)
		ch <- prometheus.MustNewConstMetric(shmemTHPDesc, prometheus.GaugeValue, float64(s.shmemTHP), labels...)
		ch <- prometheus.MustNewConstMetric(fileTHPDesc, prometheus.GaugeValue, float64(s.fileTHP), labels...)
	}

	if c.node.khugepageAvailable {
		ch <- prometheus.MustNewConstMetric(khugepageCPUDesc, prometheus.CounterValue, c.node.khugepageCPU, c.cfg.NodeName)
	}
	if c.node.ksmdAvailable {
		ch <- prometheus.MustNewConstMetric(ksmdCPUDesc, prometheus.CounterValue, c.node.ksmdCPU, c.cfg.NodeName)
	}
	if c.node.ksmProfitAvailable {
		ch <- prometheus.MustNewConstMetric(ksmProfitDesc, prometheus.GaugeValue, float64(c.node.ksmProfit))
	}
	if c.node.thpVMStatAvailable {
		ch <- prometheus.MustNewConstMetric(thpSplitPMDDesc, prometheus.CounterValue, float64(c.node.thpSplitPMD), c.cfg.NodeName)
		ch <- prometheus.MustNewConstMetric(thpCollapseAllocDesc, prometheus.CounterValue, float64(c.node.thpCollapseAlloc), c.cfg.NodeName)
	}
	if c.node.buddyAvailable {
		excludedByNUMA := excludedByNUMAMap(c.node.excludedByNuma)
		unmovableByNUMA := excludedByNUMAMap(c.node.unmovableByNuma)
		for _, b := range c.node.buddyByNuma {
			ch <- prometheus.MustNewConstMetric(buddyOrderGe9Desc, prometheus.GaugeValue, float64(b.OrderGe9Bytes), c.cfg.NodeName, b.NUMA)
			ch <- prometheus.MustNewConstMetric(buddyAllOrdersDesc, prometheus.GaugeValue, float64(b.AllOrdersBytes), c.cfg.NodeName, b.NUMA)
			if c.node.pagetypeAvailable {
				e := excludedByNUMA[b.NUMA]
				u := unmovableByNUMA[b.NUMA]
				ch <- prometheus.MustNewConstMetric(unmovableOrderGe9Desc, prometheus.GaugeValue, float64(u.UnmovableOrderGe9Bytes), c.cfg.NodeName, b.NUMA)
				ch <- prometheus.MustNewConstMetric(unmovableAllOrdersDesc, prometheus.GaugeValue, float64(u.UnmovableAllOrdersBytes), c.cfg.NodeName, b.NUMA)
				movableGe9 := subtractExcludedBytes(b.OrderGe9Bytes, e.excludedOrderGe9Bytes())
				movableAll := subtractExcludedBytes(b.AllOrdersBytes, e.excludedAllOrdersBytes())
				ch <- prometheus.MustNewConstMetric(movableOrderGe9Desc, prometheus.GaugeValue, float64(movableGe9), c.cfg.NodeName, b.NUMA)
				ch <- prometheus.MustNewConstMetric(movableAllOrdersDesc, prometheus.GaugeValue, float64(movableAll), c.cfg.NodeName, b.NUMA)
			}
		}
	} else if c.node.pagetypeAvailable {
		for _, e := range c.node.unmovableByNuma {
			ch <- prometheus.MustNewConstMetric(unmovableOrderGe9Desc, prometheus.GaugeValue, float64(e.UnmovableOrderGe9Bytes), c.cfg.NodeName, e.NUMA)
			ch <- prometheus.MustNewConstMetric(unmovableAllOrdersDesc, prometheus.GaugeValue, float64(e.UnmovableAllOrdersBytes), c.cfg.NodeName, e.NUMA)
		}
	}
	if c.node.zoneinfoAvailable {
		for _, z := range c.node.zoneByNuma {
			ch <- prometheus.MustNewConstMetric(zonePresentBytesDesc, prometheus.GaugeValue, float64(z.PresentBytes), c.cfg.NodeName, z.NUMA, z.Zone)
			ch <- prometheus.MustNewConstMetric(zoneFreeBytesDesc, prometheus.GaugeValue, float64(z.FreeBytes), c.cfg.NodeName, z.NUMA, z.Zone)
		}
	}
}

func (c *Collector) poll(ctx context.Context) {
	c.log.Debug("cgroup: starting poll cycle")

	var (
		results      []vmiMemStats
		scrapeErrors int
	)

	for _, obj := range c.podStore.List() {
		pod, ok := obj.(*corev1.Pod)
		if !ok {
			continue
		}
		if pod.Status.Phase != corev1.PodRunning {
			continue
		}
		if pod.Labels["kubevirt.io"] != "virt-launcher" {
			continue
		}
		vmiName := pod.Labels["vm.kubevirt.io/name"]
		if vmiName == "" {
			continue
		}

		info, err := c.criClient.FindComputePID(ctx, pod.Name, pod.Namespace)
		if err != nil {
			c.log.Warn("cgroup: finding compute container PID", "namespace", pod.Namespace, "pod", pod.Name, "error", err)
			scrapeErrors++
			continue
		}

		cgroupPath, err := readCgroupV2Path(info.PID, c.cfg.ProcPath)
		if err != nil {
			c.log.Warn("cgroup: reading cgroup path", "pid", info.PID, "error", err)
			scrapeErrors++
			continue
		}
		cgroupPath = resolveCgroupPath(cgroupPath, c.cgroupNSRoot)

		stat, err := readMemoryStat(c.cfg.CgroupRoot, cgroupPath)
		if err != nil {
			c.log.Warn("cgroup: reading memory.stat", "pid", info.PID, "path", cgroupPath, "error", err)
			scrapeErrors++
			continue
		}

		results = append(results, vmiMemStats{
			namespace:    pod.Namespace,
			name:         vmiName,
			pod:          pod.Name,
			activeAnon:   stat.activeAnon,
			inactiveAnon: stat.inactiveAnon,
			anonTHP:      stat.anonTHP,
			shmemTHP:     stat.shmemTHP,
			fileTHP:      stat.fileTHP,
		})
	}

	ns := c.collectNodeStats()

	c.mu.Lock()
	c.vmiResults = results
	c.node = ns
	c.scrapeErrors += float64(scrapeErrors)
	c.lastPollTS = float64(time.Now().Unix())
	c.mu.Unlock()

	c.log.Debug("cgroup: poll cycle complete", "vms", len(results), "errors", scrapeErrors)
}

func (c *Collector) collectNodeStats() nodeStats {
	var ns nodeStats

	if c.khugepagePID > 0 {
		cpu, err := readProcessCPUSeconds(c.khugepagePID, c.cfg.ProcPath)
		if err != nil {
			c.log.Debug("cgroup: reading khugepaged CPU, will re-discover", "error", err)
			c.rediscoverKhugepaged()
			if c.khugepagePID > 0 {
				cpu, err = readProcessCPUSeconds(c.khugepagePID, c.cfg.ProcPath)
			}
		}
		if err == nil {
			ns.khugepageCPU = cpu
			ns.khugepageAvailable = true
		}
	}

	if c.ksmdPID > 0 {
		cpu, err := readProcessCPUSeconds(c.ksmdPID, c.cfg.ProcPath)
		if err != nil {
			c.log.Debug("cgroup: reading ksmd CPU, will re-discover", "error", err)
			c.rediscoverKsmd()
			if c.ksmdPID > 0 {
				cpu, err = readProcessCPUSeconds(c.ksmdPID, c.cfg.ProcPath)
			}
		}
		if err == nil {
			ns.ksmdCPU = cpu
			ns.ksmdAvailable = true
		}
	}

	profit, ok := readKSMGeneralProfit(c.cfg.SysPath)
	ns.ksmProfit = profit
	ns.ksmProfitAvailable = ok

	if vmstat, err := readVMStatTHP(c.cfg.ProcPath); err != nil {
		c.log.Debug("cgroup: reading vmstat THP counters", "error", err)
	} else {
		ns.thpSplitPMD = vmstat.splitPMD
		ns.thpCollapseAlloc = vmstat.collapseAlloc
		ns.thpVMStatAvailable = true
	}

	buddySnap, buddyErr := readBuddySnapshot(c.cfg.ProcPath)
	if buddyErr != nil {
		c.log.Debug("cgroup: reading buddyinfo", "error", buddyErr)
	} else {
		ns.buddyByNuma = buddySnap.thpByNUMA
		ns.buddyAvailable = true
	}

	zonesByNUMA := buddySnap.zonesByNUMA
	if buddyErr != nil {
		zonesByNUMA, _ = buddyZonesByNUMA(c.cfg.ProcPath)
	}
	if zonesByNUMA != nil {
		if excluded, unmovable, err := readPagetypeBuddyStats(c.cfg.ProcPath, zonesByNUMA); err != nil {
			c.log.Debug("cgroup: reading pagetypeinfo", "error", err)
		} else {
			ns.excludedByNuma = excluded
			ns.unmovableByNuma = unmovable
			ns.pagetypeAvailable = true
		}
	}

	if zones, err := readZoneinfo(c.cfg.ProcPath); err != nil {
		c.log.Debug("cgroup: reading zoneinfo", "error", err)
	} else {
		ns.zoneByNuma = zones
		ns.zoneinfoAvailable = true
	}

	return ns
}

func (c *Collector) discoverKernelThreads() {
	c.rediscoverKhugepaged()
	c.rediscoverKsmd()
}

func (c *Collector) rediscoverKhugepaged() {
	pid, err := findKernelThreadPID("khugepaged", c.cfg.ProcPath)
	if err != nil {
		c.log.Info("cgroup: khugepaged not found, THP CPU metric unavailable", "error", err)
		c.khugepagePID = 0
		return
	}
	c.khugepagePID = pid
	c.log.Info("cgroup: discovered khugepaged", "pid", pid)
}

func (c *Collector) rediscoverKsmd() {
	pid, err := findKernelThreadPID("ksmd", c.cfg.ProcPath)
	if err != nil {
		c.log.Info("cgroup: ksmd not found, KSM CPU metric unavailable", "error", err)
		c.ksmdPID = 0
		return
	}
	c.ksmdPID = pid
	c.log.Info("cgroup: discovered ksmd", "pid", pid)
}
