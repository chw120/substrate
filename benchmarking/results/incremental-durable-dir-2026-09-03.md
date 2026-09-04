# Incremental durable-dir snapshots on a cluster — 2026-09-03

Measures `ATEOM_INCREMENTAL_DURABLE_DIR` against the full-capture path it
replaces, on the two `durdir_partial_*` scenarios: a durable directory held at
a fixed size while one eighth of it is rewritten per cycle. That is the shape
an arrangement that archives a delta exists for, and the size sweep gives it no
room to show anything, because there every cycle rewrites everything.

It was measured twice. The first pass said incremental capture makes suspend
much cheaper and resume much dearer; the resume half turned out to be a re-hash
of the restored tree that the full path has no equivalent of, and removing it
is `4a8fcad`. The second pass, after that commit, has resume at parity.
**Both passes are below, because the ceiling the first one establishes on the
capture side still stands.**

## Conclusion

Incremental capture is worth about a quarter of the suspend/resume cycle at
both volume sizes, and the saving is bounded by SHA-256 throughput rather than
by the size of the delta.

| | | ateom tar | ateom restore | client Suspend | client Resume | objects/cycle |
|---|---|---|---|---|---|---|
| **128 MiB** | full | 749 | 1 054 | 2 500 | 1 700 | 1.00 |
| | incremental | **430** (−43%) | 1 059 (+0.5%) | **1 500** (−40%) | 1 700 (0%) | 8.68 |
| **512 MiB** | full | 3 376 | 1 345 | 7 500 | 3 100 | 1.00 |
| | incremental | **1 907** (−44%) | 1 486 (+10%) | **5 000** (−33%) | **2 900** (−6%) | 8.72 |

Whole cycle, client side: 4 200 → 3 200 ms at 128 MiB (−24%), 10 600 → 7 900 ms
at 512 MiB (−25%). All figures p50 in milliseconds, `4a8fcad`, four arms rerun
on one cluster and one worker node.

Two things stop it being ready to enable by default, neither of them a
performance result:

* **Nine objects per suspend against an upload path that does not retry.** One
  transient GCS 503 is enough to lose the actor permanently, and it happened
  once in five runs.
* **δ = 1 was not measured and is a loss by construction.** At δ = 1 an
  incremental capture is a full tar plus a full hash, so the penalty is exactly
  the hash — 1 679 ms at 512 MiB, +34% to +50% on the capture step depending on
  the tar rate. A durable directory that is one large file rewritten every
  cycle is the common agent shape, and it needs the fall-back-to-full policy
  that does not exist yet.

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

Timings are ateom's own phase fields from the `Actor checkpointed` and
`Actor restored (durable-dir volumes, cold boot)` log lines. `ateom tar` is the
`durable_dir` field: building the archive, not uploading it. `ateom restore`
spans the whole Restore RPC, so it carries the guest cold boot as well as the
extraction; the cold boot is common to both arms, which is why the absolute
difference matters and the percentage flatters the smaller volume. Client
latencies come from the locust summary.

## 1. The first pass said: suspend −41/−54%, resume +40/+126%

At `24aa6af`:

| | | ateom tar | ateom restore | atelet Checkpoint | client Suspend | client Resume |
|---|---|---|---|---|---|---|
| **128 MiB** | full | 746 | 1 032 | 2 474 | 2 500 | 1 600 |
| | incremental | 437 (−41%) | 1 449 (**+40%**) | 1 523 (−38%) | 1 500 (−40%) | 2 200 (**+38%**) |
| **512 MiB** | full | 4 945 | 1 334 | 8 649 | 8 600 | 3 200 |
| | incremental | 2 274 (−54%) | 3 011 (**+126%**) | 5 502 (−36%) | 5 500 (−36%) | 4 200 (**+31%**) |

Samples 101/109 cycles at 128 MiB, 80/90 at 512 MiB. Whole cycle: −10% at
128 MiB, −18% at 512 MiB. The obvious reading is that reassembling a chain of
nine archives costs what capturing a delta saves, and that the scheme is
roughly a wash.

## 2. It is not the chain. It is one constant.

| volume | restore penalty | implied rate |
|---|---|---|
| 128 MiB | +417 ms | 307 MiB/s |
| 512 MiB | +1 677 ms | 305 MiB/s |

