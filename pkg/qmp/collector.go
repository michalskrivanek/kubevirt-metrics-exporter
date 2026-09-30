// Copyright 2026 The KubeVirt Metrics Exporter Authors
// SPDX-License-Identifier: Apache-2.0

package qmp

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/openshift-virtualization/kubevirt-metrics-exporter/pkg/cri"
	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/tools/cache"
)

var vmiResource = schema.GroupVersionResource{
	Group:    "kubevirt.io",
	Version:  "v1",
	Resource: "virtualmachineinstances",
}

var (
	latencyDesc = prometheus.NewDesc(
		"kubevirt_vmi_storage_io_latency_seconds",
		"Block I/O latency histogram for KubeVirt VMI disks via QMP",
		[]string{"namespace", "name", "node", "pod", "disk", "persistentvolumeclaim", "operation"},
		nil,
	)

	scrapeErrorsDesc = prometheus.NewDesc(
		"kme_qmp_scrape_errors_total",
		"Total number of errors encountered during QMP scrape cycles",
		nil, nil,
	)

	lastPollDesc = prometheus.NewDesc(
		"kme_qmp_last_poll_timestamp_seconds",
		"Unix timestamp of the last successful QMP poll cycle",
		nil, nil,
	)

	virtqueueInuseDesc = prometheus.NewDesc(
		"kubevirt_vmi_storage_queue_inuse",
		"Number of in-flight descriptors in a storage virtqueue",
		[]string{"namespace", "name", "node", "pod", "disk", "persistentvolumeclaim", "queue", "bus"},
		nil,
	)

	virtqueueSizeDesc = prometheus.NewDesc(
		"kubevirt_vmi_storage_queue_size",
		"Maximum number of descriptors (capacity) of a storage virtqueue",
		[]string{"namespace", "name", "node", "pod", "disk", "persistentvolumeclaim", "queue", "bus"},
		nil,
	)
)

type VMIResult struct {
	Namespace  string
	Name       string
	Node       string
	PodName    string
	Devices    []DeviceResult
	Virtqueues []VirtqueueResult
}

type DeviceResult struct {
	Disk  string
	PVC   string
	Stats BlockStats
}

type VirtqueueResult struct {
	Disk     string
	PVC      string
	Bus      string
	Queue    int
	Inuse    uint32
	VringNum uint32
}

type PollerConfig struct {
	NodeName     string
	PollInterval time.Duration
	BoundariesNs []int64
	QMPTimeout   time.Duration
	Concurrency  int
	Namespaces   []string
	LabelFilter  string
}

type vmConnection struct {
	mu        sync.Mutex
	client    *Client
	namespace string
	vmi       string
	podName   string
	armed     map[string]bool
	numQueues map[string]int    // device path → number of virtqueues (cached)
	pvcMap    map[string]string // disk alias → PVC name
	closed    bool
}

func (vc *vmConnection) close() {
	vc.mu.Lock()
	defer vc.mu.Unlock()
	if !vc.closed {
		vc.closed = true
		vc.client.Close()
	}
}

type computePIDFinder interface {
	FindComputePID(context.Context, string, string) (*cri.ContainerInfo, error)
}

type Collector struct {
	cfg       PollerConfig
	podStore  cache.Store
	criClient computePIDFinder
	dynClient dynamic.Interface
	log       *slog.Logger

	mu           sync.RWMutex
	results      []VMIResult
	scrapeErrors float64
	lastPollTS   float64

	connMu      sync.RWMutex
	connections map[string]*vmConnection
}

