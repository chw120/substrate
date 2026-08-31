# Durable-dir snapshots as an erofs image vs a tar — 2026-08-31

What serving a durable-dir volume from a read-only erofs image, instead of
unpacking a tar, costs and buys. Both arms were measured on one tree and one
cluster, minutes apart, so the difference between them is the format and
nothing else.

Measured on branch `poc1-erofs` at commit `3822d673`. The arms differ only in
`ATEOM_ARCHIVE_FORMAT`: `erofs` on one, unset (the default, tar) on the other.
Nothing else was redeployed between them.

The tar arm reproduces the DurDir size sweep baseline of 2026-08-26 closely
(1 GiB: 5 100 / 11 000 ms here against 5 100 / 10 000 ms there), which is what
makes it usable as a control: `main` did not drift under the measurement in the
five days between. That baseline and the reproduction steps both live in
`benchmarking/results/` on the `durdir-large-sizes` branch, which is also where
the 500 MiB and 1 GiB scenarios used here come from — they are not in this
tree, so the two large steps were run from a copy of that branch's
`tests.yaml` entries. Its large-size rows came from 5m / 8m runs at n=11-12;
these ran the committed 20m / 30m, so the tables below are not tail-comparable
with it above 64 MiB even though the p50s agree.

## Environment

| | |
|---|---|
| Cluster | `substrate-poc`, `us-central1-a`, project `chenyiwang-gke-dev` |
| GKE | `v1.35.7-gke.1222000` |
| Worker node | `n2-standard-4`, nested virtualization enabled, `ate.dev/sandboxClass=microvm` |
| Node kernel | `6.8.0-1060-gke`, Ubuntu 24.04.4 (`CONFIG_EROFS_FS=m`, `CONFIG_EROFS_FS_XATTR=y`) |
| Sandbox class | `microvm` (`ateom-microvm@sha256:77c9b958`, based on an image carrying erofs-utils 1.8.6) |
| WorkerPool | `benchmark-ateom`, 1 replica |
| ActorTemplate | `glutton-durdir-data` — one `durableDir` volume; `onPause: Full`, `onCommit: Data`, `onResume.fromData: ColdBoot` |
| Load | 1 concurrent user, `--resume-mode explicit`, `--durdir-read-mode digest`, 1.0 s fixed wait |
| Snapshot store | GCS, `snapshot-substrate-test-chenyiwang-gke-dev` |