One number at both sizes, scaling with the size of the **tree** rather than
with the length of the chain or the size of the delta. Chain length was 8 in
both, so a per-archive cost would not scale with volume size at all, and a
per-byte cost of reassembly would be the extraction, which both arms pay.

`incrtar.Restore` re-read and re-hashed every file it had just written, in
`verifyTree`. The full-capture path verifies nothing, so this half of the
comparison was never like for like: it is the price of an integrity check,
charged to the only arm that has one.

## 3. The check can be had for free

**`4a8fcad`.** A chain has one failure mode a single archive does not: an
archive that is not the one the manifest was written against, whether because a
generation is missing or because the wrong object was supplied for it.
Extraction already knows which paths it took from which generation, so
recording that and comparing it with the manifest catches exactly that failure
at no cost.

What the hash added on top was detection of an archive whose bytes are corrupt
but whose entries still have the right names and sizes — and the object store
checksums its own objects against precisely that. Size and symlink targets are
still checked; `openForHashing`, which widened the mode of an unreadable file
just long enough to read it, went with the hash.

## 4. Re-measured, all four arms

At `4a8fcad`, same cluster and same node, capture side untouched by the commit
and rerun as a control:

| | | ateom tar | ateom restore | client Suspend | client Resume | objects/cycle |
|---|---|---|---|---|---|---|
| **128 MiB** | full | 749 | 1 054 | 2 500 | 1 700 | 1.00 |
| | incremental | **430** (−43%) | **1 059** (+0.5%) | **1 500** (−40%) | **1 700** (0%) | 8.68 |
| **512 MiB** | full | 3 376 | 1 345 | 7 500 | 3 100 | 1.00 |
| | incremental | **1 907** (−44%) | **1 486** (+10%) | **5 000** (−33%) | **2 900** (−6%) | 8.72 |

101–102 cycles per arm at 128 MiB, 85–100 at 512 MiB, no failures in any of the
four. The resume regression is gone: **+5 ms at 128 MiB and +141 ms at
512 MiB**, against +417 and +1 677. What is left at 512 MiB is the extra open,
read, and close of nine objects where a full capture handles one.

Two caveats, both of which understate the incremental arm rather than flatter
it. The 512 MiB incremental arm ran on a worker pod that had already served the
128 MiB run, where the other three each started on a fresh one; a warm worker
is the harder starting point, so +141 ms is an upper bound. And the capture-side
figures are not comparable across the two passes — full capture at 512 MiB
measured 4 945 ms in the first and 3 376 ms in the second with no code between
them, which is the run-to-run variance of this scenario and the reason all four
arms were rerun rather than three.

## 5. The chain reassembles correctly

The workload's own check does not cover this: `readDisk` reads back only the
file the cycle just wrote, which always comes from the newest archive. The
seven files served by older generations are never read back by the client.

The capture-side scan covers it instead, one cycle later and through an
independent code path. It hashes the **restored** tree and compares against the
previous manifest, so a file whose restored bytes or metadata differed would be
marked changed and swept into the new archive, showing up as an extra
generation that cycle.

Across **224 cycle transitions** — 125 at 128 MiB, 99 at 512 MiB — every single
one added exactly one new generation, and the chain held at 8. That is roughly
1 500 restores of files inherited from archives one to seven cycles old, each
one matching what the manifest recorded.

## 6. The remaining bottleneck: SHA-256, on capture, unavoidable

Change detection has to hash the whole tree. Timestamps and inodes are useless
here — every cycle begins with an extraction that gives every file a fresh one.
At 512 MiB, using the first pass's rates:

```
hash 512 MiB @ 305 MiB/s   = 1 679 ms
tar    64 MiB @ 103 MiB/s  =   621 ms
                             -------
                             2 300 ms predicted, 2 274 ms observed
```

So the saving is bounded not by δ but by the ratio of tar throughput to hash
throughput. Even at δ → 0 the incremental capture of a 512 MiB volume cannot go
below ~1.7 s against 4.9 s for a full one: **a ceiling of about 2.9×, reached
already at δ = 1/8.**

