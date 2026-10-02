#!/usr/bin/env python3
# Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
# SPDX-License-Identifier: MIT-0
"""Derive GRPO group-completion time from per-microVM sandbox latency samples.

# Why this exists

Every latency figure the sweeps produce is *per microVM*: mean, p99, jitter for one sandbox
running one round of work. That is the wrong unit for a GRPO customer.

GRPO computes advantage group-relatively. For a group of G completions to a single prompt:

    A_i = (r_i - mean(r_1..r_G)) / std(r_1..r_G)

Both moments need every member of the group, so **no completion in a group can contribute to
the gradient until all G have finished**. The group is an atomic synchronisation barrier, and
the quantity that gates training throughput is therefore

    T_group = max(t_1, ..., t_G)

not the mean or the p99 of the individual t_i. PPO with a learned critic does not have this
property because each trajectory carries its own value baseline; GRPO and its variants (DAPO,
Dr. GRPO, GSPO) all do.

The consequence is that a rare slow sandbox is not rare at the group level. With a 1% chance
per rollout of landing in the tail, the chance that a group of 64 contains at least one is
1 - 0.99^64, about 47%. Half of all gradient steps wait on a straggler.

# Method

Bootstrap. For a given cell (config, density) we have N measured round durations from the
sweep. Draw G of them with replacement, take the max, repeat many times, and read percentiles
off the resulting distribution of group-completion times.

Sampling *with* replacement across the whole cell treats the measured durations as an empirical
distribution of "how long does one rollout take at this density". That is the right model when
rollouts are assigned to sandboxes dynamically, which is how a rollout scheduler works: a
worker picks up the next prompt when it frees up, so there is no fixed sandbox-to-group
affinity.

# Assumptions, stated because they bound what the output means

1. **Independence.** Bootstrap draws are independent, so this ignores correlated slowness --
    a NUMA node under pressure, or a noisy neighbour affecting several sandboxes at once. Real
    correlation makes max-of-G *worse* than modelled, not better, so these figures are a
    lower bound on the straggler effect.
2. **One round == one rollout.** A measured round is a fixed quantum of compute. A real
    agentic RLVR episode is 10-50 tool calls over seconds to minutes, so absolute times do not
    transfer. **Ratios do**, which is why this reports the amplification factor rather than
    leaning on the raw microseconds.
3. **No sandbox reuse cost.** Snapshot-restore between episodes is untested and not modelled.
4. **Homogeneous prompts.** Real prompt difficulty varies, adding a second source of spread on
    top of the infrastructure spread measured here.

Read the output as "what the infrastructure contributes to group-completion time", not as a
prediction of wall-clock training step time.
"""

from __future__ import annotations

import argparse
import csv
import math
import random
import statistics
import sys
from collections import defaultdict
from dataclasses import dataclass


# Group sizes that appear in the GRPO literature. 8 and 16 are the common defaults
# (DeepSeekMath used 64 for some runs); 64 is included because the straggler effect scales
# with G and large groups are where it becomes the dominant cost.
DEFAULT_GROUP_SIZES = (4, 8, 16, 32, 64)

# Bootstrap draws per (cell, G). 20k keeps the p99 of the group distribution stable to well
# under a percent while staying fast enough to run over every cell in a dataset.
DEFAULT_TRIALS = 20_000

# Fixed seed. A reader reproducing a published number must get the same value, and an
# unseeded bootstrap would make the numbers unrepeatable for no benefit.
DEFAULT_SEED = 0x5EED


@dataclass(frozen=True)
class Cell:
    """One config-and-density cell's measured rollout durations."""

    config: str
    density: int
    durations_us: tuple[float, ...]

    @property
    def n(self) -> int:
        return len(self.durations_us)


@dataclass
class GroupStats:
    """Group-completion statistics for one cell at one group size."""

    config: str
    density: int
    group_size: int
    n_samples: int
    # Per-rollout reference statistics, for the amplification comparison.
    rollout_mean_us: float
    rollout_p50_us: float
    rollout_p99_us: float
    # Group-completion (max-of-G) distribution.
    group_mean_us: float
    group_p50_us: float
    group_p99_us: float

    @property
    def amplification(self) -> float:
        """Median group-completion time divided by median single-rollout time.

        This is the headline number: how much longer a gradient step waits than a typical
        rollout takes. It is dimensionless, so it survives the fact that the synthetic
        workload's absolute durations do not transfer to a real episode.
        """
        if self.rollout_p50_us == 0:
            return 0.0
        return self.group_p50_us / self.rollout_p50_us

    @property
    def tail_exposure(self) -> float:
        """Fraction of groups whose completion time exceeds the per-rollout p99.

        Interpretation: the share of gradient steps gated by a rollout that the per-sandbox
        view would call a rare outlier. Under a group barrier these are not rare.
        """
        return self._tail_fraction

    _tail_fraction: float = 0.0


