#!/usr/bin/env bash
# Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
# SPDX-License-Identifier: MIT-0
#
# Fetch a Terminal-Bench task directory from upstream.
#
# tbench-to-rootfs.sh needs a task directory on local disk. This retrieves one without cloning the
# whole benchmark, which is large and mostly irrelevant to any single conversion.
#
# The task files are NOT vendored into this repository. They carry Terminal-Bench's canary string,
# which exists so benchmark data can be detected in training corpora, and committing them here
# would put that data in one more place it does not belong. Fetching on demand keeps the benchmark
# out of the repo while leaving the conversion reproducible: the commit pinned below fixes exactly
# which revision of a task was used.
#
# Usage:
#   ./fetch-tbench-task.sh hello-world
#   ./fetch-tbench-task.sh hello-world --out /tmp/tasks
#   ./fetch-tbench-task.sh --list
#
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib/common.sh
source "${SCRIPT_DIR}/lib/common.sh"

# The 1.x/2.0 benchmark, which is where original-tasks/ lives. Resolving the redirect matters more
# than it looks: laude-institute/terminal-bench now points at harbor-framework/terminal-bench-1,
# NOT at harbor-framework/terminal-bench. That second repo is the successor benchmark -- it has a
# tasks/ tree of 74 harbor-format tasks and no original-tasks/ at all, so aiming at it returns 404
# for every task here. It also no longer carries hello-world, removed upstream on 2026-07-21.
TBENCH_REPO="${TBENCH_REPO:-harbor-framework/terminal-bench-1}"
# Pinned so a conversion is reproducible. Upstream edits tasks in place, so "main" would silently
# change what was measured between one session and the next. This is the repo HEAD as of
# 2026-08-07; original-tasks/ was last touched by the 2026-01-22 rename that created it, so every
# task directory below is byte-identical to what the committed campaigns measured.
TBENCH_REF="${TBENCH_REF:-d28711d0da2675d0bb1d56de45ae5df6082438a3}"
# Newer harbor-format tasks use task.toml rather than task.yaml, which tbench-to-rootfs.sh does
# not read.
TBENCH_PATH="original-tasks"

TASK=""
OUT_BASE="${PWD}/tbench-tasks"
LIST=0

while [[ $# -gt 0 ]]; do
  case "$1" in
    --out)   [[ $# -ge 2 ]] || die "--out needs a directory"; OUT_BASE="$2"; shift 2 ;;
    --ref)   [[ $# -ge 2 ]] || die "--ref needs a commit SHA"; TBENCH_REF="$2"; shift 2 ;;
    --list)  LIST=1; shift ;;
    -h|--help) sed -n '2,18p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    -*) die "unknown argument: $1" ;;
    *)  TASK="$1"; shift ;;
  esac
done

require_cmd curl
require_cmd python3

API="https://api.github.com/repos/${TBENCH_REPO}/contents"
RAW="https://raw.githubusercontent.com/${TBENCH_REPO}/${TBENCH_REF}"

if (( LIST )); then
  log "listing tasks in ${TBENCH_REPO}/${TBENCH_PATH} @ ${TBENCH_REF}"
  curl -sSfL --max-time 60 "${API}/${TBENCH_PATH}?ref=${TBENCH_REF}" \
    | python3 -c 'import sys,json; [print(e["name"]) for e in json.load(sys.stdin) if e["type"]=="dir"]'
  exit 0
fi

[[ -n "$TASK" ]] || die "name a task, or pass --list. For example: $0 hello-world"

DEST="${OUT_BASE}/${TASK}"
mkdir -p "$DEST"

section "Fetching ${TASK}"

# Listed rather than assumed, because tasks carry different fixture files: analyze-access-logs
# ships an access_log, build-tcc-qemu a setup.sh. Guessing a fixed file list would produce an
# image whose Dockerfile COPY fails, or worse, one silently missing a fixture the tests need.
fetch_dir() {
  local rel="$1"
  local listing
  listing="$(curl -sSfL --max-time 60 "${API}/${TBENCH_PATH}/${TASK}/${rel}?ref=${TBENCH_REF}" 2>/dev/null)" \
    || return 1

  python3 -c '
import sys, json
for e in json.load(sys.stdin):
    print(e["type"], e["name"])
' <<<"$listing" | while read -r type name; do
    case "$type" in
      file)
        mkdir -p "${DEST}/${rel}"
        curl -sSfL --max-time 120 -o "${DEST}/${rel}/${name}" "${RAW}/${TBENCH_PATH}/${TASK}/${rel}/${name}" \
          && log "  ${rel}/${name}"
        ;;
      dir)
        fetch_dir "${rel}/${name}"
        ;;
    esac
  done
}

# Top level first, then recurse into subdirectories such as tests/.
listing="$(curl -sSfL --max-time 60 "${API}/${TBENCH_PATH}/${TASK}?ref=${TBENCH_REF}")" \
  || die "task ${TASK} not found in ${TBENCH_REPO}/${TBENCH_PATH} @ ${TBENCH_REF}. Try --list."

python3 -c '
import sys, json
for e in json.load(sys.stdin):
    print(e["type"], e["name"])
' <<<"$listing" | while read -r type name; do
  case "$type" in
    file)
      curl -sSfL --max-time 120 -o "${DEST}/${name}" "${RAW}/${TBENCH_PATH}/${TASK}/${name}" \
        && log "  ${name}"
      ;;
    dir)
      fetch_dir "$name"
      ;;
  esac
done

for f in task.yaml Dockerfile run-tests.sh; do
  [[ -f "${DEST}/${f}" ]] || die "fetched ${TASK} but ${f} is missing; upstream layout may have changed"
done
[[ -d "${DEST}/tests" ]] || die "fetched ${TASK} but tests/ is missing; nothing would decide pass or fail"

ok "fetched ${TASK} to ${DEST}"

cat <<SUMMARY

  Convert it, checking the reward moves in both directions:

    ./tbench-to-rootfs.sh --task ${DEST} --dry-run           # unsolved: tests must fail
    ./tbench-to-rootfs.sh --task ${DEST} --solve --dry-run   # solved: tests must pass

SUMMARY