func NewCollector(cfg PollerConfig, podStore cache.Store, criClient *cri.Client, dynClient dynamic.Interface, log *slog.Logger) *Collector {
	return &Collector{
		cfg:         cfg,
		podStore:    podStore,
		criClient:   criClient,
		dynClient:   dynClient,
		log:         log,
		connections: make(map[string]*vmConnection),
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
	for id, conn := range c.connections {
		conn.close()
		delete(c.connections, id)
	}
}

// Update replaces the cached poll results. Exported for testing.
func (c *Collector) Update(results []VMIResult, scrapeErrors int, lastPollTS float64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.results = results
	c.scrapeErrors += float64(scrapeErrors)
	c.lastPollTS = lastPollTS
}

func (c *Collector) Describe(ch chan<- *prometheus.Desc) {
	ch <- latencyDesc
	ch <- scrapeErrorsDesc
	ch <- lastPollDesc
	ch <- virtqueueInuseDesc
	ch <- virtqueueSizeDesc
}

func (c *Collector) Collect(ch chan<- prometheus.Metric) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	ch <- prometheus.MustNewConstMetric(scrapeErrorsDesc, prometheus.CounterValue, c.scrapeErrors)
	ch <- prometheus.MustNewConstMetric(lastPollDesc, prometheus.GaugeValue, c.lastPollTS)

	for _, vmi := range c.results {
		for _, dev := range vmi.Devices {
			type opData struct {
				hist    *LatencyHist
				totalNs uint64
			}
			operations := map[string]opData{
				"read":  {dev.Stats.RdLatencyHistogram, dev.Stats.RdTotalTimeNs},
				"write": {dev.Stats.WrLatencyHistogram, dev.Stats.WrTotalTimeNs},
				"flush": {dev.Stats.FlushLatencyHistogram, dev.Stats.FlushTotalTimeNs},
			}
			for op, data := range operations {
				if data.hist == nil {
					continue
				}
				buckets, count := ConvertBuckets(data.hist)
				if count == 0 {
					continue
				}
				sum := float64(data.totalNs) / 1e9
				h, err := prometheus.NewConstHistogram(
					latencyDesc,
					count, sum, buckets,
					vmi.Namespace, vmi.Name, vmi.Node, vmi.PodName, dev.Disk, dev.PVC, op,
				)
				if err != nil {
					continue
				}
				ch <- h
			}
		}

		for _, vq := range vmi.Virtqueues {
			queueLabel := strconv.Itoa(vq.Queue)
			ch <- prometheus.MustNewConstMetric(virtqueueInuseDesc, prometheus.GaugeValue, float64(vq.Inuse),
				vmi.Namespace, vmi.Name, vmi.Node, vmi.PodName, vq.Disk, vq.PVC, queueLabel, vq.Bus)
			ch <- prometheus.MustNewConstMetric(virtqueueSizeDesc, prometheus.GaugeValue, float64(vq.VringNum),
				vmi.Namespace, vmi.Name, vmi.Node, vmi.PodName, vq.Disk, vq.PVC, queueLabel, vq.Bus)
		}
	}
}

