#!/usr/bin/env python3
# Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
# SPDX-License-Identifier: MIT-0
"""Turn a recorded agent trajectory into a replayable episode script.

# Why this exists

Every measurement in this repo so far ran a task's *verifier* once: pytest, or SWE-bench's
`bin/test`. That is one component of an RL episode, not an episode. A real GRPO rollout is tens of
turns -- the policy proposes a command, the sandbox runs it, the policy sees the output -- and only
then does the verifier decide the reward. The two have different shapes: an episode is dominated by
short process spawns and repeated file reads over a warm page cache, while a verifier run is one
long compute-bound process.

Replaying a recorded trajectory reproduces the episode shape deterministically. There is no
inference, so nothing waits on a policy server and the measurement stays about the sandbox.

# What a SWE-agent trajectory contains

`{"environment": ..., "trajectory": [...], "history": [...], "info": {...}}`, where each entry in
`trajectory` has `action`, `observation`, `response`, `state` and `thought`. The action is what the
agent asked the environment to do.

Roughly half of those actions are *not* shell. SWE-agent gives the model a small DSL -- `open`,
`create`, `edit`, `find_file`, `submit` -- which its own harness implements. In a real rollout that
DSL runs in the harness, but its **effects** land in the sandbox: files are read, files are written,
processes are spawned. Those effects are what we are measuring, so each DSL verb is translated to
the shell equivalent that produces the same work. Skipping them would leave an episode that does
nothing and a verifier that cannot pass.

Every translation is recorded in the manifest, because a replay that quietly differs from the
recorded run is worse than no replay.

# Usage

    traj-to-episode.py --traj <file.traj> --workdir /testbed --out episode.sh
    traj-to-episode.py --traj <file.traj> --dry-run          # print the plan, write nothing
"""

import argparse
import json
import os
import re
import shlex
import sys

# The window `open` shows. SWE-agent's default is 100 lines, and the read matters: opening a file is
# a real page-cache read of that many lines, which is part of what an episode costs.
DEFAULT_WINDOW = 100

# Verbs SWE-agent implements in its harness rather than passing to the shell.
DSL_VERBS = {
    "open",
    "create",
    "edit",
    "find_file",
    "submit",
    "goto",
    "scroll_up",
    "scroll_down",
}


class Deviation(Exception):
    """A trajectory construct this translator will not silently guess at."""


def load_trajectory(path):
    with open(path) as f:
        data = json.load(f)
    steps = data.get("trajectory")
    if not isinstance(steps, list) or not steps:
        raise Deviation(f"{path} has no non-empty 'trajectory' array")
    return data, steps


