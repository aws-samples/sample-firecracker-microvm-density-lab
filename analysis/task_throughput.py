#!/usr/bin/env python3
# Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
# SPDX-License-Identifier: MIT-0
"""Turn a real-task sweep into tasks/hour per host, the unit a customer can act on.

A latency table answers "how long does one task take". The question a customer running GRPO
actually has is "how many SWE-bench instances can one host finish per hour, and where does adding
more stop paying". Those differ: per-task latency rises with density while throughput keeps
climbing, so the two curves peak in different places.

Reads the per-cell sweep CSVs written by labsweep, plus each one's sibling sweep JSON for the host
block and the workload identity. Percentiles come from the raw per-cell values rather than being
recomputed, so this agrees with what the harness reported.

Usage:
    task_throughput.py raw/task-sweep-20260803
    task_throughput.py raw/tbench-heavy-20260803 --instance build-stp
"""

import csv
import json
import sys
from collections import defaultdict
from pathlib import Path

# Upstream's evaluation harness guidance: max_workers should not exceed ~24 even on a large machine,
# because each worker is a Docker container running a full test suite.
UPSTREAM_WORKERS = 24


def arm_of(host):
    """Name the measurement arm from the sweep's own host block.

    The run label cannot carry this. One campaign used the labels `heavy-A` and `heavy-B` on an m8i
    host and an m8a host both, so the label distinguishes the config and nothing else; inferring the
    host from it files every m8a cell under m8i. `instance_type` and `smt_state` are recorded by the
    harness on the host at run time, and are the only authoritative source.

    An SKU with one thread per core reports `smt_state` as `notsupported` rather than `off`, which is
    a different claim: nothing was turned off, there was never a second thread to disable. Such an
    arm is named by family alone, so it is never read as a toggled state.
    """
    family = host["instance_type"].split(".")[0]
    smt = host.get("smt_state", "")
    if smt in ("on", "off"):
        return f"{family}-smt-{smt}"
    return family


def workload_identity(workload):
    """Return (unit, instance_id) for a sweep's workload.

    A directory can hold more than one workload -- one campaign mixed a runnable task with one
    documented as unrunnable -- so the identity has to be carried per cell and grouped on. Averaging
    across it reports the unrunnable task's failures as the runnable one's pass rate.
    """
    for unit in ("episode", "task"):
        spec = workload.get(unit)
        if spec:
            return unit, spec.get("instance_id", "unknown")
    return "task", "unknown"


def column(row, *names, default=None):
    """First present column among names, as an int.

    Memory columns were renamed on 2026-09-18 when a second measurement was added: what used to be
    `fleet_pss_kib` is now `fleet_pss_kib_boot`, alongside a new `fleet_pss_kib_working` taken after
    the workload rather than after boot. Every dataset committed before then carries the old
    spelling, and re-deriving a published figure without paid hardware is the whole reason those CSVs
    are in the repository -- so both spellings are read rather than migrating the files.

    Raises if no name matches and no default was given: a silent 0 would be published as a real
    measurement.
    """
    for n in names:
        v = row.get(n)
        if v not in (None, ""):
            return int(float(v))
    if default is not None:
        return default
    raise KeyError(f"none of {names} present in row; columns are {sorted(row)}")


def load(root, instance_filter=None):
    rows = []
    for path in sorted(Path(root).rglob("sweep-*.csv")):
        meta_path = path.with_suffix(".json")
        if not meta_path.exists():
            raise SystemExit(
                f"{path} has no sibling {meta_path.name}. The host block and the workload's"
                f" instance_id exist only in the JSON, and guessing either from the filename"
                f" silently mislabels a run, so this script will not guess."
            )
        meta = json.loads(meta_path.read_text())
        unit, instance = workload_identity(meta["workload"])
        if instance_filter and instance != instance_filter:
            continue
        host = meta["host"]
        arm = arm_of(host)
        cores = host["physical_cores"]

        label = path.name.replace("sweep-", "").split("-2026")[0]
        if label == "gate":
            continue
        with open(path) as f:
            for r in csv.DictReader(f):
                rows.append(
                    {
                        "label": label,
                        "arm": arm,
                        "cores": cores,
                        "unit": unit,
                        "instance": instance,
                        "config": r["config"],
                        "density": int(r["density_requested"]),
                        "responded": int(r["responded"]),
                        "repeat": int(r["repeat"]),
                        "task_runs": int(r["task_runs"]),
                        "task_passed": int(r["task_passed"]),
                        "trustworthy": r["trustworthy"] == "true",
                        "p50": float(r["p50_us"]),
                        "p99": float(r["p99_us"]),
                        "p100": float(r["max_us"]),
                        "mean": float(r["mean_us"]),
                        "pss_kib": column(r, "fleet_pss_kib_boot", "fleet_pss_kib"),
                        # Zero on any dataset written before 2026-09-18, which had no
                        # working-set measurement at all. Distinguish "not measured" from
                        # "measured as zero" by checking pss_kib_working > 0 before using it.
                        "pss_kib_working": column(r, "fleet_pss_kib_working", default=0),
                        "per_vm_dirty_kib": column(
                            r, "per_vm_private_dirty_kib_boot", "per_vm_private_dirty_kib"
                        ),
                        "per_vm_dirty_kib_working": column(
                            r, "per_vm_private_dirty_kib_working", default=0
                        ),
                    }
                )
    return rows


