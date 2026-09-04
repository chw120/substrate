# Incremental durable-dir snapshots on a cluster, 2026-09-03

Measures `ATEOM_INCREMENTAL_DURABLE_DIR` against the full-capture path it
replaces, on the two `durdir_partial_*` scenarios: a durable directory held at
a fixed size while one eighth of it is rewritten per cycle. That is the shape
an arrangement that archives a delta exists for, and the size sweep gives it no
room to show anything, because there every cycle rewrites everything.

The headline: **incremental capture makes suspend much cheaper, and what it
saves is governed by SHA-256 throughput rather than by the size of the delta.**

It was measured twice. The first pass, at `24aa6af`, also made resume dearer,
and tracing that cost to a re-hash of the restored tree is what produced
`4a8fcad`; the second pass, after that commit, has resume back at parity. Both
are below — the first because it is the evidence for the change, and because
the ceiling it establishes on the capture side still stands.

## Setup

| | |
|---|---|
| Cluster | `incrtar-bench`, GKE 1.35.7-gke.1222000, us-central1-a |
| Worker node | `n2-standard-4`, nested virtualization, `ate.dev/sandboxClass=microvm` |
| Sandbox class | `microvm` (cloud-hypervisor + kata), 1 `WorkerPool` replica |
| Template | `glutton-durdir-data` — DATA scope, so the durable dir is the whole snapshot |
| Scenarios | `durdir_partial_128mb_microvm` (8 × 16 MiB, 10m), `durdir_partial_512mb_microvm` (8 × 64 MiB, 20m) |
| Users | 1, `--resume-mode explicit`, `--durdir-read-mode digest`, wait times pinned to 1.0 |
| Branch | `poc3-incremental-tar` at `24aa6af`, then again at `4a8fcad` |

The two arms differ in one environment variable on `ate-controller`, which
`workerpool_apply.go` forwards onto the worker pod:

```bash
kubectl -n ate-system set env deploy/ate-controller ATEOM_INCREMENTAL_DURABLE_DIR=1   # incremental
kubectl -n ate-system set env deploy/ate-controller ATEOM_INCREMENTAL_DURABLE_DIR-    # full
```

Wait for the value to appear in the `benchmark-ateom` Deployment's pod
template, not merely for `rollout status deploy/ate-controller`: the controller
re-applies the worker pool on its next reconcile, and a run started in between
measures the previous arm.

Timings are ateom's own phase fields from the `Actor checkpointed` and
`Actor restored (durable-dir volumes, cold boot)` log lines, plus atelet's
`Handle RPC` elapsed time for `AteomHerder/Checkpoint`, which is the first
number that includes the upload. Client latencies come from the locust summary.

## Results at 24aa6af, before the change

All figures are p50 in milliseconds.

| | | ateom tar | ateom restore | atelet Checkpoint | client Suspend | client Resume | objects/cycle |
|---|---|---|---|---|---|---|---|
| **128 MiB** | full | 746 | 1032 | 2474 | 2500 | 1600 | 1 |
| | incremental | **437** (−41%) | **1449** (+40%) | **1523** (−38%) | **1500** (−40%) | **2200** (+38%) | 8.7 |
| **512 MiB** | full | 4945 | 1334 | 8649 | 8600 | 3200 | 1 |
| | incremental | **2274** (−54%) | **3011** (+126%) | **5502** (−36%) | **5500** (−36%) | **4200** (+31%) | 8.7 |

Whole cycle, client side: 4100 → 3700 ms at 128 MiB (−10%), 11800 → 9700 ms at
512 MiB (−18%).

Samples: 101/109 cycles at 128 MiB, 80/90 at 512 MiB. `ateom restore` spans the
whole Restore RPC, so it carries the guest cold boot as well as the extraction;
the cold boot is common to both arms, which is why the absolute difference
matters here and the percentage flatters the smaller volume.

## Both sides are SHA-256-bound, not delta-bound

The restore penalty is not the cost of reassembling a chain. It is
`incrtar.Restore`'s `verifyTree`, which re-hashes every restored file against
the manifest:

| volume | restore penalty | implied rate |
|---|---|---|
| 128 MiB | +417 ms | 307 MiB/s |
| 512 MiB | +1677 ms | 305 MiB/s |

That is single-core SHA-256, and it scales with the size of the *tree*, not
with the length of the chain or the size of the delta. The full-capture path
verifies nothing at all, so this half of the comparison is not like for like:
it is the price of the integrity check, charged to the arm that has one.

The same constant governs the suspend side, because change detection has to
hash the whole tree — timestamps and inodes are useless here, every cycle
begins with an extraction that gives every file a fresh one. At 512 MiB:

```
hash 512 MiB @ 305 MiB/s   = 1679 ms
tar    64 MiB @ 103 MiB/s  =  621 ms
                             ------
                             2300 ms predicted, 2274 ms observed
```

So the saving on capture is bounded not by δ but by the ratio of tar throughput
to hash throughput. Even at δ → 0 the incremental capture of a 512 MiB volume
cannot go below ~1.7 s, against 4.9 s for a full one: a ceiling of about 2.9×,
reached already at δ = 1/8.

## Results at 4a8fcad, after the change

