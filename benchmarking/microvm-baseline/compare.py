#!/usr/bin/env python3
"""Diff two test cases step by step.

    ./compare.py baseline after-teardown-async
    ./compare.py baseline drop-caches --action suspend

Steps are matched on (action, scope, snapshot_kind, step), so a case that only
changed the suspend path still lines up against the full baseline.
"""

import argparse
import csv
import os
import sys

HERE = os.path.dirname(os.path.abspath(__file__))


def read(name):
    path = os.path.join(HERE, name)
    if not os.path.exists(path):
        sys.exit(f"missing {path}")
    with open(path) as f:
        return list(csv.DictReader(f))


def delta(old, new):
    if not old:
        return "     n/a"
    return f"{(new - old) / old * 100:+7.1f}%"


def main():
    p = argparse.ArgumentParser()
    p.add_argument("base")
    p.add_argument("candidate")
    p.add_argument("--action", help="suspend or resume; default both")
    args = p.parse_args()

    timings = read("timings.csv")
    cases = {r["test_case"] for r in timings}
    for c in (args.base, args.candidate):
        if c not in cases:
            sys.exit(f"unknown test_case '{c}'. known: {', '.join(sorted(cases))}")

    # snapshot_kind is deliberately NOT part of the join key for suspend rows: the
    # checkpoint metric gained ate_snapshot_kind after the baseline was recorded,
    # so the same step reads "-" on one case and "latest" on another. Keying on it
    # would silently report every suspend step as present-in-one-case-only.
    def key(r):
        kind = "-" if r["action"] == "suspend" else r["snapshot_kind"]
        return (r["action"], r["scope"], kind, r["step"])

    base = {key(r): r for r in timings if r["test_case"] == args.base}
    cand = {key(r): r for r in timings if r["test_case"] == args.candidate}

    print(f"\n{args.base}  ->  {args.candidate}\n")
    header = f"{'action/scope/step':52s} {'base':>9s} {'cand':>9s} {'delta':>9s}  scales_with"
    print(header)
    print("-" * len(header))

    last_group = None
    for k in sorted(base.keys() & cand.keys(),
                    key=lambda k: (k[0], k[1], k[2], float(base[k]["pct_of_total"]) * -1)):
        action, scope, kind, step = k
        if args.action and action != args.action:
            continue
        group = (action, scope, kind)
        if group != last_group:
            print()
            last_group = group
        b, c = float(base[k]["duration_ms"]), float(cand[k]["duration_ms"])
        label = f"{action}/{scope}" + (f"/{kind}" if kind != "-" else "") + f"  {step}"
        print(f"{label:52s} {b:8.1f}ms {c:8.1f}ms {delta(b, c):>9s}  "
              f"{base[k]['scales_with']}")

    only_base = base.keys() - cand.keys()
    only_cand = cand.keys() - base.keys()
    if only_base:
        print(f"\nonly in {args.base}:     " + ", ".join("/".join(k) for k in sorted(only_base)))
    if only_cand:
        print(f"only in {args.candidate}: " + ", ".join("/".join(k) for k in sorted(only_cand)))

    # Bytes matter as much as milliseconds: every step marked snapshot_bytes
    # moves with this number.
    res = read("resources.csv")
    tot = {r["test_case"]: r for r in res if r["resource_type"] == "snapshot_total"}
    if args.base in tot and args.candidate in tot:
        b, c = float(tot[args.base]["mib"]), float(tot[args.candidate]["mib"])
        print(f"\nsnapshot total{'':38s} {b:8.1f}MiB {c:7.1f}MiB {delta(b, c):>9s}")


if __name__ == "__main__":
    main()
