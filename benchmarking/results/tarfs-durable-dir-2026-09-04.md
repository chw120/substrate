# Durable-dir restores from a mounted tar vs an unpacked one — 2026-09-04

What serving a durable-dir volume by mounting the snapshot's tar through a
metadata-only erofs index costs and buys, against unpacking that same tar.
Both arms were measured on one tree and one cluster, interleaved run by run, so
the difference between them is the landing mode and nothing else.

Measured on branch `poc1-tarfs` at commit `285912f2`. The arms differ only in
`ATEOM_DURABLE_LANDING`: `tarfs` on one, unset (the default, extract) on the
other. Both arms write the same thing — an ordinary tar,
`ATEOM_ARCHIVE_FORMAT` unset — so the snapshot store is byte-identical between
them and the only question is what a restore does with the tar it downloaded.

This is the second of two arrangements measured against the same baseline. The
first, [an erofs image as the archive
format](erofs-durable-dir-2026-08-31.md), also removes the unpack, but pays for
it by rebuilding the image with `mkfs.erofs` on every suspend. tarfs keeps the
tar as the wire format and builds only an index over it.

## Environment

| | |
|---|---|
| Cluster | `erofs-rerun`, `us-central1-a`, project `chenyiwang-gke-dev` |
| Worker node | `n2-standard-4`, nested virtualization enabled, `ate.dev/sandboxClass=microvm` |
| Node kernel | `6.8.0-1060-gke`, Ubuntu 24.04.4 |
| erofs-utils | 1.8.6 (`mkfs.erofs --tar=i -b 512`) |
| Sandbox class | `microvm` (`ateom-microvm@sha256:d9953721`) |
| WorkerPool | `benchmark-ateom`, 1 replica |
| ActorTemplate | `glutton-durdir-data` — one `durableDir` volume; `onPause: Full`, `onCommit: Data`, `onResume.fromData: ColdBoot` |
| Load | 1 concurrent user, `--resume-mode explicit`, `--durdir-read-mode digest`, 1.0 s fixed wait, 1 GiB |
| Rounds | 4 per arm, 5m each, interleaved tarfs / extract |

Every checkpoint in the measured cycles is scope `DATA` and every resume is a
`DATA` cold boot — the path the durable-dir format exists for. Zero failures
across all eight runs, and zero fallbacks: no restore in the tarfs arm gave up
and unpacked.

### One harness change

The router's `--route-timeout` was raised from its 10 s default to 5m on this
cluster. At 1 GiB the initial `DurDirWrite` takes ~15 s, so at the default
every write 504'd, the load generator's bootstrap failed, and the cycle never
reached a suspend — the first attempt at this measurement produced four runs of
nothing but fresh actors resuming from the golden snapshot with an empty
durable dir. That is a property of the cluster, not of either arm, and it was
fixed before any figure below was taken.

## Client-observed latency

Milliseconds, median across the four rounds of each arm. **Δ** is tarfs against
extract; negative is faster.

| | extract p50 | tarfs p50 | Δ | extract p95 | tarfs p95 |
|---|---|---|---|---|---|
| ResumeActor | 6 500 | **4 300** | **−34%** | 11 000 | **5 200** |
| SuspendActor | 11 000 | 13 000 | **+18%** | 13 500 | 13 500 |
| DurDirServeAfterResume | 11 000 | 11 000 | — | 11 500 | 12 000 |
| DurDirServeWarm | 4 400 | 4 400 | — | 4 450 | 4 450 |
| DurDirOverwrite | 11 000 | **8 400** | **−24%** | 12 000 | 8 900 |

`ServeAfterResume` and `ServeWarm` are flat. Reading a gibibyte back through
the tarfs overlay costs exactly what reading it out of an unpacked tree costs
(~95 MB/s either way), so the mount adds no read penalty — which is the
precondition for the whole arrangement being worth anything.

**Resume tail improves more than the median.** p95 falls by more than half,
from 11 000 to 5 200 ms, because the unpack is the variable part: it competes
with whatever else the node is writing, and mounting does not.

## Server-side decomposition

From the worker's own log, all runs pooled. `land` is the phase this change
exists to remove: on the extract path it unpacks the archive, on the tarfs path
it adopts the tar and builds the index over it.