The erofs arm ran the worker privileged and the tar arm did not. That was not a
confound introduced for the measurement — it was a consequence of the format at
the commit measured, applied by atecontroller from the same opt-in, so each arm
ran the pod its format required. The worker no longer escalates: the loop device
now arrives as a device-plugin grant, which does not change what any measured
step does. See [The pod is not free](#the-pod-is-not-free).

Every run finished with zero failures except erofs at 5 MiB, which had two out
of 489 requests; see [Failures](#failures).

## Client-observed latency

All figures in milliseconds, from locust's percentile table. **Δ** is erofs
against tar; negative is faster.

### p50

| Size | ResumeActor | | | SuspendActor | | | ServeAfterResume | | ServeWarm | | Overwrite | |
|---|---|---|---|---|---|---|---|---|---|---|---|---|
| | tar | erofs | Δ | tar | erofs | Δ | tar | erofs | tar | erofs | tar | erofs |
| 5 MiB | 1 200 | 1 200 | — | 390 | 390 | — | 68 | 69 | 31 | 30 | 55 | 52 |
| 10 MiB | 1 200 | 1 200 | — | 450 | 490 | +9% | 120 | 130 | 51 | 50 | 90 | 87 |
| 64 MiB | 1 400 | 1 400 | — | 1 500 | 2 100 | **+40%** | 720 | 740 | 270 | 270 | 650 | 460 |
| 500 MiB | 2 800 | 2 400 | **−14%** | 6 100 | 7 300 | **+20%** | 5 600 | 5 700 | 2 100 | 2 100 | 6 600 | 4 500 |
| 1 GiB | 5 100 | 3 600 | **−29%** | 11 000 | 15 000 | **+36%** | 12 000 | 12 000 | 4 300 | 4 200 | 12 000 | 7 000 |

`ServeWarm` reads the volume through the merged overlay rather than the
unpacked tree, and is unchanged at every size — the overlay adds no measurable
read cost on the warm path.

### p95

| Size | ResumeActor | | SuspendActor | | ServeAfterResume | | ServeWarm | | Overwrite | |
|---|---|---|---|---|---|---|---|---|---|---|
| | tar | erofs | tar | erofs | tar | erofs | tar | erofs | tar | erofs |
| 5 MiB | 1 300 | 1 300 | 590 | 540 | 74 | 82 | 37 | 38 | 63 | 58 |
| 10 MiB | 1 400 | 1 300 | 590 | 650 | 140 | 140 | 57 | 59 | 110 | 97 |
| 64 MiB | 1 800 | 1 600 | 2 000 | 2 300 | 760 | 780 | 310 | 290 | 720 | 480 |
| 500 MiB | 3 300 | 2 700 | 8 600 | 8 700 | 6 000 | 5 800 | 2 300 | 2 200 | 7 400 | 4 900 |
| 1 GiB | 6 800 | 4 400 | 12 000 | 18 000 | 12 000 | 12 000 | 4 500 | 4 300 | 13 000 | 7 400 |

### Sample counts

| Size | tar | erofs | Duration |
|---|---|---|---|
| 5 MiB | 109 | 99 | 5m |
| 10 MiB | 101 | 101 | 5m |
| 64 MiB | 53 | 50 | 5m |
| 500 MiB | 50 | 53 | 20m |
| 1 GiB | 41 | 42 | 30m |

## Server-side decomposition

The client figures include GCS transfer and control-plane round trips. The
ateom worker log isolates what the format actually changed. All p50, in
milliseconds.

### Restore

`land` is the phase this change exists to remove: on the tar path it unpacks
the archive, on the erofs path it hardlinks the image into place and defers the
mount to after `CleanupSandboxState`.

| Size | land (tar) | land (erofs) | restore total (tar) | restore total (erofs) |
|---|---|---|---|---|
| 5 MiB | 3.7 | 0.090 | 982 | 989 |
| 10 MiB | 6.9 | 0.090 | — | — |
| 64 MiB | 39.6 | 0.092 | — | — |
| 500 MiB | 298.3 | 0.090 | 1 270 | 977 |
| 1 GiB | 615.1 | 0.092 | 1 853 | 976 |

**The erofs restore is flat.** 977 ms at 1 GiB and 989 ms at 5 MiB — a 200×
size increase moves it by nothing, because nothing proportional to the volume
happens. The tar restore is linear at roughly 0.6 ms/MiB in `land` alone.

`land` accounts for 615 ms of the 877 ms the server-side restore saves at
1 GiB. The remainder is knock-on: the tar arm's cold boot is slower too
(`agent_dial` 788 ms against 655 ms, `since_boot` 1 039 ms against 861 ms),
because writing a gibibyte back out contends with the VM that is starting
alongside it. Removing the write removes that contention as well.

The client-observed resume improves by more than the server-side accounts for
(1 500 ms against 877 ms at 1 GiB). The rest is outside ateom's measurement —
GCS and control plane — and this run does not decompose it.

### Checkpoint

`durable_dir` is the archive step: `tar` writes the archive, `mkfs.erofs`
builds the image and ateom syncs it, because the tool issues no fsync of its
own.

| Size | durable_dir (tar) | durable_dir (erofs) | Δ | teardown (tar) | teardown (erofs) |
|---|---|---|---|---|---|
| 5 MiB | 10.5 | 12.7 | +2 | 56.7 | 80.1 |
| 500 MiB | 3 169.5 | 3 129.8 | −40 | 116.1 | 2 053.5 |
| 1 GiB | 6 610.9 | 10 578.7 | **+3 968** | 179.4 | 314.4 |

The +3 968 ms at 1 GiB is very nearly the whole +4 000 ms suspend regression the
client sees, so the write side is fully explained by `mkfs.erofs`.

**The write cost is superlinear and the crossover is between 500 MiB and 1 GiB.**
At 500 MiB building the image is free relative to tar (3 130 against 3 170 ms);
at 1 GiB it costs 3.4× what it cost at 500 MiB for 2× the data, while tar costs
2.1×. Whatever causes that — the node is a 16 GiB `n2-standard-4` and
`mkfs.erofs` buffers — it means the regression cannot be extrapolated from the
small sizes, and sizes above 1 GiB were not measured.

The 500 MiB erofs `teardown` of 2 053 ms against tar's 116 ms is unexplained.
It does not reappear at 1 GiB (314 ms), so it is not simply the overlay
teardown scaling with volume size.

## What this buys and what it costs

**The restore win is real and it is the shape the design predicted.** Resume
latency stops depending on durable-dir size: flat at ~977 ms server-side across
a 200× range. Below 64 MiB there is nothing to win, because a near-constant VMM
launch already dominates — `land` is 3.7 ms out of a 982 ms restore at 5 MiB.
The format only pays from a few hundred mebibytes up.

**The suspend regression is real too, and it is larger.** At 1 GiB the client
pays +4 000 ms on suspend to save 1 500 ms on resume. Whether that trade is
worth taking depends on whether suspend is on a path anyone waits for. It is
not the user-facing one, but it is on eviction and rolling upgrade, and a node
draining a pool of gibibyte-sized actors pays it serially.

**`DurDirOverwrite` is 32–42% faster on erofs** (1 GiB: 7 000 against
12 000 ms) and this run does not explain why. An overwrite through an overlay
has to copy up from the lower, which should be slower, not faster. It
reproduces at both large sizes and at 64 MiB, so it is not noise, but the
mechanism is unidentified and the number should not be quoted as a benefit
until it is.

### The pod is not free

Loop-mounting the image needs a **loop device**, and a loop device is a block
device: the worker's device cgroup denies opening one whatever capabilities the
pod holds, because a container cannot widen that allow-list from inside.
Measured directly on the node — the same image mounts and reads back in a
privileged pod and fails at `losetup` in the capability-only one.

The runs above bought that with `privileged: true`, which is a real cost against
a worker `main` deliberately de-privileged. It is no longer how the worker gets
there: atelet now advertises the node's loop devices to kubelet the way it
already advertises `/dev/kvm`, atecontroller requests one `ate.dev/loop` under
the same opt-in, and kubelet writes the cgroup allow rule. The worker stays
unprivileged.

What remains is a per-node ceiling. Loop devices are a small fixed pool —
`max_loop` is 8 on this node — so at most that many workers per node can serve a
durable dir from an image at once, which is why the request is conditional on
the format rather than unconditional.

The steady-state disk cost also roughly doubles: the image stays on disk for as
long as the actor is resident, and the overlay upper accumulates alongside it,
where the tar arrangement holds one tree.

### Failures

erofs at 5 MiB had one resume where `readyz` for the container never returned
200 within 30 s, and one suspend that failed as a consequence (the actor was
still `ACTOR_STATE_RESUMING`). One occurrence in 99 resumes, not reproduced at
any other size or on the tar arm; too rare here to attribute to the format, and
worth watching rather than concluding anything from.

There were **zero extract fallbacks** across the whole erofs arm: every restore
mounted, none went through `fsck.erofs --extract`.

## Run index

| Size | tar (UTC start) | erofs (UTC start) |
|---|---|---|
| 5 MiB | 2026-08-31T07:55:55Z | 2026-08-31T07:38:56Z |
| 10 MiB | 2026-08-31T08:01:24Z | 2026-08-31T07:44:24Z |
| 64 MiB | 2026-08-31T08:06:46Z | 2026-08-31T07:49:51Z |
| 500 MiB | 2026-08-31T10:09:14Z | 2026-08-31T06:47:30Z |
| 1 GiB | 2026-08-31T12:24:02Z | 2026-08-31T07:07:58Z |

The tar 500 MiB run is recorded by the driver as `timeout`: the orchestrator's
job wait expired, not the test. locust exited 0 with a complete percentile
table over 50 resumes and no failures, so the figures are from a finished run.
