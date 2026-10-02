#!/usr/bin/env bash
# Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
# SPDX-License-Identifier: MIT-0
#
# One-shot host setup: fetch staged artifacts, provision, build the guest kernel, build the
# guest agent, and build the rootfs.
#
# This exists because the same six steps have been driven by hand over SSM in every session so
# far, and each session rediscovered the same three snags:
#
#   1. provision-host.sh ends with an interactive reboot prompt, so it must be run with stdin
#      closed and its non-zero exit tolerated.
#   2. Rust is NOT preinstalled on Amazon Linux 2023. An earlier session only had cargo because
#      an unrelated command pulled it in, so the next session failed with exit 127.
#   3. glibc-static is needed for the +crt-static build; without it the linker cannot find -lc.
#
# It does not reboot. Kernel args and KVM module parameters need one, so the caller reboots and
# then runs validate-host.sh.
#
# Usage (on the host, as root):
#   ./setup-lab-host.sh --bucket fc-lab-transfer-<account-id>
#
# To also convert a SWE-bench instance image into a task rootfs, add:
#   --swebench-image <ref> --task-cmd '<argv>' [--test-patch <file>]
#
# Or, for Terminal-Bench, name the tasks and the host fetches and builds each one:
#   --tbench-tasks hello-world,analyze-access-logs [--tbench-solve]
#
# One benchmark per host. Each produces its own task rootfs, and mixing them in one session would
# make it unclear which image a result came from.
#
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib/common.sh
source "${SCRIPT_DIR}/lib/common.sh"

BUCKET=""
REGION="${AWS_REGION:-us-east-1}"
WORK="/root"
# Converting a real task image is opt-in: it pulls Docker onto the host and adds several minutes
# and a few GiB, which a synthetic-workload session has no use for.
SWEBENCH_IMAGE=""
SWEBENCH_TASK_CMD=""
SWEBENCH_TEST_PATCH=""
# An episode script from traj-to-episode.py. Baked into the converted image so RUN_EPISODE
# replays a full RL episode -- the agent's recorded turns, then the verifier -- rather than only
# the verifier run that RUN_TASK measures.
EPISODE_SCRIPT=""
# Terminal-Bench tasks are named, not published as images: the host fetches the task directory and
# builds it. Comma-separated, since a session measures several tasks and each becomes its own rootfs.
TBENCH_TASKS=""
# Apply each task's oracle solution, so the converted image is a *solved* instance whose tests pass.
# Off by default, matching the SWE-bench path where the unsolved instance is the thing measured.
TBENCH_SOLVE=0

while [[ $# -gt 0 ]]; do
  case "$1" in
    --bucket) BUCKET="$2"; shift 2 ;;
    --region) REGION="$2"; shift 2 ;;
    --work)   WORK="$2"; shift 2 ;;
    --swebench-image) SWEBENCH_IMAGE="$2"; shift 2 ;;
    --task-cmd)       SWEBENCH_TASK_CMD="$2"; shift 2 ;;
    --test-patch)     SWEBENCH_TEST_PATCH="$2"; shift 2 ;;
    --episode)        EPISODE_SCRIPT="$2"; shift 2 ;;
    --tbench-tasks)   TBENCH_TASKS="$2"; shift 2 ;;
    --tbench-solve)   TBENCH_SOLVE=1; shift ;;
    -h|--help) sed -n '2,24p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) die "unknown argument: $1" ;;
  esac
done

# Checked before anything is installed or built, so a missing task command is reported in seconds
# rather than after the kernel and rootfs builds have already run.
if [[ -n "$SWEBENCH_IMAGE" && -z "$SWEBENCH_TASK_CMD" && -z "$EPISODE_SCRIPT" ]]; then
  die "--swebench-image needs --task-cmd or --episode: the image's CMD is a shell, not a test command"
fi
if [[ -n "$EPISODE_SCRIPT" && ! -f "$EPISODE_SCRIPT" ]]; then
  die "episode script not found: $EPISODE_SCRIPT"
fi
if [[ -n "$SWEBENCH_IMAGE" && -n "$TBENCH_TASKS" ]]; then
  die "--swebench-image and --tbench-tasks both given; each produces its own task rootfs, so run one benchmark per host"
fi

# Both benchmarks need Docker and git on the host.
NEEDS_DOCKER=0
[[ -n "$SWEBENCH_IMAGE" || -n "$TBENCH_TASKS" ]] && NEEDS_DOCKER=1

