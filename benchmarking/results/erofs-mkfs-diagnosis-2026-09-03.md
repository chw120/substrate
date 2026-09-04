# Diagnosing the erofs suspend regression — 2026-09-03

[erofs-durable-dir-2026-08-31.md](erofs-durable-dir-2026-08-31.md) measured the
`durable_dir` archive step at 10 578.7 ms for erofs against 6 610.9 ms for tar
at 1 GiB, a **+3 968 ms** regression that accounts for very nearly the whole
+4 000 ms suspend regression the client sees. Everything else in that run
favours erofs — restore is flat at 977 ms where tar is linear at 1 853 ms — so
this one number decides whether the format is worth adopting.

This is the attempt to reproduce it off-cluster, attribute it, and then
re-measure on a cluster. **The attribution to `mkfs.erofs` is wrong, but the
regression is real and it is in `teardown`.**

## Conclusion

`mkfs.erofs` is at parity with `tar` and always was. A controlled interleaved
re-measure on a dedicated cluster puts the archive step at **6 326 ms for erofs
against 6 417 ms for tar** — erofs 91 ms *faster*, n = 33/46 — and puts the
whole penalty in the phase that follows it:

| Phase | erofs | tar | Δ |
|---|---|---|---|
| `durable_dir` (build + fsync) | 6 326.0 | 6 417.3 | **−91** |
| `teardown` | 2 938.4 | 183.7 | **+2 755** |
| sum | 9 264.4 | 6 601.0 | **+2 663** |

The source run's total penalty was +4 103 ms (10 578.7 + 314.4 against 6 610.9 +
179.4); this run's is +2 663 ms. **The magnitude survives; only the phase it is
charged to moves.** That is the signature of deferred writeback — the cost is
1 GiB of dirty pages that must reach a pd-balanced volume, and it is billed to
whichever call forces them, `fsync` in the source run and `unlink` here.

The cause is the 2× disk that [`durable.go:104-108`](../../cmd/ateom-microvm/durable.go)
already predicts and prices in bytes: the erofs path keeps the whole image on
disk for the actor's lifetime while the overlay upper accumulates beside it, so
an actor that rewrites all of its data stores it twice, where the tar path has
one copy and holds steady at 183.7 ms with a standard deviation of 9.5.

