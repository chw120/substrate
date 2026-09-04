# erofs vs tar for durable-dir — what we found — 2026-09-03

A one-page summary. The full elimination is in
[erofs-mkfs-diagnosis-2026-09-03.md](erofs-mkfs-diagnosis-2026-09-03.md).

## What we were testing

Whether erofs should replace tar as the durable-dir archive format. erofs wins
on restore — it is mounted, not extracted, so restore is flat at 977 ms where
tar is linear and hits 1 853 ms at 1 GiB. The question was what it costs on
suspend.

## 1. The first benchmark said: erofs is 4 s slower, and it is `mkfs.erofs`

At 1 GiB, suspend was +4 000 ms on erofs, and essentially all of it landed in
the `durable_dir` archive step:

| Phase | erofs | tar | Δ |
|---|---|---|---|
| `durable_dir` | 10 578.7 | 6 610.9 | **+3 968** |
| `teardown` | 314.4 | 179.4 | +135 |

Read at face value this says `mkfs.erofs` is slow, so the follow-up work was
going to be tuning it: `--workers`, compression level `-z`, and the feature
detection to turn them on safely.

## 2. Digging: the tuning target does not exist

Nine candidates were tested one at a time off-cluster. None reproduced the gap —
`mkfs.erofs` came out at 1.00–1.02× of tar every time:

CPU-bound (18–20% CPU, so it is waiting on I/O), memory (6.6–9.1 MB peak RSS),
superlinear in size (linear), fixture shape, contention with the concurrent RAM
snapshot, cold vs warm page cache, erofs-utils version, reading through the
four-layer overlay→erofs→loop→ext4 stack, and page-cache eviction.

The original numbers were also internally inconsistent: at 500 MiB the archive
step was at parity and it was *teardown* that showed +1 937 ms, which the source
doc itself calls unexplained. Two multi-second anomalies at adjacent sizes,
pointing opposite ways.

## 3. Re-measured properly

A dedicated cluster, one size (1 GiB), 4 rounds × (erofs, tar) × 5 min,
**arms interleaved** by flipping `ATEOM_ARCHIVE_FORMAT` on the controller
between every arm. This fixes the two confounds the source run named: it ran the
arms as separate deployments, and its erofs worker was `privileged` while its tar
worker was not.

| Phase | erofs | tar | Δ |
|---|---|---|---|
| `durable_dir` | 6 326.0 | 6 417.3 | **−91** |
| `teardown` | 2 938.4 | 183.7 | **+2 755** |
| sum | 9 264.4 | 6 601.0 | +2 663 |

n = 33 erofs / 46 tar. The teardown populations do not overlap at all: erofs
1 024–3 390 ms, tar 169–190 ms.

**`mkfs.erofs` is at parity and always was. The magnitude of the regression
survives; the phase it is charged to moves.** The tuning work was cancelled.

## 4. Digging again: it is not the reclaim either

`teardown` frees the actor's state, and the erofs path has more to free (the
image plus the overlay upper, against the tar path's one directory). That is the
obvious reading. It is wrong.

**Instrumented each `RemoveAll` separately** — medians over n = 14:

| sandbox_sweep | rootfs_upper | durable_image | durable_upper | durable_work |
|---|---|---|---|---|
| 1.05 ms | 0.27 ms | 6.2 µs | 4.6 µs | 4.4 µs |

**Timed a real gibibyte** on the same volume, using trees left over from the
matrix run:

| tree | size | `rm -rf` |
|---|---|---|
| `durable-upper` (overlay shape) | 1 025 MB | 57 ms, 60 ms |
| `durable-dir` (plain shape) | 1 025 MB | 57 ms, 57 ms |

Freeing a gibibyte costs 57 ms — 2% of 2 755 ms — and the overlay shape is no
more expensive than the plain one.

## 5. The bottleneck: the volume, at 156 MB/s

`dd` on the same node:

| | |
|---|---|
| write 1 GiB + `conv=fsync` | 6 538 ms → **156 MB/s** |
| write 1 GiB, no sync | 1 516 ms |
| `unlink` immediately after, pages still dirty | 308 ms |

6 538 ms to write and sync a gibibyte, against a measured 6 326 / 6 417 ms for
the two arms to build and sync a gibibyte-scale archive. **`durable_dir` is not
`mkfs.erofs` and not `tar` — it is the disk, at line rate, and that is why the
two formats tie.**

The erofs path then keeps **three** live gibibyte copies where tar keeps two:

| | copies on disk during a suspend |
|---|---|
| tar | `durable-dir/` + the tar archive |
| erofs | `durable-dir.erofs` (read-only lower, held for the actor's lifetime) + `durable-upper/` (overlayfs copies up **whole files**, so a rewrite duplicates the lower) + the new image |

At 33 checkpoints per 300 s, each gibibyte copy is 113 MB/s of demand. One copy
fits under 156 MB/s. The surplus does not, and a backlogged volume stalls
whichever syscall comes next — which is `teardown`, immediately after the
archive's `fsync` returns.

## Where this leaves it

- **`mkfs.erofs` tuning: cancelled.** There is no measured problem to solve.
- **Moving teardown off the critical path: cancelled.** Teardown does 1.5 ms of
  work; there is nothing to move, and deferring an unlink does not create
  bandwidth.
- **The fix is to stop writing the extra copy.** Either archive only what the
  upper actually changed (chunk-CAS), or replace erofs+overlay with qcow2 +
  backing file, whose CoW granularity is a 64 KiB cluster rather than a whole
  file. Both belong to those lines of work, not to the erofs line.
- **erofs as it stands is a net loss**: it buys ~880 ms on restore and costs
  2 663 ms on suspend.

**Not yet confirmed:** the queueing mechanism is consistent with every
measurement but has not been demonstrated directly. Two ways to close it — run
the matrix on a faster volume (pd-ssd) and see whether the 2 755 ms disappears,
or get a run that actually enters the overlay path, which needs a golden
snapshot taken on the erofs arm. Since the instrumented worker was deployed,
every run has shown erofs at parity with tar because no actor resumed from an
erofs snapshot.

## Unrelated finding

The microvm node holds **171 leaked directories under
`/var/lib/ateom-gvisor/actors/`, 14.6 GB**, each still containing a
`durable-dir` and a `checkpoint-state`. Teardown removes the sandbox state, the
rootfs upper and the durable-dir overlay trees, but nothing removes the actor
directory once the actor is gone.
