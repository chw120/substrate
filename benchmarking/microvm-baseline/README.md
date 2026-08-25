# microVM suspend/resume baseline

A frozen reference point for the microVM suspend/resume path, so a change can be
measured against something instead of against memory.

Three CSVs, joined on `test_case`. Adding a new experiment means appending rows
with a new `test_case` — never editing existing ones.

| File | One row is | Answers |
|---|---|---|
| `testcases.csv` | one experiment | *what was different about this run* |
| `timings.csv` | one step of one action | *what action, and where its time went* |
| `resources.csv` | one byte-valued thing | *how big the snapshot was, and what filled it* |

## timings.csv

```
test_case, action, scope, snapshot_kind, step, parent_step, layer,
duration_ms, pct_of_total, samples, measured_by, scales_with, notes
```

- **action** — `suspend` or `resume`. The thing being timed.
- **scope / snapshot_kind** — which variant of that action (`full`, `data`,
  `data_on_golden`; `golden`, `latest`, `local`).
- **step / parent_step** — the breakdown, as a tree. `total` is the root; every
  other step names its parent, so children of one parent sum to it (modulo the
  `*_other` remainders below). Walk it with:

  ```
  suspend / full
    total                     920.8 ms  100.0%
      ateom_checkpoint        519.3      56.4%
        pause                   2.6       0.3%
        snapshot              160.8      17.5%   <- CH writes memory-ranges
        durable_dir             3.9       0.4%
        teardown              333.6      36.2%
          ch_api_shutdown     234.2      25.4%   <- waiting for CH to exit
          teardown_other       99.4      10.8%
        ateom_other            18.4       2.0%
      persist                 394.4      42.8%   <- zstd + upload
      atelet_other              7.1       0.8%
  ```

- **layer** — `atelet` / `ateom` / `cloud-hypervisor`. Which process is busy.
  Nothing is ever attributed to the guest: it is frozen from `pause` onward.
- **scales_with** — `snapshot_bytes`, `guest_touched_pages`, `nothing`, … This is
  the column that makes a regression actionable. A step marked `nothing` that got
  slower did so for a structural reason, not because the snapshot grew.
- **`*_other` rows** are derived remainders (`derived` in `measured_by`) so the
  tree adds up and unclaimed time stays visible. Only computed for `suspend`;
  the resume steps overlap (download runs concurrently with `manifest_fetch` and
  `oci_unpack`), so a remainder there would be fiction.

## Adding a test case

```bash
TEST_CASE=after-teardown-async CYCLES=10 ./collect.sh
# then describe it — a timing with no test case describing it compares to nothing
$EDITOR testcases.csv
./compare.py baseline after-teardown-async
```

`collect.sh` refuses to reuse a `test_case` name that is already in `timings.csv`.

## Comparing

```
$ ./compare.py baseline after-teardown-async

suspend/full  total                    920.8ms   687.2ms    -25.4%  -
suspend/full  ateom_checkpoint         519.3ms   285.7ms    -45.0%  -
suspend/full  teardown                 333.6ms   100.0ms    -70.0%  nothing
suspend/full  ch_api_shutdown          234.2ms     0.6ms    -99.7%  nothing
suspend/full  persist                  394.4ms   394.1ms     -0.1%  snapshot_bytes
```

Steps are matched on `(action, scope, snapshot_kind, step)`, so a change that
only touches suspend still lines up against the full baseline. `snapshot_kind`
is dropped from the key on suspend rows: the checkpoint metric gained
`ate_snapshot_kind` after `baseline` was recorded, so the same step reads `-`
there and `latest` on newer cases, and keying on it would report every suspend
step as present in only one case.

## What the `baseline` case actually is

See `testcases.csv`. The parts that decide whether *your* run is comparable:

1. **Object storage is local** — rustfs in-cluster, not GCS. `persist` (394 ms)
   and `download` (429 ms) are loopback figures, and they are the largest step
   on either side. A run against real GCS is **not** comparable: start a new
   baseline rather than diffing against this one.
