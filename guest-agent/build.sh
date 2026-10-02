#!/usr/bin/env bash
# Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
# SPDX-License-Identifier: MIT-0
# Build the static guest agent. Intended to run on a Linux host, where the default toolchain
# produces a static musl binary directly.
set -euo pipefail
TARGET="${TARGET:-x86_64-unknown-linux-musl}"
rustup target add "$TARGET" >/dev/null 2>&1 || true
cargo build --release --target "$TARGET"
BIN="target/$TARGET/release/fc-guest-agent"
echo "built $BIN"
file "$BIN" 2>/dev/null || true
size "$BIN" 2>/dev/null || true
