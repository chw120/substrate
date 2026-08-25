#!/usr/bin/env python3
"""Turn one collect.sh run into timings.csv / resources.csv rows.

Kept beside collect.sh rather than inline so the schema — which is the whole
point of these files — is editable without touching the orchestration.
"""

import csv
import json
import os
import sys

TMP = os.environ["TMP"]
HERE = os.environ["HERE"]
CASE = os.environ["TEST_CASE"]

# Where each step runs and what its cost is proportional to. This is the part a
# raw metric scrape cannot tell you, and it is what makes a regression
# actionable: a step that scales with nothing got slower for a structural
# reason, not because the snapshot grew.
STEP_META = {
    #  step                 parent              layer               scales_with
    "total":            ("-",                "atelet",            "-"),
    "ateom_checkpoint": ("total",            "ateom",             "-"),
    "ateom_restore":    ("total",            "ateom",             "nothing"),
    "persist":          ("total",            "atelet",            "snapshot_bytes"),
    "download":         ("total",            "atelet",            "snapshot_bytes"),
    "manifest_fetch":   ("total",            "atelet",            "nothing"),
    "oci_unpack":       ("total",            "atelet",            "image_bytes"),
    "sandbox_assets":   ("total",            "atelet",            "nothing"),
    "volume_mount":     ("total",            "atelet",            "nothing"),
    "pause":            ("ateom_checkpoint", "ateom",             "-"),
    "snapshot":         ("ateom_checkpoint", "cloud-hypervisor",  "guest_touched_pages"),
    "durable_dir":      ("ateom_checkpoint", "ateom",             "durable_volume_bytes"),
    "teardown":         ("ateom_checkpoint", "ateom",             "nothing"),
    "ch_api_shutdown":  ("teardown",         "cloud-hypervisor",  "nothing"),
}

TIMING_HEADER = ["test_case", "action", "scope", "snapshot_kind", "step",
                 "parent_step", "layer", "duration_ms", "pct_of_total",
                 "samples", "measured_by", "scales_with", "notes"]
RESOURCE_HEADER = ["test_case", "resource_type", "item", "bytes", "mib",
                   "pct_of_snapshot", "measured_by", "notes"]


def load(name):
    with open(f"{TMP}/{name}") as f:
        return json.load(f)


def series(blob, key):
    """label-dict-tuple -> float, for one /api/v1/query result."""
    out = {}
    for r in blob[key].get("data", {}).get("result", []):
        m = {k: v for k, v in r["metric"].items() if k != "__name__"}
        out[tuple(sorted(m.items()))] = float(r["value"][1])
    return out


def means(before, after, sum_key, count_key):
    """delta(sum)/delta(count) per series: the mean over just this window."""
    bs, bc = series(before, sum_key), series(before, count_key)
    as_, ac = series(after, sum_key), series(after, count_key)
    for labels, s in as_.items():
        d_sum = s - bs.get(labels, 0.0)
        d_cnt = ac.get(labels, 0.0) - bc.get(labels, 0.0)
        if d_cnt > 0:
            yield dict(labels), d_sum / d_cnt, int(d_cnt)


before, after = load("before.json"), load("after.json")

# ----------------------------------------------------------------- timings

# (action, scope, kind) -> {step: (ms, samples)}
groups = {}
for metric, sk, ck in (("checkpoint", "checkpoint_sum", "checkpoint_count"),
                       ("restore", "restore_sum", "restore_count")):
    action = "suspend" if metric == "checkpoint" else "resume"
    prom = f"ate_actor_{metric}_duration_seconds"
    for labels, mean_s, n in means(before, after, sk, ck):
        key = (action,
               labels.get("ate_snapshot_scope", "-"),
               labels.get("ate_snapshot_kind", "-"))
        groups.setdefault(key, {})[labels.get("ate_snapshot_phase", "?")] = (
            mean_s * 1000, n, f"prometheus:{prom}", "")

# ateom's four segments are log-only. slog writes durations as nanoseconds.
log_path = f"{TMP}/ateom.log"
segs = {}  # (scope, step) -> [values_ms]
if os.path.exists(log_path):
    for line in open(log_path):
        try:
            rec = json.loads(line)
        except ValueError:
            continue
        # "CH API shutdown done" carries only a duration, no scope, so it cannot
        # name the checkpoint it belongs to. Park it under a sentinel and attach
        # it below, once we know which suspend scopes the window actually saw.
        scope = (rec.get("scope") or "__noscope__").lower()
        for wire, step in (("took", "ch_api_shutdown"), ("pause", "pause"),
                           ("snapshot", "snapshot"), ("durable_dir", "durable_dir"),
                           ("teardown", "teardown")):
            if wire == "took" and rec.get("msg") != "CH API shutdown done":
                continue
            if wire != "took" and rec.get("msg") != "Actor checkpointed":
                continue
            if wire in rec:
                segs.setdefault((scope, step), []).append(float(rec[wire]) / 1e6)