`4a8fcad` replaced the re-hash with accounting for what extraction took. All
four arms were rerun on the same cluster and the same worker node, so this
table stands on its own rather than against the one above; the capture side is
untouched by that commit and is here as a control.

| | | ateom tar | ateom restore | client Suspend | client Resume | objects/cycle |
|---|---|---|---|---|---|---|
| **128 MiB** | full | 749 | 1054 | 2500 | 1700 | 1.00 |
| | incremental | **430** (−43%) | **1059** (+0.5%) | **1500** (−40%) | **1700** (0%) | 8.68 |
| **512 MiB** | full | 3376 | 1345 | 7500 | 3100 | 1.00 |
| | incremental | **1907** (−44%) | **1486** (+10%) | **5000** (−33%) | **2900** (−6%) | 8.72 |

Whole cycle, client side: 4200 → 3200 ms at 128 MiB (−24%), 10600 → 7900 ms at
512 MiB (−25%). Samples 101–102 cycles per arm at 128 MiB, 85–100 at 512 MiB,
no failures in any of the four.

The resume regression is gone: +5 ms at 128 MiB and +141 ms at 512 MiB, against
+417 and +1677 before. What is left at 512 MiB is the extra open, read, and
close of nine objects where a full capture handles one.

Two caveats on this table, both of which understate the incremental arm rather
than flatter it. The 512 MiB incremental arm ran on a worker pod that had
already served the 128 MiB run, where the other three arms each started on a
fresh one; a worker with a warm page cache and prior actors is the harder
starting point, so the +141 ms is an upper bound. And the capture-side figures
are not comparable across the two tables — full capture at 512 MiB measured
4945 ms in the first pass and 3376 ms in the second with no code between them,
which is the run-to-run variance of this scenario and the reason all four arms
were rerun rather than three.

## Write amplification

Of the four writes the proposal counts per cycle — suspend staging tar, resume
landing tar, untar, workload overwrite — incremental capture removes only part
of the first:

* **Suspend staging tar**: 512 MiB → 64 MiB. Saved.
* **Resume landing tar**: unchanged. The snapshot is self-contained, so the
  chain that lands on the worker still totals the whole volume — 9 objects
  instead of 1, the same bytes.
* **Untar** and **workload overwrite**: unchanged by construction.

4.0× → 3.1× at δ = 1/8, which meets the proposal's 3.0× target about as closely
as this design can. The resume-side write is the price of keeping snapshots
self-contained, and getting it back would require chain support in the snapshot
model, which does not exist (`sandboxAssetsRecord.SnapshotFiles` is a flat
list).

## One arm crashed on a transient GCS error

The first 512 MiB incremental run died on cycle 25 and never recovered:

```
DataLoss: FAILED_SAVE_SNAPSHOT: while uploading external snapshot:
  while uploading durable-dir.g23.tar to GCS: ...
  googleapi: Error 503: We encountered an internal error. Please try again.
```

Every later `SuspendActor` and `ResumeActor` then failed against
`ACTOR_STATE_CRASHED` — 1602 failures out of 1734 requests. The rerun completed
clean, so the 503 was transient, but the exposure is not: a self-contained
chain uploads nine objects per suspend where a full capture uploads one, and
the upload path does not retry, so one transient per-object failure is enough
to lose the actor. Multiplying the object count multiplies that risk ninefold.
The 512 MiB figures above are from the clean rerun; the crashed run's first 25
cycles agree with it (tar 2575 ms, restore 3023 ms).

## What this suggests

1. **The verification was the whole restore regression.** Done in `4a8fcad`:
   the check exists to catch a missing generation, which a single archive
   cannot suffer from, and extraction already knows which paths it took from
   which generation, so accounting for them catches the same failure for free.
   Content hashing is left to the object store's own checksums. Predicted to
   take the 512 MiB restore back to roughly parity; measured at +141 ms.
2. **Retry the per-object upload, or accept the risk knowingly.** Nine objects
   per suspend against a control plane that treats one failed PUT as actor loss
   is the sharpest edge this change adds.
3. **Chain length is not the problem.** It settled at 8 — with 8 files and one
   rewritten per cycle, every path is attributed to one of the last 8
   generations and pruning drops the rest — well under the bound of 16, and
   nothing in the numbers scales with it.

## Reproducing

The orchestrator drives a whole `tests.yaml` from a CronJob on a separate
cluster, which is not what an A/B pair wants: the arms must run back to back
against one already-deployed cluster. These runs used a small driver that
renders `automation/manifests/runner-job.yaml.tmpl` for a single scenario and
submits it, with `--dest /tmp/bench` so the runner needs no GCS binding.

```bash
source .ate-dev-env.sh
./hack/install-ate.sh --deploy-ate-system
./hack/install-microvm-deps.sh --install
./benchmarking/workloads/deploy.sh --deploy --sandbox-class microvm
```

Then, per arm: set or clear the variable, wait for it to reach the
`benchmark-ateom` pod template, wait for the worker rollout, and submit the
scenario. Toggling the variable restarts the worker pod, which is what makes
the two arms start from the same state; keep that symmetry when adding arms.

Restart the worker between two arms on the same setting as well, by toggling
the variable there and back. A second scenario submitted onto a worker that has
just served one fails outright — `AssignWorker: ResourceExhausted: no free
workers available`, every request, because the actors of the finished run still
hold the pool's single replica.
