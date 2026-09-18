// Copyright 2026 The KubeVirt Metrics Exporter Authors
// SPDX-License-Identifier: Apache-2.0

package qga

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/tools/cache"

	"github.com/openshift-virtualization/kubevirt-metrics-exporter/pkg/cri"
	"github.com/openshift-virtualization/kubevirt-metrics-exporter/pkg/qmp"
)

type CollectorConfig struct {
	NodeName     string
	PollInterval time.Duration
	QGATimeout   int32
	ExecWait     time.Duration
	MaxRetries   int
	Concurrency  int
	Namespaces   []string
	LabelFilter  string
}

type vmState struct {
	mu           sync.Mutex
	client       *qmp.Client
	namespace    string
	vmi          string
	podName      string
	prevSnapshot map[string]DiskCounters
	pvcMap       map[string]string // volume name -> PVC claim name
	diskMap      map[int]string    // PhysicalDrive index -> volume name
	retryCount   int
	stopped      bool
	stopReason   string
	closed       bool
}

func (vs *vmState) close() {
	vs.mu.Lock()
	defer vs.mu.Unlock()
	if !vs.closed {
		vs.closed = true
		vs.client.Close()
	}
}

type enrichedDisk struct {
	DiskMetrics
	Disk string // KubeVirt volume name (empty if unmapped)
	PVC  string // PVC claim name (empty if not a PVC)
}

type vmiResult struct {
	Namespace string
	Name      string
	Node      string
	PodName   string
	Disks     []enrichedDisk
}

type computePIDFinder interface {
	FindComputePID(context.Context, string, string) (*cri.ContainerInfo, error)
}

type Collector struct {
	cfg       CollectorConfig
	podStore  cache.Store
	criClient computePIDFinder
	dynClient dynamic.Interface
	log       *slog.Logger

	mu           sync.RWMutex
	results      []vmiResult
	scrapeErrors float64
	lastPollTS   float64

	connMu sync.RWMutex
	vms    map[string]*vmState
}

func NewCollector(cfg CollectorConfig, podStore cache.Store, criClient *cri.Client, dynClient dynamic.Interface, log *slog.Logger) *Collector {
	return &Collector{
		cfg:       cfg,
		podStore:  podStore,
		criClient: criClient,
		dynClient: dynClient,
		log:       log,
		vms:       make(map[string]*vmState),
	}
}

func (c *Collector) Run(ctx context.Context) {
	c.poll(ctx)
	ticker := time.NewTicker(c.cfg.PollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			c.closeAll()
			return
		case <-ticker.C:
			c.poll(ctx)
		}
	}
}

func (c *Collector) closeAll() {
	c.connMu.Lock()
	defer c.connMu.Unlock()
	for id, vs := range c.vms {
		vs.close()
		delete(c.vms, id)
	}
}

func (c *Collector) Describe(ch chan<- *prometheus.Desc) {
	ch <- latencyAvgDesc
	ch <- iopsDesc
	ch <- qgaScrapeErrorsDesc
	ch <- qgaLastPollDesc
}

func (c *Collector) Collect(ch chan<- prometheus.Metric) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	ch <- prometheus.MustNewConstMetric(qgaScrapeErrorsDesc, prometheus.CounterValue, c.scrapeErrors)
	ch <- prometheus.MustNewConstMetric(qgaLastPollDesc, prometheus.GaugeValue, c.lastPollTS)

	for _, vmi := range c.results {
		for _, disk := range vmi.Disks {
			if disk.RdIOPS > 0 || disk.RdLatSec > 0 {
				ch <- prometheus.MustNewConstMetric(
					latencyAvgDesc, prometheus.GaugeValue, disk.RdLatSec,
					vmi.Namespace, vmi.Name, vmi.Node, vmi.PodName, disk.Disk, disk.PVC, "read", disk.Name,
				)
				ch <- prometheus.MustNewConstMetric(
					iopsDesc, prometheus.GaugeValue, disk.RdIOPS,
					vmi.Namespace, vmi.Name, vmi.Node, vmi.PodName, disk.Disk, disk.PVC, "read", disk.Name,
				)
			}
			if disk.WrIOPS > 0 || disk.WrLatSec > 0 {
				ch <- prometheus.MustNewConstMetric(
					latencyAvgDesc, prometheus.GaugeValue, disk.WrLatSec,
					vmi.Namespace, vmi.Name, vmi.Node, vmi.PodName, disk.Disk, disk.PVC, "write", disk.Name,
				)
				ch <- prometheus.MustNewConstMetric(
					iopsDesc, prometheus.GaugeValue, disk.WrIOPS,
					vmi.Namespace, vmi.Name, vmi.Node, vmi.PodName, disk.Disk, disk.PVC, "write", disk.Name,
				)
			}
		}
	}
}

