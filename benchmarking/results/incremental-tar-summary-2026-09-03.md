# Incremental durable-dir snapshots — what we found — 2026-09-03

A one-page summary. The full measurement is in
[incremental-durable-dir-2026-09-03.md](incremental-durable-dir-2026-09-03.md).

## What we were testing

Whether archiving only what changed should replace the full re-tar of the
durable directory at every suspend. The scenario is the one the scheme exists
for: a directory held at a fixed size with one eighth of it rewritten per cycle
(8 files, one per cycle). The question was how much of the suspend cost that
actually removes, and what it costs elsewhere.

## 1. The first benchmark said: suspend is half the price, resume is double

At 512 MiB, building the archive dropped from 4 945 to 2 274 ms — and restore
went the other way, 1 334 → 3 011 ms:

| | ateom tar | ateom restore | client Suspend | client Resume |
|---|---|---|---|---|
| full | 4 945 | 1 334 | 8 600 | 3 200 |
| incremental | 2 274 (−54%) | 3 011 (**+126%**) | 5 500 (−36%) | 4 200 (**+31%**) |

Whole cycle: −18%. Read at face value this says reassembling nine archives
costs roughly what capturing a delta saves, and the scheme is close to a wash —
so the follow-up work was going to be about making the chain cheaper to
reassemble: fewer generations, a tighter chain bound, maybe merging archives.

## 2. Digging: it is not the chain, it is one constant

| volume | restore penalty | implied rate |
|---|---|---|
| 128 MiB | +417 ms | 307 MiB/s |
| 512 MiB | +1 677 ms | 305 MiB/s |

The same number at both sizes, scaling with the size of the **tree** — not with
chain length, which was 8 in both, and not with the delta. That is single-core
SHA-256.

`incrtar.Restore` re-read and re-hashed every file it had just written. The
full-capture path verifies nothing at all, so this half of the comparison was
never like for like: it is the price of an integrity check, billed entirely to
the arm that has one.

**The chain-reassembly work had no measured problem to solve.**

## 3. The check can be had for free

A chain has exactly one failure mode a single archive does not: an archive that
is not the one the manifest was written against. Extraction already knows which
paths it took from which generation, so comparing that with the manifest catches
it at no cost. What the hash added on top was detection of an archive whose
bytes are corrupt but whose entries still have the right names and sizes — and
the object store checksums its objects against precisely that. `4a8fcad`
removes the re-hash and keeps the accounting.

## 4. Re-measured, all four arms

Same cluster, same node, capture side rerun as a control:

| | | ateom tar | ateom restore | client Suspend | client Resume | objects |
|---|---|---|---|---|---|---|
| **128 MiB** | full | 749 | 1 054 | 2 500 | 1 700 | 1.00 |
| | incremental | **430** (−43%) | 1 059 (+0.5%) | **1 500** (−40%) | 1 700 (0%) | 8.68 |
| **512 MiB** | full | 3 376 | 1 345 | 7 500 | 3 100 | 1.00 |
| | incremental | **1 907** (−44%) | 1 486 (+10%) | **5 000** (−33%) | **2 900** (−6%) | 8.72 |

Resume regression: **+417/+1 677 ms → +5/+141 ms**. Whole cycle: **−24% and
−25%**, up from −10% and −18%. No failures in any arm.

Correctness held up under a check the workload does not perform. The client only
reads back the file it just wrote, which always comes from the newest archive —
but the next cycle's capture scan hashes the *restored* tree against the previous
manifest, so a badly reassembled file would surface as an extra generation.
Across 224 cycle transitions, every one added exactly one generation and the
chain held at 8: roughly 1 500 restores from archives one to seven cycles old,
all matching.

## 5. The bottleneck: SHA-256 on the capture side, and it cannot be removed

Change detection has to hash the whole tree. Every cycle begins by extracting
the volume afresh, so mtime and inode are worthless and only content can say
what changed. At 512 MiB:

```
hash 512 MiB @ 305 MiB/s  = 1 679 ms
tar    64 MiB @ 103 MiB/s =   621 ms
                            -------
                            2 300 ms predicted, 2 274 observed
```

**The saving is bounded by the ratio of tar throughput to hash throughput, not
by δ.** Even at δ → 0 a 512 MiB incremental capture cannot beat ~1.7 s against
4.9 s full — a ceiling of about 2.9×, and δ = 1/8 already reaches it. Shrinking
the delta further buys nothing.

The rate is the hardware: the node has no `sha_ni`, so Go takes the AVX2 path at
~9 cycles/byte, and 2.8 GHz ÷ 9 = 297 MiB/s against 305 measured. ALU-bound, on
one core of four, because `scan.go` hashes inline in a single walk.

## Where this leaves it

* **Chain-reassembly optimisation: cancelled.** There was no measured problem.
* **Restoring the resume-side re-hash: no.** It only adds detection of what the
  object store's checksum already covers, at a second pass over every byte on
  every resume.
* **Blocking before this can be on by default — retry the per-object upload.**
  Nine objects per suspend against a control plane that treats one failed PUT as
  actor loss is the sharpest edge this change adds. A transient GCS 503 killed
  an actor in one of five runs; 1 602 of 1 734 subsequent requests failed.
* **Blocking — the fall-back-to-full policy (I4).** At δ = 1 an incremental
  capture is a full tar *plus* a full hash, so it loses by exactly the hash:
  +1 679 ms at 512 MiB, +34% to +50% on the capture step. A durable directory
  that is one large file rewritten every cycle is a common agent shape.
* **Raising the ceiling, cheapest first.** A CPU with SHA-NI (`n2d`, `c3`) is
  5–6× on the hash with no code change; parallel hashing across four cores is
  close to 4× more but needs files ≥ cores and does nothing for a single large
  file; blake3 or xxh128 needs a vendored dependency. Only keeping the durable
  dir warm on the worker — so mtime is trustworthy and the hash can be skipped —
  removes the cost instead of dividing it, and that is a proposal of its own.
* **What this change buys is CPU and staging write, not network or storage.**
  Snapshots are self-contained, so the whole chain still lands on the worker:
  the same bytes in 9 objects instead of 1. Write amplification 4.0× → 3.1×.
  Recovering the rest needs chain support in the snapshot model, which does not
  exist — `sandboxAssetsRecord.SnapshotFiles` is a flat list.

**Not yet confirmed:** δ = 1 is arithmetic, not a measurement — nothing has been
run in that shape. The parallel-hash figure is likewise projected from the
single-core rate and the core count.

## Unrelated finding

A scenario submitted onto a worker that has just served another fails every
request with `AssignWorker: ResourceExhausted: no free workers available` — the
finished run's actors still hold the pool's single replica, and nothing releases
them when the job ends. Benchmark arms have to restart the worker between runs
even when nothing about the configuration changes.
