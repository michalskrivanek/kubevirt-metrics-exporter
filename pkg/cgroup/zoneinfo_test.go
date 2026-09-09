package cgroup

import (
	"os"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("readZoneinfo", func() {
	var procRoot string

	BeforeEach(func() {
		procRoot = GinkgoT().TempDir()
	})

	It("parses present and pages free per NUMA node and zone", func() {
		data, err := os.ReadFile(filepath.Join("testdata", "zoneinfo_single_numa_kernelcore"))
		Expect(err).NotTo(HaveOccurred())
		Expect(os.WriteFile(filepath.Join(procRoot, "zoneinfo"), data, 0o644)).To(Succeed())

		results, err := readZoneinfo(procRoot)
		Expect(err).NotTo(HaveOccurred())
		Expect(results).To(HaveLen(4))

		Expect(results[0]).To(Equal(numaZoneMemory{
			NUMA:         "0",
			Zone:         "DMA",
			PresentBytes: 4000 * zonePageSize,
			FreeBytes:    2200 * zonePageSize,
		}))
		Expect(results[1].Zone).To(Equal("DMA32"))
		Expect(results[1].PresentBytes).To(Equal(uint64(390000 * zonePageSize)))
		Expect(results[2].Zone).To(Equal("Movable"))
		Expect(results[2].PresentBytes).To(Equal(uint64(24000000 * zonePageSize)))
		Expect(results[3].Zone).To(Equal("Normal"))
		Expect(results[3].PresentBytes).To(Equal(uint64(131072 * zonePageSize)))
	})

	It("returns error when zoneinfo is missing", func() {
		_, err := readZoneinfo(procRoot)
		Expect(err).To(HaveOccurred())
	})
})
