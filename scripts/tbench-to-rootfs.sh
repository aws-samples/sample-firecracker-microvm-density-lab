#!/usr/bin/env bash
# Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
# SPDX-License-Identifier: MIT-0
#
# Convert a Terminal-Bench task directory into a bootable Firecracker rootfs.
#
# Companion to docker-to-rootfs.sh, which converts a *published* image. Terminal-Bench does not
# publish per-task images: each task ships a Dockerfile plus its tests, and the upstream harness
# builds and bind-mounts them at run time. So this script builds first, stages what the harness
# would have mounted, and then hands off to docker-to-rootfs.sh for the conversion itself.
#
# # A Terminal-Bench task directory
#
#   task.yaml            instruction, parser_name, max_test_timeout_sec
#   Dockerfile           FROM ghcr.io/laude-institute/t-bench/<base>:<date>, plus task setup
#   docker-compose.yaml  one `client` service; passes TEST_DIR to the container
#   run-tests.sh         installs uv + pytest, then runs pytest against $TEST_DIR
#   tests/               the pytest files that decide pass or fail
#   solution.sh          the oracle solution
#
# # Three things the harness does that a converted image must do for itself
#
#   1. Build the image. There is nothing to pull.
#   2. Supply run-tests.sh and tests/. Upstream bind-mounts them, so `docker export` of a built
#      image contains neither, and the guest would have nothing to run.
#   3. Set TEST_DIR. Compose passes it as an environment variable; the guest agent execs the task
#      with execv and no environment, so the path is resolved into the command at build time.
#
# # Network
#
# Upstream's run-tests.sh runs `apt-get update` and downloads uv from astral.sh *at test time*. At
# density that is one egress per microVM per task, it makes results depend on package-mirror
# availability, and it charges network latency to the task's timing. This script installs uv and
# pytest during conversion and rewrites run-tests.sh to run offline. The original is kept in the
# image as /tests/run-tests.sh.upstream so the deviation is visible in the artifact.
#
# # Usage
#
#   sudo ./tbench-to-rootfs.sh --task <dir> --agent <path> [--solve] [--out <file>]
#   ./tbench-to-rootfs.sh --task <dir> --dry-run     # build and report, write no image, no root
#
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib/common.sh
source "${SCRIPT_DIR}/lib/common.sh"

TASK_DIR=""
AGENT_BIN=""
OUT=""
MANIFEST=""
DRY_RUN=0
# Apply the task's oracle solution. The Terminal-Bench analogue of SWE-bench's gold patch, and it
# exists for the same reason: a reward that only ever reports failure is indistinguishable from a
# broken harness, so both directions have to be demonstrable.
SOLVE=0
PLATFORM="linux/amd64"
# uv and pytest versions come from upstream's run-tests.sh, so the pre-baked environment matches
# what the task would have installed for itself.
UV_VERSION="0.7.13"
PYTEST_VERSION="8.4.1"
# Where the harness's bind mounts land inside the image.
TESTS_DIR="/tests"

while [[ $# -gt 0 ]]; do
  case "$1" in
    --task)      TASK_DIR="$2"; shift 2 ;;
    --agent)     AGENT_BIN="$2"; shift 2 ;;
    --out)       OUT="$2"; shift 2 ;;
    --manifest)  MANIFEST="$2"; shift 2 ;;
    --platform)  PLATFORM="$2"; shift 2 ;;
    --solve)     SOLVE=1; shift ;;
    --dry-run)   DRY_RUN=1; shift ;;
    -h|--help)   sed -n '2,37p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) die "unknown argument: $1" ;;
  esac
done

[[ -n "$TASK_DIR" ]] || die "--task is required: a Terminal-Bench task directory"
[[ -d "$TASK_DIR" ]] || die "task directory not found: $TASK_DIR"
require_cmd docker
require_cmd python3

TASK_DIR="$(cd "$TASK_DIR" && pwd)"
TASK_NAME="$(basename "$TASK_DIR")"

section "1. Inspect the task"

for f in task.yaml Dockerfile run-tests.sh; do
  [[ -f "${TASK_DIR}/${f}" ]] || die "${TASK_DIR} has no ${f}; is this a Terminal-Bench task directory?"
done
[[ -d "${TASK_DIR}/tests" ]] || die "${TASK_DIR} has no tests/ directory, so nothing decides pass or fail"

