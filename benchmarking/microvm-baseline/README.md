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
only touches suspend still lines up against the full baseline.

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

## Provenance of the byte figures

Every `snapshot_file` row depends on `atelet.snapshot.size` recording
`st_blocks*512` rather than `fi.Size()` (branch `atelet-snapshot-allocated-size`).
Without that fix `memory-ranges` reports a constant 2 GiB — the guest's
configured RAM — on every actor, and none of the size or share figures exist.
The `guest_memory` rows come from `ate.microvm.guest.memory.bytes`, added on this
branch.