func matchesLabelFilter(labels map[string]string, filter string) bool {
	for _, part := range strings.Split(filter, ",") {
		kv := strings.SplitN(strings.TrimSpace(part), "=", 2)
		if len(kv) != 2 {
			continue
		}
		if labels[kv[0]] != kv[1] {
			return false
		}
	}
	return true
}

type podInfo struct {
	namespace string
	podName   string
	vmiName   string
}

// scrapeBudget is the wall-clock deadline for one VM scrape: guest-exec plus
// guest-exec-status polls and the sleeps between them, with a small margin.
func (c *Collector) scrapeBudget() time.Duration {
	agent := time.Duration(c.cfg.QGATimeout) * time.Second
	return agent*(1+guestExecStatusAttempts) + c.cfg.ExecWait*time.Duration(guestExecStatusAttempts) + 2*time.Second
}

func isDeadClient(err error) bool {
	return errors.Is(err, qmp.ErrClientClosed) ||
		errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, context.Canceled)
}

func (c *Collector) dropVM(containerID string, vs *vmState, reason string) {
	c.log.Info("qga: dropping VM connection",
		"vmi", vs.vmi, "namespace", vs.namespace, "reason", reason)
	vs.close()
	c.connMu.Lock()
	if cur := c.vms[containerID]; cur == vs {
		delete(c.vms, containerID)
	}
	c.connMu.Unlock()
}

func (c *Collector) poll(ctx context.Context) {
	c.log.Info("qga: starting poll cycle")

	var allPods []podInfo
	nsFilter := make(map[string]bool, len(c.cfg.Namespaces))
	for _, ns := range c.cfg.Namespaces {
		nsFilter[ns] = true
	}

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
		if c.cfg.LabelFilter != "" && !matchesLabelFilter(pod.Labels, c.cfg.LabelFilter) {
			continue
		}
		if len(nsFilter) > 0 && !nsFilter[pod.Namespace] {
			continue
		}
		vmiName := pod.Labels["vm.kubevirt.io/name"]
		if vmiName == "" {
			continue
		}
		allPods = append(allPods, podInfo{
			namespace: pod.Namespace,
			podName:   pod.Name,
			vmiName:   vmiName,
		})
	}

	c.log.Info("qga: found virt-launcher pods", "count", len(allPods))

	type target struct {
		podInfo
		containerID string
		pid         int
	}

	var targets []target
	for _, pod := range allPods {
		info, err := c.criClient.FindComputePID(ctx, pod.podName, pod.namespace)
		if err != nil {
			c.log.Warn("qga: finding compute container", "namespace", pod.namespace, "pod", pod.podName, "error", err)
			continue
		}
		targets = append(targets, target{
			podInfo:     pod,
			containerID: info.ContainerID,
			pid:         info.PID,
		})
	}

	activeIDs := make(map[string]bool, len(targets))
	for _, t := range targets {
		activeIDs[t.containerID] = true
	}

	c.connMu.Lock()
	for id, vs := range c.vms {
		if !activeIDs[id] {
			c.log.Info("qga: removing departed VM", "vmi", vs.vmi, "namespace", vs.namespace)
			vs.close()
			delete(c.vms, id)
		}
	}
	existing := make(map[string]bool, len(c.vms))
	for id := range c.vms {
		existing[id] = true
	}
	c.connMu.Unlock()

	for _, t := range targets {
		if existing[t.containerID] {
			continue
		}

		vs, err := c.connectVM(ctx, t.namespace, t.vmiName, t.podName, t.pid)
		if err != nil {
			c.log.Error("qga: connecting to VM", "namespace", t.namespace, "vmi", t.vmiName, "error", err)
			continue
		}

		c.connMu.Lock()
		if _, dup := c.vms[t.containerID]; !dup {
			c.vms[t.containerID] = vs
		} else {
			vs.close()
		}
		c.connMu.Unlock()
	}

	var (
		resultsMu    sync.Mutex
		results      []vmiResult
		scrapeErrors int
	)

	sem := make(chan struct{}, c.cfg.Concurrency)
	var wg sync.WaitGroup

	c.connMu.RLock()
	snapshot := make(map[string]*vmState, len(c.vms))
	for k, v := range c.vms {
		snapshot[k] = v
	}
	c.connMu.RUnlock()

	for containerID, vs := range snapshot {
		vs.mu.Lock()
		stopped := vs.stopped
		dead := vs.closed || vs.client.Closed()
		stopReason := vs.stopReason
		vs.mu.Unlock()

		if stopped {
			c.log.Debug("qga: skipping VM", "vmi", vs.vmi, "reason", stopReason)
			continue
		}
		if dead {
			c.dropVM(containerID, vs, "client already closed")
			scrapeErrors++
			continue
		}

		wg.Add(1)
		go func(containerID string, vs *vmState) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			result, err := c.scrapeVM(ctx, vs)

			resultsMu.Lock()
			defer resultsMu.Unlock()

			if err != nil {
				scrapeErrors++
				c.handleScrapeError(containerID, vs, err)
				return
			}
			if result != nil {
				results = append(results, *result)
			}
		}(containerID, vs)
	}

	wg.Wait()

	c.mu.Lock()
	c.results = results
	c.scrapeErrors += float64(scrapeErrors)
	c.lastPollTS = float64(time.Now().Unix())
	c.mu.Unlock()

	c.log.Info("qga: poll cycle complete", "vms_with_data", len(results), "errors", scrapeErrors)
}

