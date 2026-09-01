# qcow2 vs tar durable-dir arrangement — 2026-09-01

The DurDir size sweep run twice on the same cluster, once with the tar
arrangement and once with the qcow2 arrangement, to measure what Proposal 5
claims: that sealing a qcow2 backing-file chain takes constant time in the
paused window, where packing a tar grows with the size of the directory.

Measured on branch `poc5-qcow2` at commit `bd8ddbba`, which is the first commit
at which the qcow2 arrangement boots at all on cloud-hypervisor. The size steps
come from `durdir-large-sizes`, cherry-picked onto this branch as `97bed514`
and `aeb08aa6`; the tar arm here is a re-run of
[baseline-2026-08-26.md](baseline-2026-08-26.md) under this branch's durations,
not a reuse of its numbers.

To repeat this measurement, read [REPRODUCING.md](REPRODUCING.md), then the
[Separating the two arms](#separating-the-two-arms) section below, which is
specific to this comparison.

## Environment

| | |
|---|---|
| Cluster | `substrate-poc`, `us-central1-a`, project `chenyiwang-gke-dev` |
| GKE | `v1.35.7-gke.1222000` |
| Worker node | `n2-standard-4`, nested virtualization enabled, labeled `ate.dev/sandboxClass=microvm` and tainted `ate.dev/sandboxClass=microvm:NoSchedule` |
| Sandbox class | `microvm` (`ateom-microvm@sha256:515784be`) |
| WorkerPool | `benchmark-ateom`, 1 replica |
| ActorTemplate | `glutton-durdir-data` — one `durableDir` volume; `onPause: Full`, `onCommit: Data`, `onResume.fromData: ColdBoot` |
| Load | 1 concurrent user, `--resume-mode explicit`, `--durdir-read-mode digest`, 1.0 s fixed wait |
| Snapshot store | GCS, `snapshot-substrate-test-chenyiwang-gke-dev` |

Both arms ran back to back on this one cluster within 95 minutes, against the
same node, the same image, and the same scenario definitions. Only the two
environment variables in the next section differ.

## Separating the two arms

| | tar arm | qcow2 arm |
|---|---|---|
| `ATEOM_DURABLE_BACKEND` | unset | `qcow2` |
| `ATEOM_DURABLE_QCOW2_MAX_CHAIN` | unset | `1` |

`MAX_CHAIN=1` flattens the chain on every seal. Without it the seal cost is a
function of how deep the chain has grown, which is a property of the workload's
history rather than of the arrangement, and the two arms stop being comparable.

An `ActorTemplate`'s golden snapshot carries the arrangement it was built with,
and every actor cloned from it inherits that — ateom keeps an existing
arrangement rather than converting it under a live actor. Deleting the golden
*actor* is not enough: the snapshot ID outlives it on
`ActorTemplate.status.goldenSnapshot`. Each arm therefore deleted and recreated
`glutton-durdir-data` and waited for a new `goldenSnapshot` to reach
`phase: Ready` before its first scenario. The two arms are confirmed separate in
the logs: every checkpoint in the tar arm reports zero qcow2 layers, and every
checkpoint in the qcow2 arm reports two.

## Validity

| Scenario | tar | qcow2 |
|---|---|---|
| 5 MiB | 0 failures / 326 requests | 0 failures / 215 requests |
| 10 MiB | 0 / 296 | 0 / 185 |
| 64 MiB | 0 / 270 | 0 / 110 |
| 500 MiB | 0 / 125 | 0 / 81 |
| 1 GiB | **37 / 188 (19.7%)** | **43 / 218 (19.7%)** |

> [!WARNING]
> **The 1 GiB step measures nothing about the durable-dir arrangement in either
> arm.** Every `DurDirWrite` returned `HTTP 504: upstream request timeout`, and
> the reported latency of 10 000 ms at every percentile is the client's timeout
> ceiling, not a measurement. Neither arm recorded a single `SuspendActor`: the
> initial write never returned, so the cycle never reached a suspend, and the
> actor was torn down through `DeleteActor` instead. The `ResumeActor` figures
> for 1 GiB (770 ms tar, 1 000 ms qcow2) are resumes of an actor whose durable
> dir was never successfully filled, so they are fast for the wrong reason.
>
> The 1 GiB row is reproduced below for completeness and is excluded from every
> conclusion. The other four steps are clean in both arms.
>
> The cause is Envoy's route timeout, which defaults to 10 s and which
> `hack/install-ate.sh` had no way to raise, so the router on this cluster ran
> without `--route-timeout` at all. The script now takes `--route-timeout`; see
> [REPRODUCING.md](REPRODUCING.md).

## Results

### End-to-end, as reported by locust

All figures in milliseconds. `n` is the `SuspendActor` count, which is one full
cycle.

#### p50

| Size | Arm | ResumeActor | SuspendActor | ServeAfterResume | ServeWarm | Overwrite | n |
|---|---|---|---|---|---|---|---|
| 5 MiB | tar | 1 200 | 380 | 67 | 31 | 54 | 66 |
| 5 MiB | qcow2 | 2 300 | 740 | 63 | 26 | 50 | 43 |
| 10 MiB | tar | 1 200 | 450 | 120 | 52 | 88 | 60 |
| 10 MiB | qcow2 | 2 800 | 970 | 110 | 42 | 79 | 37 |
| 64 MiB | tar | 1 400 | 1 500 | 720 | 270 | 660 | 53 |
| 64 MiB | qcow2 | 9 500 | 3 300 | 630 | 220 | 450 | 21 |
| 500 MiB | tar | 5 900 | 5 400 | 5 600 | 2 100 | 4 600 | 24 |
| 500 MiB | qcow2 | 24 000 | 3 800 | 4 900 | 1 600 | 3 300 | 16 |
| 1 GiB | tar | 770 | — | — | — | — | 0 |
| 1 GiB | qcow2 | 1 000 | — | — | — | — | 0 |

#### p95

| Size | Arm | ResumeActor | SuspendActor | ServeAfterResume | ServeWarm | Overwrite |
|---|---|---|---|---|---|---|
| 5 MiB | tar | 1 200 | 530 | 75 | 34 | 58 |
| 5 MiB | qcow2 | 2 500 | 1 100 | 69 | 27 | 56 |
| 10 MiB | tar | 1 400 | 750 | 140 | 62 | 100 |
| 10 MiB | qcow2 | 3 000 | 1 400 | 120 | 47 | 90 |
| 64 MiB | tar | 1 600 | 1 900 | 750 | 300 | 780 |
| 64 MiB | qcow2 | 10 000 | 3 500 | 650 | 230 | 480 |
| 500 MiB | tar | 6 200 | 6 500 | 5 700 | 2 200 | 4 800 |
| 500 MiB | qcow2 | 25 000 | 4 400 | 5 000 | 1 700 | 3 400 |

### Inside the suspend, as reported by ateom

Milliseconds, from the `Actor checkpointed` record. `durable` is the work done
with the guest paused — packing the tar, or sealing the chain. `guest_flush` is
the pre-pause `sync(2)` in the guest, which only the qcow2 arrangement needs,
and which runs with the guest live. `n` is the number of checkpoints.

| Size | Arm | n | guest_flush p50 | guest_flush p95 | pause p50 | **durable p50** | durable p95 | teardown p50 |
|---|---|---|---|---|---|---|---|---|
| 5 MiB | tar | 67 | 0 | 0 | 2.33 | **10.40** | 11.86 | 55.6 |
| 5 MiB | qcow2 | 43 | 118.9 | 139.9 | 2.67 | **5.68** | 6.36 | 57.7 |
| 10 MiB | tar | 61 | 0 | 0 | 2.25 | **29.65** | 32.97 | 55.1 |
| 10 MiB | qcow2 | 38 | 230.1 | 242.1 | 2.67 | **5.71** | 6.80 | 59.8 |
| 64 MiB | tar | 54 | 0 | 0 | 2.27 | **309.99** | 546.09 | 65.5 |
| 64 MiB | qcow2 | 22 | 1 354.5 | 1 463.4 | 2.64 | **5.70** | 6.40 | 63.0 |
| 500 MiB | tar | 25 | 0 | 0 | 2.32 | **2 972.29** | 2 991.35 | 115.6 |
| 500 MiB | qcow2 | 16 | 761.4 | 1 307.2 | 2.75 | **5.88** | 6.67 | 115.9 |
| 1 GiB | tar | 37 | 0 | 0 | 2.34 | **4 869.58** | 5 101.47 | 159.2 |
| 1 GiB | qcow2 | 43 | 1 487.9 | 1 693.3 | 2.60 | **6.63** | 9.06 | 184.0 |

### Run index

| Size | Scenario | tar UTC start | qcow2 UTC start | Duration |
|---|---|---|---|---|
| 5 MiB | `durdir_size_5mb_microvm` | 08:26:19Z | 07:24:59Z | 3m |
| 10 MiB | `durdir_size_10mb_microvm` | 08:29:40Z | 07:28:20Z | 3m |
| 64 MiB | `durdir_size_64mb_microvm` | 08:33:01Z | 07:31:51Z | 5m |
| 128 MiB | `durdir_size_128mb_microvm` | 10:18:57Z | 10:03:24Z | 6m |
| 256 MiB | `durdir_size_256mb_microvm` | 10:25:27Z | 10:10:15Z | 8m |
| 500 MiB | `durdir_size_500mb_microvm` | 08:38:29Z | 07:37:18Z | 10m |
| 1 GiB | `durdir_size_1gb_microvm` | 08:48:49Z | 07:47:39Z | 12m |

Durations are the overrides used, not the durations committed in `tests.yaml`.
See [Caveats](#caveats).

The 128 and 256 MiB steps ran ~1.5 h after the rest, as their own pair of arms
with their own golden rebuilds, and with the router's route timeout already
raised to 5m. Neither size writes for anywhere near 10 s, so the raised timeout
cannot have changed them; everything else about the cluster was unchanged.

## What the numbers say

**The paused window behaves as designed.** Sealing the chain costs 5.68 ms at
5 MiB and 5.88 ms at 500 MiB — flat across a 100× increase in data, and flat to
within the p95 spread. Packing the tar over the same range goes 10.40 ms →
2 972.29 ms, tracking the data linearly. At 500 MiB the qcow2 arrangement holds
the guest still for 1/500th as long. This is the central claim of Proposal 5 and
the data supports it without qualification.

**But the cost moves rather than disappears, and it stays inside the RPC.**
The qcow2 arrangement has to `sync(2)` the guest before it pauses, and that
`guest_flush` is charged to the same `SuspendActor` call. End to end, tar wins
at 5, 10, and 64 MiB and qcow2 wins at 500 MiB; the crossover sits at ~128 MiB
(see [The crossover](#the-crossover-and-where-the-flatten-cost-actually-comes-from)).
What the arrangement buys below the crossover is not a
faster suspend but a shorter *stall* — 5.7 ms versus 310 ms of frozen guest at
64 MiB — which matters for a workload holding a connection open, and does not
show up in `SuspendActor` at all.

**Everything on the read path is faster under qcow2, by 15–25%.** At 500 MiB,
`ServeWarm` 2 100 → 1 600 ms, `Overwrite` 4 600 → 3 300 ms,
`ServeAfterResume` 5 600 → 4 900 ms. The durable dir is a virtio-blk ext4 image
rather than a virtio-fs share, and the guest page cache does the rest. This is
a real benefit that Proposal 5 does not claim.

**`ResumeActor` regresses 1.9–4.1×, and the whole regression is the flatten on
the restore path.** See [the next section](#where-the-resumeactor-regression-goes).

**One flatten failed, at the end of the scenario.** The qcow2 arm logged a
single `Failed to flatten the durable-dir chain; continuing on the deep chain`
in the 500 MiB run, with `err: signal: killed`. The restore it belonged to
started at 07:47:19.813Z and returned `context canceled` after 11.84 s; the
500 MiB scenario's 10-minute window closed at ~07:47:18Z. So the harness tore
down while a flatten was in flight, the `RestoreWorkload` context was canceled,
and `qemu-img convert` was killed with it. This is an artifact of stopping the
run, not a defect — and the fallback did the right thing, leaving the arriving
chain intact and mountable.

## Where the ResumeActor regression goes

Not queuing. `flattenDurableQcow2` runs on the **restore** path
([`durableqcow2.go:214`](../../cmd/ateom-microvm/durableqcow2.go)), and its own
`took` accounts for nearly all of the gap:

| Size | ResumeActor tar → qcow2 | Difference | Flatten p50 | Restore total tar → qcow2 | Chain landed |
|---|---|---|---|---|---|
| 5 MiB | 1 200 → 2 300 | +1 100 | 810 | 0.97 s → 1.95 s | 33 MiB |
| 10 MiB | 1 200 → 2 800 | +1 600 | 1 194 | 0.97 s → 2.38 s | 53 MiB |
| 64 MiB | 1 400 → 9 500 | +8 100 | 7 491 | 1.00 s → 8.68 s | 315 MiB |
| 500 MiB | 5 900 → 24 000 | +18 100 | 19 559 | 3.91 s → 21.10 s | 1 123 MiB |

Two things compound:

1. **`MAX_CHAIN=1` makes the flatten fire on every single restore.** The guard
   is `len(layers) >= qcow2.MaxChain()`, and a landed chain always has at least
   one layer. The code comment describes the cost as "one restore in every
   `MaxChain()`" — true at the default of 8, but at 1 it is every restore. This
   setting is what makes the *seal* comparable between the arms, so the
   comparison above is honest about the paused window and simultaneously shows
   the resume path at its worst case. Both readings are correct; they are just
   about different halves of the cycle.
2. **`qcow2.Flatten` runs `qemu-img convert -c`**, i.e. zlib. At 500 MiB it
   compresses a 1 123 MiB chain down to a 622 MiB base in 19.6 s, about
   57 MiB/s — CPU-bound and linear in the data, which is exactly the shape of
   the regression.

Two hypotheses this rules out:

* **Queuing for a worker.** The load is one serial user against a one-replica
  pool, and the regression grows with size; queuing has neither property. The
  earlier reading of this data blamed queuing, and that was wrong.
* **The qcow2 image bloating with the ext4 free space.** The flattened base is
  622 MiB for 500 MiB of data. Sparseness is working; the chain is 2.2× the
  data because `MAX_CHAIN=1` keeps a full base plus a full delta, and the
  workload rewrites the whole file every cycle, so the delta dedupes to nothing.

The fix is a flatten policy, not an instrumentation gap. The options, none of
them measured yet: drop `-c` and trade image size for CPU; move the flatten off
the restore path to a background job, which needs an answer for a suspend that
arrives mid-flatten; or flatten at seal instead and give back part of the
constant-time pause the arrangement exists to provide.

## The crossover, and where the flatten cost actually comes from

The 128 and 256 MiB steps were added to find where the two arrangements cross
over on end-to-end suspend cost. Both arms, both sizes, zero failures. p50 in
milliseconds, with the surrounding sizes from the main sweep for context:

| Size | SuspendActor tar | SuspendActor qcow2 | Winner |
|---|---|---|---|
| 5 MiB | 380 | 740 | tar |
| 10 MiB | 450 | 970 | tar |
| 64 MiB | 1 500 | 3 300 | tar |
| **128 MiB** | **2 300** | **2 400** | **tied** |
| **256 MiB** | **3 500** | **2 200** | **qcow2** |
| 500 MiB | 5 400 | 3 800 | qcow2 |

**The crossover is at ~128 MiB**, where the two are within 4% of each other.
Below it the pre-pause guest sync costs more than the tar pack it replaces;
above it the tar pack runs away and the sync does not.

The rest of the 128 / 256 MiB p50s:

| Size | Arm | ResumeActor | SuspendActor | ServeAfterResume | ServeWarm | Overwrite | n |
|---|---|---|---|---|---|---|---|
| 128 MiB | tar | 1 600 | 2 300 | 1 400 | 540 | 1 500 | 42 |
| 128 MiB | qcow2 | 15 000 | 2 400 | 1 300 | 440 | 840 | 18 |
| 256 MiB | tar | 2 600 | 3 500 | 2 900 | 1 100 | 2 400 | 34 |
| 256 MiB | qcow2 | 11 000 | 2 200 | 2 500 | 860 | 1 800 | 24 |

ateom-side, same two steps:

| Size | Arm | n | guest_flush | pause | durable | restore total | flatten | chain landed |
|---|---|---|---|---|---|---|---|---|
| 128 MiB | tar | 43 | 0 | 2.35 | 734.15 | 1.05 s | — | — |
| 128 MiB | qcow2 | 19 | 273.2 | 2.61 | **5.52** | 13.60 s | 12 390 ms | 507 MiB |
| 256 MiB | tar | 35 | 0 | 2.36 | 1 480.89 | 1.56 s | — | — |
| 256 MiB | qcow2 | 25 | 39.4 | 2.57 | **5.35** | 9.99 s | 8 732 ms | 515 MiB |

The seal stays flat — 5.52 and 5.35 ms — extending the constant-time result to
the middle of the range.

But look at the two chain sizes: 507 MiB for a 128 MiB durable dir, 515 MiB for
a 256 MiB one. Nearly identical, and both far above the live data. **The image
grows to a plateau that has little to do with the file in it**, and because the
flatten reads the whole image, the resume cost tracks the plateau rather than
the workload. That is why qcow2's 128 MiB resume (15 s) is *worse* than its
256 MiB resume (11 s), and why the regression looked super-linear in the main
sweep.

Chain size per cycle, in MiB, over the life of one actor:

```
128 MiB:  131  259  387  387  387  507  507  507 ... 507   (top layer: 129 throughout)
256 MiB:  259  515  515  515  515  515  515  515 ... 515   (top layer: 257 throughout)
```

Each cycle overwrites the whole file, and each overwrite adds a fresh set of
clusters to the image while the clusters it replaced are never released — so the
image climbs until ext4 starts reusing blocks and then plateaus, at ~4× the live
data for 128 MiB and ~2× for 256 MiB. The top layer, which is what the *seal*
touches, stays exactly at the file size; only the base accumulates.

The direct reading is that nothing punches the freed blocks back out: the guest
does not discard, and the disk is not configured to pass discards through. If
that is right, mounting with `discard` (or a periodic `fstrim`) plus
`DiskConfig` discard support would hold the image near the live data and take
most of the flatten cost with it. That inference is not tested — no discard
experiment has been run — but it is the cheapest thing to try before any of the
flatten-policy options above, because it shrinks the input rather than
rescheduling the work.

## Caveats

* **Durations are shorter than the committed ones.** `tests.yaml` runs the two
  largest steps for 20m and 30m; these runs used 10m and 12m so both arms would
  fit in one cluster session. Combined with the qcow2 arm's lower throughput,
  the 500 MiB cells rest on n=16 and n=24 cycles. Treat p50 as indicative and
  ignore anything above p95.
* **`guest_flush` is not monotonic in size** — 1 354.5 ms at 64 MiB against
  761.4 ms at 500 MiB. It measures how much dirty page cache happens to be
  outstanding when the suspend arrives, which depends on where in its write loop
  the workload was interrupted, not on the total size of the durable dir. Do not
  read it as a per-byte cost.
* **The 1 GiB qcow2 checkpoints are partial.** `durable_top_bytes` has a p50 of
  697 MiB against a nominal 1 GiB, consistent with the timed-out writes: most of
  those cycles were sampled mid-write. Another reason the 1 GiB row is not a
  comparison point.
* **`DurDirWrite` has n=1 per scenario** by construction — the fill happens once
  per actor lifetime, and `DurDirOverwrite` is the repeated operation. The 1 GiB
  rows are the exception, and only because every write failed.
* **Single node, single worker replica, single concurrent user.** Nothing here
  says how either arrangement behaves under contention.

## Follow-ups

1. Try discard. The flatten reads an image that plateaus at 2–4× the live data
   because nothing releases the clusters an overwrite replaces. Shrinking the
   input is cheaper than rescheduling the flatten, and it would help the upload
   and the download too.
2. Settle the flatten policy. This is what blocks making qcow2 the default:
   with `MAX_CHAIN=1` it costs 19.6 s of `qemu-img convert -c` on every
   500 MiB resume. Measuring the uncompressed convert is the cheapest first
   step.
3. Re-run the two largest steps at their committed 20m and 30m durations, now
   that the route timeout no longer caps a write at 10 s. Until then the sweep
   has no valid data point above 500 MiB.
4. Instrument the landing step. `Landed the durable-dir chain` records the
   chain's size but not how long the download took, which is the one part of
   the restore path still unaccounted for.