2. **samples = 1–2.** Proportions are trustworthy; absolute milliseconds are not.
   `collect.sh` defaults to 10 cycles for exactly this reason.
3. **Single node, laptop, Lima VM.** No scheduling pressure, no neighbours.
4. **`snapshot_file_legacy` rows are historical.** They record what `fi.Size()`
   returned before the size fix, kept so the 13× correction on `memory-ranges`
   stays visible. Do not diff against them.

## Which scopes a suspend/resume loop can actually reach

`collect.sh` drives `suspend` then `resume` in a loop, and that loop reaches
exactly two of the four scope combinations the `baseline` case recorded. This
is structural, not a gap in the script:

- The suspend scope is the template's `snapshotsConfig.onCommit` — except in
  the golden atespace, which always commits `Full` (`commitSnapshotScope`, in
  `cmd/ateapi/internal/controlapi/workflow_suspend.go`). So `suspend/full` and
  `resume/full/golden` are the **golden-actor bootstrap**, which happens once
  when the ActorTemplate is reconciled. A demo actor never produces them.
- `resume --boot` is consulted only when the actor has no `latestSnapshot`
  (`loadActorForResume`, `workflow_resume.go`); every suspend writes one, so
  from cycle 1 onwards the flag is silently ignored.

To sample the full path, delete the ActorTemplate and let it rebuild its
golden — one sample per rebuild, not something to loop.

## What guest RAM does and does not buy (`main-guest2048` vs `main-guest896`)

Halving guest RAM (2048 → 896 MiB) moved the warm loop by ~11%: resume total
347.1 → 304.2 ms, suspend total 89.8 → 80.0 ms. That is not a memory effect.
The shift is spread evenly across steps that scale with nothing
(`ch_api_shutdown` −2%, `pause` ±0%), and the one step that *should* scale —
`snapshot`, marked `guest_touched_pages` — is 0.0 ms in both, with a ~4 KiB
persisted snapshot in both.

The reason is the template's `onCommit: Data` + `onResume.fromData: Golden`:
the loop only ever writes a data delta, which carries no guest memory. Guest
RAM is paid on the golden bootstrap, which the loop never re-enters. **Sizing
experiments on this path measure ambient load, not guest memory.**

One hard result did fall out of it: at 384 MiB of guest RAM (the demo's
shipped `512Mi` limit minus the 128 MiB VMM reserve) the guest **cold-boots
fine but cannot be restored from a golden snapshot** — readyz times out after
2 minutes, twice, with no OOM or panic on the guest console. A control at 2176
Mi on the same worker pods and the same `full/golden` path went RUNNING in
about 3 seconds. 896 MiB and 2048 MiB both work; the floor is somewhere
between 384 and 896 and has not been bisected.

## Known gaps this baseline cannot fill

- `ateom_restore` (264 ms, 37% of resume) is a single black box — `restore.go`
  has one timer and one log line, so `parent_step` bottoms out there.
- The four ateom suspend segments are log-only, never metrics, so `collect.sh`
  has to scrape pod logs to get them.
- Compressed bytes-on-the-wire are recorded nowhere. The upload path logs
  `populated_bytes` but does not export it, so the byte figure driving
  `download` is the on-disk size, not the transferred size.
- No host-side density metric, so "how many actors fit on a node" is still
  unanswerable.
- `ate.microvm.guest.memory.bytes` does not reach Prometheus — in fact no
  ateom-sourced metric does, with no registration error in the ateom logs. The
  `main-guest*` cases therefore have no `guest_memory` rows at all, unlike
  `baseline`.

## Provenance of the byte figures

Every `snapshot_file` row depends on `atelet.snapshot.size` recording
`st_blocks*512` rather than `fi.Size()` (branch `atelet-snapshot-allocated-size`).
Without that fix `memory-ranges` reports a constant 2 GiB — the guest's
configured RAM — on every actor, and none of the size or share figures exist.
The `guest_memory` rows come from `ate.microvm.guest.memory.bytes`, added on this
branch.