The rate is what the hardware gives. The node has no `sha_ni` — `n2` is Cascade
Lake, flags `avx2` and `avx512f` only — so Go takes the AVX2 path at roughly
9 cycles/byte: 2.8 GHz ÷ 9 = 297 MiB/s, against 305 measured. That is ALU-bound,
not memory-bandwidth-bound; 305 MiB/s is a rounding error against this node's
DRAM bandwidth.

[`scan.go`](../../cmd/ateom-microvm/internal/incrtar/scan.go) hashes inline in a
single `filepath.WalkDir` callback, so all of this is on one core of four, and
the ateom container has no CPU limit.

## 7. Write amplification

Of the four writes per cycle — suspend staging tar, resume landing tar, untar,
workload overwrite — incremental capture removes only part of the first:

* **Suspend staging tar**: 512 MiB → 64 MiB. Saved.
* **Resume landing tar**: unchanged. The snapshot is self-contained, so the
  chain that lands on the worker still totals the whole volume — 9 objects
  instead of 1, the same bytes.
* **Untar** and **workload overwrite**: unchanged by construction.

4.0× → 3.1× at δ = 1/8, which meets the proposal's 3.0× target about as closely
as this design can. Getting the resume-side write back would require chain
support in the snapshot model, which does not exist
(`sandboxAssetsRecord.SnapshotFiles` is a flat list). **What this change buys is
CPU and staging write, not network or storage.**

## 8. One arm crashed on a transient GCS error

The first 512 MiB incremental run died on cycle 25 and never recovered:

```
DataLoss: FAILED_SAVE_SNAPSHOT: while uploading external snapshot:
  while uploading durable-dir.g23.tar to GCS: ...
  googleapi: Error 503: We encountered an internal error. Please try again.
```

Every later `SuspendActor` and `ResumeActor` then failed against
`ACTOR_STATE_CRASHED` — 1 602 failures out of 1 734 requests. The rerun
completed clean, so the 503 was transient, but the exposure is not: a
self-contained chain uploads nine objects per suspend where a full capture
uploads one, and the upload path does not retry, so one transient per-object
failure is enough to lose the actor. **Multiplying the object count multiplies
that risk ninefold.** The first-pass 512 MiB figures are from the clean rerun;
the crashed run's first 25 cycles agree with it (tar 2 575 ms, restore
3 023 ms).

## Where this leaves it

* **The verification cost is gone and should stay gone.** Restoring the
  re-hash would buy only detection of what the object store's checksum already
  covers, at the price of a second pass over every byte on every resume.
* **Retry the per-object upload.** Nine objects per suspend against a control
  plane that treats one failed PUT as actor loss is the sharpest edge this
  change adds, and the fix is small. This is the one blocking item.
* **Build the fall-back-to-full policy (I4) before enabling by default.** At
  δ = 1 incremental is a full tar plus a full hash and loses by exactly the
  hash time.
* **Chain length is not a problem.** It settled at 8 — with 8 files and one
  rewritten per cycle, every path is attributed to one of the last 8
  generations and pruning drops the rest — well under the bound of 16, and
  nothing in the numbers scales with it.
* **Raising the ceiling, in order of cost.** Run on a CPU with SHA-NI (`n2d`,
  `c3`) for 5–6× on the hash and no code change at all; hash in parallel across
  the four cores for close to 4× more, which needs files ≥ cores and does
  nothing for a single large file; vendor blake3 or xxh128; or keep the durable
  dir warm on the worker so mtime becomes trustworthy again and the hash can be
  skipped entirely. Only the last removes the cost rather than dividing it.

**Not yet confirmed:** δ = 1 is projected, not measured — the arithmetic says a
full tar plus a full hash, but nothing has been run in that shape. The parallel-
hash speedup is likewise a projection from the single-core rate and the core
count.

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
`benchmark-ateom` pod template — not merely for `rollout status
deploy/ate-controller`, since the controller re-applies the worker pool on its
next reconcile and a run started in between measures the previous arm — wait for
the worker rollout, and submit the scenario.

Restart the worker between two arms on the same setting as well, by toggling the
variable there and back. A second scenario submitted onto a worker that has just
served one fails outright — `AssignWorker: ResourceExhausted: no free workers
available`, every request, because the actors of the finished run still hold the
pool's single replica.