func (c *Collector) handleScrapeError(containerID string, vs *vmState, err error) {
	if errors.Is(err, ErrCommandBlacklisted) {
		vs.mu.Lock()
		vs.stopped = true
		vs.stopReason = "command blacklisted"
		vs.mu.Unlock()
		c.log.Warn("qga: stopping collection, command blacklisted",
			"namespace", vs.namespace, "vmi", vs.vmi)
		return
	}

	// Closed/cancelled transport cannot be reused; drop so the next poll re-dials.
	if isDeadClient(err) || vs.client.Closed() {
		c.dropVM(containerID, vs, err.Error())
		return
	}

	vs.mu.Lock()
	vs.retryCount++
	retries, max := vs.retryCount, c.cfg.MaxRetries
	ns, vmi := vs.namespace, vs.vmi
	vs.mu.Unlock()

	c.log.Info("qga: scrape error",
		"namespace", ns, "vmi", vmi,
		"error", err, "retries", retries, "max", max)

	// Soft errors: drop after MaxRetries so a later poll re-dials (agent may
	// come up after guest boot). Avoid permanent mute on the same container ID.
	if retries >= max {
		c.dropVM(containerID, vs, fmt.Sprintf("max retries (%d): %v", max, err))
	}
}

func (c *Collector) scrapeVM(ctx context.Context, vs *vmState) (*vmiResult, error) {
	vs.mu.Lock()
	if vs.closed || vs.client.Closed() {
		vs.mu.Unlock()
		return nil, qmp.ErrClientClosed
	}
	client := vs.client
	ns, vmi, podName := vs.namespace, vs.vmi, vs.podName
	needDiskMap := vs.diskMap == nil
	vs.mu.Unlock()

	scrapeCtx, cancel := context.WithTimeout(ctx, c.scrapeBudget())
	defer cancel()

	if needDiskMap {
		if diskMap := c.tryBuildDiskMap(scrapeCtx, client, ns, vmi); diskMap != nil {
			vs.mu.Lock()
			if !vs.closed && vs.diskMap == nil {
				vs.diskMap = diskMap
			}
			vs.mu.Unlock()
		}
	}

	counters, err := CollectDiskCounters(scrapeCtx, client, c.cfg.QGATimeout, c.cfg.ExecWait, c.log, vmi)
	if err != nil {
		return nil, err
	}

	currSnapshot := make(map[string]DiskCounters, len(counters))
	for _, dc := range counters {
		currSnapshot[dc.Name] = dc
		c.log.Debug("qga: raw counters",
			"vmi", vmi, "drive", dc.Name,
			"rd_qlen", dc.RdQueueLen, "wr_qlen", dc.WrQueueLen,
			"rd_ops", dc.RdOps, "wr_ops", dc.WrOps,
			"ts", dc.Timestamp100ns)
	}

	vs.mu.Lock()
	defer vs.mu.Unlock()
	if vs.closed {
		return nil, qmp.ErrClientClosed
	}

	var disks []enrichedDisk
	if vs.prevSnapshot == nil {
		c.log.Debug("qga: first snapshot, no previous data to diff", "vmi", vmi, "disks", len(currSnapshot))
	}
	if vs.prevSnapshot != nil {
		pvcRefreshed := false
		for name, curr := range currSnapshot {
			prev, ok := vs.prevSnapshot[name]
			if !ok {
				continue
			}
			m := ComputeMetrics(prev, curr)
			if m.ElapsedSec > 0 {
				ed := enrichedDisk{DiskMetrics: m}
				if idx, ok := ParseDiskIndex(m.Name); ok {
					if volName, ok := vs.diskMap[idx]; ok {
						ed.Disk = volName
						ed.PVC = vs.pvcMap[volName]
						if ed.PVC == "" && !pvcRefreshed {
							pvcRefreshed = true
							vs.pvcMap = qmp.FetchPVCMap(scrapeCtx, c.dynClient, ns, vmi, c.log)
							ed.PVC = vs.pvcMap[volName]
						}
					}
				}
				disks = append(disks, ed)
				c.log.Debug("qga: computed metrics",
					"vmi", vmi, "drive", m.Name,
					"disk", ed.Disk, "pvc", ed.PVC,
					"rd_lat_ms", m.RdLatSec*1000, "rd_iops", m.RdIOPS,
					"wr_lat_ms", m.WrLatSec*1000, "wr_iops", m.WrIOPS,
					"elapsed_s", m.ElapsedSec)
			}
		}
	}

	vs.prevSnapshot = currSnapshot
	vs.retryCount = 0

	if len(disks) == 0 {
		return nil, nil
	}

	return &vmiResult{
		Namespace: ns,
		Name:      vmi,
		Node:      c.cfg.NodeName,
		PodName:   podName,
		Disks:     disks,
	}, nil
}

