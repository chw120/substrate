# Which durable-dir landing to ship — 2026-09-04

**Ship tarfs.** Mount the snapshot's tar through a 1.5 KiB erofs index instead
of unpacking it. Measured at 1 GiB, interleaved against tar on one cluster.

| | tar | erofs image | tarfs |
|---|---|---|---|
| restore, server-side | — | −877 ms | **−1 715 ms** |
| suspend, server-side | — | +2 663 ms | **+2 235 ms** |
| disk written per cycle | 4 463 MiB | not measured | **3 260 MiB** |
| cycles per 5m22s | 6 | not measured | **7** |
| wire format | tar | erofs — fleet-wide migration | tar — none |
| rollback | — | asymmetric | **one env var, per node** |
| loop devices per worker | 0 | 1 | 2 |

## The four things worth knowing

1. **Restore stops scaling with volume size.** The unpack goes from 1 524 ms to
   3.2 ms; the whole server-side restore from 2 736 ms to 1 021 ms.

2. **The real win is disk traffic, not latency.** A cycle makes four
   gibibyte-scale writes — download, unpack, guest overwrite, new archive.
   tarfs deletes the unpack, so the node writes 27% less and fits 7 cycles into
   the wall clock where tar fits 6.

3. **The +2 s on suspend is not new work.** Both arms write during the archive
   step at 162 MB/s, the volume's line rate; tarfs's window is longer only
   because 342 MiB more passes through it — writeback the guest's overwrite
   deferred by returning 2 600 ms earlier. Overwrite plus archive sums to within
   3% on the two arms.

4. **Reads are free and identical.** Zero disk reads on either arm, and warm and
   cold serve latency are unchanged. The mount costs nothing to read through.

## What you are accepting

Two loop devices per worker instead of one, so at most **4 workers per node**
can serve a durable dir this way (`max_loop` is 8). Kernel **≥ 6.4**. Steady
disk stays 2× while the actor is resident, same as the erofs image.

## Why not the erofs image

It wins less on restore, costs more on suspend, rebuilds a gibibyte-scale image
on every suspend, and changes the wire format — which means `Sniff`, a fleet
roll, and a rollback an old worker cannot survive. Its only advantage is using
one loop device instead of two.

---

Numbers and method: [durable-dir-landing-summary-2026-09-04.md](durable-dir-landing-summary-2026-09-04.md).