[[ -n "$BUCKET" ]] || die "--bucket is required"
require_root
require_cmd aws

section "1. Fetch staged artifacts"
cd "$WORK"
aws s3 cp "s3://${BUCKET}/staging/" . --recursive --region "$REGION" --only-show-errors
for f in labsweep labctl scripts.tgz guest-agent-src.tgz; do
  [[ -f "$f" ]] || die "missing staged artifact: $f"
done
tar xzf scripts.tgz
tar xzf guest-agent-src.tgz
chmod +x labsweep labctl scripts/*.sh
ok "artifacts fetched and unpacked"

section "2. Build dependencies"
# Rust is not in the AL2023 base image, and glibc-static is needed for +crt-static. Installing
# both here rather than assuming either is present.
DEPS=(cargo rust glibc-static)
# Docker and git only when a SWE-bench image is being converted. The conversion runs on the host
# rather than the workstation because the flattened image is ~2.5 GiB and the resulting rootfs
# ~3.5 GiB, so building it here avoids moving both over the network. git is needed to apply the
# instance's test patch into the image's checkout.
if (( NEEDS_DOCKER )); then
  DEPS+=(docker git)
fi
dnf install -y "${DEPS[@]}" >/tmp/deps.log 2>&1 \
  || die "dependency install failed; see /tmp/deps.log"
ok "cargo $(cargo --version 2>/dev/null | awk '{print $2}'), glibc-static installed"
if (( NEEDS_DOCKER )); then
  systemctl enable --now docker >/dev/null 2>&1 || die "could not start docker"
  ok "docker $(docker --version | awk '{print $3}' | tr -d ,) running"
fi

section "3. Provision host"
# stdin closed: the script ends with an interactive "Reboot now?" prompt that cannot be answered
# over SSM. Its non-zero exit at that prompt is expected, so the OK count is checked instead.
cd "${WORK}/scripts"
(timeout 1800 ./provision-host.sh </dev/null >/tmp/provision.log 2>&1 || true)
provision_ok="$(grep -cE '\[ OK \]' /tmp/provision.log || true)"
if grep -q '\[FAIL\]' /tmp/provision.log; then
  grep '\[FAIL\]' /tmp/provision.log >&2
  die "provisioning reported failures; see /tmp/provision.log"
fi
ok "provisioning passed ${provision_ok} checks"

section "4. Build guest kernel"
timeout 1800 ./build-guest-kernel.sh </dev/null >/tmp/kernel.log 2>&1 \
  || die "guest kernel build failed; see /tmp/kernel.log"
[[ -f "${ARTIFACT_DIR}/vmlinux" ]] || die "vmlinux was not produced"
ok "guest kernel built ($(du -h "${ARTIFACT_DIR}/vmlinux" | cut -f1))"

section "5. Build guest agent"
cd "${WORK}/guest-agent"
# Static linking is required: the rootfs has no dynamic loader, so a dynamically linked agent
# would fail to exec as PID 1 with an opaque kernel panic.
RUSTFLAGS='-C target-feature=+crt-static -C relocation-model=static' \
  timeout 900 cargo build --release --target x86_64-unknown-linux-gnu </dev/null \
  >/tmp/agent.log 2>&1 || die "agent build failed; see /tmp/agent.log"

AGENT_BIN="$(find "${WORK}/guest-agent/target/x86_64-unknown-linux-gnu/release" \
  -maxdepth 1 -type f -executable | head -1)"
[[ -n "$AGENT_BIN" ]] || die "no agent binary produced"
ok "agent built: $(basename "$AGENT_BIN") ($(du -h "$AGENT_BIN" | cut -f1))"

section "6. Build rootfs"
cd "${WORK}/scripts"
timeout 900 ./build-rootfs.sh --agent "$AGENT_BIN" </dev/null >/tmp/rootfs.log 2>&1 \
  || die "rootfs build failed; see /tmp/rootfs.log"
[[ -f "${ARTIFACT_DIR}/rootfs.ext4" ]] || die "rootfs.ext4 was not produced"
ok "rootfs built"

TASK_ROOTFS=""
if [[ -n "$SWEBENCH_IMAGE" ]]; then
  section "7. Convert the SWE-bench image"
  # The synthetic rootfs from step 6 is kept: it is what the compute, cache and sandbox workloads
  # boot, and the task rootfs is a separate, much larger image used only by RUN_TASK.
  # Output path stated rather than discovered, so the summary and any later sweep name the same
  # file with no guessing about which artifact the conversion produced.
  TASK_ROOTFS="${ARTIFACT_DIR}/rootfs-task.ext4"
  CONVERT_ARGS=(
    --image "$SWEBENCH_IMAGE" --agent "$AGENT_BIN"
    --out "$TASK_ROOTFS"
  )
  [[ -n "$SWEBENCH_TASK_CMD" ]] && CONVERT_ARGS+=(--task-cmd "$SWEBENCH_TASK_CMD")
  [[ -n "$SWEBENCH_TEST_PATCH" ]] && CONVERT_ARGS+=(--test-patch "$SWEBENCH_TEST_PATCH")
  [[ -n "$EPISODE_SCRIPT" ]] && CONVERT_ARGS+=(--episode "$EPISODE_SCRIPT")
  timeout 2700 ./docker-to-rootfs.sh "${CONVERT_ARGS[@]}" </dev/null >/tmp/convert.log 2>&1 \
    || { tail -20 /tmp/convert.log >&2; die "conversion failed; see /tmp/convert.log"; }
  [[ -f "$TASK_ROOTFS" ]] || die "conversion reported success but ${TASK_ROOTFS} is missing"
  ok "task rootfs: $(basename "$TASK_ROOTFS") ($(du -h "$TASK_ROOTFS" | cut -f1))"
fi

if [[ -n "$TBENCH_TASKS" ]]; then
  section "7. Fetch and convert Terminal-Bench tasks"
  # One rootfs per task, because a Terminal-Bench task is its own environment: its Dockerfile
  # installs whatever that task needs, and the sizes differ by more than an order of magnitude.
  # This is the opposite of the SWE-bench path, where one converted instance serves the sweep.
  #
  # The build happens here rather than on a workstation because a heavy task compiles from source
  # (build-tcc-qemu builds a Linux kernel), which is native work on this x86 host and emulated on
  # an arm64 laptop.
  TBENCH_ROOTFS_LIST=()
  IFS=',' read -ra TASK_NAMES <<<"$TBENCH_TASKS"
  for t in "${TASK_NAMES[@]}"; do
    t="${t// /}"
    [[ -n "$t" ]] || continue
    log "fetching ${t}"
    timeout 600 ./fetch-tbench-task.sh "$t" --out "${WORK}/tbench-tasks" </dev/null \
      >>/tmp/tbench-fetch.log 2>&1 \
      || { tail -10 /tmp/tbench-fetch.log >&2; die "fetching ${t} failed; see /tmp/tbench-fetch.log"; }

    out="${ARTIFACT_DIR}/rootfs-tbench-${t}.ext4"
    ARGS=(--task "${WORK}/tbench-tasks/${t}" --agent "$AGENT_BIN" --out "$out")
    (( TBENCH_SOLVE )) && ARGS+=(--solve)
    log "converting ${t}$( ((TBENCH_SOLVE)) && echo " (solution applied)" )"
    # Generous timeout: a task that builds a kernel from source dominates this step, and a
    # conversion killed partway leaves an unusable image rather than a clear error.
    timeout 5400 ./tbench-to-rootfs.sh "${ARGS[@]}" </dev/null \
      >>"/tmp/tbench-convert-${t}.log" 2>&1 \
      || { tail -20 "/tmp/tbench-convert-${t}.log" >&2; die "converting ${t} failed; see /tmp/tbench-convert-${t}.log"; }
    [[ -f "$out" ]] || die "conversion of ${t} reported success but ${out} is missing"
    ok "${t}: $(basename "$out") ($(du -h "$out" | cut -f1))"
    TBENCH_ROOTFS_LIST+=("$out")
  done
  ok "${#TBENCH_ROOTFS_LIST[@]} Terminal-Bench task rootfs image(s) built"
fi

cat <<SUMMARY

  Artifacts:  ${ARTIFACT_DIR}
  Binaries:   ${WORK}/labsweep, ${WORK}/labctl
${TASK_ROOTFS:+  Task image: ${TASK_ROOTFS}}
$( for r in "${TBENCH_ROOTFS_LIST[@]:-}"; do [[ -n "$r" ]] && printf '  Task image: %s\n' "$r"; done )

  A reboot is required for kernel args and KVM module parameters. Then validate:

    reboot
    ${WORK}/scripts/validate-host.sh

SUMMARY

ok "setup complete, reboot required"