func ConvertBuckets(hist *LatencyHist) (map[float64]uint64, uint64) {
	buckets := make(map[float64]uint64, len(hist.Boundaries))
	var cumulative uint64

	for i, count := range hist.Bins {
		cumulative += count
		if i < len(hist.Boundaries) {
			buckets[hist.Boundaries[i]/1e9] = cumulative
		}
	}

	return buckets, cumulative
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

func (c *Collector) poll(ctx context.Context) {
	c.log.Info("qmp: starting poll cycle")

	type podInfo struct {
		namespace string
		podName   string
		vmiName   string
	}

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

	c.log.Info("qmp: found virt-launcher pods", "count", len(allPods))

	type target struct {
		podInfo
		containerID string
	}

	var targets []target
	for _, pod := range allPods {
		info, err := c.criClient.FindComputePID(ctx, pod.podName, pod.namespace)
		if err != nil {
			c.log.Warn("qmp: finding compute container", "namespace", pod.namespace, "pod", pod.podName, "error", err)
			continue
		}
		targets = append(targets, target{
			podInfo:     pod,
			containerID: info.ContainerID,
		})
	}

	activeIDs := make(map[string]bool, len(targets))
	for _, t := range targets {
		activeIDs[t.containerID] = true
	}

	c.connMu.Lock()
	for id, conn := range c.connections {
		if !activeIDs[id] {
			c.log.Info("qmp: removing departed VM", "vmi", conn.vmi, "namespace", conn.namespace)
			conn.close()
			delete(c.connections, id)
		}
	}
	existing := make(map[string]bool, len(c.connections))
	for id := range c.connections {
		existing[id] = true
	}
	c.connMu.Unlock()

	for _, t := range targets {
		if existing[t.containerID] {
			continue
		}

		info, err := c.criClient.FindComputePID(ctx, t.podName, t.namespace)
		if err != nil {
			c.log.Warn("qmp: getting PID for new VM", "namespace", t.namespace, "vmi", t.vmiName, "error", err)
			continue
		}

		conn, err := c.connectVM(ctx, t.namespace, t.vmiName, t.podName, info.PID)
		if err != nil {
			c.log.Error("qmp: connecting to VM", "namespace", t.namespace, "vmi", t.vmiName, "error", err)
			continue
		}

		c.connMu.Lock()
		if _, dup := c.connections[t.containerID]; !dup {
			c.connections[t.containerID] = conn
		} else {
			conn.close()
		}
		c.connMu.Unlock()
	}

	var (
		resultsMu    sync.Mutex
		results      []VMIResult
		scrapeErrors int
	)

	sem := make(chan struct{}, c.cfg.Concurrency)
	var wg sync.WaitGroup

	c.connMu.RLock()
	snapshot := make(map[string]*vmConnection, len(c.connections))
	for k, v := range c.connections {
		snapshot[k] = v
	}
	c.connMu.RUnlock()

	for containerID, conn := range snapshot {
		wg.Add(1)
		go func(containerID string, conn *vmConnection) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			result, err := c.scrapeVM(ctx, conn)
			resultsMu.Lock()
			defer resultsMu.Unlock()
			if err != nil {
				c.log.Error("qmp: scraping VM", "namespace", conn.namespace, "vmi", conn.vmi, "error", err)
				scrapeErrors++
				conn.close()
				c.connMu.Lock()
				delete(c.connections, containerID)
				c.connMu.Unlock()
				return
			}
			results = append(results, *result)
		}(containerID, conn)
	}

	wg.Wait()

	c.mu.Lock()
	c.results = results
	c.scrapeErrors += float64(scrapeErrors)
	c.lastPollTS = float64(time.Now().Unix())
	c.mu.Unlock()

	c.log.Info("qmp: poll cycle complete", "vms", len(results), "errors", scrapeErrors)
}

func (c *Collector) connectVM(ctx context.Context, ns, vmi, podName string, pid int) (*vmConnection, error) {
	sockPath := fmt.Sprintf("/proc/%d/root/run/libvirt/virtqemud-sock", pid)
	if _, err := os.Stat(sockPath); err != nil {
		return nil, fmt.Errorf("virtqemud socket not found at %s: %w", sockPath, err)
	}

	domainName := ns + "_" + vmi
	connectCtx, cancel := context.WithTimeout(ctx, c.cfg.QMPTimeout)
	defer cancel()
	client, err := DialContext(connectCtx, sockPath, domainName)
	if err != nil {
		return nil, fmt.Errorf("dialing QMP for %s: %w", domainName, err)
	}

	c.log.Info("qmp: connected to VM", "namespace", ns, "vmi", vmi, "pid", pid)
	return &vmConnection{
		client:    client,
		namespace: ns,
		vmi:       vmi,
		podName:   podName,
		armed:     make(map[string]bool),
		numQueues: make(map[string]int),
		pvcMap:    FetchPVCMap(ctx, c.dynClient, ns, vmi, c.log),
	}, nil
}