def translate(action, workdir, traj_workdir, deviations, env_python=""):
    """Translate one recorded action into shell lines that do equivalent work.

    Returns (list_of_shell_lines, kind) where kind is "shell" or "dsl". Raises Deviation for a verb
    that is not handled, rather than dropping it: a silently skipped turn would make the episode
    look cheaper than it is, which is the exact error this whole exercise is correcting.
    """
    lines = action.rstrip("\n").split("\n")
    head = lines[0].strip()
    verb = head.split()[0] if head else ""

    # Paths in the trajectory are relative to wherever the recording ran, which is not where the
    # evaluation image puts the repo. Rewrite so an absolute path from the recording resolves.
    def fix(s):
        return s.replace(traj_workdir, workdir) if traj_workdir else s

    if verb not in DSL_VERBS:
        # Genuine shell. `pip install -e .[dev]` is the one case worth intercepting: the evaluation
        # image already has the package editable-installed into its conda env, so the install is
        # redundant, and letting it run would mean one network egress per microVM per episode at
        # density. --no-index keeps the same pip work -- resolve, check, build metadata -- without
        # reaching a package index.
        cmd = fix(head)
        # `python` and `pip` in the recording resolved to the agent environment's interpreter. In a
        # SWE-bench evaluation image the package lives in a conda env and the base interpreter cannot
        # import it -- verified on this image: `import marshmallow` fails under /opt/miniconda3/bin/
        # python and succeeds under the testbed env. A bare `python` would fail every turn that runs
        # the reproduction script, for a reason with nothing to do with the episode.
        if env_python:
            cmd = re.sub(r"^python3?\b", env_python, cmd)
            cmd = re.sub(r"^pip3?\b", f"{env_python} -m pip", cmd)
        if "pip" in cmd and " install " in cmd and "--no-index" not in cmd:
            deviations.append(
                f"'{cmd}' rewritten with --no-index: the image already has the package installed, "
                "and reaching an index would mean one network egress per microVM per episode"
            )
            cmd = cmd.replace(
                "pip install", "pip install --no-index --no-build-isolation", 1
            )
        return [cmd], "shell"

    if verb == "open":
        # `open <file> [line]` displays a window. sed reads exactly that range, so the page-cache
        # cost is the same as the agent's read.
        parts = shlex.split(head)
        target = fix(parts[1]) if len(parts) > 1 else ""
        if not target:
            raise Deviation(f"open with no file: {head!r}")
        if len(parts) > 2 and parts[2].isdigit():
            centre = int(parts[2])
            lo = max(1, centre - DEFAULT_WINDOW // 2)
            hi = centre + DEFAULT_WINDOW // 2
        else:
            lo, hi = 1, DEFAULT_WINDOW
        return [f"sed -n '{lo},{hi}p' {shlex.quote(target)} >/dev/null"], "dsl"

    if verb == "create":
        parts = shlex.split(head)
        if len(parts) < 2:
            raise Deviation(f"create with no file: {head!r}")
        return [f": > {shlex.quote(fix(parts[1]))}"], "dsl"

    if verb == "edit":
        # `edit <a>:<b>` followed by replacement lines and an `end_of_edit` sentinel. Spliced with
        # python rather than sed because the line range is inclusive and exact, and sed's line
        # arithmetic on a multi-line insert is easy to get subtly wrong.
        m = re.match(r"^edit\s+(\d+):(\d+)\s*$", head)
        if not m:
            raise Deviation(f"unrecognised edit form: {head!r}")
        lo, hi = int(m.group(1)), int(m.group(2))
        body = []
        for ln in lines[1:]:
            if ln.strip() == "end_of_edit":
                break
            body.append(ln)
        # The file being edited is whatever `open`/`create` last selected. SWE-agent tracks that in
        # `state.open_file`; the caller passes it in via EDIT_TARGET so this stays a pure function.
        #
        # Emitted as a single-line command so it can go through run_turn like every other turn. The
        # body travels base64-encoded rather than inline: it is arbitrary source text that may
        # contain quotes, backslashes or a heredoc sentinel, any of which would break a quoted shell
        # string, and this trajectory's own bodies contain both quotes and significant leading
        # whitespace that must survive byte-for-byte.
        import base64

        payload = base64.b64encode(json.dumps(body).encode()).decode()
        # SWE-agent's `edit` does not blindly write: it applies the edit, compiles the result, and
        # *rejects* it if the syntax broke, leaving the file untouched and telling the model to retry.
        # This trajectory exercises exactly that -- turn 10's body is unindented and was rejected,
        # turn 11 resubmits it correctly -- and the rejected attempt is real sandbox work: a write, a
        # compile, and a rollback. Writing unconditionally would make the replay report 14/14 turns
        # succeeding where the recording had 13, and would quietly drop the compile cost.
        script = (
            "import base64, json, sys; "
            f"body = json.loads(base64.b64decode('{payload}')); "
            "path = sys.argv[1]; "
            "orig = open(path).read(); "
            "lines = orig.split(chr(10)); "
            # Inclusive 1-based range, as the DSL defines it.
            f"lines[{lo} - 1:{hi}] = body; "
            "new = chr(10).join(lines); "
            "open(path, 'w').write(new); "
            "check = compile(new, path, 'exec') if path.endswith('.py') else None; "
            "sys.exit(0)"
        )
        # A failed compile must restore the original and report failure, mirroring the DSL. Done in
        # the shell wrapper so the python one-liner stays a single expression per statement.
        guard = (
            f'cp "$EDIT_TARGET" "$EDIT_TARGET.bak" 2>/dev/null; '
            f'if python3 -c {shlex.quote(script)} "$EDIT_TARGET"; then '
            f'rm -f "$EDIT_TARGET.bak"; else '
            f'mv -f "$EDIT_TARGET.bak" "$EDIT_TARGET"; exit 1; fi'
        )
        return [guard], "dsl"

    if verb == "find_file":
        parts = shlex.split(head)
        pattern = parts[1] if len(parts) > 1 else "*"
        where = fix(parts[2]) if len(parts) > 2 else "."
        return [
            f"find {shlex.quote(where)} -name {shlex.quote(pattern)} >/dev/null"
        ], "dsl"

    if verb == "submit":
        # What the harness collects at the end of an episode: the diff. A real repo walk, so it
        # belongs in the measured work.
        return ["git diff >/dev/null"], "dsl"

    if verb in ("goto", "scroll_up", "scroll_down"):
        # Another windowed read of the already-open file.
        return ["sed -n '1,%dp' \"$EDIT_TARGET\" >/dev/null" % DEFAULT_WINDOW], "dsl"

    raise Deviation(f"unhandled DSL verb {verb!r}")


def open_file_for_step(steps, index, workdir, traj_workdir):
    """Which file an `edit` at this index applies to.

    SWE-agent records the open file in each step's `state`, so this is read from the trajectory
    rather than inferred. Falling back to scanning previous `open`/`create` actions covers
    trajectories whose state is absent.
    """
    for i in range(index, -1, -1):
        state = steps[i].get("state")
        if state:
            try:
                parsed = json.loads(state) if isinstance(state, str) else state
                target = parsed.get("open_file")
                if target and target != "n/a":
                    return (
                        target.replace(traj_workdir, workdir)
                        if traj_workdir
                        else target
                    )
            except (json.JSONDecodeError, AttributeError):
                pass
    for i in range(index, -1, -1):
        head = steps[i].get("action", "").split("\n")[0].strip()
        if head.startswith(("open ", "create ")):
            parts = shlex.split(head)
            if len(parts) > 1:
                t = parts[1]
                return t.replace(traj_workdir, workdir) if traj_workdir else t
    return None


def build(steps, workdir, traj_workdir, verify_cmd, env_python=""):
    """Render the episode script. Returns (text, stats, deviations)."""
    deviations = []
    counts = {"shell": 0, "dsl": 0}
    out = [
        "#!/bin/bash",
        "# Generated by traj-to-episode.py from a recorded SWE-agent trajectory.",
        "#",
        "# Each turn below is one action the agent took. DSL actions (open, edit, create, find_file,",
        "# submit) are translated to shell that does equivalent filesystem and process work, because",
        "# in a real rollout the DSL runs in the agent harness while its effects land here.",
        "#",
        "# Turn failures are NOT fatal. Real rollouts contain mistakes -- this trajectory has an edit",
        "# rejected for bad indentation, retried successfully on the next turn -- and that wasted work",
        "# is part of what an episode costs. The reward comes from the verifier at the end.",
        "set -u",
        "",
        "# A writable HOME on tmpfs. pip, git and pytest all write under it -- pip failed with",
        "# \"[Errno 30] Read-only file system: '/root/.local'\" before this was set -- and the real",
        "# home lives on the read-only image.",
        "export HOME=/tmp/episode-home",
        "export XDG_CACHE_HOME=/tmp/episode-home/.cache",
        'mkdir -p "$HOME" "$XDG_CACHE_HOME" || exit 1',
        "",
        "TURNS_OK=0",
        "TURNS_RUN=0",
        "run_turn() {",
        "  TURNS_RUN=$((TURNS_RUN + 1))",
        '  if "$@"; then TURNS_OK=$((TURNS_OK + 1)); fi',
        "}",
        "",
        "# A writable working directory. The rootfs is mounted read-only so that one host page-cache",
        "# copy serves every microVM, but an episode writes: it creates a reproduction script, edits",
        "# source, and removes files. Symlinks rather than copies, because a repo checkout can be",
        "# gigabytes and copying per microVM would defeat the shared image entirely.",
        "EPISODE_WORK=/tmp/episode-work",
        'mkdir -p "$EPISODE_WORK" || exit 1',
        "",
        "# Replace each symlinked parent directory on a path with a real directory whose entries are",
        "# re-linked to the image. Without this, a write to $EPISODE_WORK/src/pkg/mod.py follows the",
        "# `src` symlink straight into the read-only image and fails -- silently, because the edit",
        "# still reports success while the file the verifier reads is unchanged.",
        "materialise_path() {",
        '  local rel="$1"',
        '  local base="$2"',
        '  local acc="$EPISODE_WORK"',
        '  local src="$base"',
        "  local part",
        '  for part in $(dirname "$rel" | tr "/" " "); do',
        '    [ "$part" = "." ] && continue',
        '    acc="$acc/$part"; src="$src/$part"',
        '    if [ -L "$acc" ]; then',
        '      rm -f "$acc"; mkdir -p "$acc"',
        '      for e in "$src"/* "$src"/.[!.]*; do',
        '        [ -e "$e" ] || continue',
        '        ln -sfn "$e" "$acc/" 2>/dev/null || true',
        "      done",
        '    elif [ ! -d "$acc" ]; then',
        '      mkdir -p "$acc"',
        "    fi",
        "  done",
        "}",
        f"for entry in {workdir}/* {workdir}/.[!.]*; do",
        '  [ -e "$entry" ] || continue',
        '  ln -sfn "$entry" "$EPISODE_WORK/" 2>/dev/null || true',
        "done",
        'cd "$EPISODE_WORK" || exit 1',
        "",
    ]

    edited = set()
    for i, step in enumerate(steps):
        action = step.get("action", "")
        if not action.strip():
            continue
        head = action.split("\n")[0].strip()
        try:
            lines, kind = translate(
                action, workdir, traj_workdir, deviations, env_python
            )
        except Deviation as e:
            raise Deviation(f"turn {i + 1}: {e}")
        counts[kind] += 1

        out.append(f"# --- turn {i + 1} [{kind}] {head}")
        # Every turn goes through run_turn so the count and the success tally are complete. An edit
        # that bypassed it would make the episode report fewer turns than it ran, and the host's
        # turn-count check -- the guard against an episode whose turns silently stopped executing --
        # would be comparing against the wrong number.
        if head.startswith("edit "):
            target = open_file_for_step(steps, i, workdir, traj_workdir)
            if not target:
                raise Deviation(f"turn {i + 1}: edit with no open file to apply it to")
            # An absolute path into the read-only image cannot be written through a symlink: the
            # write would follow the link to the shared image and fail with EROFS. Replace the
            # symlink with a real copy the first time a file is edited, so the write lands on tmpfs
            # while every other path still resolves to the shared image.
            if target not in edited:
                rel = (
                    os.path.relpath(target, workdir)
                    if target.startswith(workdir)
                    else target
                )
                out += [
                    f"# Materialise {rel} on tmpfs. The top-level working directory is a tree of",
                    "# symlinks into the read-only image, so every parent directory on this path is",
                    "# itself a link: mkdir -p would succeed against the *target* and the write would",
                    "# then follow the link into the image and fail. So each parent is replaced by a",
                    "# real directory whose contents are re-linked, and only the edited file is copied.",
                    f'export EDIT_TARGET="$EPISODE_WORK"/{shlex.quote(rel)}',
                    f"materialise_path {shlex.quote(rel)} {shlex.quote(workdir)}",
                    f'if [ ! -f "$EDIT_TARGET" ] || [ -L "$EDIT_TARGET" ]; then',
                    f'  rm -f "$EDIT_TARGET"',
                    f'  cp {shlex.quote(target)} "$EDIT_TARGET" 2>/dev/null || : > "$EDIT_TARGET"',
                    "fi",
                ]
                edited.add(target)
            else:
                rel = (
                    os.path.relpath(target, workdir)
                    if target.startswith(workdir)
                    else target
                )
                out.append(f'export EDIT_TARGET="$EPISODE_WORK"/{shlex.quote(rel)}')
            out.append(f"run_turn bash -c {shlex.quote(lines[0])}")
        elif len(lines) == 1:
            out.append(f"run_turn bash -c {shlex.quote(lines[0])}")
        else:
            out.append(f"run_turn bash -c {shlex.quote('; '.join(lines))}")
        out.append("")

    # The verifier. This is the reward, and it is timed separately by the guest agent so the
    # episode-to-verifier ratio -- the whole point of this work -- is readable.
    #
    # It runs from the tmpfs working directory, NOT the image's workdir. The episode's edits live on
    # tmpfs, so a verifier that cd'd back to the read-only original would test the unpatched tree:
    # the reward could never move off zero, and the failure would look like a bad patch rather than a
    # harness error. PYTHONPATH puts the patched sources first for the same reason -- the package is
    # editable-installed from the image path, which still holds the original file.
    out += [
        "# --- verifier: this exit code is the reward",
        'echo "EPISODE_TURNS=$TURNS_RUN"',
        'echo "EPISODE_TURNS_OK=$TURNS_OK"',
        "echo EPISODE_VERIFY_START",
        'cd "$EPISODE_WORK" || exit 1',
        "# Patched sources first: the package is editable-installed from the read-only image, whose",
        "# copy of the edited file is unchanged.",
        'export PYTHONPATH="$EPISODE_WORK/src:$EPISODE_WORK${PYTHONPATH:+:$PYTHONPATH}"',
        verify_cmd,
        "VERIFY_RC=$?",
        'echo "EPISODE_VERIFY_RC=$VERIFY_RC"',
        "exit $VERIFY_RC",
        "",
    ]
    return "\n".join(out), counts, deviations


def main():
    ap = argparse.ArgumentParser(description=__doc__.split("\n")[0])
    ap.add_argument("--traj", required=True, help="recorded .traj file")
    ap.add_argument(
        "--workdir",
        default="/testbed",
        help="where the repo lives in the evaluation image (SWE-bench uses /testbed)",
    )
    ap.add_argument(
        "--env-python",
        default="/opt/miniconda3/envs/testbed/bin/python",
        help="interpreter the task is installed into; bare `python` in a recording\nresolves to the agent environment, not the image conda env. Empty to leave as recorded",
    )
    ap.add_argument(
        "--verify-cmd",
        default=None,
        help="the verifier command; defaults to the instance's pytest invocation",
    )
    ap.add_argument("--out", help="episode script to write")
    ap.add_argument(
        "--manifest", help="manifest JSON to write (default: alongside --out)"
    )
    ap.add_argument(
        "--dry-run", action="store_true", help="print the plan, write nothing"
    )
    args = ap.parse_args()

    data, steps = load_trajectory(args.traj)

    # The directory the recording used, so absolute paths can be rewritten to this image's layout.
    traj_workdir = ""
    for step in steps:
        state = step.get("state")
        if not state:
            continue
        try:
            parsed = json.loads(state) if isinstance(state, str) else state
            wd = parsed.get("working_dir")
            if wd and wd != "n/a":
                traj_workdir = wd.rstrip("/")
                break
        except (json.JSONDecodeError, AttributeError):
            continue

    # The verifier must be able to FAIL on the unpatched tree, or a passing exit code proves nothing.
    #
    # There is no default that can be guessed safely. This trajectory's instance
    # (marshmallow-1867) is a SWE-agent *demonstration*, not a benchmark instance: it appears in no
    # SWE-bench split, so it has no published FAIL_TO_PASS test. An earlier version of this script
    # defaulted to `pytest tests/test_fields.py`, which passes 152 tests on the *unpatched* tree --
    # the reward was a constant and the episode's exit 0 meant nothing.
    #
    # So --verify-cmd is required, and the caller states what decides the reward. For a real
    # benchmark instance that is its fail-to-pass test; for a demonstration instance, the agent's
    # own reproduction script, whose output changes with the patch.
    if not args.verify_cmd:
        print(
            "error: --verify-cmd is required. There is no safe default: a verifier that passes on\n"
            "the unpatched tree makes the reward a constant, which is exactly the trap this work is\n"
            "correcting. For a benchmark instance pass its FAIL_TO_PASS test; for a demonstration\n"
            "instance pass a check whose result changes with the patch, for example\n"
            "  --verify-cmd '/opt/miniconda3/envs/testbed/bin/python -c \"...assert...\"'",
            file=sys.stderr,
        )
        return 1
    verify = args.verify_cmd

    try:
        text, counts, deviations = build(
            steps, args.workdir.rstrip("/"), traj_workdir, verify, args.env_python
        )
    except Deviation as e:
        print(f"error: {e}", file=sys.stderr)
        return 1

    instance = os.path.basename(args.traj).replace(".traj", "")
    manifest = {
        "instance_id": instance,
        "trajectory_source": args.traj,
        "trajectory_environment": data.get("environment"),
        "recorded_working_dir": traj_workdir,
        "image_working_dir": args.workdir,
        "turns": counts["shell"] + counts["dsl"],
        "turns_shell": counts["shell"],
        "turns_dsl_translated": counts["dsl"],
        "verify_command": verify,
        "network_at_episode_time": False,
        "deviations_from_recorded_run": [
            "DSL actions (open/create/edit/find_file/submit) are translated to shell that does "
            "equivalent filesystem and process work; in the recorded run they executed inside the "
            "SWE-agent harness rather than the sandbox",
            "Edited files are copied onto a tmpfs working directory, because the rootfs is mounted "
            "read-only so one host page-cache copy can serve the whole fleet",
            f"`open` reads a {DEFAULT_WINDOW}-line window via sed rather than the agent's pager",
        ]
        + deviations,
    }

    print(f"instance:        {instance}")
    print(
        f"turns:           {manifest['turns']} "
        f"({counts['shell']} shell, {counts['dsl']} DSL translated)"
    )
    print(f"recorded workdir {traj_workdir or '(none found)'} -> {args.workdir}")
    print(f"verifier:        {verify}")
    print(f"deviations:      {len(manifest['deviations_from_recorded_run'])}")

    if args.dry_run:
        print("\ndry run, nothing written")
        return 0
    if not args.out:
        print("error: --out is required unless --dry-run", file=sys.stderr)
        return 1

    with open(args.out, "w") as f:
        f.write(text)
    os.chmod(args.out, 0o755)
    mpath = args.manifest or (os.path.splitext(args.out)[0] + ".manifest.json")
    with open(mpath, "w") as f:
        json.dump(manifest, f, indent=2, sort_keys=True)
    print(f"\nwrote {args.out}\nwrote {mpath}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
