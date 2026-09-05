# qcow2 vs tar durable-dir arrangement — 2026-09-01

The DurDir size sweep run twice on the same cluster, once with the tar
arrangement and once with the qcow2 arrangement, to measure what Proposal 5
claims: that sealing a qcow2 backing-file chain takes constant time in the
paused window, where packing a tar grows with the size of the directory.

Measured on branch `poc5-qcow2` at commit `bd8ddbba`, which is the first commit
at which the qcow2 arrangement boots at all on cloud-hypervisor. [A second
sweep](#at-the-default-max_chain8) at the shipped `MAX_CHAIN` default ran later
on `9fbef6e9`. The size steps come from `durdir-large-sizes`, cherry-picked onto
this branch as `97bed514` and `aeb08aa6`; the tar arm here is a re-run of
[baseline-2026-08-26.md](baseline-2026-08-26.md) under this branch's durations,
not a reuse of its numbers.

To repeat this measurement, read [REPRODUCING.md](REPRODUCING.md), then the
[Separating the two arms](#separating-the-two-arms) section below, which is
specific to this comparison.

> [!IMPORTANT]
> **Read [the drain section](#it-is-the-restore-paths-dirty-pages-and-the-fix-is-a-background-drain)
> before any qcow2 suspend number below.** Every one of them includes a
> `guest_flush` that turned out to be the restore path's own dirty page cache,
> collected at the guest's `fsync` by ext4's `data=ordered` journal, and a
> background `syncfs` after landing removes it: 1 233 → 35 ms on the flush,
> 1 300 ms off the cycle. The partial-update comparison is
> [re-run against tar with it in](#against-tar-with-the-drain-in), and it changes
> who wins — 128 MiB partial goes from tar by 1.24× to a tie, 512 MiB partial
> from qcow2 by 1.09× to 1.30×.

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
See [Caveats](#caveats). The `MAX_CHAIN=8` sweep is indexed in [its own
section](#at-the-default-max_chain8).

The partial-update pairs ran on 2026-09-03 as their own session, `MAX_CHAIN=8`
in the qcow2 arm, with a golden rebuild between the arms:

| Scenario | qcow2 UTC start | tar UTC start | Duration |
|---|---|---|---|
| `durdir_size_128mb_microvm` | 03:43:29Z | 04:17:59Z | 6m |
| `durdir_partial_128mb_microvm` | 03:50:00Z | 04:24:30Z | 6m |
| `durdir_size_500mb_microvm` | 03:56:30Z | 04:31:01Z | 10m |
| `durdir_partial_512mb_microvm` | 04:07:14Z | 04:41:34Z | 10m |

The flush breakdown, the `Direct` A/B and the chain-depth sweep all ran against
`durdir_partial_128mb_microvm` for 3m per arm, on 2026-09-03 at 07:39Z, 08:27Z
and 16:34Z respectively.

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
That crossover holds at `MAX_CHAIN=1` only — [at the shipped default of
8](#resume-improves-everywhere-suspend-regresses-everywhere) the deeper chain
costs more to upload than the tar pack it replaces, and qcow2 loses the suspend
at every size. What the arrangement buys below the crossover is not a
faster suspend but a shorter *stall* — 5.7 ms versus 310 ms of frozen guest at
64 MiB — which matters for a workload holding a connection open, and does not
show up in `SuspendActor` at all.

**On a full rewrite the arrangement never wins the end-to-end cycle; on a
partial update it does.** Everything in this section and the three that follow
it rewrites the durable dir completely every cycle, which is the worst case for
shipping a delta. [A partial-update
workload](#a-partial-update-workload) holds the volume fixed and rewrites an
eighth of it, halves the qcow2 cycle, leaves tar's unchanged, and takes the
512 MiB cell outright. Read the full-rewrite results as a floor, not as the
arrangement's verdict.

**Everything on the read path is faster under qcow2, by 15–25%.** At 500 MiB,
`ServeWarm` 2 100 → 1 600 ms, `Overwrite` 4 600 → 3 300 ms,
`ServeAfterResume` 5 600 → 4 900 ms. The durable dir is a virtio-blk ext4 image
rather than a virtio-fs share, and the guest page cache does the rest. This is
a real benefit that Proposal 5 does not claim.

**`ResumeActor` regresses 1.9–4.1×, and the whole regression is the flatten on
the restore path.** See [the next section](#where-the-resumeactor-regression-goes).
Most of that flatten is the `-c` in the `qemu-img convert` it runs, which on
this workload costs 6.6× and saves nothing — see
[The cost of `-c`](#the-cost-of--c). Strip the flatten out and landing a chain
is constant-time too, and beats tar's unpack above ~200 MiB; see
[The restore path without the flatten](#the-restore-path-without-the-flatten).

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

The fix is a flatten policy, not an instrumentation gap. The options: drop `-c`,
measured in [The cost of `-c`](#the-cost-of--c) below; move the flatten off the
restore path to a background job, which needs an answer for a suspend that
arrives mid-flatten; or flatten at seal instead and give back part of the
constant-time pause the arrangement exists to provide.

> **The background job was built and reverted.** It does not survive contact
> with an actor whose activation is shorter than its own flatten, and the way
> it fails is to grow the chain past what cloud-hypervisor will open. See
> [The background flatten hits cloud-hypervisor's nesting
> limit](#the-background-flatten-hits-cloud-hypervisors-nesting-limit).

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
does not discard, and the disk is not configured to pass discards through.
Mounting with `discard` (or a periodic `fstrim`) plus `DiskConfig` discard
support would hold the image near the live data. That inference is not tested —
no discard experiment has been run.

How much it would buy, if the image fell all the way to the floor of one base
plus one full top layer, is bounded by how far each size sits above that floor
today:

| Size | Chain now | Floor | Bloat | Flatten now | Flatten at the floor |
|---|---|---|---|---|---|
| 5 MiB | 33 MiB | ~10 MiB | 3.3× | 810 | ~240 |
| 10 MiB | 53 MiB | ~20 MiB | 2.6× | 1 194 | ~450 |
| 64 MiB | 315 MiB | ~130 MiB | 2.4× | 7 491 | ~3 100 |
| 128 MiB | 507 MiB | ~258 MiB | 2.0× | 12 390 | ~6 300 |
| 256 MiB | 515 MiB | ~514 MiB | **1.0×** | 8 732 | ~8 700 |
| 500 MiB | 1 123 MiB | ~1 002 MiB | **1.1×** | 19 559 | ~17 400 |

The bloat is a small-file effect, and it inverts the intuition: ext4's allocator
roams when the file is small relative to the free space, so it takes many cycles
to wrap around and start reusing LBAs, and every cycle until then adds clusters.
A 500 MiB file in the same 32 GiB image wraps almost immediately and barely
bloats at all. So discard would help most exactly where the absolute resume cost
is already smallest, and would do nothing for the 256 and 500 MiB steps that
dominate the regression. It is a storage-and-transfer argument — 507 MiB down to
258 MiB is halved upload, download, and GCS footprint — not a latency one.

## The cost of `-c`

`Flatten` runs `qemu-img convert -f qcow2 -O qcow2 -c`. What the `-c` costs was
measured directly, on the same worker node, against a synthetic chain built to
match the 500 MiB arm's landed chain: 32 GiB virtual (the ateom default), a
622 MiB compressed base standing in for a previous flatten's output, and a
506 MiB uncompressed top standing in for the cycle's fresh guest writes —
1 129 MiB in total against the real chain's 1 123 MiB. The payload is
`crypto/rand`, because that is what the workload writes: `WriteDisk` fills the
file from `crypto/rand`, so none of it compresses.

The synthetic chain reproduces the measured cost. Times in ms, `qemu-img`
10.0.11 on the `n2-standard-4` worker, `-T none -t none` so neither end goes
through the page cache:

| Command | Run 1 | Run 2 | Output |
|---|---|---|---|
| `convert -c` (what `Flatten` runs today) | 19 511 | 19 525 | 502 MiB |
| `convert`, no `-c` | 2 952 | 2 950 | **502 MiB** |

19 511 ms against the 19 559 ms p50 the 500 MiB arm actually recorded, a 0.2%
match, so the synthetic stands in for the real thing.

**Dropping `-c` is 6.6× faster and the output is not one byte larger.** On
incompressible data deflate returns what it was given, so the whole 16.6 s is
spent producing an image identical in size to the one a plain convert produces.

Parallelism cannot recover it — compressed cluster writes do not scale with
`convert`'s coroutine count:

| Command | Time |
|---|---|
| `convert -c -m 1` | 24 398 |
| `convert -c -m 8` (the default) | 19 511 |
| `convert -c -m 16` | 19 871 |
| `convert -m 16` | 2 984 |

Applying the 6.6× to the flatten at every size, and subtracting the saving from
the measured `ResumeActor` p50:

| Size | tar | qcow2 now | qcow2 without `-c` | vs tar |
|---|---|---|---|---|
| 5 MiB | 1 200 | 2 300 | ~1 600 | 9.4× → 1.3× |
| 10 MiB | 1 200 | 2 800 | ~1 800 | — |
| 64 MiB | 1 400 | 9 500 | ~3 100 | 6.8× → 2.2× |
| 128 MiB | 1 600 | 15 000 | ~4 500 | 9.4× → 2.8× |
| 256 MiB | 2 600 | 11 000 | ~3 600 | 4.2× → 1.4× |
| 500 MiB | 5 900 | 24 000 | ~7 400 | 4.1× → 1.3× |

Only the first two columns are measured; the third is the measured 6.6× applied
to the measured flatten, and the arithmetic assumes the rest of the restore is
unchanged.

The reason the scaling is legitimate is that the flatten is linear in the chain
it reads — 41 MiB/s at every size up to 128 MiB, 57–59 MiB/s at 256 and 500 MiB:

| Size | Chain | Flatten | Rate |
|---|---|---|---|
| 5 MiB | 33 MiB | 810 | 41 MiB/s |
| 10 MiB | 53 MiB | 1 194 | 44 MiB/s |
| 64 MiB | 315 MiB | 7 491 | 42 MiB/s |
| 128 MiB | 507 MiB | 12 390 | 41 MiB/s |
| 256 MiB | 515 MiB | 8 732 | 59 MiB/s |
| 500 MiB | 1 123 MiB | 19 559 | 57 MiB/s |

One caveat on generality: this workload is incompressible by construction, which
is the worst case for `-c`'s benefit and a fair case for its cost. A workload
with compressible data would get a smaller image out of `-c`, and the
image-size-against-CPU trade would come back. That case has not been measured.
The conclusion this run supports is narrower than "delete `-c`": for
incompressible data it is pure loss, so the flatten wants to be told which it
is dealing with rather than always compressing.

## The restore path without the flatten

Subtracting the flatten from the restore total isolates what the qcow2
arrangement costs to land and mount, as against tar's unpack. Seconds, ateom
side:

| Size | tar restore | qcow2 restore | qcow2 minus flatten |
|---|---|---|---|
| 5 MiB | 0.97 | 1.95 | 1.14 |
| 10 MiB | 0.97 | 2.38 | 1.19 |
| 64 MiB | 1.00 | 8.68 | 1.19 |
| 128 MiB | 1.05 | 13.60 | 1.21 |
| 256 MiB | 1.56 | 9.99 | **1.26** |
| 500 MiB | 3.91 | 21.10 | **1.54** |

**Landing a chain is close to constant time — 1.14 s to 1.54 s over a 100×
range — the same shape as the seal.** tar has to unpack, so it grows with the
data, and the two cross at ~200 MiB. The restore path is not inherently the
qcow2 arrangement's weak side; the flatten is the only thing making it look
that way, and the flatten is a policy knob.

The rightmost column is an optimistic bound rather than a prediction: it comes
from two-layer chains, and a deeper chain costs more to land and read through.
[At the default `MAX_CHAIN=8`](#at-the-default-max_chain8) prices that.

## At the default `MAX_CHAIN=8`

Everything above ran with `ATEOM_DURABLE_QCOW2_MAX_CHAIN=1`, which is what makes
the seal comparable and what makes the flatten fire on every single restore.
That is not the configuration a production default would ship. This section is a
second sweep at the shipped default of 8, on commit `9fbef6e9` — the commit that
also drops the `-c`. The tar arm is not re-run: neither change touches it, so
the tar columns above stay the comparison.

Ran 2026-09-01 23:45Z to 2026-09-02 00:44Z on the same cluster and node, with a
fresh golden snapshot, at 3m / 6m / 8m / 10m / 30m. 500 MiB gets 30m because a
flatten now fires once every eight cycles, and the 10m window that gave n=16
would have contained two of them. Zero failures in all five.

> [!NOTE]
> Two variables moved at once, so end-to-end deltas against the `MAX_CHAIN=1`
> arm mix them. The directions are still separable. Dropping `-c` can only
> affect the flatten, which runs on restore; it never ran on the suspend path at
> all. So **the suspend numbers isolate chain depth cleanly**, and only the
> resume numbers are confounded.

### End-to-end, p50 in milliseconds

| Size | ResumeActor | SuspendActor | ServeAfterResume | ServeWarm | Overwrite | n |
|---|---|---|---|---|---|---|
| 5 MiB | 1 400 | 880 | 65 | 26 | 50 | 52 |
| 64 MiB | 1 900 | 3 500 | 650 | 220 | 450 | 44 |
| 128 MiB | 4 400 | 5 200 | 1 300 | 440 | 840 | 37 |
| 256 MiB | 7 200 | 6 400 | 2 500 | 850 | 1 800 | 30 |
| 500 MiB | 14 000 | 7 700 | 4 900 | 1 700 | 3 300 | 52 |

p95:

| Size | ResumeActor | SuspendActor | ServeAfterResume | ServeWarm | Overwrite |
|---|---|---|---|---|---|
| 5 MiB | 1 600 | 1 100 | 71 | 29 | 55 |
| 64 MiB | 5 000 | 4 300 | 680 | 240 | 470 |
| 128 MiB | 10 000 | 6 200 | 1 300 | 460 | 870 |
| 256 MiB | 17 000 | 7 800 | 2 600 | 890 | 1 800 |
| 500 MiB | 34 000 | 14 000 | 5 300 | 1 700 | 3 500 |

The three read operations are unchanged from the `MAX_CHAIN=1` arm to within
noise. Chain depth does not cost anything to read through once the chain is
mounted, which is worth stating because it was not obvious in advance.

### Resume improves everywhere, suspend regresses everywhere

p50 in milliseconds, the three arms side by side:

| Size | Resume tar | Resume MC=1 | Resume MC=8 | Suspend tar | Suspend MC=1 | Suspend MC=8 |
|---|---|---|---|---|---|---|
| 5 MiB | 1 200 | 2 300 | 1 400 | 380 | 740 | 880 |
| 64 MiB | 1 400 | 9 500 | 1 900 | 1 500 | 3 300 | 3 500 |
| 128 MiB | 1 600 | 15 000 | 4 400 | 2 300 | 2 400 | 5 200 |
| 256 MiB | 2 600 | 11 000 | 7 200 | 3 500 | 2 200 | 6 400 |
| 500 MiB | 5 900 | 24 000 | 14 000 | 5 400 | 3 800 | 7 700 |

**This reverses the crossover result.** At `MAX_CHAIN=1` the qcow2 arrangement
won `SuspendActor` at 256 MiB and above; at the shipped default it loses at
every size measured. Resume improves by 1.6× to 5× but still loses to tar
everywhere.

### Why suspend regresses: the chain is the upload

`durable_bytes` on the `Actor checkpointed` record is the chain that gets pushed
to GCS. In MiB:

| Size | Chain MC=1 | Chain MC=8 p50 | Chain MC=8 max | Top layer |
|---|---|---|---|---|
| 5 MiB | 33 | 54 | 79 | 6 |
| 64 MiB | 315 | 337 | 596 | 65 |
| 128 MiB | 507 | 775 | 1 292 | 129 |
| 256 MiB | 515 | 1 296 | 2 325 | 257 |
| 500 MiB | 1 123 | 2 769 | 4 526 | 502 |

At 500 MiB the chain grows 2.5× and the suspend grows 2.0×. `snapshot` has a p50
of 0 across every scenario, so the memory snapshot is not involved — the cost is
the chain, and nothing else changed on that path.

The reason the chain scales with depth is [the same one the crossover section
found](#the-crossover-and-where-the-flatten-cost-actually-comes-from): the
workload overwrites the entire file every cycle, so successive layers share no
clusters and a depth-8 chain is close to eight full copies. **Chain depth only
pays for itself when a cycle touches a subset of the data, and this benchmark
never does.** That is a property of the workload, not of the arrangement, and it
means neither `MAX_CHAIN` value measured here is the one a partial-update
workload would want. It also bounds what this report can say about the default:
the regression is real for a full-rewrite workload and says nothing about any
other.

### The `-c` removal, on the real path

`Flattened the durable-dir chain`, p50 in milliseconds, against the same record
from the `MAX_CHAIN=1` arm:

| Size | Flatten with `-c` | Bytes read | Flatten without `-c` | Bytes read | Output |
|---|---|---|---|---|---|
| 5 MiB | 810 | 33 MiB | 97 | ~79 MiB | 35 MiB |
| 64 MiB | 7 491 | 315 MiB | 1 909 | ~596 MiB | 139 MiB |
| 128 MiB | 12 390 | 507 MiB | 3 138 | ~1 292 MiB | 387 MiB |
| 256 MiB | 8 732 | 515 MiB | 4 933 | ~2 325 MiB | 523 MiB |
| 500 MiB | 19 559 | 1 123 MiB | 10 026 | ~4 526 MiB | 1 013 MiB |

Two to eight times faster *while reading a two-to-four-times deeper chain*.

> [!WARNING]
> An earlier version of this section read that as input throughput of 57 MiB/s
> against 451 MiB/s, and concluded the 6.6× microbenchmark was a lower bound.
> Both figures are confounded — the two arms differ in chain depth as well as in
> `-c`, and nominal input is not comparable across depths. [`MAX_CHAIN=1`
> without the `-c`](#max_chain1-without-the--c) measures the flag on its own and
> gets 2.6–10.4×, with 6.6× inside the range. Use that section, not this table,
> for what `-c` costs.

Flatten counts are low by construction — n=7, 6, 5, 4, 7 across the five sizes,
one per eight cycles — so read these as an order of magnitude, not a p50 with
any precision behind it.

### The seal survives a depth-8 chain

Milliseconds, from `Actor checkpointed`:

| Size | n | guest_flush p50 | pause p50 | **seal p50** | seal p95 | teardown p50 |
|---|---|---|---|---|---|---|
| 5 MiB | 53 | 261.7 | 2.65 | **6.72** | 7.88 | 57.3 |
| 64 MiB | 44 | 1 721.5 | 2.66 | **6.78** | 8.05 | 69.3 |
| 128 MiB | 37 | 2 605.4 | 2.58 | **6.43** | 7.75 | 80.8 |
| 256 MiB | 30 | 2 274.5 | 2.49 | **6.36** | 7.89 | 105.2 |
| 500 MiB | 52 | 1 372.3 | 2.52 | **6.50** | 8.07 | 148.5 |

**6.36 to 6.78 ms across a 100× range at chain depth 8**, against tar's 2 972 ms
at 500 MiB. This is Proposal 5's actual claim, and the shipped default does not
weaken it: the paused window is still constant and still ~450× smaller. Chain
depth cycled 2..8 in every scenario, so these are genuinely spread across
depths rather than concentrated at one.

## `MAX_CHAIN=1` without the `-c`

Three configurations were now measured — `MAX_CHAIN=1` with `-c`, `MAX_CHAIN=8`
without it — leaving the fourth corner, shallow chain and no compression, as the
one combination that had the flatten's two costs both minimized. This is that
run: same image as the `MAX_CHAIN=8` sweep, `MAX_CHAIN` back to 1, fresh golden,
2026-09-02 05:29Z to 06:03Z, at the same durations as the original
`MAX_CHAIN=1` arm so the cells line up. Zero failures in all five.

It does not beat tar. An extrapolation in an earlier draft of this section
predicted it would, and that prediction was wrong by 3 s — see [Where the
extrapolation went wrong](#where-the-extrapolation-went-wrong).

### End-to-end, p50 in milliseconds

| Size | ResumeActor | SuspendActor | ServeAfterResume | ServeWarm | Overwrite | n |
|---|---|---|---|---|---|---|
| 5 MiB | 1 400 | 640 | 64 | 26 | 50 | 56 |
| 64 MiB | 3 700 | 3 600 | 640 | 220 | 450 | 32 |
| 128 MiB | 4 500 | 4 300 | 1 300 | 430 | 830 | 29 |
| 256 MiB | 7 100 | 5 300 | 2 600 | 850 | 1 800 | 27 |
| 500 MiB | 10 000 | 3 800 | 5 000 | 1 600 | 3 300 | 24 |

### All four arms

`ResumeActor` + `SuspendActor` p50 in milliseconds, which is the whole cycle the
caller pays:

| Size | tar | qcow2 MC=1 `-c` | qcow2 MC=8 no `-c` | qcow2 MC=1 no `-c` |
|---|---|---|---|---|
| 5 MiB | **1 580** | 3 040 | 2 280 | 2 040 |
| 64 MiB | **2 900** | 12 800 | 5 400 | 7 300 |
| 128 MiB | **3 900** | 17 400 | 9 600 | 8 800 |
| 256 MiB | **6 100** | 13 200 | 13 600 | 12 400 |
| 500 MiB | **11 300** | 27 800 | 21 700 | 13 800 |

**Shallow chain and no compression is the best of the three qcow2
configurations at every size except 64 MiB, and it still loses to tar at every
size.** At 500 MiB it closes the gap from 2.5× to 1.22×, which is the most this
arrangement has managed, and is not a win.

### Where the extrapolation went wrong

The earlier draft took the 451 MiB/s flatten throughput from the `MAX_CHAIN=8`
sweep, applied it to the 1 123 MiB chain of the `MAX_CHAIN=1` arm, and got a
2.5 s flatten. The measured flatten is **6 958 ms**, at 163 MiB/s.

The borrowed rate was inflated by the deeper chain it came from. A depth-8 chain
is nominally 4 526 MiB of input, but most of those clusters are superseded by a
later layer and never reach the output, and many are still in page cache from
the layers written moments before. Dividing wall time by nominal input therefore
flatters a deep chain and says nothing about a shallow one. Input throughput is
not a portable rate across chain depths, and this report should not have treated
it as one.

### What `-c` actually costs, measured cleanly

This run differs from the original `MAX_CHAIN=1` arm in exactly one thing, so it
is the apples-to-apples measurement the other two comparisons could not be.
Flatten p50 in milliseconds, and the same figure as throughput over the landed
chain:

| Size | With `-c` | Without `-c` | Speedup | MiB/s with | MiB/s without |
|---|---|---|---|---|---|
| 5 MiB | 810 | 78 | 10.4× | 41 | 405 |
| 64 MiB | 7 491 | 1 591 | 4.7× | 42 | 204 |
| 128 MiB | 12 390 | 2 226 | 5.6× | 41 | 217 |
| 256 MiB | 8 732 | 3 321 | 2.6× | 59 | 235 |
| 500 MiB | 19 559 | 6 958 | 2.8× | 57 | 163 |

Between 2.6× and 10.4×, and the 6.6× of [The cost of `-c`](#the-cost-of--c)
sits inside that range rather than below it. The O_DIRECT microbenchmark was a
fair estimate, not a lower bound; the claim in the `MAX_CHAIN=8` section that it
understated the real path came from the confounded comparison and does not
survive this one.

### The rest of the restore is unchanged

Seconds, ateom side, from `Actor restored (durable-dir volumes, cold boot)`:

| Size | Restore total | minus flatten | Same column at MC=1 with `-c` |
|---|---|---|---|
| 5 MiB | 1.16 | 1.08 | 1.14 |
| 64 MiB | 2.70 | 1.11 | 1.19 |
| 128 MiB | 3.36 | 1.14 | 1.21 |
| 256 MiB | 5.46 | 2.14 | 1.26 |
| 500 MiB | 8.16 | 1.21 | 1.54 |

Landing and mounting a chain is 1.08–1.21 s over a 100× range, reproducing the
constant-time result from [the earlier
subtraction](#the-restore-path-without-the-flatten) on an independent run. The
256 MiB cell is the one outlier and has no explanation here.

The seal is 5.54–5.75 ms across all five sizes, with `pause` at 2.56–2.71 ms —
indistinguishable from both other arms.

### A caution about `SuspendActor` at the middle sizes

`SuspendActor` at 128 and 256 MiB is 4 300 and 5 300 ms here against 2 400 and
2 200 ms in the original `MAX_CHAIN=1` arm, and `-c` cannot explain that because
it never runs on the suspend path. `guest_flush` does: it is 2 192 and 2 789 ms
here against 273 and 39 ms there. As the caveats already note, `guest_flush`
measures how much dirty page cache happens to be outstanding when the suspend
lands, which is a lottery on where the write loop was interrupted.

That term is large enough to move `SuspendActor` by 2× between runs of the same
configuration, so **suspend comparisons between arms at 64–256 MiB are inside
the noise.** The 5 MiB and 500 MiB cells, where `guest_flush` happened to be
small in every arm, are the only suspend numbers this report should be read as
comparing.

## A partial-update workload

Every arm above rewrites the durable dir end to end every cycle, which the
[`MAX_CHAIN=8` section](#why-suspend-regresses-the-chain-is-the-upload) already
names as the worst case for a backing-file chain: successive layers share no
clusters, so depth is pure cost and a delta is the whole volume. That was
follow-up 1. `durdir_partial_128mb_microvm` and `durdir_partial_512mb_microvm`
hold the directory at the same total size and rewrite an eighth of it — 8 files,
one rewritten per cycle — which is closer to an agent editing a few files in a
workspace it otherwise leaves alone.

Ran 2026-09-03 03:43Z to 04:52Z, both arrangements, on the same cluster and node,
each with its own golden rebuild. Each partial scenario is paired with the
equally-sized full-rewrite step in the same session, so the only thing that
differs inside a pair is the per-cycle delta. `MAX_CHAIN=8` throughout — this
section is about the delta, not about depth. Zero failures in all eight runs.

### The cycle the caller pays

`ResumeActor` + `SuspendActor` p50, milliseconds:

| Volume | Delta | tar | qcow2 | Winner |
|---|---|---|---|---|
| 128 MiB | full (128 MiB) | **4 000** | 10 100 | tar, 2.5× |
| 128 MiB | partial (16 MiB) | **4 100** | 5 100 | tar, 1.24× |
| ~500 MiB | full (500 MiB) | **9 100** | 22 000 | tar, 2.4× |
| ~512 MiB | partial (64 MiB) | 11 800 | **10 800** | **qcow2, 1.09×** |

> [!NOTE]
> Both partial rows were re-measured after the
> [background drain](#against-tar-with-the-drain-in) and both moved: 128 MiB to
> a tie and 512 MiB to qcow2 by 1.30×. The two full-rewrite rows were not
> re-run, and the drain does not change what they say — a full delta gives the
> chain nothing to be sparse about.

**Going partial halves the qcow2 cycle and does nothing for tar.** 10 100 →
5 100 and 22 000 → 10 800 on one side; 4 000 → 4 100 and 9 100 → 11 800 on the
other. That asymmetry is the whole design argument, and it is the first thing in
this report to show up as an end-to-end win: at 512 MiB with a one-eighth delta,
qcow2 takes the cycle.

tar getting *slower* on the 512 MiB partial than on the 500 MiB full step is not
noise — it packs the same bytes either way, but into 8 files rather than 1, and
pays per-file overhead for the privilege. It cannot benefit from a delta because
it does not ship one.

### Where the halving comes from

ateom side, p50 in milliseconds. `top` is `durable_top_bytes`, the layer the
seal produces; `chain` is `durable_bytes`, what gets uploaded.

| Volume | Delta | Arm | n | guest_flush | **seal** | teardown | top MB | chain MB |
|---|---|---|---|---|---|---|---|---|
| 128 MiB | full | tar | 42 | 0 | 761.7 | 89.7 | — | — |
| 128 MiB | full | qcow2 | 26 | 4 332.5 | **8.4** | 90.3 | 135.6 | 812.1 |
| 128 MiB | partial | tar | 64 | 0 | 754.6 | 68.8 | — | — |
| 128 MiB | partial | qcow2 | 54 | 1 246.7 | **8.1** | 71.0 | **18.0** | 235.0 |
| ~500 MiB | full | tar | 25 | 0 | 3 270.2 | 159.5 | — | — |
| ~500 MiB | full | qcow2 | 19 | 3 584.7 | **7.9** | 169.2 | 526.0 | 2 638.6 |
| ~512 MiB | partial | tar | 41 | 0 | 5 113.8 | 120.1 | — | — |
| ~512 MiB | partial | qcow2 | 41 | 4 210.3 | **8.3** | 93.5 | **68.5** | 824.0 |

Two readings:

**The delta is real and it is the whole saving.** `durable_top_bytes` goes
135.6 MB → 18.0 MB and 526.0 MB → 68.5 MB, both close to the nominal eighth. The
chain follows, 812 → 235 MB and 2 639 → 824 MB, and the chain is the upload. tar
has no equivalent column because it has no equivalent behavior: its
`durable_dir` is 761.7 ms for the full 128 MiB and 754.6 ms for the partial one,
identical to within 1%.

**The seal is untouched by any of it** — 7.9 to 8.4 ms across full, partial,
128 MiB and 500 MiB. Proposal 5's constant-time claim survives its fourth
independent test.

### The read path, again

p50 in milliseconds. Consistent with the size sweep: qcow2 is faster on every
read, by 12–19%.

| Volume | Op | tar | qcow2 |
|---|---|---|---|
| 128 MiB partial | `ServeWarm` | 74 | **60** |
| 128 MiB partial | `ServeAfterResume` | 180 | **160** |
| 512 MiB partial | `ServeWarm` | 270 | **220** |
| 512 MiB partial | `ServeAfterResume` | 700 | **590** |

## Where `guest_flush`'s time goes

`guest_flush` is the pre-pause `sync(2)`, and by this point it is the largest
term the qcow2 arrangement adds. On the 128 MiB partial workload it is 1 246.7 ms
to get 18.0 MB of delta onto a disk that the flatten drives at 165 MiB/s. That
is ~14 MB/s, and the gap wants an explanation.

Six candidates were tested, each on that same 128 MiB partial scenario so the
numbers compare directly. Five are ruled out.

| Candidate | Verdict | Evidence |
|---|---|---|
| Small (4 KiB) requests / an IOPS ceiling | **ruled out** | 239.6 KiB average write during the flush window, 746 IOPS |
| The `ExecProcess` round-trip into the guest | **ruled out** | a no-op helper: 30.0 ms |
| `sync(2)` flushing the other filesystems too | **ruled out** | `syncfs(2)` on the durable mount alone: 32.1 ms, but teardown 66 → 1 154 ms |
| The host page cache | **ruled out** | `Direct: true` on the disk: 1 238.4 vs 1 221.0 ms |
| The qcow2 format itself | **ruled out** | host-side `qemu-img` flattens the same files at 165 MiB/s |
| Backing-chain depth | **partial, ~31%** | 927.4 / 1 033.1 / 1 337.0 ms at 2 / 3 / 5 layers |

### Instrumentation

A scratch build split `guest_flush` into the host-side staging write and the
guest round-trip, and made the helper's program selectable, so one image could
serve every arm. Staging is 0.2 ms in every arm; the entire cost is inside the
guest call.

| Helper | `exec` p50 | `durable_top_bytes` |
|---|---|---|
| `sync(2)` (what ateom ships) | 1 227.7 | 18.0 MB |
| `syncfs(2)` on the durable mount | 31.8 | 18.0 MB |
| `exit(0)` | 29.7 | **0.5 MB** |

> [!WARNING]
> **The no-op arm loses data and is not a configuration.** Its
> `durable_top_bytes` is 0.5 MB against the other arms' 18.0 MB: it seals
> whatever happened to reach the disk on its own. It is a yardstick for what the
> round-trip costs and nothing else.

### `syncfs` does not help; it relocates

`syncfs(2)` on the durable mount alone brings the flush to 32.1 ms and seals the
same 18.0 MB — so it is not losing writes. But `teardown` goes 66.1 → 1 154.0 ms,
and `teardown` is the cloud-hypervisor VMM shutdown. The bytes `sync(2)` was
waiting for are still written; they are just written while the VMM is being torn
down instead of before the pause. Total cycle cost is unchanged.

This is worth knowing even though it is not a win: it says the cost is the
*writeback itself*, not the choice of syscall, and it means a flush that "got
faster" needs its teardown checked before it is believed.

### It is the restore path's dirty pages, and the fix is a background drain

The flush is not slow. It is waiting for bytes it did not write.

Sampling `/proc/meminfo` and `/proc/diskstats` on the node every 200 ms through
a run, and lining the samples up against ateom's own log, gives the whole cycle:

```
restore                    dirty page cache 0 -> 272 MB in ~600 ms
                           (memcpy speed: nothing has reached the device yet)
flatten                    device saturated at 176 MB/s, ~90 MB drains
flatten returns            174 MB still dirty
guest boots, serves        174 MB still dirty. Device idle for 2.4 s.
                           Nothing writes it back: dirty_expire is 30 s and
                           the background threshold is 10% of 16 GB.
guest_flush                174 MB drains at 176 MB/s   <-- 1 000-1 500 ms
```

The link between the two ends is ext4's journal, not the image. The guest's
`sync(2)` becomes a virtio-blk flush, cloud-hypervisor turns that into an
`fsync` of the durable image, and ext4 in its default `data=ordered` mode
cannot commit that transaction until every inode's ordered data is on the
device. So an 18 MB flush waits on 174 MB belonging to files it has never
touched.

That the coupling is journal-wide and not per-file is measurable, and it is what
makes the cheap fix not work: `sync_file_range(WRITE|WAIT_AFTER)` on the freshly
flattened base returns in 11-49 ms with the 174 MB still dirty, and the flush is
unchanged. The base is already clean when the flatten returns —  `qemu-img
convert` runs at device speed and is dirty-throttled the whole way. The dirty
pages are the rest of the restore's: ateom writes ~185 MB per cycle by its own
`/proc/<pid>/io` accounting, on a path that cold-boots the guest every time.
The flatten is not the main producer either — at `MAX_CHAIN=8`, where only 6 of
51 restores flatten at all, the flush still costs 1 233 ms.

This also explains the `Direct: true` result above. Putting the durable disk on
direct I/O takes the *image* out of the page cache, which is not where the dirty
pages were.

Draining the filesystem — `syncfs(2)` on the layer directory, at the end of
landing, before the guest runs — removes it completely. Three arms on the
128 MiB partial workload, `MAX_CHAIN=8`, 6m each, arms run hypothesis-first
because the second arm of a pair measures 90-290 ms slower on `guest_flush`
whichever arm it is:

| | none | `syncfs`, waited for | `syncfs`, backgrounded |
|---|---|---|---|
| `guest_flush` p50 | 1 233.1 / 1 510.3 | **35.3** | **38.8** |
| suspend total, server | 1 317.0 / 1 592.0 | 115.7 | 120.7 |
| `SuspendActor` p50 | 3 200 / 3 600 | 2 000 | 1 900 |
| `ResumeActor` p50 | 2 000 / 2 200 | 3 300 | 2 600 |
| cycle (R+S) | 5 200 / 5 800 | 5 300 | **4 500** |
| iterations completed | 52 / 48 | 51 | **57** |

(Two figures for the no-drain arm because it was run as the control for both.)

Waiting for the drain is a straight trade and the wrong way round: 1 200 ms off
suspend, 1 300 ms onto resume, which is the latency an actor's caller sits
through. Backgrounding it is free, because the 2.4 s the guest spends cold
booting is 2.4 s in which it asks the device for nothing — the drain fits inside
a window that was already idle. `Drained the durable-dir filesystem` reports a
p50 of 1 302 ms against a `ResumeActor` cost of 400 ms, so most of it lands in
that gap.

The cycle improves by 1 300 ms, which is outside the ±600 ms band established
below, and the arm ordering works against the result rather than for it.

> [!NOTE]
> None of this is specific to qcow2. Any arrangement whose restore path writes
> a hundred megabytes and whose guest then calls `sync(2)` pays the same bill.
> The tar arrangement never sees it only because virtio-fs is write-through and
> it has no pre-suspend flush at all — the same bytes are still written, just by
> the kernel's own writeback, off any measured path.

### Against tar, with the drain in

The partial-update comparison above was run before any of this, and its qcow2
arm was paying the flush. Re-run against tar in one session on one node,
2026-09-04 03:29Z to 03:57Z, `MAX_CHAIN=8`, 6m an arm, qcow2 first in each pair
so the drift works against it. p50 in milliseconds:

| | tar 128 | qcow2 128 | tar 512 | qcow2 512 |
|---|---|---|---|---|
| `durable_dir` (the seal) | 753.4 | **7.9** | 7 058.7 | **8.2** |
| `guest_flush` | 0.0 | 37.5 | 0.0 | 76.6 |
| suspend total, server | 816.1 | **116.3** | 7 155.8 | **175.4** |
| `SuspendActor` | 2 400 | **1 900** | 9 900 | **4 000** |
| `ResumeActor` | **1 700** | 2 400 | **3 200** | 6 100 |
| **cycle (R+S)** | **4 100** | 4 300 | 13 100 | **10 100** |

**128 MiB partial goes from tar by 1.24× to a tie** — 200 ms apart, inside the
band — and qcow2 now takes the suspend half of it outright, 1 900 against
2 400. **512 MiB partial goes from qcow2 by 1.09× to qcow2 by 1.30×**, 3 000 ms
of cycle.

The shape of the trade is unchanged and is now clean enough to state: qcow2
moves work off suspend and onto resume. At 512 MiB it moves 5 900 ms of suspend
for 2 900 ms of resume, and wins. At 128 MiB there is only 753 ms of tar seal to
move, which does not cover what resume costs, and it draws. Where the resume
goes, p50 in milliseconds:

| | tar 128 | qcow2 128 | tar 512 | qcow2 512 |
|---|---|---|---|---|
| restore total | 1 035.7 | 1 614.2 | 1 372.3 | 3 160.3 |
| `containers` (the guest mounting it) | 65.9 | 280.3 | 67.7 | 1 232.7 |
| `agent_dial` | 619.3 | 736.7 | 627.5 | 742.7 |
| flatten, when it fires | — | 1 010.5 (8/60) | — | 9 104.4 (3/26) |
| drain, in the background | — | 1 215.9 | — | 4 659.3 |

Three terms, in order of size: the amortized flatten (~135 ms a cycle at
128 MiB, ~1 050 at 512 MiB), the guest mounting an ext4 chain rather than a
virtio-fs share (`containers`, 4× and 18×), and the background drain competing
for bandwidth. Only the first has a known fix — follow-up 2 — and at 512 MiB it
is most of the gap.

### Multiqueue could not be tested

The disk's queue count follows the guest's vCPU count
([`run.go`](../../cmd/ateom-microvm/run.go), `NumQueues: int32(vcpus)`), and
`default_vcpus = 1` in the kata config, so the durable disk has a single
virtio-blk queue and cloud-hypervisor runs a single host thread for it. Raising
it fails at every value tried — 2, 4 and 8 alike:

```
Failed to validate config
Queue count (4) must not exceed boot vCPUs (1)
```

That is a cloud-hypervisor invariant, not a resource limit: `VmConfig::validate`
rejects any device whose queue count exceeds `boot_vcpus`. So **the single queue
is not independently adjustable** — testing multiqueue means giving the guest
more vCPUs, which changes the workload's compute as well and needs its own
design. The serialization hypothesis is untested, not refuted.

> [!NOTE]
> The failure surfaces on `vm.add-net`, which is misleading: the disk's queue
> count is what violates the rule, but cloud-hypervisor re-validates the whole
> config on every mutating API call and `vm.add-net` is simply the next one.
> Reading it at all required a fix — `AddNetWithFDs` and `RestoreWithNetFDs`
> logged the status line and dropped the response body, so every rejection read
> as `HTTP/1.1 500 Internal Server Error` regardless of cause.

## Chain depth has an optimum, and it is not 1 or 8

Both `MAX_CHAIN` values used above are extremes: 1 flattens on every restore, 8
is the shipped default. On the 128 MiB partial workload, with the same image and
three fresh goldens, 2026-09-03 16:34Z to 16:45Z, 3m each:

| `MAX_CHAIN` | n | guest_flush | seal | teardown | Flattens | Flatten p50 | Amortized | **Per cycle** |
|---|---|---|---|---|---|---|---|---|
| 1 | 10 | 927.4 | 7.0 | 68.5 | 10/10 | 926.2 | 926.2 | **1 932.2** |
| 4 | 26 | 1 033.1 | 7.2 | 66.7 | 8/26 | 1 026.1 | 315.7 | **1 425.8** |
| 8 | 25 | 1 337.0 | 8.4 | 66.4 | 3/25 | 1 358.0 | 163.0 | **1 577.9** |

"Per cycle" is the suspend phases plus the flatten amortized over the cycles
between flattens. **Depth 4 is the optimum**, and the throughput agrees
independently: 26 checkpoints in three minutes against 10 and 25.

The shape is a genuine trade-off rather than a tuning accident. Depth costs
~137 ms of `guest_flush` per extra layer, because the guest writes through a
deeper backing chain; shallowness costs a flatten on a larger fraction of
restores. `MAX_CHAIN=1` pays a 926 ms flatten on **100%** of restores, which is
why it is the worst of the three despite having the cheapest flush.

The flatten rate is 161–166 MiB/s at every depth, so the flatten itself is
linear in the chain and holds no surprises; only how often it fires changes.

> [!NOTE]
> cloud-hypervisor refuses to open a backing chain nested deeper than 11, so the
> knob has a hard ceiling well below where this curve would flatten out anyway.

> [!IMPORTANT]
> **Superseded — this optimum is an artifact of the per-layer flush cost, which
> the background drain removed.** Half of the trade-off above is the ~137 ms of
> `guest_flush` an extra layer costs, and with the drain in place `guest_flush`
> measures 35.1 ms at 2 layers and 38.8 ms at 5. With that side of the trade
> gone, only the flatten's frequency remains, and it falls monotonically with
> depth. The re-run confirms it: 4 is now the *worst* setting measured, and 8 and
> 11 tie. See [The chain depth sweep](#the-chain-depth-sweep).

## Write amplification

Node write counters sampled across a full run of the 128 MiB partial workload,
both arrangements, against the 16 MiB the workload actually writes per cycle:

| Arm | Cycles | Total written | Per cycle | Amplification | Avg request |
|---|---|---|---|---|---|
| tar | 32 | 4 935 MiB | 154 MiB | **10×** | 185.0 KiB |
| qcow2 | 27 | 7 248 MiB | 268 MiB | **17×** | 182.4 KiB |

Both arrangements amplify heavily and qcow2 amplifies 1.7× more. The request
sizes are indistinguishable, which is what rules out the small-I/O explanation
for the flush: whatever qcow2 costs, it does not cost it in badly-shaped writes.

> **Most of this is the stager, not either arrangement.** atelet byte-copies
> every file a local checkpoint names into the restore directory before ateom
> runs, which is one extra write of the whole durable data set per restore —
> the whole chain for qcow2, the tarball for tar. See [The dirty pages the drain
> waits for are atelet's, not
> ateom's](#the-dirty-pages-the-drain-waits-for-are-atelets-not-ateoms).

## The background flatten hits cloud-hypervisor's nesting limit

The flatten is the one operation left on the restore path that scales with the
actor's data, so the obvious move is [follow-up 2](#follow-ups): run it in the
background while the guest is up, with a synchronous backstop at twice
`MAX_CHAIN` for an actor that never gives it a window. Built, measured, and
reverted. What follows is why, because the failure is a property of the idea and
not of the implementation.

### It never finishes at 512 MiB, and finishing is not optional

| Arm | Flattens started | Completed | Abandoned at seal |
|---|---|---|---|
| qcow2 128 MiB | 7 | **7** | 0 |
| qcow2 512 MiB | 40 | **0** | **40** |

A 512 MiB durable dir takes 9.1 s to flatten. The actor's activation in this
workload is shorter than that, so every flatten was cancelled by the suspend
that ended the activation, and the chain grew by a layer on every cycle instead
of collapsing every eighth. Its depth walked 3, 4, 5 … and stopped at 12:

```
depth  3  4  5  6  7  8  9 10 11 12
lands  1  1  1  1  1  1  1  1  1 38
```

Eleven layers boots. Twelve fails `vm.boot` with

```
Cannot open disk path (path=.../durable-dir.layer-0011.qcow2)
Backing file open error: .../layer-0010.qcow2
  ... one per layer, down to layer-0001
"Maximum disk nesting depth exceeded"
```

which is cloud-hypervisor's cap of a top layer plus ten backing files. **38 of
that arm's 47 restores failed** — `ResumeActor` reported 29 failures out of 38
requests, 76.32%.

The cap is absorbing, which is what makes this a correctness bug rather than a
slow path. A boot that fails lands no new layer and collapses none, so the chain
that was one too deep is exactly as deep on the next attempt, and every
activation from then on fails the same way. The actor is dead until something
outside it rewrites the chain. The backstop did not help: at twice the shipped
`MAX_CHAIN` of 8 it sits at 16, five layers past what the hypervisor will open.

`MaxChain()` now clamps to eleven, so no configuration can ask for a chain CH
cannot follow, and the flatten is back on the restore path.

### It is not free at 128 MiB either

Where the background flatten *does* complete, it completes a cycle late, and the
chain is one layer deeper for the whole of that cycle:

| qcow2 128 MiB | Depth sawtooth | Chain bytes p50 | `ResumeActor` p50 |
|---|---|---|---|
| Flatten on the restore path | 2–8 | 233 MB | 2 400 ms |
| Flatten in the background | 3–9 | 300 MB | 3 000 ms |

Restore cost tracks the chain's total size, not the flatten's placement, so
moving the flatten out of the critical path and paying a deeper chain for it is
a losing trade in both directions this workload measures.

## The cycle at 512 MiB, with the flatten back on the restore path

Four arms, 6 minutes each, `MAX_CHAIN=8`, qcow2 first in each pair. Zero
failures anywhere; `nestfail=0` in all four. p50 in milliseconds.

| | qcow2 128 | tar 128 | qcow2 512 | tar 512 |
|---|---|---|---|---|
| `ResumeActor` | 3 200 | **2 800** | 10 000 | **7 600** |
| `SuspendActor` | **1 900** | 2 500 | **4 000** | 9 400 |
| Cycle | **5 100** | 5 300 | **14 000** | 17 000 |
| Checkpoints in 6 min | 104 | 104 | **40** | 34 |

**qcow2 loses resume and wins the cycle.** At 512 MiB it gives up 2 400 ms of
resume and takes back 5 400 ms of suspend, for 3 000 ms — 18% — off the round
trip, and 18% more cycles in the same wall clock. The suspend advantage is the
one that scales: sealing a chain is hardlinks and a manifest at any size, while
tar's suspend goes 2 500 → 9 400 ms between the two steps. The resume penalty
grows too, but more slowly.

At 128 MiB the two are a wash (5 100 against 5 300, inside the ±600 ms band this
report's caveats give for a cycle). The crossover is somewhere below 512 MiB and
this run does not locate it.

### Where the resume penalty is

atelet's own `Restore timing breakdown`, same run, p50 in milliseconds:

| | `download` | `ateom_restore` | atelet `total` |
|---|---|---|---|
| qcow2 128 | 1 419 | 1 563 | 3 133 |
| tar 128 | 1 602 | 1 035 | 2 719 |
| qcow2 512 | 6 669 | 2 700 | 10 046 |
| tar 512 | 5 483 | 1 840 | 7 548 |

`download` is the largest term in every arm and it is mostly *not* the durable
data: this is a `DATA_ON_GOLDEN` restore, so it fetches the golden snapshot from
object storage concurrently with copying the actor's own files off local disk,
and at 128 MiB the two backends are indistinguishable in it. The chain costs
**+1 186 ms** over one tar at 512 MiB and nothing measurable at 128.

The rest — +528 ms at 128, +860 ms at 512 — is inside ateom, and it is the
guest, not the landing:

| p50 ms | `agent_dial` | `containers` | `readyz` | `since_boot` | drain | chain MB | top MB |
|---|---|---|---|---|---|---|---|
| qcow2 128 | 694 | **300** | 146 | 1 173 | 1 209 | 233 | 18.0 |
| tar 128 | 616 | **65** | 142 | 825 | — | — | — |
| qcow2 512 | 699 | **1 247** | 152 | 2 122 | 4 171 | 823 | 68.5 |
| tar 512 | 819 | **69** | 148 | 1 106 | — | — | — |

`containers` is flat under tar across a 4× change in size (65 → 69 ms) and
near-linear under qcow2 (300 → 1 247). That shape is the whole explanation: a
virtio-fs read is a file-level request the host serves out of a page cache it
has just written, while a virtio-blk read is guest ext4 walking inode tables and
extent trees it has no cache for, in 4 KiB requests, each resolved through a
qcow2 L2 lookup into one of up to eight layer files. The guest's page cache is
empty on a cold boot and there is nothing on the qcow2 side that corresponds to
the host cache tar's arrangement reads through.

Note that ateom's pre-boot work is *faster* for qcow2 — `since_boot` is
+1 016 ms while `ateom_restore` is only +860 — because landing a chain is
hardlinks where landing a tar is an unpack. The arrangement wins the landing and
gives it back with interest in the guest's first reads.

### The dirty pages the drain waits for are atelet's, not ateom's

[Write amplification](#write-amplification) recorded that a restore leaves far
more dirty host page cache than the delta it landed, and could not say what
wrote it. It is atelet's staging: before ateom is called at all, atelet writes
every file the snapshot names into the restore directory, and for a chain that
is every layer. In these arms that is `downloadExternalCheckpoint` — the
restores all ran as `CHECKPOINT_TYPE_EXTERNAL` — and the sibling path
`copyLocalCheckpoint` would write the same bytes for a resume from a pause.

The arithmetic closes. At 512 MiB the chain is 823 MB, the node writes at a
measured 183 MB/s, and the drain takes 4 171 ms — 763 MB of writeback. ateom's
own contribution to that cycle is the 68.5 MB top layer. The staged files land
on the same filesystem ateom's `syncfs` covers, which is why the drain waits
for them.

So the drain is not cleaning up after the chain arrangement; it is cleaning up
after the stager, and tar pays a version of the same bill in its own `download`
column.

Hardlinking the staged files, so the restore directory shares inodes instead of
holding a second copy, is the obvious remedy and does not apply here: an
EXTERNAL restore's bytes arrive from object storage and there is no local inode
to share. It would help a resume from a pause, which none of these arms are.
Taking the cost off *this* path means not writing the bytes twice — having the
download land where ateom will read it. Either way it is an atelet change and
out of this branch's scope.

## The chain depth sweep

Two open questions, one set of arms. Whether the guest's cold-boot read cost is
the depth of the backing chain or something no amount of flattening reaches; and
what `MAX_CHAIN` should be now that the drain has removed the per-layer flush
cost the recorded optimum of 4 was measured against.

`durdir_partial_512mb_microvm`, the depth-clamping build, 8m per arm, a fresh
golden before each, 2026-09-04 08:10Z to 10:42Z. Caps of 1 and 2 are the same
experiment — both arrive at the cap on every restore, so both flatten every time
and both hand cloud-hypervisor a depth of 2 — so the sweep starts at 2. Eleven is
the deepest chain CH will open. tar runs first and last on an unchanged binary,
because the previous session moved a tar arm's resume p50 by more than 2× and an
in-session bracket is the only thing that separates a setting from a day.

`nestfail=0` in every arm, and the deepest chain each arm handed CH was exactly
its cap — 2, 4, 8, 11. The `MAX_CHAIN=11` arm ran 36 restores at the clamp's
boundary without a single nesting failure, which is the first direct
confirmation that [`maxNestingDepth`](#the-background-flatten-hits-cloud-hypervisors-nesting-limit)
is set to the right number rather than a safe underestimate.

### Depth is not what the guest's first reads cost

Every restore logs the depth it handed CH, and the `Actor boot phases` record
for the boot that followed carries the same `trace_id`, so the pairs recover
exactly and pool across arms. This matters because the cap is a ceiling, not a
setting: the chain sawtooths between 2 and the cap inside a single arm, so an
arm-level mean measures a mixture and not a depth.

| depth | 2 | 3 | 4 | 5 | 6 | 7 | 8 | 9 | 10 | 11 |
|---|---|---|---|---|---|---|---|---|---|---|
| `containers` p50 | 1 059 | 1 192 | 1 243 | 1 225 | 1 158 | 1 082 | 1 278 | 1 023 | 1 390 | 951 |
| n | 40 | 15 | 18 | 9 | 9 | 9 | 9 | 4 | 3 | 3 |

Flat, across a 5.5× range of depth. Whatever the guest is paying for on its
first reads, it is not the L2 lookup per layer.

This also kills the other candidate. The 40 samples at depth 2 are all boots
that immediately followed a flatten, and a flatten is a `qemu-img convert` that
has just written the entire image — so if the cost were device reads that a warm
host page cache would absorb, those boots would be the fast ones. They are not;
1 059 ms is the middle of the band. A cheap prefetch of the chain into the host
cache before boot would buy nothing.

What is left is the guest side: an empty guest page cache, and ext4 walking
inode tables and extent trees in 4 KiB requests. Only a restore that brings the
guest's page cache back with it — a `FULL` scope — attacks that, which is
follow-up 2.

> The same table on the 128 MiB workload does trend, 123 ms at depth 5 to 434 at
> depth 9. Depth is a real cost there and is simply swamped at 512 MiB, where
> the volume of guest reads is four times larger and the depth term is not.

### The default stays at 8

| arm | `ResumeActor` | `SuspendActor` | cycle | checkpoints | flattens | drain | `containers` |
|---|---|---|---|---|---|---|---|
| tar (first) | 5 100 | 7 200 | 12 300 | 62 | — | — | 69 |
| qcow2 `MAX_CHAIN=2` | 8 700 | 4 600 | 13 300 | 52 | every restore | 3 416 | 1 097 |
| qcow2 `MAX_CHAIN=4` | 9 800 | 4 500 | 14 300 | 52 | 8/26 | 3 781 | 1 188 |
| qcow2 `MAX_CHAIN=8` | 5 900 | 4 300 | 10 200 | 68 | 4/34 | 4 936 | 1 224 |
| qcow2 `MAX_CHAIN=11` | 5 300 | 4 400 | 9 700 | 74 | 3/37 | 5 350 | 1 147 |
| tar (last) | 3 100 | 9 800 | 12 900 | 60 | — | — | 66 |

p50 in milliseconds. Locust n is 25–37 per arm.

**Read the raw resume column and you will get the wrong answer.** The arms ran in
increasing order of depth, and over the same session the tar control's resume
p50 fell 5 100 → 3 100, or 39%. The qcow2 resume fell 8 700 → 5 300 over the same
span — also 39%. Depth and elapsed session time are collinear here, and the raw
column cannot tell them apart.

Normalizing each qcow2 arm against the tar control linearly interpolated between
the two brackets:

| ÷ tar | `MAX_CHAIN=2` | `MAX_CHAIN=4` | `MAX_CHAIN=8` | `MAX_CHAIN=11` |
|---|---|---|---|---|
| `ResumeActor` | 1.85 | 2.28 | **1.51** | **1.51** |
| `SuspendActor` | 0.60 | 0.55 | **0.49** | **0.47** |
| cycle | 1.07 | 1.14 | **0.81** | **0.76** |

Three things fall out.

**Shallow is expensive and the recorded optimum of 4 is now the worst setting
measured.** The trade the old sweep described has lost one of its two sides: an
extra layer no longer costs a flush, so nothing pushes back against depth, and
all that remains is the flatten's frequency — 9.1 s at this size, on the restore
path, on 100% of restores at a cap of 2 and a third of them at 4.

**8 and 11 are indistinguishable once the drift is removed**, at 1.51 and 1.51 on
resume. The gap in the raw column is the session, not the setting. So there is no
case for raising the default to sit against cloud-hypervisor's hard limit, where
a miscount in either direction is a dead actor rather than a slow restore.

**The cycle result reproduces.** qcow2 at the default runs the full cycle in
0.76–0.81× of tar, against 0.82× measured independently in [the previous
run](#the-cycle-at-512-mib-with-the-flatten-back-on-the-restore-path), with
throughput agreeing at +20%. The suspend win and the resume loss both reproduce
too, at 0.49× and 1.51×.

That resume figure is the whole of the remaining case against this arrangement,
and the sweep says no setting of this knob addresses it. Both of its terms are
elsewhere: atelet's copy, and the guest's cold page cache.

> The drain grows monotonically with depth, 3 416 ms to 5 350, because a deeper
> chain is more bytes for atelet to copy. It is backgrounded and does not show up
> in the cycle, which is the drain working as intended.

## Caveats

* **The control drifted hard between sessions and only within-run comparisons
  hold.** On an unchanged tar arm, `ResumeActor` p50 went 1 700 → 2 800 ms at
  128 MiB and 3 200 → 7 600 ms at 512 MiB between the 05:08Z and 06:26Z runs on
  2026-09-04. The device was re-measured at 183 MB/s against 176 earlier and the
  filesystem was 37% full, so it is not the disk. Nothing in this report should
  be compared across sessions; every table above pairs its arms inside one run
  for that reason.
* **The run-to-run band is ±100-300 ms on `guest_flush` and ±600 ms on the
  cycle, and it is not symmetric.** Two identical no-drain arms run back to back
  measured 1 219.8 and 1 311.6 ms; five no-drain-equivalent runs on one day
  measured 937 / 1 028 / 1 220 / 1 312 / 1 482. Worse, **the second arm of a
  pair measured slower in every pair run** — +92 ms and +287 ms on `guest_flush`
  — so this is drift, not noise, and an A/B that puts its hypothesis second will
  manufacture a result. Every pair from the drain work onward runs the
  hypothesis first for that reason. Nothing in this report smaller than ~600 ms
  of cycle should be read as a difference, including cells where an earlier
  section drew one.
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
* **The flush breakdown ran on a scratch build.** The `sync` / `syncfs` / `noop`
  selector, the staging-versus-round-trip split, the `Direct` flag and the queue
  override are all environment variables added for these experiments and not
  part of the branch. Only the response-body fix in `AddNetWithFDs` and
  `RestoreWithNetFDs` is meant to be kept.
* **The partial-update runs are one-eighth deltas on eight equal files.** A
  real workspace has an uneven file-size distribution and a delta that moves
  between cycles. Nothing here says how the chain behaves when the same clusters
  are rewritten repeatedly, which is the case where COW should pay best.
* **No metadata or small-file scenario was run.** Every workload in this report
  writes a handful of large files. An arrangement's behavior on many small files
  is a different question, and virtio-fs and ext4-on-virtio-blk have no reason
  to rank the same way on it.

## Follow-ups

1. **Make atelet's staging stop writing the data set twice.** This is the
   largest single item on the restore path: `download` is the biggest term in
   every arm measured, ~1.2 s of qcow2's 512 MiB resume, and all of qcow2's
   drain — which exists only because those pages are dirty.

   Hardlinking looked like the answer and is not. Every restore in every arm
   ran as
   `CHECKPOINT_TYPE_EXTERNAL`, which fetches from object storage; there is no
   local inode to link and `copyLocalCheckpoint` was never called. Sharing an
   inode helps a resume from a pause and nothing else, and no scenario in the
   benchmark suite produces one — the locust workloads drive `SuspendActor`
   and `ResumeActor`, and a suspend uploads. A scenario that pauses is the
   precondition for measuring that change at all.

   The allow-list this branch carries is `ateompath.DurableDirSnapshotFile`, so
   it already covers a chain — `durable-dir.chain.json` and every
   `durable-dir.layer-*.qcow2` — and not the tar alone. That is sound here
   because `landDurableQcow2` stacks a fresh top layer and never writes through
   to an adopted one. The version proposed upstream shares `durable-dir.tar`
   alone, since the chain's names do not exist there; widening it is a change
   that only makes sense alongside the arrangement that writes them.

   What would take the cost off the measured path is not writing the bytes
   twice: staging the download directly where ateom will read it. atelet, not
   this branch.
2. **Test a `FULL` scope restore.** `containers` is the other half of the resume
   gap — 1 247 ms against tar's 69 at 512 MiB — and [the sweep](#the-chain-depth-sweep)
   has eliminated every explanation for it but one. It is not chain depth, which
   the cost is flat across from 2 to 11; and it is not cold host page cache,
   since the boots that follow a flatten sit in the middle of the band despite
   `qemu-img convert` having just written the whole image. What is left is the
   guest's own page cache being empty and ext4 walking metadata in 4 KiB reads,
   and the only restore that brings a guest page cache back with it is a `FULL`
   one. This is now the only untried lever on the second half of the resume gap.
3. Re-run the two largest steps at their committed 20m and 30m durations, now
   that the route timeout no longer caps a write at 10 s. Until then the sweep
   has no valid data point above 500 MiB.
4. Reduce the `guest_flush` lottery, or stop reporting `SuspendActor` at the
   middle sizes as a comparison. A term that swings 39 ms to 2 789 ms at
   256 MiB between runs of the same configuration makes those cells
   uninterpretable — see [A caution about `SuspendActor` at the middle
   sizes](#a-caution-about-suspendactor-at-the-middle-sizes).
5. Try discard. Worth doing for the storage and transfer footprint rather than
   for resume latency — see the bloat table above for why it does nothing at
   256 MiB and above.
6. Measure a workload whose delta lands on the same clusters each cycle. The
   partial runs rewrite a different file each cycle, so consecutive layers still
   share nothing and the chain grows by a full delta every time. The case COW is
   actually for — repeated edits to the same region — has not been measured, and
   it is the one where depth should pay for itself rather than cost.
7. Measure a metadata- or small-file-heavy scenario. Nothing in this report
   distinguishes virtio-fs from ext4-on-virtio-blk on anything but bulk
   throughput.
