// Copyright 2026 The KubeVirt Metrics Exporter Authors
// SPDX-License-Identifier: Apache-2.0

package qmp

import (
	"context"
	"io"
	"log/slog"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/openshift-virtualization/kubevirt-metrics-exporter/pkg/cri"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/cache"
)

type pollCRI struct{}

func (pollCRI) FindComputePID(_ context.Context, podName, _ string) (*cri.ContainerInfo, error) {
	return &cri.ContainerInfo{ContainerID: podName}, nil
}

func testPoller() *Collector {
	GinkgoHelper()
	c := NewCollector(PollerConfig{Concurrency: 2, QMPTimeout: 100 * time.Millisecond},
		cache.NewStore(cache.MetaNamespaceKeyFunc), nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	c.criClient = pollCRI{}
	return c
}

func addPollVM(c *Collector, name string, client *Client) {
	GinkgoHelper()
	Expect(c.podStore.Add(&corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: "default",
			Labels: map[string]string{"kubevirt.io": "virt-launcher", "vm.kubevirt.io/name": name},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	})).To(Succeed())
	c.connections[name] = &vmConnection{
		client: client, namespace: "default", vmi: name, podName: name,
		armed: make(map[string]bool), numQueues: make(map[string]int),
	}
}

func pollAndWait(c *Collector) {
	GinkgoHelper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		c.poll(ctx)
		done <- nil
	}()
	Expect(awaitError(done)).NotTo(HaveOccurred())
}

var _ = Describe("QMP poll recovery", func() {
	DescribeTable("finishes polling healthy VMs and recovers after an unresponsive VM reconnects", func(stallAt int) {
		c := testPoller()
		stuck, _ := connectedClient(func(i int) []byte {
			if i >= stallAt {
				return nil
			}
			return xdrString(`{"return":[]}`)
		})
		healthy, _ := connectedClient(func(int) []byte { return xdrString(`{"return":[]}`) })
		addPollVM(c, "stuck", stuck)
		addPollVM(c, "healthy", healthy)
		pollAndWait(c)
		Expect(c.lastPollTS).To(BeNumerically(">", 0))
		Expect(c.scrapeErrors).To(Equal(float64(1)))
		Expect(c.results).To(HaveLen(1))
		Expect(c.results[0].Name).To(Equal("healthy"))
		Expect(c.connections).NotTo(HaveKey("stuck"), "failed connection must be removed to allow reconnection")

		By("replacing the failed connection as on the next cycle")
		reconnected, _ := connectedClient(func(int) []byte { return xdrString(`{"return":[]}`) })
		addPollVM(c, "stuck", reconnected)
		pollAndWait(c)
		Expect(c.results).To(HaveLen(2))
		Expect(c.scrapeErrors).To(Equal(float64(1)))
	},
		Entry("blockstats timeout", 4),
		Entry("optional virtio query timeout", 5),
	)

	It("finishes cleanup when the last VM departs and its connection is unresponsive", func() {
		c := testPoller()
		client, _ := connectedClient(func(int) []byte { return nil })
		c.connections["departed"] = &vmConnection{client: client, vmi: "departed"}
		// No pods remain, and the old connection won't answer a close RPC.
		pollAndWait(c)
		Expect(c.lastPollTS).To(BeNumerically(">", 0))
		Expect(c.connections).To(BeEmpty())
	})

	It("keeps polling when an optional QMP command is unsupported", func() {
		c := testPoller()
		client, _ := connectedClient(func(i int) []byte {
			if i%2 == 0 {
				return xdrString(`{"return":[]}`)
			}
			return xdrString(`{"error":{"class":"CommandNotFound","desc":"unsupported"}}`)
		})
		addPollVM(c, "older-qemu", client)
		pollAndWait(c)
		pollAndWait(c)
		Expect(c.scrapeErrors).To(BeZero())
		Expect(c.results).To(HaveLen(1))
		Expect(c.connections).To(HaveLen(1))
	})
})