suspend_scopes = {k[1] for k in groups if k[0] == "suspend"}
for (scope, step), vals in sorted(segs.items()):
    if scope == "__noscope__":
        # Unambiguous only when the window saw a single suspend scope. With a
        # mixed window, attributing it to either one would be a guess, and a
        # wrong ch_api_shutdown is worse than an absent one.
        if len(suspend_scopes) != 1:
            print(f"skipping {step}: {len(suspend_scopes)} suspend scopes in this "
                  f"window, and the log line carries no scope", file=sys.stderr)
            continue
        scope = next(iter(suspend_scopes))
    scope = "full" if "full" in scope else ("data" if "data" in scope else scope)
    # Match on (action, scope) and let the kind fall where it may: the checkpoint
    # metric carries ate_snapshot_kind on some versions and not others, while the
    # log line never had it. Hardcoding a kind here silently drops every ateom
    # segment the moment the metric grows the label.
    for key in [k for k in groups if k[0] == "suspend" and k[1] == scope]:
        groups[key][step] = (sum(vals) / len(vals), len(vals),
                             "ateom_log:Actor checkpointed", "")

rows = []
for (action, scope, kind), steps in sorted(groups.items()):
    total = steps.get("total", (0.0,))[0]
    if not total:
        continue

    # Derived remainders make the hierarchy add up, which is the only way to
    # see time that no instrumented step claims. Only meaningful on suspend:
    # the restore phases overlap, so a remainder there would be fiction.
    if action == "suspend":
        acc = steps.get("ateom_checkpoint", (0.0,))[0]
        per = steps.get("persist", (0.0,))[0]
        if acc and per:
            steps["atelet_other"] = (total - acc - per, steps["total"][1],
                                     "derived", "total minus ateom_checkpoint and persist")
        inner = sum(steps.get(s, (0.0,))[0]
                    for s in ("pause", "snapshot", "durable_dir", "teardown"))
        if acc and inner:
            steps["ateom_other"] = (acc - inner, steps["ateom_checkpoint"][1],
                                    "derived", "gRPC + WaitReady + checkpoint dir setup")
        td = steps.get("teardown", (0.0,))[0]
        ch = steps.get("ch_api_shutdown", (0.0,))[0]
        if td and ch:
            steps["teardown_other"] = (td - ch, steps["teardown"][1], "derived",
                                       "kill CH + kill virtiofsd + unmount + sweep")

    order = list(STEP_META) + ["atelet_other", "ateom_other", "teardown_other"]
    for step in sorted(steps, key=lambda s: order.index(s) if s in order else 99):
        ms, n, src, note = steps[step]
        parent, layer, scales = STEP_META.get(
            step, ({"atelet_other": "total", "ateom_other": "ateom_checkpoint",
                    "teardown_other": "teardown"}.get(step, "total"),
                   "atelet" if step == "atelet_other" else "ateom", "-"))
        rows.append([CASE, action, scope, kind, step, parent, layer,
                     round(ms, 1), round(ms / total * 100, 1), n, src, scales, note])

# --------------------------------------------------------------- resources

res = []
sizes = {}
for labels, mean_b, n in means(before, after, "size_sum", "size_count"):
    sizes[labels.get("file_name", "?")] = mean_b
snapshot_total = sum(sizes.values()) or 1.0
for name, b in sorted(sizes.items(), key=lambda kv: -kv[1]):
    res.append([CASE, "snapshot_file", name, int(b), round(b / 2**20, 2),
                round(b / snapshot_total * 100, 2),
                "prometheus:atelet_snapshot_size_bytes", ""])
res.append([CASE, "snapshot_total", "all_files", int(snapshot_total),
            round(snapshot_total / 2**20, 2), 100.00, "derived",
            "sum of the snapshot_file rows"])

gm = load("guestmem.json")
for r in sorted(gm.get("data", {}).get("result", []),
                key=lambda r: -float(r["value"][1])):
    comp = r["metric"].get("ate_guest_memory_component", "?")
    b = float(r["value"][1])
    pct = "-" if comp == "free" else round(b / snapshot_total * 100, 1)
    res.append([CASE, "guest_memory", comp, int(b), round(b / 2**20, 2), pct,
                "prometheus:ate_microvm_guest_memory_bytes", ""])

if not rows:
    sys.exit("no timing rows produced — check metric names and that cycles ran")


def append(path, header, new):
    exists = os.path.exists(path)
    with open(path, "a", newline="") as f:
        w = csv.writer(f)
        if not exists:
            w.writerow(header)
        w.writerows(new)


append(f"{HERE}/timings.csv", TIMING_HEADER, rows)
append(f"{HERE}/resources.csv", RESOURCE_HEADER, res)
print(f"appended {len(rows)} timing rows and {len(res)} resource rows "
      f"for test_case={CASE}", file=sys.stderr)