def nearest_rank(sorted_values: list[float], pct: float) -> float:
    """Nearest-rank percentile, matching the Go harness's measure.LatencySamples.

    Using the same definition as the harness matters: an interpolating percentile here would
    make these figures subtly incomparable with every p99 already published in
    analysis/PINNING-RESULTS.md.
    """
    n = len(sorted_values)
    if n == 0:
        return 0.0
    if pct <= 0:
        return sorted_values[0]
    if pct >= 100:
        return sorted_values[-1]
    rank = math.ceil(n * pct / 100.0)
    rank = max(1, min(rank, n))
    return sorted_values[rank - 1]


def load_cells(
    path: str,
    *,
    include_round_zero: bool = False,
    configs: set[str] | None = None,
) -> list[Cell]:
    """Read a samples CSV into per-cell duration lists.

    Round 0 is excluded by default. m8a's first round is about 33% slower than steady state
    (first-touch page allocation), and including it would put a startup artifact into the
    straggler analysis, inflating exactly the tail this script is about.
    """
    buckets: dict[tuple[str, int], list[float]] = defaultdict(list)

    with open(path, newline="") as fh:
        reader = csv.DictReader(fh)
        required = {"config", "density", "round", "duration_us"}
        missing = required - set(reader.fieldnames or ())
        if missing:
            raise SystemExit(
                f"{path}: missing required column(s): {', '.join(sorted(missing))}"
            )
        for row in reader:
            if not include_round_zero and int(row["round"]) == 0:
                continue
            cfg = row["config"]
            if configs and cfg not in configs:
                continue
            buckets[(cfg, int(row["density"]))].append(float(row["duration_us"]))

    cells = [
        Cell(config=cfg, density=density, durations_us=tuple(vals))
        for (cfg, density), vals in sorted(buckets.items())
    ]
    return cells


def group_completion(
    cell: Cell,
    group_size: int,
    *,
    trials: int,
    rng: random.Random,
) -> GroupStats:
    """Bootstrap the max-of-G distribution for one cell."""
    if cell.n == 0:
        raise ValueError(f"cell {cell.config}@{cell.density} has no samples")

    ordered = sorted(cell.durations_us)
    rollout_p50 = nearest_rank(ordered, 50)
    rollout_p99 = nearest_rank(ordered, 99)
    rollout_mean = statistics.fmean(ordered)

    # Draw G durations with replacement and keep the max. random.choices is used rather than
    # a per-draw loop because it pushes the sampling into C and this runs 20k times per cell
    # per group size.
    pool = cell.durations_us
    maxima = [max(rng.choices(pool, k=group_size)) for _ in range(trials)]
    maxima.sort()

    over_p99 = sum(1 for m in maxima if m > rollout_p99)

    stats = GroupStats(
        config=cell.config,
        density=cell.density,
        group_size=group_size,
        n_samples=cell.n,
        rollout_mean_us=rollout_mean,
        rollout_p50_us=rollout_p50,
        rollout_p99_us=rollout_p99,
        group_mean_us=statistics.fmean(maxima),
        group_p50_us=nearest_rank(maxima, 50),
        group_p99_us=nearest_rank(maxima, 99),
    )
    stats._tail_fraction = over_p99 / trials
    return stats


def analytic_tail_probability(group_size: int, per_rollout_tail: float = 0.01) -> float:
    """P(at least one of G rollouts lands in the tail), assuming independence.

    Kept as a cross-check on the bootstrap: for a tail defined at the per-rollout p99, the
    bootstrapped tail_exposure should land near this value. A large disagreement means the
    bootstrap or the percentile convention is wrong.
    """
    return 1.0 - (1.0 - per_rollout_tail) ** group_size


