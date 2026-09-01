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
| 500 MiB | `durdir_size_500mb_microvm` | 08:38:29Z | 07:37:18Z | 10m |
| 1 GiB | `durdir_size_1gb_microvm` | 08:48:49Z | 07:47:39Z | 12m |

Durations are the overrides used, not the durations committed in `tests.yaml`.
See [Caveats](#caveats).

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
at 5, 10, and 64 MiB and qcow2 wins at 500 MiB; the crossover is somewhere
between 64 and 500 MiB. What the arrangement buys below the crossover is not a
faster suspend but a shorter *stall* — 5.7 ms versus 310 ms of frozen guest at
64 MiB — which matters for a workload holding a connection open, and does not
show up in `SuspendActor` at all.

**Everything on the read path is faster under qcow2, by 15–25%.** At 500 MiB,
`ServeWarm` 2 100 → 1 600 ms, `Overwrite` 4 600 → 3 300 ms,
`ServeAfterResume` 5 600 → 4 900 ms. The durable dir is a virtio-blk ext4 image
rather than a virtio-fs share, and the guest page cache does the rest. This is
a real benefit that Proposal 5 does not claim.

**`ResumeActor` regresses 1.9–4.1× and is not explained by ateom's own
accounting.** 1 200 → 2 300 ms at 5 MiB, 1 400 → 9 500 ms at 64 MiB, 5 900 →
24 000 ms at 500 MiB. Yet ateom's boot decomposition is nearly identical between
the arms: `since_boot` sits at roughly 1.0 s in both at 500 MiB, and
`Actor restore phases` reports `durable: 0` in both, because both arms take the
cold-boot path for durable-dir actors rather than a memory restore. So the extra
time is spent before ateom starts working — most plausibly queuing for a worker,
since the qcow2 arm completes only 60–65% as many cycles per unit time
(16 vs 25 at 500 MiB) with a single-replica pool. That is a hypothesis, not a
measurement: the resume path is not instrumented finely enough to attribute it,
and closing this gap is the main follow-up.

**One flatten failed.** The qcow2 arm logged a single
`Failed to flatten the durable-dir chain; continuing on the deep chain` in the
500 MiB scenario, out of 16 checkpoints. The fallback behaved as intended — the
actor kept running on the unflattened chain — but the cause is unknown.

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

1. Fix the 1 GiB `DurDirWrite` timeout — raise the client timeout or split the
   write — and re-run the two largest steps at their committed 20m and 30m
   durations. Until then the sweep has no valid data point above 500 MiB.
2. Instrument the resume path finely enough to attribute the `ResumeActor`
   regression. If it is queuing, it is a throughput artifact of a
   single-replica pool and not a property of the arrangement; if it is not,
   it blocks making qcow2 the default.
3. Find the crossover between 64 and 500 MiB, which is where a size-based
   default would have to sit.
4. Investigate the single flatten failure at 500 MiB.