func (c *Collector) tryBuildDiskMap(ctx context.Context, client *qmp.Client, ns, vmi string) map[int]string {
	domainXML, err := client.DomainGetXMLDesc()
	if err != nil {
		c.log.Warn("qga: DomainGetXMLDesc failed, disk mapping unavailable", "vmi", vmi, "error", err)
		return nil
	}
	guestDisks, err := GuestGetDisks(ctx, client, c.cfg.QGATimeout)
	if err != nil {
		c.log.Warn("qga: guest-get-disks failed, disk mapping unavailable", "vmi", vmi, "error", err)
		return nil
	}
	for _, gd := range guestDisks {
		c.log.Debug("qga: guest-get-disks entry", "vmi", vmi,
			"name", gd.Name, "drive_index", gd.DriveIndex, "serial", gd.Serial,
			"ctrl_domain", gd.Location.Controller.Domain,
			"ctrl_bus", gd.Location.Controller.Bus,
			"ctrl_slot", gd.Location.Controller.Slot,
			"ctrl_fn", gd.Location.Controller.Function,
			"bus", gd.Location.Bus, "target", gd.Location.Target, "unit", gd.Location.Unit)
	}
	diskMap, err := BuildDiskMapping(domainXML, guestDisks)
	if err != nil {
		c.log.Warn("qga: building disk mapping failed", "vmi", vmi, "error", err)
		return nil
	}
	c.log.Info("qga: disk mapping established", "vmi", vmi, "mappings", diskMap)
	return diskMap
}

func (c *Collector) connectVM(ctx context.Context, ns, vmi, podName string, pid int) (*vmState, error) {
	sockPath := fmt.Sprintf("/proc/%d/root/run/libvirt/virtqemud-sock", pid)
	if _, err := os.Stat(sockPath); err != nil {
		return nil, fmt.Errorf("virtqemud socket not found at %s: %w", sockPath, err)
	}

	domainName := ns + "_" + vmi
	connectCtx, cancel := context.WithTimeout(ctx, time.Duration(c.cfg.QGATimeout)*time.Second)
	defer cancel()
	client, err := qmp.DialContext(connectCtx, sockPath, domainName)
	if err != nil {
		return nil, fmt.Errorf("dialing for QGA %s: %w", domainName, err)
	}

	c.log.Info("qga: connected to VM", "namespace", ns, "vmi", vmi, "pid", pid)

	agentCtx, agentCancel := context.WithTimeout(ctx, time.Duration(c.cfg.QGATimeout)*time.Second)
	defer agentCancel()

	pvcMap := qmp.FetchPVCMap(agentCtx, c.dynClient, ns, vmi, c.log)
	diskMap := c.tryBuildDiskMap(agentCtx, client, ns, vmi)

	return &vmState{
		client:    client,
		namespace: ns,
		vmi:       vmi,
		podName:   podName,
		pvcMap:    pvcMap,
		diskMap:   diskMap,
	}, nil
}