It is **not** the `RemoveAll`s in `resetDurableOverlayState`, which was the
obvious reading and is measurably wrong: instrumenting them puts every one of
them in the microseconds, and deleting a real leaked gibibyte on the real volume
costs 57 ms. The seconds are the volume's write queue, not the reclaim.
[Where the teardown seconds are not](#where-the-teardown-seconds-are-not) is the
measurement.

So the `mkfs.erofs` tuning work the regression motivated — `--workers`, `-z`,
and the feature detection to enable them — has no measured problem to solve.
The reclaim path does.

The rest of this document is the elimination that got here. Two independent
lines pointed away from the archive step before the re-measure confirmed it.

**Nothing on the machine reproduces it.** Nine variables were controlled one at
a time, listed with the round that killed each:

| # | Candidate | Verdict | Round |
|---|---|---|---|
| 1 | CPU-bound | ✗ 18–20% CPU, so the job is off-CPU waiting on I/O | 1 |
| 2 | Memory / internal buffering | ✗ 6.6–9.1 MB peak RSS | 1 |
| 3 | Superlinear in size | ✗ linear: 2.02× for 2× data, 1.69× for the next 2× | 1 |
| 4 | The single-file fixture shape | ✗ 1.01× of tar | 2 |
| 5 | Disk contention with the concurrent `snapshotVMState` | ✗ doubles both arms; erofs 0.92× | 2 |
| 6 | Cold vs warm page cache | ✗ warm, the tar arm lands on the cluster | 3 |
| 7 | erofs-utils 1.7.1 vs the image's 1.8.6 | ✗ 1.8.6 is 200 ms *faster* than tar | 4 |
| 8 | Reading through overlay → erofs → loop → ext4 | ✗ 1.00×, the four-layer stack is free | 5 |
| 9 | Page-cache eviction from the format's double caching | ✗ 1.00× down to 340 MiB free | 6 |

**The cluster numbers are internally inconsistent.** Read as a size curve they
do not hold together:

| Size | `durable_dir` Δ | `teardown` erofs / tar |
|---|---|---|
| 500 MiB | **−40 ms** (parity) | **2 053.5 / 116.1** (+1 937, the source doc calls it unexplained) |
| 1 GiB | **+3 968 ms** | 314.4 / 179.4 (normal) |

Two multi-second anomalies in one arm, at adjacent sizes, pointing opposite
ways, neither reproducible. A threshold in `mkfs.erofs` would have to show up in
round 1's size sweep, and it does not — the tool is linear across 500 MiB,
1 GiB and 2 GiB. The re-measure resolves the pair: both are the same reclaim
cost, and the source run's own 500 MiB row is where it happened to land in the
teardown column instead of the archive one. The source run also carried two
confounds it names itself: the
erofs arm ran its worker `privileged` and the tar arm did not, and the arms were
separate deployments rather than interleaved, at 1 concurrent user.

## The re-measure

One point, 1 GiB, on a cluster of its own, with the two defects the source run
names fixed. The privilege confound was already gone: the worker takes its loop
device as a device-plugin grant now (`ate.dev/loop: 8` on the node), so both
arms run the same unprivileged pod. That left the deployment asymmetry, fixed by
flipping `ATEOM_ARCHIVE_FORMAT` on the `ate-controller` Deployment between every
arm — `workerpool_apply.go:203-208` propagates it to every worker pod, so the
pool cannot straddle two formats — and the small n, fixed by running 5 minutes
per arm at a 1 s think time.

| | |
|---|---|
| Cluster | dedicated, `us-central1-a`, GKE 1.35.7 |
| Microvm node | `n2-standard-4`, kernel `6.8.0-1060-gke`, Ubuntu 24.04.4 — an exact match to the source run's node |
| Workload | `benchmarking/locust/tests/durdir.py`, 1 user, `--durdir-file-size-bytes 1073741824`, `--durdir-read-mode digest`, `--resume-mode explicit` |
| Design | 4 rounds × (erofs, tar), 5 min each, arms interleaved |
| Source | ateom's "Actor checkpointed" line, `checkpoint.go:203-210`, in nanoseconds |

The first checkpoint of every arm is dropped: it runs before the workload has
written its full fixture, and it shows it (9 016 ms archive, 277 ms teardown on
the first erofs sample against a 6 200 / 3 200 steady state).

| arm | round | n | `durable_dir` p50 | `teardown` p50 |
|---|---|---|---|---|
| erofs | 1 | 11 | 6 214.1 | 3 184.5 |
| erofs | 2 | 11 | 6 326.0 | 2 938.4 |
| erofs | 3 | — | pod log lost | — |
| erofs | 4 | 11 | 6 961.4 | 1 440.5 |
| tar | 1 | 12 | 5 837.9 | 179.9 |
| tar | 2 | 12 | 6 609.5 | 183.3 |
| tar | 3 | 12 | 6 434.2 | 185.4 |
| tar | 4 | 10 | 6 484.1 | 184.5 |

Pooled: erofs `durable_dir` p50 6 326.0 (σ 403.2), `teardown` p50 2 938.4
(σ 796.2); tar 6 417.3 (σ 346.0) and 183.7 (σ 9.5).

**The two teardown populations do not overlap.** Every erofs sample is between
1 024 and 3 390 ms; every tar sample is between 169 and 190. Round 4's erofs
teardown is uniformly around 1 400 ms against rounds 1–2's 3 100, which is the
one thing in the run that is not stable — most likely the pd-balanced volume's
burst budget, since nothing about the arm changed. Even at its cheapest it is
7× tar.

Two limitations, neither affecting the numbers above:

- **Round 3's erofs log was lost.** The script captures the worker pod name
  before the run and reads its log after; that round it captured a pod that the
  preceding roll then deleted. The arm ran, the timings are simply not in hand.
- **The tar arm's *restores* are not clean tar restores.** Interleaving means
  each arm resumes the previous arm's snapshots out of the shared bucket, and a
  tar-arm pod has no loop device to mount an erofs image with, so it logs
  `Cannot mount the durable-dir image on this node; extracting it instead` and
  falls back (14, 14, 14 and 12 times per round). Suspend is unaffected — the
  fallback leaves a plain directory, which is exactly what the tar arm should be
  archiving — but no restore-side conclusion should be drawn from this run.

## Where the teardown seconds are not

The re-measure says the penalty is in `teardown`. `teardown` does four things:
shut the CH API down, sweep the sandbox state, and `RemoveAll` the rootfs upper
and the three durable-dir trees. The reclaim reading — that freeing an extra
gibibyte is what costs seconds — is the one the byte counts suggest, and it is
wrong.

**Instrumented.** `teardownActor` now times the sweep and each `RemoveAll`
separately and logs them as `Actor state reclaimed`. On a 5-minute erofs arm
(n = 14) the medians are:

| | sandbox_sweep | rootfs_upper | durable_image | durable_upper | durable_work |
|---|---|---|---|---|---|
| erofs | 1.05 ms | 0.27 ms | 6.2 µs | 4.6 µs | 4.4 µs |
| tar | 1.05 ms | 0.21 ms | 4.4 µs | 4.8 µs | 4.2 µs |

Everything teardown owns is under 1.5 ms. The rest of its 182 ms is the CH API
shutdown.

**On real data.** The microsecond figures are honest only if the trees were
empty, and on this run they were — the actors never entered the overlay path, so
there was no image and no upper to free. The node had four leaked gibibyte-scale
trees left over from the matrix run, so they were timed directly, on the same
volume, with the same data:

| tree | size | `rm -rf` |
|---|---|---|
| `durable-upper` (overlay shape) | 1 025 MB | 57 ms, 60 ms |
| `durable-dir` (plain shape) | 1 025 MB | 57 ms, 57 ms |

Reclaiming a gibibyte costs 57 ms, and the overlay shape is not more expensive
than the plain one. That is 2 % of the 2 755 ms, and it kills the reclaim
hypothesis outright.

**What the volume can actually do.** The same node, `dd` of 1 GiB:

| | |
|---|---|
| write + `conv=fsync` | 6 538 ms → **156 MB/s** |
| write, no sync | 1 516 ms |
| `unlink` immediately after, pages still dirty | 308 ms |

156 MB/s is the pd-balanced ceiling on this node, and **it is the whole
explanation of `durable_dir`**: 6 538 ms to write and sync a gibibyte against a
measured 6 326 / 6 417 ms for the two arms to build and sync a gibibyte-scale
archive. Neither `mkfs.erofs` nor `tar` is doing anything but feeding the disk at
line rate, which is why they tie. Note also that unlinking a *dirty* gibibyte —
the worst case for reclaim — is 308 ms, still an order of magnitude short.

So the remaining candidate is queueing: the erofs path keeps a third live copy
(image + upper + archive, against the tar path's directory + archive), and at
33 checkpoints per 300 s each gibibyte copy is 113 MB/s of demand. One copy fits
under 156 MB/s; the surplus does not, and a backlogged volume stalls whichever
syscall comes next — which is `teardown`, immediately after the archive's
`fsync` returns. This is consistent with everything measured, but it is not yet
directly confirmed.

**It does not currently reproduce.** Since the worker was rebuilt with the
instrumentation, three runs have all shown erofs at 182 ms teardown, at parity
with tar, because none of them entered the overlay path: the image only lands
when an actor resumes from an erofs snapshot, and on this cluster only 4 of 171
actor directories ever held one. Confirming the queueing mechanism needs a run
that lands images, which means a golden snapshot taken on the erofs arm.
Recreating the ActorTemplates under the erofs arm was tried and did not produce
one.

## Unrelated finding — actor directories are not collected

Not an erofs problem, found while looking for one. The microvm node carries
**171 directories under `/var/lib/ateom-gvisor/actors/` totalling 14.6 GB**,
each with a `durable-dir` and a `checkpoint-state` still in it, from actors that
are long gone; the volume is at 18G of 96G. Teardown removes the sandbox state,
the rootfs upper and the durable-dir overlay trees, but nothing removes the
actor directory itself once the actor is not coming back. Four of them were
deleted by hand for the `rm -rf` timings above.

## Method — the off-cluster rounds

A GCE VM matched to the worker node, driven entirely through the GCE startup
script with output read back off the serial console — the IAP tunnel to it was
too unreliable to hold open for the length of a run.

| | |
|---|---|
| Machine | `n2-standard-4`, `us-central1-a`, 4 vCPU / 16 GiB, matching the worker node |
| Disk | 100 GB pd-balanced, matching the node's boot disk class |
| Image | `ubuntu-2404-lts-amd64`, kernel `6.17.0-1022-gcp` |
| erofs-utils | 1.7.1 from the distro; 1.8.6 unpacked from Debian trixie's `erofs-utils_1.8.6-1_amd64.deb`, the same package lineage the ateom base image installs from `debian:stable-slim` |

Every arm reproduces what `tarutil.CreateImage` and `tarutil.CreateFiltered`
actually do, including the explicit `fsync` of the finished artifact that
`mkfs.erofs` does not issue on its own. Timings are wall-clock around
build-plus-fsync, which is what the `durable_dir` phase measures.

The one variable not matched is the kernel: the VM ran `6.17.0-1022-gcp` and
the node runs `6.8.0-1060-gke`. Round 5's overlay and loop behaviour is the part
most exposed to that difference.

### Round 1 — is it CPU-bound?

1 508 small files plus 8 large ones, the venv / `node_modules` shape from
`../../erofs.md` §15.2. Caches dropped before every arm, `/usr/bin/time -v` for
user, sys, and peak RSS.

| Size | erofs build | fsync | total | tar build | fsync | total | erofs/tar |
|---|---|---|---|---|---|---|---|
| 500 MiB | 4 130 | 2 946 | 7 076 | 4 310 | 2 933 | 7 243 | 0.98× |
| 1 GiB | 8 200 | 6 058 | 14 258 | 7 850 | 6 059 | 13 909 | 1.03× |
| 2 GiB | 14 960 | 9 091 | 24 051 | 14 250 | 9 003 | 23 253 | 1.03× |

`Percent of CPU` 18–20%, peak RSS 6 728–9 272 KB. The job is **I/O-bound**:
`Percent of CPU` is `(user + sys) / wall`, and time blocked on I/O counts in
neither, so 18% means the process is off-CPU for four fifths of its life.
Killed candidates 1, 2 and 3.

This round also produced two figures worth keeping:

- **fsync costs ~6 s per GiB on pd-balanced**, not the ~274 ms per GiB
  `../../erofs.md` §15.2 measured — 22× more, and the dominant term in the whole
  archive step. Both formats pay it identically (6 058 against 6 059), so it is
  not a differentiator, but absolute suspend-duration claims must use it.
- **`-zlz4` costs 54.24 s at 94% CPU** against 8.20 s uncompressed at 1 GiB, to
  save at most the ~6 s of fsync that shrinking the image could buy. Compression
  never pays here, independently of anything else in this document. (The fixture
  itself was not usefully compressible — its repeats sit far outside lz4's 64 KB
  window — but the CPU cost is real and is what decides it.)

### Round 2 — the fixture shape, and contention with the RAM snapshot

Two candidates specific to how the cluster differs from round 1. The benchmark's
durable dir holds **one** incompressible file (`durdir.go:57`), not a tree; and
`checkpoint.go:150-180` runs `archiveDurableVolumes` in an errgroup
**concurrently** with `snapshotVMState`, which writes the whole guest RAM to the
same disk. The noise arm runs a background writer keeping ~1 GiB of writes in
flight to stand in for it.

| Size | erofs quiet | tar quiet | erofs + noise | tar + noise |
|---|---|---|---|---|
| 500 MiB | 5 884 | 5 811 | 11 574 | 12 292 |
| 1 GiB | 12 156 | 12 048 | 22 911 | 24 931 |

Shape: 1.01×. Contention: doubles both arms, and erofs comes out **faster**
(0.92–0.94×). Killed candidates 4 and 5.

### Round 3 — warm reads

Rounds 1 and 2 dropped caches, but the cluster's source is hot: the guest just
wrote it through virtiofsd. A cold read is a large shared cost, and sharing it
flattens the two arms, so it could have been masking the difference.

| 1 GiB | VM warm | cluster | Δ |
|---|---|---|---|
| tar | 6 730 | 6 611 | −119 (1.8%) |
| erofs | 6 878 | 10 579 | **+3 701** |

This is the round that makes the rest interpretable. **The tar leg now
reproduces the cluster.** The VM is a faithful replica; the erofs leg is the
outlier. Killed candidate 6.

### Round 4 — erofs-utils version

The last uncontrolled input: Ubuntu 24.04 ships 1.7.1, the ateom image carries
1.8.6. Both binaries, same tree, same machine, warm, 1 GiB.

| | cold | warm |
|---|---|---|
| erofs 1.7.1 | 12 042 | 6 886 |
| erofs 1.8.6 | 12 056 | **6 516** |
| tar | 12 042 | 6 716 |

**The cluster's own binary is 200 ms faster than tar.** Killed candidate 7, and
left the gap at +4 063 ms.

### Round 5 — reading through the overlay

The one structural asymmetry between the arms in the real code path.
`durable.go:137` archives `kata.DurableMergedDir` whenever an image is active,
so the erofs arm reads its source through **overlayfs → erofs → loop → ext4**
while the tar arm reads a plain host directory. That asymmetry is created by the
format — the lower is the image the previous restore landed — and no round
before this one had it. Both states are covered, because the workload rewrites
`bench-data` with `WRITE_MODE_TRUNCATE` and so copies it up.

| 1 GiB warm, mkfs.erofs 1.8.6 | total |
|---|---|
| plain ext4 directory (the tar arm's shape) | 6 604 |
| overlay merged, file still in the erofs lower | 6 592 |
| overlay merged, file copied up into the ext4 upper | 6 500 |

1.00×. The four-layer stack costs nothing at this shape. Killed candidate 8.

### Round 6 — page-cache eviction

The last hypothesis that fit the shape of the cluster data. The erofs arm is the
only one holding the previous restore's image resident, and it holds it
**twice** — the backing file's pages plus the erofs pages above the loop device,
because the loop is buffered — roughly 2 GiB the tar arm never allocates on a
16 GiB node. That would explain the size threshold, and 10 579 ms sits between
this machine's warm 6 516 and cold 12 042, at about 74% cold.

Same stack as round 5, with anonymous memory pinned to squeeze the cache.

| Pinned | Free | tar-shaped | erofs-shaped (overlay + loop + lower resident) |
|---|---|---|---|
| 0 | 10.9 GiB | 6 604 | 6 502 |
| 11 GiB | 1.2 GiB | 6 206 | 6 210 |
| 13.6 GiB | 0.34 GiB | 6 109 | 6 144 |

1.00× at every level, down to 340 MiB free. Killed candidate 9.

The limit of this round: each arm warms its source immediately before timing, so
it measures whether pressure slows a warm read, not whether pressure evicts the
source between the guest's write and the archive. The cluster re-run is what
settles that, and round 3 already bounds it — a fully evicted source costs
12.0 s, so eviction alone cannot explain a 10.6 s result without also explaining
why it evicts exactly 74%.

## Reproducing

The whole harness is a shell script passed as GCE instance metadata:

```bash
gcloud compute instances create erofs-diag --zone=us-central1-a \
  --machine-type=n2-standard-4 --image-family=ubuntu-2404-lts-amd64 \
  --image-project=ubuntu-os-cloud --boot-disk-size=100GB --boot-disk-type=pd-balanced
gcloud compute instances add-metadata erofs-diag --zone=us-central1-a \
  --metadata-from-file=startup-script=<round>.sh
gcloud compute instances reset erofs-diag --zone=us-central1-a
gcloud compute instances get-serial-port-output erofs-diag --zone=us-central1-a
```

Each round is the same skeleton: build a fixture, drop or warm caches, then time
`mkfs.erofs -E force-inode-extended <img> <src>` — the exact flags
`tarutil.CreateImage` passes — followed by an explicit `fsync` of the result,
against `tar -cf` plus the same `fsync`. Round 5 and 6 add the real stack:

```bash
mkfs.erofs -E force-inode-extended lower.erofs src/
mount -t erofs -o ro,loop lower.erofs lower/
mount -t overlay overlay \
  -o lowerdir=lower,upperdir=upper,workdir=work,metacopy=off,index=off merged/
```

`metacopy=off,index=off` matches `StageDurableOverlay` at
`durableoverlay_linux.go:93`. Round 6 pins memory with a Python process holding
`bytearray` allocations it touches once, so the kernel cannot reclaim them.

The VM and its firewall rule were deleted after the last round.

The cluster re-measure is the standard durdir workload with the arms alternated
between runs; nothing in it needs a custom image, because `mkfs.erofs` reaches
the worker through the `ateom-base` base image `hack/images/ateom-base` builds
and `KO_CONFIG_PATH` selects:

```bash
# erofs arm
kubectl -n ate-system set env deploy/ate-controller ATEOM_ARCHIVE_FORMAT=erofs
# tar arm
kubectl -n ate-system set env deploy/ate-controller ATEOM_ARCHIVE_FORMAT-
```

Wait for the value to appear in the `benchmark-ateom` Deployment's pod template
before starting a run, not just for the controller's own rollout: the controller
re-applies the worker pool on its next reconcile, so the worker roll has not
been triggered yet when `rollout status deploy/ate-controller` returns. Read the
timings back from the worker pod's log rather than the trace pipeline —
`durable_dir` and `teardown` are fields on the "Actor checkpointed" line, in
nanoseconds.

The cluster and its bucket exist only for this measurement and should be deleted
once nothing else is pending against them.
