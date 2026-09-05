# qcow2 durable-dir — summary

A reading guide to [qcow2-durable-dir-2026-09-01.md](qcow2-durable-dir-2026-09-01.md),
which is long because it records the wrong turns as well as the results. Every
number here is sourced there.

## The result

At a 512 MiB durable dir, against the tar arrangement on the same node, binary
and workload:

| | qcow2 ÷ tar |
|---|---|
| `ResumeActor` | **1.51× — worse** |
| `SuspendActor` | 0.47–0.49× |
| full cycle | 0.76–0.81× |
| throughput | +20% |

The arrangement wins the cycle and loses the latency a caller actually waits
through. **Neither of the two terms behind that resume loss is in the qcow2
arrangement itself**, which is the main finding.

## What was measured

Two ways to hold an actor's durable directory across a suspend:

* **tar** — a virtio-fs share, packed into a tarball on checkpoint.
* **qcow2** — an ext4 image on virtio-blk, sealed by stacking a layer onto a
  backing-file chain. Hardlinks and a manifest, so the seal is constant time and
  does not scale with the actor's data.

Workload `durdir_partial_512mb_microvm`: eight 64 MiB files, one rewritten per
cycle, cold boot, `DATA` scope.

## What the digging found, in order

**1. `guest_flush` was 1 233 ms, and it was not the guest's.** ext4's
`data=ordered` couples the guest's flush to host pages the restore had just
dirtied. Backgrounding a `syncfs(2)` after landing (`qcow2.Drain`) moves that
off the critical path: **1 233 → 35 ms**.

**2. Moving the flatten off the restore path breaks actors.** A flatten costs
9.1 s at this size and sits on resume, so it was tried in the background. It
never finishes inside one activation — 40 of 40 were cancelled by the next seal —
and the chain grew past the nesting depth cloud-hypervisor will open. 76% of
resumes then failed.

The failure is absorbing, which is what makes it a correctness bug rather than a
slow path: a boot that fails lands no layer and collapses none, so the chain is
exactly as deep next time and the actor never starts again. Fixed by clamping
`MaxChain()` to a measured `maxNestingDepth = 11` and reverting the background
job. The flatten stays on the restore path because there is nowhere else to put
it.

**3. The 12× write amplification is the stager's, not either arrangement's.**
Before ateom is called, atelet writes every file the snapshot names into the
restore directory — 823 MB of chain at this size, fetched from object storage.
That is also the entire reason the drain has anything to do.

**4. Chain depth is not the guest's cold-boot cost.** A `MAX_CHAIN` sweep of
2/4/8/11 with tar controls at both ends:

* `containers` is flat at ~1 100 ms across depths 2 through 11. Depth is not it.
* The 40 samples at depth 2 are all boots right after a flatten — a `qemu-img
  convert` that has just written the whole image — and they sit in the middle of
  the band. A cold host page cache is not it either, so prefetching would buy
  nothing.
* Shallow is worse, not better: 4 is now the slowest setting measured, because
  the drain removed the per-layer flush cost that used to push back against
  depth, leaving only the flatten's frequency.
* 8 and 11 tie once the session drift is normalized out. **The default stays
  at 8**, with no case for moving it against cloud-hypervisor's hard limit.
* The `MAX_CHAIN=11` arm ran 36 restores at the clamp's boundary with zero
  nesting failures — the first direct confirmation that 11 is the right number.

## Where the resume loss actually is

| term | qcow2 − tar |
|---|---|
| atelet's staging of the snapshot | **+1 186 ms** |
| the guest's first reads (`containers`) | **+1 178 ms** |
| ateom's own landing — hardlinks against an unpack | −156 ms |
| **`total`** | **+2 498 ms** |

Two terms, 95% of the gap, and ateom's own work is the only part that is
already faster.

**atelet's staging** is the term, but not by the route this document first gave
it. Every restore in every arm here is `CHECKPOINT_TYPE_EXTERNAL`: atelet
fetches the snapshot from object storage and writes it into the restore
directory. `copyLocalCheckpoint` — the local-checkpoint path, taken only for a
resume from a pause — never ran. The mechanism stands, since either path writes
the whole snapshot before ateom is called, and so does the arithmetic tying
those writes to the drain. What does not stand is the remedy: an object-storage
fetch has no local inode to share, so hardlinking cannot take this cost off the
measured workload. It would only help a pause/resume, which nothing here
measures — and on the arrangement this document is about it would not help even
then: the hardlinking that atelet has since grown shares one snapshot file,
`durable-dir.tar`, so a chain's `durable-dir.chain.json` and
`durable-dir.layer-*.qcow2` are still copied in full. Covering them is a further
change, and a safe one, because `landDurableQcow2` stacks a fresh top layer and
so never writes through to an adopted inode. Making an EXTERNAL restore cheaper
is a different change again, and an open question.

**The guest's first reads** are an empty guest page cache and ext4 walking
metadata in 4 KiB requests. Every other explanation has been eliminated, and the
only lever anyone identified was a `FULL` scope restore, which brings the
guest's memory — and so its page cache — back with it. It was measured on
`glutton-durdir-full` and it does not work: resume p50 goes from 5 100 ms to
8 800 ms on qcow2 and 3 200 to 4 700 on tar, and the ratio widens from 1.59× to
1.87×. Moving the memory costs more than the cold reads it saves. This term has
no known remedy.

Parity was the case for continuing, and it no longer holds: of the two terms,
one has no fix and the other's fix does not apply to the path measured. Even a
complete win on the staging term leaves qcow2 behind tar on resume.

## Status

The branch carries the depth clamp, the reverted background flatten, and the
drain. Nothing is committed.
