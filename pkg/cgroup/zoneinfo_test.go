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

		snap, err := readZoneinfo(procRoot)
		Expect(err).NotTo(HaveOccurred())
		Expect(snap.Zones).To(HaveLen(4))
		Expect(snap.SlabReclaimable).To(BeEmpty())

		Expect(snap.Zones[0]).To(Equal(numaZoneMemory{
			NUMA:         "0",
			Zone:         "DMA",
			PresentBytes: 4000 * zonePageSize,
			FreeBytes:    2200 * zonePageSize,
		}))
		Expect(snap.Zones[1].Zone).To(Equal("DMA32"))
		Expect(snap.Zones[1].PresentBytes).To(Equal(uint64(390000 * zonePageSize)))
		Expect(snap.Zones[2].Zone).To(Equal("Movable"))
		Expect(snap.Zones[2].PresentBytes).To(Equal(uint64(24000000 * zonePageSize)))
		Expect(snap.Zones[3].Zone).To(Equal("Normal"))
		Expect(snap.Zones[3].PresentBytes).To(Equal(uint64(131072 * zonePageSize)))
	})

	It("parses per-node nr_slab_reclaimable once per NUMA node", func() {
		data, err := os.ReadFile(filepath.Join("testdata", "zoneinfo_dual_numa_slab"))
		Expect(err).NotTo(HaveOccurred())
		Expect(os.WriteFile(filepath.Join(procRoot, "zoneinfo"), data, 0o644)).To(Succeed())

		snap, err := readZoneinfo(procRoot)
		Expect(err).NotTo(HaveOccurred())
		Expect(snap.SlabReclaimable).To(Equal([]numaSlabReclaimable{
			{NUMA: "0", ReclaimableBytes: 117275 * zonePageSize},
			{NUMA: "1", ReclaimableBytes: 127201 * zonePageSize},
		}))
		Expect(snap.Zones).To(HaveLen(3))
		Expect(snap.Zones[0].Zone).To(Equal("DMA"))
		Expect(snap.Zones[1].Zone).To(Equal("DMA32"))
		Expect(snap.Zones[2].NUMA).To(Equal("1"))
		Expect(snap.Zones[2].Zone).To(Equal("Normal"))
	})

	It("returns error when zoneinfo is missing", func() {
		_, err := readZoneinfo(procRoot)
		Expect(err).To(HaveOccurred())
	})
})
