# tar vs erofs vs tarfs for durable dirs — 2026-09-04

A one-page comparison of the three ways a restored durable-dir volume can be
made to appear on a worker. Full runs:
[erofs](erofs-durable-dir-2026-08-31.md),
[the erofs re-measurement](erofs-summary-2026-09-03.md),
[tarfs](tarfs-durable-dir-2026-09-04.md).

All figures at 1 GiB, the only size where the three differ enough to choose
between them.

## The three arrangements

| | what a suspend writes | what a restore does |
|---|---|---|
| **tar** (today) | a tar of the merged tree | unpacks it, file by file |
| **erofs image** | an erofs image, built by `mkfs.erofs` | loop-mounts it as an overlay lower |
| **tarfs** | a tar — unchanged | builds a 1.5 KiB index over the tar and mounts the pair |

tarfs is the erofs arrangement without the format change: the wire format stays
a tar, and `mkfs.erofs --tar=i` writes only an inode table whose data offsets
point into the tar itself.

## Latency

Each arm is quoted against **its own** interleaved tar control, because the two
experiments ran on different clusters and the tar controls differ (`land` 615 ms
against 1 524 ms). The Δ is comparable; the absolute numbers are not.

### Restore, server-side (ms)

| | tar | erofs | Δ | tar | tarfs | Δ |
|---|---|---|---|---|---|---|
| `land` | 615 | 0.09 | −615 | 1 524 | 3.2 | −1 521 |
| restore total | 1 853 | 976 | **−877** | 2 736 | 1 021 | **−1 715** |

### Suspend, server-side (ms)

| | tar | erofs | Δ | tar | tarfs | Δ |
|---|---|---|---|---|---|---|
| `durable_dir` | 6 417 | 6 326 | −91 | 6 498 | 8 575 | **+2 077** |
| `teardown` | 184 | 2 938 | **+2 755** | 222 | 380 | +158 |
| sum | 6 601 | 9 264 | **+2 663** | 6 720 | 8 955 | **+2 235** |

The erofs suspend figures are the 2026-09-03 re-measurement, which is the
trustworthy one: the 2026-08-31 run charged +3 968 ms to `durable_dir` and made
`mkfs.erofs` look slow, and it is not — interleaved, it is at parity with `tar`
and the same regression appears in `teardown` instead. The erofs restore figures
have no equivalent re-measurement; they are from 2026-08-31.

**The phase a penalty lands in is not stable across runs, and neither format's
penalty is where it first appears to be.** Compare the sums, not the phases.

### Client-observed (p50, ms)

| | tar | erofs | tar | tarfs |
|---|---|---|---|---|
| ResumeActor | 5 100 | 3 600 (−29%) | 6 500 | 4 300 (**−34%**) |
| SuspendActor | 11 000 | 15 000 (+36%) | 11 000 | 13 000 (+18%) |
| ServeAfterResume | 12 000 | 12 000 | 11 000 | 11 000 |
| ServeWarm | 4 300 | 4 200 | 4 400 | 4 400 |
| Overwrite | 12 000 | 7 000 (−42%) | 11 000 | 8 400 (−24%) |

**Reads are identical in all three.** Neither mount adds a read penalty, warm or
cold, which is the precondition for either being worth anything.

The client numbers carry ~3.3–3.8 s of GCS transfer and control-plane round trip
that no format changes, so the percentages understate the effect on the part
that was actually changed.

## Disk traffic: the measurement that settles it

`/proc/diskstats` on the microvm node, sampled once a second across one
interleaved round of each arm (2026-09-04, `sda`, 5m22s per arm).

| per suspend/resume cycle | tar | tarfs |
|---|---|---|
| written | 4 463 MiB | **3 260 MiB** |
| read | 0 | 0 |
| cycles in 5m22s | 6 | **7** |

**tarfs removes one of the four gibibyte writes per cycle.** The four are: the
downloaded tar landing on disk, the unpack, the guest's overwrite, and the new
archive. tarfs mounts the tar it already wrote to disk instead of unpacking it,
so the second write disappears and the cycle costs 3× the volume instead of 4×.
That is a 27% cut in the node's disk traffic, and it is why the same 5 minutes
fit 7 cycles instead of 6.

