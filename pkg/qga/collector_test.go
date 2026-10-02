// Copyright 2026 The KubeVirt Metrics Exporter Authors
// SPDX-License-Identifier: Apache-2.0

package qga

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/openshift-virtualization/kubevirt-metrics-exporter/pkg/cri"
	"github.com/openshift-virtualization/kubevirt-metrics-exporter/pkg/qmp"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/cache"
)

type pollCRI struct {
	pid int
}

func (p pollCRI) FindComputePID(_ context.Context, podName, _ string) (*cri.ContainerInfo, error) {
	return &cri.ContainerInfo{ContainerID: podName, PID: p.pid}, nil
}

func testCollector(maxRetries int) *Collector {
	GinkgoHelper()
	c := NewCollector(CollectorConfig{
		Concurrency: 2,
		QGATimeout:  1,
		ExecWait:    10 * time.Millisecond,
		MaxRetries:  maxRetries,
	}, cache.NewStore(cache.MetaNamespaceKeyFunc), nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	c.criClient = pollCRI{pid: 1}
	return c
}

func pipeClient() *qmp.Client {
	GinkgoHelper()
	a, b := net.Pipe()
	DeferCleanup(func() {
		a.Close()
		b.Close()
	})
	return qmp.ClientForTest(a)
}

func addLauncherPod(c *Collector, name string) {
	GinkgoHelper()
	Expect(c.podStore.Add(&corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "default",
			Labels: map[string]string{
				"kubevirt.io":         "virt-launcher",
				"vm.kubevirt.io/name": name,
			},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	})).To(Succeed())
}

func addVM(c *Collector, id string, client *qmp.Client) *vmState {
	GinkgoHelper()
	vs := &vmState{
		client:    client,
		namespace: "default",
		vmi:       id,
		podName:   id,
	}
	c.vms[id] = vs
	return vs
}

var _ = Describe("GuestExecWait cancellation", func() {
	It("returns when the context is canceled during the initial wait", func() {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := GuestExecWait(ctx, nil, 1, 1, time.Hour)
		Expect(err).To(MatchError(context.Canceled))
	})

	It("returns when the context expires during the initial wait", func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()
		start := time.Now()
		_, err := GuestExecWait(ctx, nil, 1, 1, time.Hour)
		Expect(err).To(MatchError(context.DeadlineExceeded))
		Expect(time.Since(start)).To(BeNumerically("<", time.Second))
	})
})

var _ = Describe("QGA scrape recovery", func() {
	It("drops a VM whose client is already closed so the next poll can reconnect", func() {
		c := testCollector(3)
		client := pipeClient()
		Expect(client.Close()).To(Succeed())
		vs := addVM(c, "stuck", client)
		addLauncherPod(c, "stuck")

		// connectVM will fail (no /proc sock); drop of dead client still happens first.
		c.poll(context.Background())
		Expect(c.vms).NotTo(HaveKey("stuck"))
		Expect(vs.closed).To(BeTrue())
	})

	It("drops on ErrClientClosed so the next poll can reconnect", func() {
		c := testCollector(3)
		vs := addVM(c, "vm", pipeClient())
		c.handleScrapeError("vm", vs, qmp.ErrClientClosed)
		Expect(c.vms).NotTo(HaveKey("vm"))
		Expect(vs.closed).To(BeTrue())
	})

	It("drops on context deadline so a stalled RPC reconnects next cycle", func() {
		c := testCollector(3)
		vs := addVM(c, "vm", pipeClient())
		c.handleScrapeError("vm", vs, context.DeadlineExceeded)
		Expect(c.vms).NotTo(HaveKey("vm"))
		Expect(vs.closed).To(BeTrue())
	})

	It("moves a VM to passive probing after MaxRetries instead of dropping", func() {
		c := testCollector(2)
		c.cfg.PollInterval = time.Minute
		vs := addVM(c, "vm", pipeClient())
		vs.prevSnapshot = map[string]DiskCounters{"0 C:": {}}
		vs.diskMap = map[int]string{0: "vol-0"}
		soft := errors.New("guest-exec: Guest agent is not responding")
		c.handleScrapeError("vm", vs, soft)
		Expect(c.vms).To(HaveKey("vm"))
		Expect(vs.passive).To(BeFalse())
		c.handleScrapeError("vm", vs, soft)
		Expect(c.vms).To(HaveKey("vm"))
		Expect(vs.passive).To(BeTrue())
		Expect(vs.closed).To(BeFalse())
		Expect(vs.prevSnapshot).To(BeNil())
		Expect(vs.diskMap).To(BeNil())
		Expect(c.lastPassiveAt).NotTo(BeZero())
		Expect(c.passiveInterval()).To(Equal(60 * time.Minute))
	})

	It("skips passive VMs on the active poll until the slow cycle is due", func() {
		c := testCollector(2)
		c.cfg.PollInterval = time.Minute
		vs := addVM(c, "vm", pipeClient())
		vs.passive = true
		c.lastPassiveAt = time.Now()
		addLauncherPod(c, "vm")

		c.poll(context.Background())
		Expect(c.vms).To(HaveKey("vm"))
		Expect(vs.retryCount).To(BeZero())
		Expect(vs.passive).To(BeTrue())
		Expect(vs.closed).To(BeFalse())
	})

	It("moves blacklisted VMs to passive probing without deleting the entry", func() {
		c := testCollector(2)
		vs := addVM(c, "vm", pipeClient())
		c.handleScrapeError("vm", vs, ErrCommandBlacklisted)
		Expect(c.vms).To(HaveKey("vm"))
		Expect(vs.passive).To(BeTrue())
		Expect(vs.closed).To(BeFalse())
	})

	It("sizes the scrape budget to cover exec plus status polls", func() {
		c := testCollector(1)
		c.cfg.QGATimeout = 10
		c.cfg.ExecWait = time.Second
		base := 10*time.Second*(1+guestExecStatusAttempts) +
			time.Second*time.Duration(guestExecStatusAttempts) +
			2*time.Second
		Expect(c.scrapeBudget(false)).To(Equal(base))
		Expect(c.scrapeBudget(true)).To(Equal(base + 10*time.Second))
	})
})
