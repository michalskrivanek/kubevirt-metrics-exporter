# KubeVirt Metrics Exporter

A Prometheus exporter for OpenShift Virtualization that combines storage I/O latency tracing with VM memory and THP observability. It runs as a DaemonSet with several independent collection subsystems in a single container:

- **QMP subsystem** — connects to each VM's QEMU Monitor Protocol to collect per-disk read/write/flush latency histograms directly from the hypervisor
- **QGA subsystem** — uses the QEMU Guest Agent to collect guest-side I/O latency and IOPS from Windows VMs via Windows Performance Counters (PDH raw counters)
- **eBPF subsystem** — attaches kernel tracepoints and kprobes to capture block and NFS I/O latency across the node, correlated to Kubernetes pods and PersistentVolumeClaims
- **KVM subsystem** — reads KVM hypervisor event counters (exits, hypercalls, TLB flushes, halt exits) from the kernel debugfs at `/sys/kernel/debug/kvm/`
- **Cgroup subsystem** — reads per-VMI cgroup v2 memory stats (anonymous, THP) from QEMU process cgroups; per-node buddy/pagetype THP readiness (`/proc/buddyinfo`, `/proc/pagetypeinfo`); and kernel thread CPU (`khugepaged`, `ksmd`) plus KSM profit

All subsystems are independently enabled/disabled and degrade gracefully if one fails to start.

## Metrics

The exporter instruments several points along the I/O path from guest application to storage backend:

```
Guest application
  │
  ▼
Guest OS block layer ◄──── QGA: guest-side latency & IOPS (Windows only)
  │
  ▼
Storage virtqueue ◄─────── QMP: queue_inuse / queue_size (saturation, virtio/scsi)
  │
  ▼
QEMU block backend ◄────── QMP: I/O latency histogram (hypervisor-side)
  │
KVM (hypervisor) ◄───────── KVM: exits, hypercalls, tlb_flush, halt_exits
  │
  ├──► Host block layer ◄─ eBPF block: block_rq_issue → block_rq_complete
  │
  └──► Host NFS client ◄── eBPF NFS: nfs_initiate_* → nfs_*_done
         │
         ▼
       Storage backend
```

When diagnosing latency, compare metrics across layers: if QMP latency is high but eBPF block latency is low, the bottleneck is in the virtio/QEMU layer. If both are high, the problem is in the storage backend. If guest-side (QGA) latency is high but QMP latency is low, queuing is building up inside the guest.