// FetchPVCMap retrieves PVC claim names from VMI status.volumeStatus via dynamic client.
// This covers all volumes including hotplugged disks.
func FetchPVCMap(ctx context.Context, dynClient dynamic.Interface, ns, vmiName string, log *slog.Logger) map[string]string {
	pvcMap := make(map[string]string)
	if dynClient == nil {
		return pvcMap
	}

	ctx, cancel := context.WithTimeout(ctx, defaultConnectTimeout)
	defer cancel()
	obj, err := dynClient.Resource(vmiResource).Namespace(ns).Get(ctx, vmiName, metav1.GetOptions{})
	if err != nil {
		log.Warn("failed to fetch VMI for PVC mapping", "vmi", vmiName, "error", err)
		return pvcMap
	}

	status, ok := obj.Object["status"].(map[string]interface{})
	if !ok {
		return pvcMap
	}
	volumeStatuses, ok := status["volumeStatus"].([]interface{})
	if !ok {
		return pvcMap
	}

	for _, vs := range volumeStatuses {
		entry, ok := vs.(map[string]interface{})
		if !ok {
			continue
		}
		name, _ := entry["name"].(string)
		if name == "" {
			continue
		}
		pvcInfo, ok := entry["persistentVolumeClaimInfo"].(map[string]interface{})
		if !ok {
			continue
		}
		claimName, _ := pvcInfo["claimName"].(string)
		if claimName != "" {
			pvcMap[name] = claimName
		}
	}

	log.Debug("PVC map from VMI status", "vmi", vmiName, "pvcMap", pvcMap)
	return pvcMap
}

