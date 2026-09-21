// Copyright 2026 The KubeVirt Metrics Exporter Authors
// SPDX-License-Identifier: Apache-2.0

//go:build e2e

package e2e

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/openshift-virtualization/kubevirt-metrics-exporter/test/utils"
)

var _ = Describe("Metrics Exporter", func() {
	var exporterPod string

	BeforeEach(func() {
		var err error
		exporterPod, err = utils.GetExporterPodName(exporterNamespace)
		Expect(err).NotTo(HaveOccurred())
	})

	Context("DaemonSet deployment", func() {
		It("should have all pods ready", func() {
			Expect(utils.WaitForDaemonSetReady(
				"kubevirt-metrics-exporter",
				exporterNamespace,
				60*time.Second,
			)).To(Succeed())
		})
	})

	Context("health endpoint", func() {
		It("should return ok", func() {
			body, err := utils.PortForwardAndGetOnPort(exporterNamespace, exporterPod, 8081, "/healthz")
			Expect(err).NotTo(HaveOccurred())
			Expect(body).To(Equal("ok"))
		})
	})

	Context("metrics endpoint", func() {
		var metrics string

		BeforeEach(func() {
			var err error
			metrics, err = utils.PortForwardAndGet(exporterNamespace, exporterPod, "/metrics")
			Expect(err).NotTo(HaveOccurred())
		})

		It("should return Prometheus text format", func() {
			Expect(metrics).To(ContainSubstring("# HELP"))
			Expect(metrics).To(ContainSubstring("# TYPE"))
		})

		It("should report QMP poll timestamp", func() {
			Expect(metrics).To(ContainSubstring("kme_qmp_last_poll_timestamp_seconds"))
		})

		It("should report zero QMP scrape errors", func() {
			Expect(metrics).To(ContainSubstring("kme_qmp_scrape_errors_total 0"))
		})

		It("should report eBPF subsystem status", func() {
			Expect(metrics).To(ContainSubstring("kme_subsystem_active"))
		})
	})

	Context("eBPF block tracing", func() {
		It("should have block subsystem active", func() {
			Eventually(func() string {
				metrics, _ := utils.PortForwardAndGet(exporterNamespace, exporterPod, "/metrics")
				return metrics
			}, 30*time.Second, 5*time.Second).Should(
				ContainSubstring(`kme_subsystem_active{subsystem="block"} 1`))
		})

		It("should capture system block I/O", func() {
			Eventually(func() string {
				metrics, _ := utils.PortForwardAndGet(exporterNamespace, exporterPod, "/metrics")
				return metrics
			}, 60*time.Second, 5*time.Second).Should(
				ContainSubstring("kme_system_block_io_latency_seconds"))
		})
	})

	// NOTE: PVC resolution (persistentvolumeclaim label) is not tested here because
	// Kind's local-path provisioner does not create standard kubelet volume mount paths
	// in /proc/1/mountinfo. PVC resolution requires a CSI provisioner (e.g. Ceph RBD)
	// and is verified via manual smoke tests on OpenShift/Kubernetes clusters.

	Context("QMP without VMs", func() {
		It("should find zero virt-launcher pods without errors", func() {
			metrics, err := utils.PortForwardAndGet(exporterNamespace, exporterPod, "/metrics")
			Expect(err).NotTo(HaveOccurred())
			Expect(metrics).To(ContainSubstring("kme_qmp_scrape_errors_total 0"))
			Expect(metrics).NotTo(ContainSubstring("kubevirt_vmi_storage_io_latency_seconds"))
		})
	})
})