def write_csv(rows: list[GroupStats], out) -> None:
    w = csv.writer(out)
    w.writerow(
        [
            "config",
            "density",
            "group_size",
            "n_samples",
            "rollout_p50_us",
            "rollout_p99_us",
            "group_p50_us",
            "group_p99_us",
            "amplification",
            "tail_exposure",
        ]
    )
    for r in rows:
        w.writerow(
            [
                r.config,
                r.density,
                r.group_size,
                r.n_samples,
                f"{r.rollout_p50_us:.1f}",
                f"{r.rollout_p99_us:.1f}",
                f"{r.group_p50_us:.1f}",
                f"{r.group_p99_us:.1f}",
                f"{r.amplification:.3f}",
                f"{r.tail_exposure:.4f}",
            ]
        )


def print_report(rows: list[GroupStats], label: str, out=sys.stdout) -> None:
    """Human-readable report, leading with the interpretation rather than the table."""
    p = lambda s="": print(s, file=out)

    p(f"GRPO group-completion analysis: {label}")
    p()
    p("  Amplification = median group-completion time / median single-rollout time.")
    p("  Tail exposure = fraction of groups gated by a rollout slower than the")
    p("  per-rollout p99, i.e. the share of gradient steps waiting on what the")
    p("  per-sandbox view would call a rare outlier.")
    p()

    by_cell: dict[tuple[str, int], list[GroupStats]] = defaultdict(list)
    for r in rows:
        by_cell[(r.config, r.density)].append(r)

    p(
        f"{'cfg':<4} {'dens':>5} {'G':>4} {'rollout p50':>12} {'rollout p99':>12} "
        f"{'group p50':>11} {'group p99':>11} {'amp':>6} {'tail':>7}"
    )
    for key in sorted(by_cell):
        for r in sorted(by_cell[key], key=lambda x: x.group_size):
            p(
                f"{r.config:<4} {r.density:>5} {r.group_size:>4} "
                f"{r.rollout_p50_us:>12.0f} {r.rollout_p99_us:>12.0f} "
                f"{r.group_p50_us:>11.0f} {r.group_p99_us:>11.0f} "
                f"{r.amplification:>6.2f} {r.tail_exposure:>6.1%}"
            )

    p()
    p("  Cross-check, P(>=1 of G in the per-rollout tail) under independence:")
    sizes = sorted({r.group_size for r in rows})
    for g in sizes:
        observed = [r.tail_exposure for r in rows if r.group_size == g]
        if not observed:
            continue
        p(
            f"    G={g:<3} analytic {analytic_tail_probability(g):>6.1%}   "
            f"bootstrapped {min(observed):>6.1%} to {max(observed):>6.1%}"
        )


def main(argv: list[str] | None = None) -> int:
    ap = argparse.ArgumentParser(
        description="Derive GRPO group-completion time from sandbox latency samples.",
    )
    ap.add_argument("samples", help="path to a samples-*.csv from labsweep")
    ap.add_argument("--label", default="", help="dataset label for the report header")
    ap.add_argument(
        "--configs",
        default="",
        help="comma-separated placement configs to include (default: all)",
    )
    ap.add_argument(
        "--group-sizes",
        default=",".join(str(g) for g in DEFAULT_GROUP_SIZES),
        help="comma-separated GRPO group sizes",
    )
    ap.add_argument("--trials", type=int, default=DEFAULT_TRIALS)
    ap.add_argument("--seed", type=int, default=DEFAULT_SEED)
    ap.add_argument(
        "--include-round-zero",
        action="store_true",
        help="include round 0; excluded by default because m8a's first round is ~33%% slower",
    )
    ap.add_argument("--csv", default="", help="also write a CSV to this path")
    args = ap.parse_args(argv)

    configs = {c.strip() for c in args.configs.split(",") if c.strip()} or None
    group_sizes = [int(g) for g in args.group_sizes.split(",") if g.strip()]

    cells = load_cells(
        args.samples,
        include_round_zero=args.include_round_zero,
        configs=configs,
    )
    if not cells:
        raise SystemExit(f"{args.samples}: no cells matched")

    rng = random.Random(args.seed)
    rows = [
        group_completion(cell, g, trials=args.trials, rng=rng)
        for cell in cells
        for g in group_sizes
    ]

    print_report(rows, args.label or args.samples)

    if args.csv:
        with open(args.csv, "w", newline="") as fh:
            write_csv(rows, fh)
        print(f"\nwrote {args.csv}", file=sys.stderr)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