For memory and THP, see [THP and node memory readiness](#thp-and-node-memory-readiness) and the cgroup metrics below.

VMI-level storage and KVM metrics use the `kubevirt_vmi_*` prefix. Per-VMI cgroup memory from this exporter uses `container_memory_*` (CRI-O-aligned). Exporter operational, eBPF, and node buddy metrics use `kme_*`.

### QMP metrics

| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `kubevirt_vmi_storage_io_latency_seconds` | histogram | namespace, name, node, disk, persistentvolumeclaim, operation | Per-disk I/O latency for KubeVirt VMs |
| `kubevirt_vmi_storage_queue_inuse` | gauge | namespace, name, node, disk, persistentvolumeclaim, queue, bus | In-flight descriptors in a storage virtqueue (`bus="virtio"` for virtio-blk disks, `bus="scsi"` for virtio-scsi controllers; `disk` and `persistentvolumeclaim` are empty for SCSI) |
| `kubevirt_vmi_storage_queue_size` | gauge | namespace, name, node, disk, persistentvolumeclaim, queue, bus | Capacity (max descriptors) of a storage virtqueue (see queue_inuse for label semantics) |
| `kme_qmp_scrape_errors_total` | counter | | Errors during QMP poll cycles |
| `kme_qmp_last_poll_timestamp_seconds` | gauge | | Unix timestamp of last QMP poll |

The `bus` label distinguishes `virtio` (per-disk virtio-blk devices) from `scsi` (shared virtio-scsi controller). For virtio-scsi, `disk` and `persistentvolumeclaim` are empty because the virtqueues belong to the shared controller rather than any individual disk.

### QGA metrics (guest-side, Windows)

| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `kubevirt_vmi_storage_guest_latency_avg_seconds` | gauge | namespace, name, node, disk, persistentvolumeclaim, operation, drive | Average guest-side I/O latency per disk (read/write), derived via Little's Law |
| `kubevirt_vmi_storage_guest_iops` | gauge | namespace, name, node, disk, persistentvolumeclaim, operation, drive | Guest-side IOPS per disk (read/write) |
| `kme_qga_scrape_errors_total` | counter | | Errors during QGA poll cycles |
| `kme_qga_last_poll_timestamp_seconds` | gauge | | Unix timestamp of last QGA poll |

The `disk` label contains the KubeVirt volume name (e.g. `rootdisk`), populated by correlating guest PCI addresses (from the `guest-get-disks` QGA command) with libvirt domain XML disk aliases (`ua-<volumeName>`). The `drive` label contains the raw Windows PhysicalDisk name (e.g. `"1 E:"`). The `persistentvolumeclaim` label is derived by mapping volume names to PVC claim names via the VMI `status.volumeStatus`. If disk mapping is unavailable (e.g. old guest agent), `disk` and `persistentvolumeclaim` are empty.

Disk mapping limitations:
- Disk mappings are established when the VMI is first discovered. Volumes hotplugged after that point are not tracked (their `disk` and `persistentvolumeclaim` labels will be empty).
- SATA disks can only be mapped when a `serial` is explicitly set on the disk in the VM spec. This is a QEMU guest agent limitation. Without `<serial>` the label `persistentvolumeclaim` will be empty.

The QGA subsystem collects raw Windows Performance Counters (`Win32_PerfRawData_PerfDisk_PhysicalDisk`) via `wmic` executed through the QEMU Guest Agent. Metrics are computed by diffing two successive counter snapshots using Little's Law to derive latency from uint64 queue-length counters (avoiding uint32 overflow in the direct latency counters). VMs without a guest agent (e.g., Linux) or with `guest-exec` blacklisted are automatically detected and excluded after a configurable number of retries.

### eBPF metrics

| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `kme_block_io_latency_seconds` | histogram | node, namespace, persistentvolumeclaim, pod, operation | Block I/O latency attributed to pod volumes |
| `kme_system_block_io_latency_seconds` | histogram | node, device, operation | Block I/O latency for system/unresolved devices |
| `kme_nfs_io_latency_seconds` | histogram | node, namespace, persistentvolumeclaim, pod, operation | NFS I/O latency (tracepoint-based) |
| `kme_nfs_vfs_latency_seconds` | histogram | node, namespace, persistentvolumeclaim, pod, operation | NFS VFS call latency (kprobe-based) |
| `kme_subsystem_active` | gauge | subsystem | Whether an eBPF subsystem loaded successfully (1) or not (0) |

### KVM metrics

| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `kubevirt_vmi_kvm_exits_total` | counter | namespace, name, node, pod | Total KVM VM exits (guest→hypervisor transitions) |
| `kubevirt_vmi_kvm_hypercalls_total` | counter | namespace, name, node, pod | Total KVM hypercalls issued by the guest |
| `kubevirt_vmi_kvm_tlb_flushes_total` | counter | namespace, name, node, pod | Total KVM TLB flush events |
| `kubevirt_vmi_kvm_halt_exits_total` | counter | namespace, name, node, pod | Total KVM exits triggered by guest halt instructions |
| `kme_kvm_scrape_errors_total` | counter | | Errors during KVM debugfs poll cycles |
| `kme_kvm_last_poll_timestamp_seconds` | gauge | | Unix timestamp of last KVM poll |

Counters are read from `/sys/kernel/debug/kvm/<pid>-<fd>/` and aggregated across all KVM file-descriptor entries for a single QEMU process. A high exit rate relative to vCPU time indicates the guest is spending significant cycles in hypervisor context. A high `halt_exits` rate is normal for idle VMs (vCPUs sleeping) but abnormal for CPU-intensive ones.

### Cgroup metrics

Per-VMI memory metrics (aligned with [CRI-O PR #10143](https://github.com/cri-o/cri-o/pull/10143)):

| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `container_memory_active_anon_bytes` | gauge | namespace, name, node, pod | Active anonymous memory in bytes |
| `container_memory_inactive_anon_bytes` | gauge | namespace, name, node, pod | Inactive anonymous memory in bytes |
| `container_memory_anon_thp_bytes` | gauge | namespace, name, node, pod | Anonymous memory backed by transparent hugepages in bytes |
| `container_memory_shmem_thp_bytes` | gauge | namespace, name, node, pod | Shared memory backed by transparent hugepages in bytes (kernel 6.8+) |
| `container_memory_file_thp_bytes` | gauge | namespace, name, node, pod | File-backed memory backed by transparent hugepages in bytes |

Per-node kernel thread and KSM metrics:

| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `kme_cgroup_khugepaged_cpu_seconds_total` | counter | node | Cumulative CPU time consumed by the khugepaged kernel thread |
| `kme_cgroup_ksmd_cpu_seconds_total` | counter | node | Cumulative CPU time consumed by the ksmd kernel thread |
| `node_ksmd_general_profit_bytes` | gauge | | Net memory saved by KSM after subtracting tracking overhead (aligned with [node_exporter PR #3778](https://github.com/prometheus/node_exporter/pull/3778)) |
| `kme_node_thp_split_pmd_total` | counter | node | THP page table downgrades (`thp_split_pmd` from `/proc/vmstat`) |
| `kme_node_thp_collapse_alloc_total` | counter | node | Successful THP collapses by khugepaged (`thp_collapse_alloc` from `/proc/vmstat`) |
| `kme_node_movable_bytes_order_ge_9` | gauge | node, numa | Movable-capable free buddy memory at page order ≥9 in bytes (Movable zone if present, else Normal; buddy minus Unmovable and Isolate) |
| `kme_node_movable_bytes_all_orders` | gauge | node, numa | Movable-capable free buddy memory across all orders in bytes (Movable zone if present, else Normal; buddy minus Unmovable and Isolate) |
| `kme_node_buddy_bytes_order_ge_9` | gauge | node, numa | Total free buddy memory at page order ≥9 in bytes (Movable zone if present, else Normal; exact from `/proc/buddyinfo`) |
| `kme_node_buddy_bytes_all_orders` | gauge | node, numa | Total free buddy memory across all orders in bytes (Movable zone if present, else Normal; exact from `/proc/buddyinfo`) |
| `kme_node_unmovable_bytes_order_ge_9` | gauge | node, numa | Unmovable free buddy memory at page order ≥9 in bytes (Normal + Movable zones, `/proc/pagetypinfo`) |
| `kme_node_unmovable_bytes_all_orders` | gauge | node, numa | Unmovable free buddy memory across all orders in bytes (Normal + Movable zones, `/proc/pagetypinfo`) |
| `kme_node_zone_present_bytes` | gauge | node, numa, zone | Zone size in bytes from `/proc/zoneinfo` `present` pages |
| `kme_node_zone_free_bytes` | gauge | node, numa, zone | Zone free pages in bytes from `/proc/zoneinfo` `pages free` |
| `kme_cgroup_scrape_errors_total` | counter | | Errors during cgroup poll cycles |
| `kme_cgroup_last_poll_timestamp_seconds` | gauge | | Unix timestamp of last cgroup poll |

Per-VMI gauges come from cgroup v2 `memory.stat` on each QEMU process. Node buddy, pagetype, zoneinfo, and vmstat THP counters come from host `/proc` (per NUMA node). Buddy/movable metrics use the Movable zone when present (else Normal). The `container_memory_*` naming aligns with the [CRI-O cgroup memory proposal](https://github.com/cri-o/cri-o/pull/10143); `node_ksmd_general_profit_bytes` aligns with [node_exporter PR #3778](https://github.com/prometheus/node_exporter/pull/3778).

VM resident and domain memory (`kubevirt_vmi_memory_resident_bytes`, `kubevirt_vmi_memory_domain_bytes`) are scraped from virt-handler / KubeVirt, not from this exporter — see the dashboard and [THP and node memory readiness](#thp-and-node-memory-readiness) sections for how those metrics are used alongside KME cgroup and buddy gauges.

### Example PromQL

P99 write latency per VMI:
```promql
histogram_quantile(0.99,
  sum by (name, le) (
    rate(kubevirt_vmi_storage_io_latency_seconds_bucket{operation="write"}[5m])
  )
)
```

Virtqueue saturation per disk (ratio of in-flight descriptors to capacity):
```promql
kubevirt_vmi_storage_queue_inuse / (kubevirt_vmi_storage_queue_size > 0)
```

P99 host-side block I/O latency attributed to a pod's PVC:
```promql
histogram_quantile(0.99,
  sum by (namespace, persistentvolumeclaim, pod, operation, le) (
    rate(kme_block_io_latency_seconds_bucket{operation=~"read|write"}[5m])
  )
)
```

See [`docs/example-queries.md`](docs/example-queries.md) for a full per-metric query reference (storage, KVM, and exporter health). Memory and THP dashboard interpretation is in [THP and node memory readiness](#thp-and-node-memory-readiness) below.

## THP and node memory readiness

This section explains the **Kubevirt VM Memory (Dev Preview)** Perses dashboard (`deploy/openshift/dashboard-memory.yaml`) and the KME node memory metrics it uses.

### Three layers (do not mix them)

| Layer | Source | What it measures | Dashboard panel |
|-------|--------|------------------|-----------------|
| **Structural layout** | `/proc/zoneinfo` | Zone sizes (`present`) and in-zone free pages. When `ZONE_MOVABLE` is populated (`kernelcore=` layout): DMA+DMA32+Normal ≈ pinned kernel pool; Movable ≈ guest/THP pool. Without Movable, DMA+DMA32+Normal is essentially all node RAM — kernelcore used is not meaningful. | Kernelcore pool |
| **THP buddy stock** | `/proc/buddyinfo` + `/proc/pagetypeinfo` | Free buddy blocks in the **Movable zone** (else Normal): total, movable-capable, and pinned freelist. | Movable zone |
| **Node reclaim context** | `MemAvailable` (node_exporter) | Node-wide estimate including reclaimable cache — **not** per-NUMA and **not** buddy-only. | Movable zone (dashed) |

On split-layout hosts only, kernelcore **used** (present − free) is mostly kernel/slab/page-table allocations. **Free + used = present** (summed over DMA+DMA32+Normal per NUMA) — the dashboard plots free and used only; no separate cap line. Lines are gated on Movable zone presence (`kernelcore_zoneinfo_gate`). On fallback hosts the Kernelcore panel is empty; the Movable zone panel's **Non-THP-movable** line (`buddy − movable`) approximates the small pinned freelist there.

### Background: transparent huge pages (THP)

On typical x86-64 hosts (4 KiB base page size), **PMD-sized THP uses 2 MiB pages** (buddy **order 9**; each order *o* spans `4096 × 2^o` bytes per block). The kernel can promote anonymous and shmem mappings to THP and demote them again under pressure. Unless THP is fully disabled, the **`khugepaged`** kernel thread scans memory and attempts to **collapse** eligible small pages into huge pages. Policy (`always`, `madvise`, `never`, per-size controls on recent kernels) is described in the [Transparent Hugepage Support](https://docs.kernel.org/admin-guide/mm/transhuge.html) admin guide.

THP observability splits naturally into two questions:

1. **Guest / VM:** How much memory is **already** huge-backed?
2. **Node:** How much **free physical stock** can still support new 2 MiB allocations or khugepaged collapse on a given NUMA node?

The dashboard and KME metrics address both.

### Kernel sources

| Source | What it reports | Used for |
|--------|-----------------|----------|
| `/proc/buddyinfo` | Free buddy **blocks** per zone and NUMA node, by **order only** (not migrate type). **Exact** block counts. | `kme_node_buddy_bytes_*` |
| `/proc/pagetypeinfo` | Same free-block shape, split by **migrate type** (Movable, Unmovable, Reclaimable, Isolate, …). Counts can be **capped** (e.g. `>100000`) when the kernel avoids long zone-lock holds. | `kme_node_unmovable_bytes_*`; input to movable derivation |
| `/proc/zoneinfo` | Structural zone sizes (`present`) and free pages (`pages free`) per zone and NUMA node. | `kme_node_zone_present_bytes`, `kme_node_zone_free_bytes` |
| cgroup v2 `memory.stat` | Per-QEMU `anon_thp`, `shmem_thp`, `file_thp`, etc. | `container_memory_*_thp_bytes` |
| `/proc/vmstat` | Node counters `thp_split_pmd`, `thp_collapse_alloc` | `kme_node_thp_*_total` |
| virt-handler metrics | libvirt `dommemstat` RSS and balloon/domain size | `kubevirt_vmi_memory_resident_bytes`, `kubevirt_vmi_memory_domain_bytes` |

**Buddyinfo vs pagetypeinfo:** Buddyinfo answers “how much free memory exists at each block size?” Pagetypeinfo answers the same question but splits counts by **migrate type** — the kernel’s label for whether free pages in a page block can be relocated during compaction, migration, or hugepage grouping:

| Migrate type | Typical meaning |
|--------------|-----------------|
| **Movable** | Can be migrated/compacted; preferred for user allocations and THP grouping. |
| **Unmovable** | Pinned on the freelist (kernel structures, some DMA-related stock, etc.); not available for collapse as movable stock. |
| **Reclaimable** | File-backed pages on the freelist that can be reclaimed (metadata/cache) before reuse. |
| **Isolate** | Isolated blocks (debug/testing); treated like unmovable for THP readiness math in KME. |
| **Reserve** | Reserved for future movable use; not reported by KME. |

The kernel keeps migrate types separated within page blocks to limit fragmentation under mixed workloads.

KME reports buddy/pagetype from the **Movable zone when present** (else Normal) for THP readiness, and sums Unmovable freelist across Normal + Movable zones. Zoneinfo covers all zones (DMA, DMA32, Normal, Movable). Values are labeled by Kubernetes **node** name and **NUMA** node id.

### How KME derives buddy metrics

```
buddy (exact)          ← /proc/buddyinfo Movable zone (else Normal)
excluded               ← /proc/pagetypeinfo same THP zone (Unmovable + Isolate lines)
movable-capable        ← buddy − excluded   (per NUMA, clamped at 0)
unmovable freelist     ← pagetypeinfo Unmovable summed across Normal + Movable zones
kernelcore pool        ← zoneinfo present/free on DMA + DMA32 + Normal zones
```

Exported gauges:

| Metric | Meaning |
|--------|---------|
| `kme_node_buddy_bytes_order_ge_9` / `_all_orders` | Total free buddy memory in the THP zone (all migrate types). |
| `kme_node_movable_bytes_order_ge_9` / `_all_orders` | **Movable-capable** free buddy in the THP zone: buddy minus Unmovable and Isolate freelist pages. |
| `kme_node_unmovable_bytes_order_ge_9` / `_all_orders` | **Unmovable** migrate-type freelist only (Normal + Movable zones). Isolate is subtracted for movable-capable math but not included here. |
| `kme_node_zone_present_bytes` / `_free_bytes` | Structural zone size and free pages from zoneinfo (`present` / `pages free`). |

**Important:** `movable-capable` is **not** the pagetypeinfo “Movable” line. Buddy total includes **Reclaimable** (and other) freelist pages that are not Unmovable or Isolate; those remain in buddy and are counted in `movable-capable` because only Unmovable and Isolate are subtracted. In practice, `movable-capable` is often **above** the pagetype Movable line at the same NUMA node; the gap is largely **reclaimable freelist** memory (and any other migrate types except Isolate).

**Why both `order_ge_9` and `all_orders` for unmovable?** They measure different things:

- **`unmovable_bytes_order_ge_9`** — free Unmovable blocks at order ≥ 9 (2 MiB+). This is the portion of **THP-sized** free buddy that cannot be used for collapse. It is required internally to compute `movable_bytes_order_ge_9` and is useful for alerts/debugging (“how much 2 MiB-class free stock is pinned unmovable?”).
- **`unmovable_bytes_all_orders`** — all free Unmovable blocks (orders 0–10). Most unmovable free memory usually sits in **small** orders; this line shows total pinned freelist footprint but does **not** substitute for ge9 when assessing immediate THP readiness.

The **Kubevirt VM Memory (Dev Preview)** dashboard plots movable-capable buddy lines plus **Non-THP-movable** (`buddy − movable` in the THP zone). It does not plot `unmovable_bytes_all_orders` or `unmovable_bytes_order_ge_9` directly; the ge9 gap `buddy_ge9 − movable_ge9` already reflects unmovable (and reclaimable) stock at 2 MiB orders.

### Dashboard panels

**Limit** (`$topk`, default 5) — caps most panels to the top *N* series by **instant value** (Prometheus `topk`). Not used on the Kernelcore pool panel. Multi-line panels share an anchor so related series stay aligned: buddy free total for Movable zone node·NUMA pairs; metric sum per node for khugepaged/ksmd and split/collapse (see table).

**VM Memory** — scoped by `node` (pick one node for per-VM detail). Cgroup THP metrics use `sum without (pod)` so `node` stays on the series for the virt-handler join. Resident and domain metrics are filtered with `kubevirt_vmi_info{phase="running"}` (includes paused VMs; excludes stopped/transitional/stale post-migration series).

| Panel | Limit sort (descending) | PromQL idea | Interpretation |
|-------|-------------------------|-------------|----------------|
| **THP / Resident** | THP / resident % | `topk(N, sum(anon_thp + shmem_thp) / resident × 100)` joined on `(namespace, name, node)`; resident filtered with `kubevirt_vmi_info{phase="running"}` | Share of **RSS** already backed by THP. Rising → more guest RAM in huge pages (successful collapse or huge-friendly allocation). Low → mostly 4 KiB pages. Denominator is libvirt RSS (`kubevirt_vmi_memory_resident_bytes`), numerator from QEMU cgroup `memory.stat` (`sum without (pod)`). |
| **Resident / Configured** | resident / configured % | `topk(N, resident / domain_bytes × 100)` with running-VMI filter on both sides | Share of **ballooned domain size** actually resident. Context for memory footprint, not THP-specific. Low ratio with a large balloon means much “configured” memory is not in RAM. |

**Node — THP readiness (Movable zone)** — buddy/movable lines share the same top node·NUMA set (anchor: **buddy free total**).

| Line | Limit sort | Metric | Calculation |
|------|------------|--------|-------------|
| **≥2 MiB Movable** | anchor | `movable_bytes_order_ge_9` | `buddy − unmovable − isolate` at orders ≥9 (Movable zone, or Normal if no Movable) |
| **Movable total** | anchor | `movable_bytes_all_orders` | Same formula, orders 0–10 |
| **Buddy free total** | buddy free bytes | `buddy_bytes_all_orders` | Sum of all free buddy blocks from `/proc/buddyinfo` |
| **Non-THP-movable freelist** | anchor | `buddy − movable` | Unmovable + Isolate free buddy in the THP zone only |
| **MemAvailable** (dashed) | MemAvailable bytes | `node_memory_MemAvailable_bytes` joined to `kube_pod_info` on `(namespace, pod)` | Node-wide reclaim estimate; includes cache and all zones; `node` label from kube-state-metrics |

**Node — THP readiness (Kernelcore pool)** — **split-layout hosts only** (populated Movable zone). Empty on fallback hosts; use the Movable zone panel there (Non-THP-movable ≈ unmovable freelist).

| Line | PromQL idea | Interpretation |
|------|-------------|----------------|
| **Kernelcore free** | `sum(zone_free{DMA\|DMA32\|Normal})` × gate | Free pages on buddy freelists in core zones |
| **Kernelcore used** | `sum(zone_present{…}) − sum(zone_free{…})` × gate | Allocated pages in core zones; **free + used = sum(present)** per NUMA |

Gate: hidden dashboard variable `kernelcore_zoneinfo_gate` (`movable_present` \| `always`). To drop the gate once all nodes use split layout: set `always`, then remove the variable and trailing ` $kernelcore_zoneinfo_gate` from the two zoneinfo queries.

**Node — khugepaged & ksmd CPU** — both lines share the same top **nodes** (anchor: `khugepaged CPU % + ksmd CPU %` per node).

| Line | Limit sort |
|------|------------|
| **khugepaged CPU %** | sum anchor (per node) |
| **ksmd CPU %** | sum anchor (per node) |

`100 × rate(cpu_seconds_total[interval])` → approximate **% of one CPU core**.

- **khugepaged** — THP collapse / scanning activity. Bursts are normal when memory is being collapsed; sustained high rates under load warrant checking split vs collapse counters.
- **ksmd** — Kernel Samepage Merging (separate from THP). High ksmd CPU means active page merging; it competes for CPU but is not the same mechanism as THP.

**Node — THP split & collapse** — both lines share the same top **nodes** (anchor: `split_pmd/min + collapse_alloc/min` per node).

| Line | Limit sort |
|------|------------|
| **split_pmd/min** | sum anchor (per node) |
| **collapse_alloc/min** | sum anchor (per node) |

`60 × rate(vmstat_counter[interval])` → events **per minute**.

- **`thp_split_pmd`** — THPs split back to smaller pages (unmap, protection changes, pressure, fragmentation management).
- **`thp_collapse_alloc`** — Successful collapses by khugepaged.

Counters are **lifetime** totals; the dashboard shows **recent rate**. Near-zero rates mean a quiet window, not disabled THP. Rising splits with flat collapses suggest THP churn or pressure.

### Reading trends together

| Pattern | Likely meaning |
|---------|----------------|
| movable ge9 ↑ | More immediate 2 MiB-capable free stock on that NUMA node. |
| movable ge9 ↓ while VM resident ↑ | Guest RAM growing faster than 2 MiB free stock replenishes; new THP formation may slow. |
| unmovable ge9 ↑ (or large `buddy_ge9 − movable_ge9`) | More THP-sized free memory is **not** movable-capable. |
| collapse_alloc rate ↑ | khugepaged actively forming THPs. |
| split_pmd rate ↑, collapse flat | THPs being torn down faster than formed — check pressure, mapping churn, or policy. |
| VM THP/Resident ↑ | Guest using more huge-backed RAM. |
| VM THP/Resident low, movable ge9 high | THP opportunity not yet taken (policy, workload, or khugepaged not needed yet). |
| MemAvailable high, movable ge9 low | Common: plenty of reclaimable cache globally, but **buddy freelist** at 2 MiB orders is still tight. |
| MemAvailable ≈ Movable zone free | Low page cache (idle node): reclaim estimate tracks buddy stock. |
| MemAvailable >> Movable buddy free | Busy node with large cache: dashed MemAvailable is **not** extra THP stock. |

### Caveats and limitations

- **Pagetype saturation:** Fields like `>100000` in pagetypeinfo are a **floor**, not an exact count. Affected orders in unmovable/movable derivations can be **undercounted**; buddyinfo totals at the same order remain exact. Prefer buddy ge9 for exact 2 MiB **total** free; treat pagetype-derived unmovable at saturated orders as approximate.
- **Order 0:** Pagetype per-order sums can diverge from buddyinfo at **order 0** on some kernels; orders **8–10** typically align. Rely on **ge9** lines for THP-sized conclusions.
- **Reclaimable freelist:** `movable-capable` subtracts only Unmovable and Isolate; **Reclaimable** free buddy pages remain in the residual. That makes `movable-capable` larger than the pagetype Movable line and can include pages that are not as readily usable for collapse as strictly Movable stock.
- **Isolate migrate type:** Subtracted in movable-capable math but not included in `kme_node_unmovable_bytes_*`. Use `buddy − movable` on the dashboard for Unmovable + Isolate in the THP zone.
- **kernelcore hosts:** When `ZONE_MOVABLE` exists, THP buddy metrics read the **Movable** zone. Without it, they read **Normal**. Zoneinfo always reports all zones. Kernelcore pool panel lines (free/used) are gated on Movable zone presence; do not interpret DMA+DMA32+Normal used on non-split hosts as kernel stock.
- **Resident vs cgroup anon:** RSS and cgroup `anon` are related but not identical; small gaps in THP/Resident are expected.
- **Scope:** Buddy/pagetype metrics describe **free** buddy pages in the THP zone (Movable if present, else Normal). Zoneinfo metrics describe **structural** zone sizes (`present`) and free pages across DMA/DMA32/Normal/Movable. Neither replaces hugetlbfs pool accounting.
- **Policy:** Metrics show stock and activity, not sysfs THP policy (`/sys/kernel/mm/transparent_hugepage/…`). Low VM THP with `never` or without `MADV_HUGEPAGE` is expected regardless of movable ge9.

### Further reading

- [Transparent Hugepage Support](https://docs.kernel.org/admin-guide/mm/transhuge.html) — THP policies, khugepaged, sysfs and boot parameters.
- [Memory management documentation index](https://docs.kernel.org/admin-guide/mm/index.html) — broader MM admin topics.
- [CRI-O cgroup memory metrics proposal](https://github.com/cri-o/cri-o/pull/10143) — alignment of `container_memory_*` naming.

## Configuration

Shared flags apply to all subsystems. QMP-specific flags are prefixed with `--qmp-`, QGA-specific with `--qga-`, eBPF-specific with `--ebpf-`. All flags can be overridden via environment variables.

### Shared

| Flag | Env | Default | Description |
|------|-----|---------|-------------|
| `--listen-address` | `LISTEN_ADDRESS` | `:8080` | Metrics server listen address |
| `--log-level` | `LOG_LEVEL` | `info` | Log level (debug, info, warn, error) |
| `--boundaries` | `BOUNDARIES` | `10000000,100000000,1000000000` | Histogram bucket boundaries in nanoseconds |
| | `NODE_NAME` | (required) | Node name, typically from downward API |
| `--namespaces` | `NAMESPACES` | (all) | Comma-separated namespace filter (QMP, QGA, and eBPF) |
| `--cri-socket` | `CRI_SOCKET` | `/run/crio/crio.sock` | CRI socket path for container discovery (shared by QMP and QGA) |

### QMP

| Flag | Env | Default | Description |
|------|-----|---------|-------------|
| `--enable-qmp` | `ENABLE_QMP` | `true` | Enable QMP collection |
| `--qmp-poll-interval` | `QMP_POLL_INTERVAL` | `1m` | VM scrape interval |
| `--qmp-concurrency` | `QMP_CONCURRENCY` | `8` | Max parallel QMP operations |
| `--qmp-timeout` | `QMP_TIMEOUT` | `5s` | Per-operation QMP timeout |
| `--qmp-label-filter` | `QMP_LABEL_FILTER` | | Additional pod label selector |

### QGA

| Flag | Env | Default | Description |
|------|-----|---------|-------------|
| `--enable-qga` | `ENABLE_QGA` | `true` | Enable QGA guest-side I/O collection |
| `--qga-poll-interval` | `QGA_POLL_INTERVAL` | `1m` | Guest metrics poll interval |
| `--qga-timeout` | `QGA_TIMEOUT` | `10` | Per-command QGA timeout (seconds) |
| `--qga-exec-wait` | `QGA_EXEC_WAIT` | `1s` | Wait between guest-exec and guest-exec-status |
| `--qga-retries` | `QGA_RETRIES` | `10` | Max consecutive failures before stopping collection for a VM |
| `--qga-concurrency` | `QGA_CONCURRENCY` | `8` | Max parallel QGA operations |
| `--qga-label-filter` | `QGA_LABEL_FILTER` | | Additional pod label selector for QGA |

### eBPF

| Flag | Env | Default | Description |
|------|-----|---------|-------------|
| `--enable-ebpf` | `ENABLE_EBPF` | `true` | Enable eBPF collection |
| `--enable-ebpf-block` | `ENABLE_EBPF_BLOCK` | `true` | Enable block I/O tracing |
| `--enable-ebpf-nfs` | `ENABLE_EBPF_NFS` | `true` | Enable NFS tracing |
| `--enable-ebpf-nfs-kprobe` | `ENABLE_EBPF_NFS_KPROBE` | `false` | Enable NFS VFS kprobe tracing |
| `--ebpf-scan-interval` | `EBPF_SCAN_INTERVAL` | `30` | Device-to-pod resolution interval (seconds) |
| `--ebpf-proc-path` | `EBPF_PROC_PATH` | `/proc` | Host proc filesystem path |

### KVM

| Flag | Env | Default | Description |
|------|-----|---------|-------------|
| `--enable-kvm` | `ENABLE_KVM` | `true` | Enable KVM debugfs stats collection |
| `--kvm-poll-interval` | `KVM_POLL_INTERVAL` | `30s` | Poll interval for KVM counters |

### Cgroup

| Flag | Env | Default | Description |
|------|-----|---------|-------------|
| `--enable-cgroup` | `ENABLE_CGROUP` | `true` | Enable cgroup v2 memory and kernel thread collection |
| `--cgroup-poll-interval` | `CGROUP_POLL_INTERVAL` | `30s` | Poll interval for cgroup stats |

## Alerting

Prometheus alerting rules are included in `deploy/prometheus-rules/` and deployed automatically with `make deploy` / `make deploy-kubernetes`.

The rules cover two areas:

**Workload health** — alerts on high storage I/O latency (hypervisor-side, guest-side, and node-side) and virtqueue saturation:

| Alert | Severity | Condition |
|-------|----------|-----------|
| VMIStorageWriteLatencyHigh | warning | P99 write latency > 100ms for 10m |
| VMIStorageReadLatencyHigh | warning | P99 read latency > 100ms for 10m |
| VMIStorageFlushLatencyHigh | warning | P99 flush latency > 500ms for 15m |
| NodeVMStorageLatencyWidespread | critical | >50% of active VMs on a node with P99 > 100ms for 10m |
| VMIDiskSaturated | critical | Aggregate virtqueue occupancy > 90% for 5m |
| VMIGuestStorageLatencyHigh | warning | Guest-side avg latency > 100ms for 15m |
| PVCBlockLatencyHigh | warning | P99 block latency > 100ms for 10m |
| PVCNFSLatencyHigh | warning | P99 NFS latency > 250ms for 10m |

**Exporter health** — alerts when the exporter itself is unhealthy or producing stale data:

| Alert | Severity | Condition |
|-------|----------|-----------|
| KMEQMPPollStale | warning | QMP poll > 5 min stale |
| KMEQGAPollStale | warning | QGA poll > 5 min stale |
| KMEKVMPollStale | warning | KVM poll > 90s stale |
| KMECgroupPollStale | warning | Cgroup poll > 90s stale |
| KMEQMPScrapeErrors | warning | Sustained QMP errors for 15m |
| KMEQGAScrapeErrors | warning | Sustained QGA errors for 15m |
| KMEKVMScrapeErrors | warning | Sustained KVM errors for 15m |
| KMECgroupScrapeErrors | warning | Sustained cgroup errors for 15m |
| KMEeBPFSubsystemDown | warning | Block eBPF subsystem down for 10m |
| KMEAbsent | critical | No metrics scraped for 10m |

The `KMEAbsent` alert uses the Prometheus `up` metric with `job="kubevirt-metrics-exporter"`. If your PodMonitor uses a different job name, update the alert expression to match.

QMP histogram latency values reported in alert annotations are approximate due to the default histogram bucket granularity (10ms, 100ms, 1s). For higher precision, configure finer-grained boundaries via `--boundaries`.

## Building

Prerequisites: Go 1.25+, clang, llvm, libbpf-devel

```bash
make build        # generates eBPF bindings and builds the binary (requires clang/llvm)
make test         # runs all tests (requires generated eBPF bindings)
make test-unit    # runs unit tests for non-eBPF packages (works on macOS)
make image        # builds container image with podman (includes full toolchain)
```

To build and push a custom image:

```bash
make push IMAGE=quay.io/myuser/kubevirt-metrics-exporter TAG=v0.1.0
```

## Deploying

### From a release

Download the install manifest from the [latest release](https://github.com/openshift-virtualization/kubevirt-metrics-exporter/releases/latest):

OpenShift:

```bash
oc apply -f https://github.com/openshift-virtualization/kubevirt-metrics-exporter/releases/latest/download/install-openshift.yaml
```

Kubernetes:

```bash
kubectl apply -f https://github.com/openshift-virtualization/kubevirt-metrics-exporter/releases/latest/download/install-kubernetes.yaml
```

### From source

OpenShift:

```bash
make deploy
```

Kubernetes:

```bash
make deploy-kubernetes
```

To deploy with a custom image:

```bash
make deploy IMAGE=quay.io/myuser/kubevirt-metrics-exporter TAG=v0.1.0
```

The OpenShift variant includes SecurityContextConstraints, worker node selector, and PodMonitor for Prometheus scraping.

### Required capabilities

| Capability | Reason |
|-----------|--------|
| `hostPID` | Access VM virtqemud sockets via `/proc/<pid>/root/`; read host `/proc/buddyinfo`, `/proc/pagetypeinfo`, and `/proc/vmstat` for node memory metrics |
| `SYS_PTRACE` | Traverse `/proc/<pid>/root/` of other containers |
| `DAC_OVERRIDE` | Connect to virtqemud socket owned by qemu UID; read host proc nodes that restrict unprivileged access |
| `BPF` | Load and attach eBPF programs |
| `PERFMON` | Attach to kernel tracepoints and kprobes |
| `SYS_RESOURCE` | Increase eBPF map memory limits |