**Reads are zero.** Every source is already in page cache on a 16 GiB node, so
the volume is purely write-bound and every explanation involving cold reads,
cache eviction or overlay read overhead is excluded.

## Why `durable_dir` is 2 s slower on tarfs

It is not `tar` doing more work. Sectors written on `sda` during each
`durable_dir` window, same capture:

| | window length | written in it | rate |
|---|---|---|---|
| tar | 6.5 s | 1 055 MiB | 162 MB/s |
| tarfs | 8.6 s | 1 397 MiB | 162 MB/s |

**Both arms run the archive step at exactly the volume's line rate.** The step
is longer on tarfs because 342 MiB more passes through the window, not because
anything in it is slower. The archive itself is the same gibibyte in both.

The extra 342 MiB is writeback the guest's `DurDirOverwrite` deferred. That
call returns 2 600 ms earlier on tarfs (8 400 against 11 000 ms) with its pages
still dirty, and the archive's `fsync` is the next thing that has to wait for
them. The two move together:

| | Overwrite | `durable_dir` | sum |
|---|---|---|---|
| tar | 11 000 | 6 498 | 17 498 |
| tarfs | 8 400 | 8 575 | 16 975 |

Within 3%. **The +2 077 ms on `durable_dir` is not new work; it is the same
bytes, billed to a later phase.** This is the same mechanism the erofs
re-measurement identified as the cause of its +2 755 ms `teardown`: a volume at
156–162 MB/s, and whichever syscall comes next after a burst waits for the
backlog. The `durable_dir` / `teardown` split between the two experiments is
which syscall that happened to be.

The unexplained residue is why `DurDirOverwrite` returns faster through an
overlay at all — 24% on tarfs, 42% on erofs. An overwrite through an overlay
has to copy up from the lower, which should be slower. It reproduces in both
experiments and at every large size, and neither run identifies the mechanism.
It should not be quoted as a benefit until it is.

## Engineering properties

| | tar | erofs image | tarfs |
|---|---|---|---|
| wire format | tar | **erofs** | tar |
| rollback | — | asymmetric, fleet-wide: an old worker cannot read a new snapshot | **symmetric, node-local**: flip an env var |
| migration | — | needs `Sniff` and a full fleet roll | none |
| extra work on suspend | — | rebuild the whole image | none — both arms run the same `tar` |
| `ate.dev/loop` per worker | 0 | 1 | **2** (index + data) |
| workers per node serving this way | unbounded | 8 | **4** (`max_loop` is 8) |
| kernel floor | — | any erofs | **≥ 6.4** (`-b 512`) |
| steady-state disk | 1× | 2× | 2× (the index is 1.5 KiB) |

## Recommendation

**tarfs.** It is better than the erofs image on every axis that was measured
except loop-device consumption:

- larger restore win (−1 715 ms server-side against −877 ms)
- smaller suspend penalty (+2 235 ms against +2 663 ms)
- less disk traffic, which is the actual bottleneck: 3× the volume per cycle
  against tar's 4×, where erofs also writes a fresh gibibyte-scale image every
  suspend
- no format migration, and rollback is one env var on one node

**Against plain tar it is a net win on this workload**: 7 cycles per 5m22s
against 6, with the same load generator and think time.

The cost to accept is two loop devices per worker, halving the per-node ceiling
from 8 to 4, and a kernel floor of 6.4.

## Not confirmed

- **The 2 s is deferred writeback, not extra work.** Strongly supported —
  identical 162 MB/s rate, 342 MiB of extra bytes in the window, and an
  offsetting −2 600 ms on the preceding call — but the dirty-page accounting was
  not observed directly (`/proc/meminfo` `Dirty` was not sampled).
- **Why the overlay makes an overwrite faster.** Reproduces in both experiments;
  unexplained in both.
- **erofs's disk traffic per cycle** was never measured with diskstats, so the
  three-way disk comparison above has two arms, not three.
- **Sizes other than 1 GiB.** Below a few hundred mebibytes the VMM launch
  dominates and there is nothing to win; above 1 GiB nothing was measured.