| | extract p50 | tarfs p50 | extract p95 | tarfs p95 | n (extract / tarfs) |
|---|---|---|---|---|---|
| `land` | 1 524.5 | **3.2** | 2 668.0 | **4.0** | 24 / 28 |
| restore total | 2 736.4 | **1 021.1** | 3 841.9 | **1 144.5** | 24 / 28 |
| `durable_dir` (checkpoint) | 6 498.2 | 8 575.4 | 8 114.2 | 9 581.3 | 28 / 32 |
| `teardown` (checkpoint) | 221.9 | 380.2 | 312.7 | 623.5 | 28 / 32 |

**`land` collapses by 480×** — 1 524 ms to 3.2 ms — and it is flat, because
nothing proportional to the volume happens: the index over the 1 GiB tar is
**1 536 bytes**. The server-side restore drops 1 715 ms, more than `land`
alone accounts for; the rest is the same knock-on the erofs run saw, where
writing a gibibyte back out contends with the VM starting alongside it.

**The cost is on the checkpoint side, and it is smaller than the erofs image's
was.** `durable_dir` is +2 077 ms and `teardown` is +158 ms, against
+3 968 ms and +135 ms for the erofs image at the same size. tarfs does not
rebuild anything on suspend — both arms run the same `tar` — so the +2 s is not
a tool cost. It is consistent with write-back pressure: the tarfs arm holds
three live copies of the actor's gibibyte (the tar lower, the overlay upper the
guest's overwrite copied up, and the new archive being written) on a volume
that sustains 156 MB/s, where the extract arm holds two. That mechanism is
inferred from the shape, not confirmed by measurement here.

## Throughput

Over four 5-minute runs each, tarfs completed **28 suspend/resume cycles** to
extract's **24** — 17% more work in the same wall clock, with the same load
generator and the same 1.0 s think time. That is the net of −2 200 ms on
resume, +2 000 ms on suspend and −2 600 ms on overwrite, and it is the number
to quote when asking whether the trade is worth taking: it is, on this workload.

## What this buys and what it costs

**Resume stops depending on durable-dir size.** 1 021 ms server-side at 1 GiB,
with `land` at 3.2 ms and an index of 1.5 KiB. As with the erofs image, there
is nothing to win below a few hundred mebibytes, where a near-constant VMM
launch already dominates.

**Nothing changes about the wire format.** The snapshot is still a tar, still
readable by a worker that has never heard of tarfs, so rollback is node-local
and symmetric: flipping `ATEOM_DURABLE_LANDING` off makes the next restore
unpack the same archives. This is the practical difference from the erofs
image, where the format is fleet-wide, rollback is asymmetric, and an old
worker cannot read a new snapshot at all.

**`DurDirOverwrite` is 24% faster** (8 400 against 11 000 ms), reproducing the
32–42% the erofs image showed. An overwrite through an overlay has to copy up
from the lower, which should be slower, not faster, and this run does not
explain it either. It should not be quoted as a benefit until the mechanism is
identified.

**The pod cost doubles against erofs.** tarfs mounts a pair of devices — the
index and the tar — so the worker requests **two** `ate.dev/loop` grants where
the erofs image requested one. `max_loop` is 8 on this node, so this halves the
ceiling on workers per node serving a durable dir this way, from 8 to 4. The
worker stays unprivileged: the devices arrive through the device plugin, the
same way `/dev/kvm` does.

**Steady-state disk is the same 2× the erofs image pays**, for the same reason:
the tar stays on disk for as long as the actor is resident and the overlay
upper accumulates alongside it. It is the same two copies, not three — the
index is 1.5 KiB.

**A leaked loop device is a node-wide fault, not an actor-local one.** The
index device is attached by `mount -o loop=`, which keeps `LO_FLAGS_AUTOCLEAR`
and releases itself; the tar's data device is attached by hand and has no such
guarantee, so teardown releases it explicitly, on a context detached from the
caller's cancellation. Losing that release would burn one of eight devices per
occurrence.

## Run index

All times UTC, 2026-09-04.

| Round | tarfs | extract |
|---|---|---|
| 1 | 05:54:25 | 05:59:56 |
| 2 | 06:05:26 | 06:10:55 |
| 3 | 06:16:15 | 06:21:45 |
| 4 | 06:27:25 | 06:33:00 |
