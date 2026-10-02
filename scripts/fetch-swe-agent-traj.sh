#!/usr/bin/env bash
# Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
# SPDX-License-Identifier: MIT-0
#
# Fetch the recorded SWE-agent trajectory that the episode campaign replayed.
#
# traj-to-episode.py needs a .traj file on local disk. The file is NOT vendored into this
# repository: it is upstream's artifact, and the same argument that keeps the Terminal-Bench task
# files out of the tree applies here. Fetching on demand keeps the conversion reproducible instead,
# because the commit pinned below fixes exactly which revision was used.
#
# Which demonstration, and why it matters: upstream ships EIGHT trajectories for
# marshmallow-code__marshmallow-1867, one per agent configuration, and they are not
# interchangeable. Running each through traj-to-episode.py separates them cleanly -- four fail with
# "unrecognised edit form" because their function-calling and xml configurations emit an edit syntax
# the converter does not read, three convert to 11 turns, and exactly one converts to the 14 turns
# (6 shell + 8 DSL) that raw/episode-sweep-20260804 recorded. That one is the default __install-1
# ___install_from_source configuration selected below. The sweep JSON records only
# "SWE-agent/SWE-agent trajectories/demonstrations", which is not enough to identify it, so the
# turn count is what pins it.
#
# Usage:
#   ./fetch-swe-agent-traj.sh
#   ./fetch-swe-agent-traj.sh --out /tmp/traj
#
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib/common.sh
source "${SCRIPT_DIR}/lib/common.sh"

TRAJ_REPO="${TRAJ_REPO:-SWE-agent/SWE-agent}"
# Pinned for the same reason as TBENCH_REF: upstream revises demonstrations in place, and a replay
# measured against a different revision is not comparable to the committed campaign. Repo HEAD as
# of 2026-08-07.
TRAJ_REF="${TRAJ_REF:-3ea751c087f32b16e039a2233dd6eefecef325d5}"
TRAJ_INSTANCE="marshmallow-code__marshmallow-1867"
TRAJ_DEMO="replay__${TRAJ_INSTANCE}__default__t-0.20__p-0.95__c-2.00__install-1___install_from_source"

# The turn counts the committed campaign reported. Checked after conversion rather than trusted,
# since a silently-revised trajectory is the failure this pin exists to prevent.
EXPECT_TURNS=14
EXPECT_SHELL=6
EXPECT_DSL=8

OUT_BASE="${PWD}/swe-agent-traj"

while [[ $# -gt 0 ]]; do
  case "$1" in
    --out) [[ $# -ge 2 ]] || die "--out needs a directory"; OUT_BASE="$2"; shift 2 ;;
    --ref) [[ $# -ge 2 ]] || die "--ref needs a commit SHA"; TRAJ_REF="$2"; shift 2 ;;
    -h|--help) sed -n '2,22p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) die "unknown argument: $1" ;;
  esac
done

require_cmd curl
require_cmd python3

DEST="${OUT_BASE}/${TRAJ_INSTANCE}.traj"
mkdir -p "$OUT_BASE"

section "Fetching ${TRAJ_INSTANCE}.traj"

RAW="https://raw.githubusercontent.com/${TRAJ_REPO}/${TRAJ_REF}/trajectories/demonstrations/${TRAJ_DEMO}/${TRAJ_INSTANCE}.traj"
curl -sSfL --max-time 120 -o "$DEST" "$RAW" \
  || die "could not fetch ${TRAJ_INSTANCE}.traj from ${TRAJ_REPO} @ ${TRAJ_REF}; upstream may have moved or revised the demonstrations directory"

RECORDED="$(python3 -c 'import json,sys; t=json.load(open(sys.argv[1])).get("trajectory"); sys.exit(1) if not t else print(len(t))' "$DEST")" \
  || die "fetched ${DEST} but it has no trajectory array; upstream format may have changed"
ok "fetched ${DEST} (${RECORDED} recorded actions)"

# Converted here rather than left to the caller because the turn count is the only thing that
# identifies this demonstration among the eight, so it is also the only way to detect that upstream
# swapped it. Discovering that at the density gate costs a paid host.
section "Confirming it converts to the campaign's episode"

# Captured whole, then parsed, rather than piped straight into awk. Piping would make the
# assignment itself the failing command under pipefail, so the script would abort here and the die
# below -- the one that explains what actually went wrong -- would never run. That is not a corner
# case: the four demonstrations using an edit syntax the converter cannot read exit nonzero, which
# is exactly the "upstream swapped the demo" condition this check exists to report.
CONVERSION="$(python3 "${SCRIPT_DIR}/traj-to-episode.py" --traj "$DEST" --dry-run \
  --verify-cmd '/opt/miniconda3/envs/testbed/bin/python -m pytest -q' 2>&1)" \
  || die "traj-to-episode.py could not convert ${DEST}, so this is not the demonstration the campaign replayed. Upstream ships eight for this instance ID and four use an edit syntax the converter does not read. It said:"$'\n'"${CONVERSION}"

read -r GOT_TURNS GOT_SHELL GOT_DSL \
  <<<"$(awk '/^turns:/ {gsub(/[^0-9 ]/, ""); print; exit}' <<<"$CONVERSION")"

[[ "$GOT_TURNS" == "$EXPECT_TURNS" && "$GOT_SHELL" == "$EXPECT_SHELL" && "$GOT_DSL" == "$EXPECT_DSL" ]] \
  || die "converts to ${GOT_TURNS:-?} turns (${GOT_SHELL:-?} shell, ${GOT_DSL:-?} DSL), but raw/episode-sweep-20260804 recorded ${EXPECT_TURNS} (${EXPECT_SHELL} shell, ${EXPECT_DSL} DSL). Upstream has revised this demonstration: a replay from it is not comparable to the committed campaign."
ok "converts to ${EXPECT_TURNS} turns (${EXPECT_SHELL} shell, ${EXPECT_DSL} DSL), matching raw/episode-sweep-20260804"

cat <<SUMMARY

  Build the episode script. --verify-cmd is required and decides the reward:

    ./traj-to-episode.py --traj ${DEST} \\
      --verify-cmd '/opt/miniconda3/envs/testbed/bin/python -m pytest tests/test_utils.py -q' \\
      --out episode.sh --manifest episode-manifest.json

  Turn 10 fails and turn 11 retries, exactly as recorded: the agent's first edit was rejected for
  bad indentation, so the replay writes line 1475 unindented, fails to compile, rolls back, and
  rewrites it indented. That is why the sweep expects 13 of 14 turns to succeed:

    labsweep -episode-instance ${TRAJ_INSTANCE} -episode-turns ${EXPECT_TURNS} -episode-turns-ok 13 ...

SUMMARY