# The parser is what turns test output into a reward. Upstream tasks declare pytest, whose exit
# status is already the signal the guest agent reports. Any other parser means the exit code is
# NOT the reward -- a task could exit 0 with failing tests -- so refuse rather than silently
# record a number that means something else.
PARSER="$(python3 - "$TASK_DIR/task.yaml" <<'PY'
import re, sys
text = open(sys.argv[1]).read()
m = re.search(r'^parser_name:\s*(\S+)', text, re.M)
print(m.group(1) if m else "")
PY
)"
[[ -n "$PARSER" ]] || die "task.yaml declares no parser_name"
if [[ "$PARSER" != "pytest" ]]; then
  die "parser_name is '${PARSER}', not pytest. The guest reports the test command's exit code as
the reward, which is only correct for pytest. Supporting ${PARSER} means teaching the agent to
parse output, not just wait on a process."
fi

TEST_TIMEOUT="$(python3 - "$TASK_DIR/task.yaml" <<'PY'
import re, sys
text = open(sys.argv[1]).read()
m = re.search(r'^max_test_timeout_sec:\s*([0-9.]+)', text, re.M)
print(m.group(1) if m else "0")
PY
)"

# Compose may define sidecar services (a database, a second host). Those become separate containers
# upstream and would need separate microVMs here, which is beyond one rootfs. Detect and refuse
# rather than convert the first service and silently drop the rest.
if [[ -f "${TASK_DIR}/docker-compose.yaml" ]]; then
  SERVICES="$(python3 - "${TASK_DIR}/docker-compose.yaml" <<'PY'
import re, sys
text = open(sys.argv[1]).read()
# Service names are the keys indented one level under `services:`. A YAML parser would be better
# but is not in the base image; the compose files here are uniform enough for this to be safe.
in_services = False
names = []
for line in text.splitlines():
    if re.match(r'^services:', line):
        in_services = True
        continue
    if in_services:
        if re.match(r'^\S', line):
            break
        m = re.match(r'^  (\w[\w.-]*):', line)
        if m:
            names.append(m.group(1))
print(" ".join(names))
PY
)"
  COUNT="$(printf '%s' "$SERVICES" | wc -w | tr -d ' ')"
  if [[ "$COUNT" -gt 1 ]]; then
    die "docker-compose.yaml defines ${COUNT} services (${SERVICES}). Multi-service tasks need one
microVM per service and are out of scope; pick a single-service task."
  fi
fi

ok "task ${TASK_NAME}, parser ${PARSER}, test timeout ${TEST_TIMEOUT}s"

section "2. Build the task image"

# Tag locally: there is no upstream image to pull, and the tag is what docker-to-rootfs.sh consumes.
IMAGE_TAG="tbench-local/${TASK_NAME}:converted"
log "docker build ${IMAGE_TAG} (${PLATFORM})"
# The build context is the task directory, because Dockerfiles here COPY task fixtures from it.
docker build --platform "$PLATFORM" -t "$IMAGE_TAG" -f "${TASK_DIR}/Dockerfile" "$TASK_DIR" \
  >/tmp/tbench-build-"${TASK_NAME}".log 2>&1 \
  || { tail -20 /tmp/tbench-build-"${TASK_NAME}".log >&2; die "build failed; see /tmp/tbench-build-${TASK_NAME}.log"; }
ok "built ${IMAGE_TAG}"

# The working directory has to come from *running* the image, not inspecting it.
#
# These bases set WORKDIR in a parent layer that `docker image inspect .Config.WorkingDir` reports
# as empty, while a container started from the same image lands in /app. And it matters: upstream's
# run-tests.sh exits 1 with "No working directory set" when PWD is /, so an empty value taken from
# inspect would fail every task for a reason that has nothing to do with the task.
WORKDIR="$(docker run --rm --platform "$PLATFORM" "$IMAGE_TAG" sh -c 'pwd' 2>/dev/null | tr -d '\r\n')"
[[ -n "$WORKDIR" && "$WORKDIR" != "/" ]] \
  || die "image working directory resolved to '${WORKDIR:-empty}'; run-tests.sh refuses to run from /"
ok "working directory ${WORKDIR}"

# Resolve the interpreter to an absolute path, because the guest agent execs the task with execv,
# which takes a path and does NOT search PATH. A bare "bash" exits 127 -- command not found -- with
# bash sitting right there in the image. Cost a gate failure on hardware before being caught: the
# pass-rate check reported it, but a 2 ms "task" would otherwise have looked like a fast success.
BASH_PATH="$(docker run --rm --platform "$PLATFORM" "$IMAGE_TAG" sh -c 'command -v bash' 2>/dev/null | tr -d '\r\n')"
[[ -n "$BASH_PATH" ]] || die "no bash in the image; the task's run-tests.sh needs one"
ok "interpreter ${BASH_PATH}"