func (c *Collector) scrapeVM(ctx context.Context, conn *vmConnection) (*VMIResult, error) {
	conn.mu.Lock()
	defer conn.mu.Unlock()
	if conn.closed {
		return nil, fmt.Errorf("connection closed")
	}

	qmpCtx, cancel := context.WithTimeout(ctx, c.cfg.QMPTimeout)
	defer cancel()

	resp, err := conn.client.QueryBlockStats(qmpCtx)
	if err != nil {
		return nil, err
	}

	for i := range resp.Return {
		dev := &resp.Return[i]
		_, deviceID, ok := ExtractDiskInfo(dev.EffectiveQDev())
		if !ok {
			continue
		}
		if conn.armed[deviceID] {
			continue
		}
		if !HasHistograms(dev) {
			c.log.Info("qmp: enabling histogram", "vmi", conn.vmi, "device_id", deviceID)
			armCtx, armCancel := context.WithTimeout(ctx, c.cfg.QMPTimeout)
			err := conn.client.EnableHistogram(armCtx, deviceID, c.cfg.BoundariesNs)
			armCancel()
			if err != nil {
				if conn.client.Closed() {
					return nil, err
				}
				c.log.Warn("qmp: failed to arm histogram", "vmi", conn.vmi, "device_id", deviceID, "error", err)
				continue
			}
		}
		conn.armed[deviceID] = true
	}

	pvcRefreshed := false
	refreshPVC := func(alias string) string {
		pvc := conn.pvcMap[alias]
		if pvc == "" && !pvcRefreshed {
			pvcRefreshed = true
			conn.pvcMap = FetchPVCMap(ctx, c.dynClient, conn.namespace, conn.vmi, c.log)
			pvc = conn.pvcMap[alias]
		}
		return pvc
	}

	var devices []DeviceResult
	for i := range resp.Return {
		dev := &resp.Return[i]
		alias, _, ok := ExtractDiskInfo(dev.EffectiveQDev())
		if !ok {
			continue
		}
		if !HasHistograms(dev) {
			continue
		}
		devices = append(devices, DeviceResult{
			Disk:  alias,
			PVC:   refreshPVC(alias),
			Stats: dev.Stats,
		})
	}

	var virtqueues []VirtqueueResult
	listCtx, listCancel := context.WithTimeout(ctx, c.cfg.QMPTimeout)
	virtioDevices, err := conn.client.QueryVirtio(listCtx)
	listCancel()
	if err != nil {
		if conn.client.Closed() {
			return nil, err
		}
		c.log.Warn("qmp: x-query-virtio not available, skipping virtqueue metrics", "vmi", conn.vmi, "error", err)
	} else {
		for _, vdev := range virtioDevices {
			if !IsVirtioBlk(&vdev) {
				continue
			}
			alias, _, ok := ExtractDiskInfo(vdev.Path)
			if !ok {
				continue
			}

			nq, cached := conn.numQueues[vdev.Path]
			if !cached {
				statusCtx, statusCancel := context.WithTimeout(ctx, c.cfg.QMPTimeout)
				vs, err := conn.client.QueryVirtioStatus(statusCtx, vdev.Path)
				statusCancel()
				if err != nil {
					if conn.client.Closed() {
						return nil, err
					}
					c.log.Warn("qmp: failed to query virtio status", "vmi", conn.vmi, "path", vdev.Path, "error", err)
					continue
				}
				nq = vs.NumVqs
				if nq > 0 {
					conn.numQueues[vdev.Path] = nq
				}
			}

			for qi := 0; qi < nq; qi++ {
				qsCtx, qsCancel := context.WithTimeout(ctx, c.cfg.QMPTimeout)
				qs, err := conn.client.QueryVirtioQueueStatus(qsCtx, vdev.Path, qi)
				qsCancel()
				if err != nil {
					if conn.client.Closed() {
						return nil, err
					}
					c.log.Warn("qmp: failed to query virtqueue status", "vmi", conn.vmi, "path", vdev.Path, "queue", qi, "error", err)
					continue
				}
				virtqueues = append(virtqueues, VirtqueueResult{
					Disk:     alias,
					PVC:      refreshPVC(alias),
					Bus:      "virtio",
					Queue:    qi,
					Inuse:    qs.Inuse,
					VringNum: qs.VringNum,
				})
			}
		}

		for _, vdev := range virtioDevices {
			if !IsVirtioScsi(&vdev) {
				continue
			}
			// skip anonymous/non-peripheral paths (e.g. /machine/peripheral-anon/...)
			if _, ok := ExtractControllerName(vdev.Path); !ok {
				continue
			}

			nq, cached := conn.numQueues[vdev.Path]
			if !cached {
				statusCtx, statusCancel := context.WithTimeout(ctx, c.cfg.QMPTimeout)
				vs, err := conn.client.QueryVirtioStatus(statusCtx, vdev.Path)
				statusCancel()
				if err != nil {
					if conn.client.Closed() {
						return nil, err
					}
					c.log.Warn("qmp: failed to query virtio-scsi status", "vmi", conn.vmi, "path", vdev.Path, "error", err)
					continue
				}
				nq = vs.NumVqs
				if nq > 0 {
					conn.numQueues[vdev.Path] = nq
				}
			}

			for qi := 0; qi < nq; qi++ {
				qsCtx, qsCancel := context.WithTimeout(ctx, c.cfg.QMPTimeout)
				qs, err := conn.client.QueryVirtioQueueStatus(qsCtx, vdev.Path, qi)
				qsCancel()
				if err != nil {
					if conn.client.Closed() {
						return nil, err
					}
					c.log.Warn("qmp: failed to query virtio-scsi queue status", "vmi", conn.vmi, "path", vdev.Path, "queue", qi, "error", err)
					continue
				}
				virtqueues = append(virtqueues, VirtqueueResult{
					Disk:     "",
					PVC:      "",
					Bus:      "scsi",
					Queue:    qi,
					Inuse:    qs.Inuse,
					VringNum: qs.VringNum,
				})
			}
		}
	}

	return &VMIResult{
		Namespace:  conn.namespace,
		Name:       conn.vmi,
		Node:       c.cfg.NodeName,
		PodName:    conn.podName,
		Devices:    devices,
		Virtqueues: virtqueues,
	}, nil
}