def median(xs):
    """Nearest-rank median, matching the harness's percentile convention."""
    s = sorted(xs)
    return s[len(s) // 2] if s else 0.0


def report(instance, unit, cells):
    """Print one workload's table. Two workloads never share a table, or a baseline."""
    print(f"{unit.capitalize()}s per hour per host, from real {instance} execution")
    print(f"upstream guidance: ~{UPSTREAM_WORKERS} parallel workers")
    print()
    rate_col = f"{unit}s/hr"
    hdr = (
        f"{'arm':<13} {'cfg':<4} {'dens':>5} {'resp':>5} {'x cores':>8} "
        f"{'p50 s':>7} {'p99 s':>7} {'p100 s':>7} {'p99/p50':>8} {'p100/p50':>9} "
        f"{rate_col:>9} {'vs 24w':>7} {'pass':>6}"
    )
    print(hdr)
    print("-" * len(hdr))

    # Uncontended per-task time per arm, taken from the lowest density measured on that arm. This is
    # the baseline upstream's 24 workers would see, since 24 is well under either host's core count
    # and so runs essentially without contention.
    uncontended = {}
    for (arm, _config, density), group in cells.items():
        mean_s = median([g["mean"] for g in group]) / 1e6
        cur = uncontended.get(arm)
        if cur is None or density < cur[0]:
            uncontended[arm] = (density, mean_s)

    best = {}
    for (arm, config, density), group in sorted(
        cells.items(), key=lambda kv: (kv[0][0], kv[0][1], kv[0][2])
    ):
        responded = median([g["responded"] for g in group])
        p50 = median([g["p50"] for g in group]) / 1e6
        p99 = median([g["p99"] for g in group]) / 1e6
        p100 = median([g["p100"] for g in group]) / 1e6
        mean = median([g["mean"] for g in group]) / 1e6

        runs = sum(g["task_runs"] for g in group)
        passed = sum(g["task_passed"] for g in group)
        pass_rate = passed / runs if runs else 0.0
        trust = all(g["trustworthy"] for g in group)

        # Throughput from the mean rather than p50: over an hour a host completes
        # responded/mean_seconds tasks, and the mean is what integrates to that count. Using p50
        # would overstate throughput wherever the distribution is right-skewed, which at high
        # density it always is.
        per_hour = responded * 3600.0 / mean if mean else 0.0

        ratio = density / group[0]["cores"]
        # Upstream's ceiling on this same hardware. Its 24 workers are far below the core count, so
        # each runs essentially uncontended -- the fastest per-task time this host produced, which is
        # the lowest density measured. Using the contended mean instead would credit upstream with
        # our own contention and flatter the comparison.
        base_s = uncontended[arm][1]
        uncontended_rate = UPSTREAM_WORKERS * 3600.0 / base_s if base_s else 0.0
        speedup = per_hour / uncontended_rate if uncontended_rate else 0.0
        flag = "" if trust else "  UNTRUSTED"
        print(
            f"{arm:<13} {config:<4} {density:>5} {responded:>5} {ratio:>7.2f}x "
            f"{p50:>7.2f} {p99:>7.2f} {p100:>7.2f} {p99 / p50:>8.2f} {p100 / p50:>9.2f} "
            f"{per_hour:>9.0f} {speedup:>6.1f}x {pass_rate:>5.0%}{flag}"
        )

        if trust and per_hour > best.get(arm, (0,))[0]:
            best[arm] = (per_hour, config, density, p50, p99, p100)

    print()
    print("Peak trustworthy throughput per arm:")
    if not best:
        print("  none -- no cell for this workload passed every trustworthiness guard")
    for arm, (per_hour, config, density, p50, p99, p100) in sorted(best.items()):
        base_density, base_s = uncontended[arm]
        upstream_rate = UPSTREAM_WORKERS * 3600.0 / base_s
        print(
            f"  {arm:<13} {per_hour:>6.0f} {unit}s/hr at config {config} density {density}"
            f"  ({per_hour / upstream_rate:.1f}x upstream's {upstream_rate:.0f}/hr at"
            f" {UPSTREAM_WORKERS} workers, baselined at density {base_density})"
        )


def main():
    args = [a for a in sys.argv[1:]]
    instance_filter = None
    if "--instance" in args:
        i = args.index("--instance")
        if i + 1 >= len(args):
            print("--instance needs a workload instance_id", file=sys.stderr)
            return 2
        instance_filter = args[i + 1]
        del args[i : i + 2]
    root = args[0] if args else "raw/task-sweep-20260803"

    rows = load(root, instance_filter)
    if not rows:
        if instance_filter:
            print(f"no sweep CSVs under {root} for instance {instance_filter}")
        else:
            print(f"no sweep CSVs under {root}")
        return 1

    # Group repeats of the same cell. A single repeat cannot separate a difference from noise, so
    # everything below is a median across repeats. The workload's instance_id is part of the key,
    # not just a label: pooling two workloads averages one's latency and pass rate into the other.
    cells = defaultdict(lambda: defaultdict(list))
    units = {}
    for r in rows:
        cells[r["instance"]][(r["arm"], r["config"], r["density"])].append(r)
        units[r["instance"]] = r["unit"]

    for n, instance in enumerate(sorted(cells)):
        if n:
            print()
            print("=" * 78)
            print()
        report(instance, units[instance], cells[instance])

    print()
    print(
        "The p99/p50 ratio is what bounds max-of-G amplification under a GRPO group barrier:"
    )
    print(
        "a group of G rollouts waits for its slowest member, so the tail is what gates a"
    )
    print("gradient step. p100/p50 pulling away from p99/p50 is the earliest sign of")
    print("oversubscription, one density step before the tail itself degrades.")
    return 0


if __name__ == "__main__":
    sys.exit(main())