# Stated once. It is baked into the image, verified locally, and recorded in the manifest, and a
# mismatch between those three would mean the verified command is not the one the guest runs.
TASK_CMD="${BASH_PATH} ${TESTS_DIR}/run-tests.sh"

# The first word must be an absolute path. The guest agent uses execv, which does not search PATH,
# so a bare command name exits 127 no matter what the image contains. Checked here because the
# failure surfaces only on hardware, as an exit code with no message, and a 2 ms "task" reads as a
# suspiciously fast success rather than an error.
case "${TASK_CMD%% *}" in
  /*) ;;
  *) die "task command '${TASK_CMD}' does not begin with an absolute path; the guest execs it with
execv, which does not search PATH, so this would exit 127" ;;
esac

section "3. Stage tests and pre-bake the test environment"

# A second build layered on the first, doing what the harness's bind mounts and run-tests.sh would
# otherwise do at run time. Written to a temp dir so the task directory is never modified.
STAGE="$(mktemp -d)"
cleanup_stage() { rm -rf "$STAGE"; }
trap cleanup_stage EXIT

cp -R "${TASK_DIR}/tests" "${STAGE}/tests"
cp "${TASK_DIR}/run-tests.sh" "${STAGE}/run-tests.sh.upstream"
[[ -f "${TASK_DIR}/solution.sh" ]] && cp "${TASK_DIR}/solution.sh" "${STAGE}/solution.sh"

# The offline replacement for run-tests.sh.
#
# Upstream's version installs curl, downloads uv, creates a venv, installs pytest, then runs
# pytest. Everything before the last step is provisioning, which has already happened in the layer
# below by the time this runs. TEST_DIR is written in literally because the guest agent execs with
# execv and passes no environment.
cat > "${STAGE}/run-tests.sh" <<EOF
#!/bin/bash
# Written by tbench-to-rootfs.sh. Offline equivalent of the task's own run-tests.sh, whose
# original is kept alongside as run-tests.sh.upstream.
#
# Removed: apt-get update, apt-get install curl, the astral.sh uv download, and the venv creation.
# All of it is baked into the image, so a microVM needs no network to run its own tests. What is
# kept is the part that decides the reward: pytest's exit status.
#
# TEST_DIR is literal because the guest agent runs the task via execv with no environment, so
# nothing would expand a variable here.
#
# Requires a writable /tmp: pytest's output capture opens a temporary file and aborts with
# "No usable temporary directory found" without one. The guest agent mounts a tmpfs there at boot,
# since the rootfs itself is read-only so that one host page-cache copy can serve the whole fleet.
#
# The read-only rootfs needs two further accommodations, both found by running a build-heavy task:
#
#  -p no:cacheprovider   pytest writes .pytest_cache next to the test file, and a failure to do so
#                        aborts *collection* with exit 2 -- indistinguishable from a genuine test
#                        error unless the output is read.
#  a writable CWD        some tasks write relative paths from the working directory at test time
#                        (build-tcc-qemu does open("a.exp", "w")), and WORKDIR lives on the
#                        read-only image. Running from a tmpfs copy of the workdir keeps those
#                        writes off the shared image while leaving absolute paths into it intact.
set -u

WORK=/tmp/task-cwd
mkdir -p "\$WORK" || exit 1
# Symlink the workdir's contents rather than copying: a build task's workdir holds gigabytes of
# compiled output, and copying it per microVM would defeat the shared read-only image entirely.
# Relative reads resolve through the links; relative *writes* land on the tmpfs.
for entry in "${WORKDIR}"/* "${WORKDIR}"/.[!.]*; do
  [ -e "\$entry" ] || continue
  ln -sf "\$entry" "\$WORK/" 2>/dev/null || true
done
cd "\$WORK" || exit 1

exec python3 -m pytest "${TESTS_DIR}/test_outputs.py" -rA -p no:cacheprovider
EOF
chmod 0755 "${STAGE}/run-tests.sh"

# Layer the staged files in and install the test runner. --break-system-packages because these
# bases are externally-managed Debian/Ubuntu Pythons (PEP 668) and the image is single-purpose, so
# a venv would add a path to resolve for no isolation benefit inside a disposable microVM.
cat > "${STAGE}/Dockerfile" <<EOF
FROM ${IMAGE_TAG}
COPY tests ${TESTS_DIR}
COPY run-tests.sh ${TESTS_DIR}/run-tests.sh
COPY run-tests.sh.upstream ${TESTS_DIR}/run-tests.sh.upstream
RUN python3 -m pip install --no-cache-dir --break-system-packages "pytest==${PYTEST_VERSION}" \\
 || (apt-get update && apt-get install -y --no-install-recommends python3-pip \\
     && python3 -m pip install --no-cache-dir --break-system-packages "pytest==${PYTEST_VERSION}")
RUN python3 -m pytest --version
EOF

STAGED_TAG="tbench-local/${TASK_NAME}:staged"
log "layering tests and pytest ${PYTEST_VERSION} into ${STAGED_TAG}"
docker build --platform "$PLATFORM" -t "$STAGED_TAG" "$STAGE" \
  >/tmp/tbench-stage-"${TASK_NAME}".log 2>&1 \
  || { tail -20 /tmp/tbench-stage-"${TASK_NAME}".log >&2; die "staging build failed; see /tmp/tbench-stage-${TASK_NAME}.log"; }
ok "tests staged at ${TESTS_DIR}, pytest pre-baked"

# Verify the tests can actually run offline, before an image is written. This is the check that
# would otherwise fail on a paid host: --network none proves nothing reaches out.
log "verifying the test command runs with no network"
if docker run --rm --network none --platform "$PLATFORM" "$STAGED_TAG" \
     $TASK_CMD >/tmp/tbench-verify-"${TASK_NAME}".log 2>&1; then
  UNSOLVED_RC=0
else
  UNSOLVED_RC=$?
fi
if grep -qE "No module named pytest|command not found|Temporary failure resolving" \
     /tmp/tbench-verify-"${TASK_NAME}".log; then
  tail -10 /tmp/tbench-verify-"${TASK_NAME}".log >&2
  die "the pre-baked environment is incomplete; see /tmp/tbench-verify-${TASK_NAME}.log"
fi
# Exit non-zero is the expected outcome for an unsolved task: its tests are supposed to fail until
# the work is done. A zero here means the tests pass without the task being solved, which would
# make the reward a constant.
if [[ "$UNSOLVED_RC" -eq 0 ]]; then
  warn "the unsolved task's tests PASSED (exit 0). The reward cannot distinguish solved from"
  warn "unsolved, so this task is not usable as an RLVR signal as converted."
else
  ok "unsolved task fails as expected (exit ${UNSOLVED_RC}), so the reward can move"
fi

FINAL_TAG="$STAGED_TAG"
# Capitalised because this value is interpolated into the Python that writes the manifest, where
# `false` is a NameError rather than a boolean.
SOLVED=False

# --solve applies the oracle solution, which is how the other direction of the reward is shown.
if (( SOLVE )); then
  [[ -f "${TASK_DIR}/solution.sh" ]] || die "--solve given but ${TASK_DIR} has no solution.sh"
  cat > "${STAGE}/Dockerfile.solve" <<EOF
FROM ${STAGED_TAG}
COPY solution.sh /solution.sh
RUN bash /solution.sh
EOF
  SOLVED_TAG="tbench-local/${TASK_NAME}:solved"
  log "applying solution.sh"
  docker build --platform "$PLATFORM" -t "$SOLVED_TAG" -f "${STAGE}/Dockerfile.solve" "$STAGE" \
    >/tmp/tbench-solve-"${TASK_NAME}".log 2>&1 \
    || { tail -20 /tmp/tbench-solve-"${TASK_NAME}".log >&2; die "solution failed; see /tmp/tbench-solve-${TASK_NAME}.log"; }

  if docker run --rm --network none --platform "$PLATFORM" "$SOLVED_TAG" \
       $TASK_CMD >/tmp/tbench-solved-"${TASK_NAME}".log 2>&1; then
    ok "solved task passes (exit 0): the reward moves in both directions"
  else
    tail -10 /tmp/tbench-solved-"${TASK_NAME}".log >&2
    die "the oracle solution did not make the tests pass. Either the conversion is wrong or the
task's own solution is stale; either way the reward is not trustworthy."
  fi
  FINAL_TAG="$SOLVED_TAG"
  SOLVED=True
fi

section "4. Convert to a rootfs"

# The reward direction the caller should expect from this image. An unsolved task must fail, so its
# expected exit code is non-zero; labsweep takes this as -task-expect-exit.
EXPECT_EXIT=$(( SOLVE ? 0 : UNSOLVED_RC ))

if (( DRY_RUN )); then
  # Content size measured the same way docker-to-rootfs.sh does, because `docker image inspect
  # .Size` is unusable on these images: the ubuntu-24-04 base reports 2469 bytes against 125 MB of
  # real content. Sizing a rootfs from that would produce an image that cannot be extracted into.
  C="$(docker create --platform "$PLATFORM" "$FINAL_TAG" /bin/true)"
  BYTES="$(docker export "$C" | wc -c | tr -dc '0-9')"
  docker rm -f "$C" >/dev/null 2>&1 || true
  MIB=$(( BYTES / 1024 / 1024 ))
  cat <<SUMMARY

  Dry run, no image written.

  Task:         ${TASK_NAME}
  Image:        ${FINAL_TAG}
  Content:      ${MIB} MiB flattened
  Rootfs:       ~$(( MIB + MIB * 30 / 100 + 256 )) MiB
  Workdir:      ${WORKDIR}
  Test command: ${TASK_CMD}
  Solution:     $( ((SOLVE)) && echo applied || echo "not applied (unsolved)" )
  Expect exit:  ${EXPECT_EXIT}
  Test timeout: ${TEST_TIMEOUT}s  (host -timeout must exceed this)

  Re-run with --agent <path> and without --dry-run to build the image.

SUMMARY
  exit 0
fi

[[ -n "$AGENT_BIN" ]] || die "--agent is required: the guest agent runs as PID 1"

OUT="${OUT:-${ARTIFACT_DIR}/rootfs-tbench-${TASK_NAME}.ext4}"
MANIFEST="${MANIFEST:-${OUT%.ext4}.manifest.json}"

# Hand off rather than reimplement. docker-to-rootfs.sh already measures content by export, formats
# without a journal, creates device nodes, installs the agent as /init, writes /etc/fc-task.env and
# runs e2fsck. Duplicating its sizing logic is exactly the bug that cost a session earlier.
"${SCRIPT_DIR}/docker-to-rootfs.sh" \
  --image "$FINAL_TAG" \
  --agent "$AGENT_BIN" \
  --instance "$TASK_NAME" \
  --task-cmd "$TASK_CMD" \
  --out "$OUT" \
  --manifest "${MANIFEST}.inner" \
  --platform "$PLATFORM"

# docker-to-rootfs.sh prints its own numbered steps and a closing summary naming the .inner
# manifest. That file is an intermediate: it is merged below and deleted, and the path worth
# quoting is the merged one printed at the end of this script.
section "5. Terminal-Bench manifest"

# Terminal-Bench specific provenance, wrapping what docker-to-rootfs.sh recorded. The deviations
# from upstream matter most here: a reader has to be able to tell that the network steps were
# removed and whether the oracle solution was applied.
python3 - "$MANIFEST" "${MANIFEST}.inner" <<PY
import json, sys
out, inner = sys.argv[1], sys.argv[2]
try:
    base = json.load(open(inner))
except Exception:
    base = {}
base.update({
    "benchmark": "terminal-bench",
    "task_name": "${TASK_NAME}",
    "parser_name": "${PARSER}",
    "max_test_timeout_sec": float("${TEST_TIMEOUT}"),
    "task_workdir": "${WORKDIR}",
    "task_command": "${TASK_CMD}",
    "solution_applied": ${SOLVED},
    "expected_exit": ${EXPECT_EXIT},
    "pytest_version": "${PYTEST_VERSION}",
    "network_at_test_time": False,
    "upstream_run_tests_kept_at": "${TESTS_DIR}/run-tests.sh.upstream",
    "deviations_from_upstream": [
        "apt-get update and apt-get install curl removed; not needed offline",
        "uv ${UV_VERSION} download from astral.sh removed; pytest ${PYTEST_VERSION} pre-baked instead",
        "venv creation removed; pytest installed into the system Python of a single-purpose image",
        "TEST_DIR resolved to ${TESTS_DIR} at build time, since the guest agent execs with no environment",
    ],
})
json.dump(base, open(out, "w"), indent=2, sort_keys=True)
print(f"wrote {out}")
PY
rm -f "${MANIFEST}.inner"
ok "wrote ${MANIFEST}"

cat <<SUMMARY

  Rootfs:       ${OUT}
  Manifest:     ${MANIFEST}
  Task:         ${TASK_NAME}
  Solution:     $( ((SOLVE)) && echo applied || echo "not applied (unsolved)" )
  Expect exit:  ${EXPECT_EXIT}

  Boot one microVM and run the task. -task-timeout-sec makes labsweep refuse a per-request
  deadline below the task's own ${TEST_TIMEOUT}s, which would count a slow task as a failed one:

    labsweep -task-instance ${TASK_NAME} -task-expect-exit ${EXPECT_EXIT} \\
      -task-timeout-sec ${TEST_TIMEOUT} -request-timeout $(( ${TEST_TIMEOUT%.*} + 60 ))s \\
      -configs B -densities 1 -repeats 1 -rootfs ${OUT} -mem 2048 -timeout 20m

SUMMARY

ok "done"
