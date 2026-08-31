#!/usr/bin/env bash

# Copyright 2026 Google LLC
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#      http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# Report whether this host can serve durable-dir volumes from a qcow2 image,
# i.e. whether ATEOM_DURABLE_BACKEND=qcow2 is safe to set on it.
#
# The Go side covers the round trip it depends on — cmd/ateom-microvm/internal/qcow2
# builds a chain, walks it and checks it, and ateom itself refuses to start when
# that fails (qcow2.Preflight). This script answers the questions those cannot,
# because they are about the MACHINE and about behavior that only shows up at
# size:
#
#   1. Are qemu-img and mkfs.ext4 installed, and which versions? Both are
#      external binaries that have to be in the ateom container image.
#   2. Does this qemu-img support the flags the chain depends on — a backing
#      file with an explicit format (-F), an unsafe rebase (-u), a
#      backing-chain listing, and compressed convert?
#   3. Is the actor directory's filesystem SPARSE? The whole size story rests
#      on a mostly-empty 32 GiB image occupying megabytes. A filesystem that
#      does not do sparse files (or a copy that fills the holes) turns every
#      layer into a full-size file.
#   4. Does a hardlink work between an actor's layer directory and its
#      checkpoint directory? That link is what makes suspend O(metadata); if
#      the two land on different filesystems it silently becomes a copy.
#   5. How long does creating the base layer actually take here? It is the one
#      fixed cost a cold boot pays, and mkfs.ext4 on a slow disk is where it
#      goes.
#
# Run it INSIDE the ateom-microvm container rather than on the node: checks 1
# and 2 are about that image, and 3 to 5 are about the filesystem the pod sees.
# No root needed — nothing here mounts anything, which is rather the point
# compared to the erofs arrangement.
#
# Exits non-zero if qcow2 is unusable here.

set -euo pipefail

fail=0
note() { printf '  %s\n' "$*"; }
ok() { printf 'ok    %s\n' "$*"; }
bad() {
  printf 'FAIL  %s\n' "$*"
  fail=1
}
skip() { printf 'skip  %s\n' "$*"; }

# Where ateom keeps an actor's layers. Overridable so the script can be pointed
# at the real per-pod path, whose filesystem is what checks 3-5 are about.
WORKDIR="${WORKDIR:-${TMPDIR:-/tmp}}"

echo "== 1. tools =="
if ! command -v qemu-img >/dev/null; then
  bad "qemu-img is not on PATH; the ateom image needs qemu-utils"
else
  ok "qemu-img at $(command -v qemu-img)"
  note "$(qemu-img --version 2>&1 | head -1)"
fi
if ! command -v mkfs.ext4 >/dev/null; then
  bad "mkfs.ext4 is not on PATH; the ateom image needs e2fsprogs"
else
  ok "mkfs.ext4 at $(command -v mkfs.ext4)"
  note "$(mkfs.ext4 -V 2>&1 | head -1)"
fi

if [[ $fail -ne 0 ]]; then
  echo
  echo "qcow2 is NOT usable on this host; leave ATEOM_DURABLE_BACKEND unset"
  exit 1
fi

work="$(mktemp -d "${WORKDIR%/}/qcow2-check.XXXXXX")"
trap 'rm -rf "$work"' EXIT

echo
echo "== 2. qemu-img features =="
# A 1 GiB virtual image: large enough for the sparseness check below to mean
# something, and it costs nothing to create.
qemu-img create -q -f qcow2 "$work/base.qcow2" 1G
if qemu-img create -q -f qcow2 -F qcow2 -b base.qcow2 "$work/delta.qcow2" 2>/dev/null; then
  ok "backing file with an explicit format (-F)"
else
  bad "qemu-img create rejects -F; this version is too old to record the backing format, and CH will probe it instead"
fi
if [[ "$(cd "$work" && qemu-img info --output=json --backing-chain -f qcow2 delta.qcow2 | grep -c '"filename"')" == 2 ]]; then
  ok "--backing-chain walks the chain"
else
  bad "qemu-img info --backing-chain did not report both layers"
fi
if qemu-img rebase -q -u -f qcow2 -b base.qcow2 -F qcow2 "$work/delta.qcow2" 2>/dev/null; then
  ok "unsafe rebase (-u) is supported"
else
  bad "qemu-img rebase -u failed; relocating a chain would have to rewrite data"
fi
if qemu-img convert -f qcow2 -O qcow2 -c "$work/base.qcow2" "$work/flat.qcow2" 2>/dev/null; then
  ok "compressed convert (-c) is supported, so chains can be flattened"
else
  bad "qemu-img convert -c failed; a flattened base would be uncompressed"
fi

echo
echo "== 3. sparseness =="
# A qcow2 of a 1 GiB disk with nothing written to it should occupy well under a
# mebibyte. Compare the allocated blocks, not the apparent size.
alloc="$(stat -c %b "$work/base.qcow2")"
blksz="$(stat -c %B "$work/base.qcow2")"
bytes=$((alloc * blksz))
if [[ $bytes -lt $((4 * 1024 * 1024)) ]]; then
  ok "an empty 1 GiB image occupies $bytes bytes on disk"
else
  bad "an empty 1 GiB image occupies $bytes bytes: this filesystem is not storing it sparsely, so every layer will be full size"
fi
note "filesystem: $(stat -f -c %T "$work" 2>/dev/null || echo unknown)"

echo
echo "== 4. hardlink between layer and checkpoint dirs =="
mkdir -p "$work/layers" "$work/checkpoint"
if ln "$work/base.qcow2" "$work/checkpoint/base.qcow2" 2>/dev/null; then
  ok "hardlink succeeded; sealing a chain costs metadata only"
  rm -f "$work/checkpoint/base.qcow2"
else
  bad "cannot hardlink within the work dir; sealing would fall back to a rename or a copy"
fi

echo
echo "== 5. base layer creation cost =="
# What ateom actually does on a cold boot, at the default size: a sparse raw
# file, mkfs.ext4 over it, then convert to qcow2.
size_gib="${ATEOM_DURABLE_QCOW2_SIZE_GIB:-32}"
start="$(date +%s%N)"
truncate -s "${size_gib}G" "$work/raw.img"
mkfs.ext4 -F -q -L ate-durable -m 0 -E lazy_itable_init=0,lazy_journal_init=0 "$work/raw.img"
qemu-img convert -f raw -O qcow2 "$work/raw.img" "$work/fresh.qcow2"
elapsed=$((($(date +%s%N) - start) / 1000000))
freshb=$(($(stat -c %b "$work/fresh.qcow2") * $(stat -c %B "$work/fresh.qcow2")))
ok "built a ${size_gib} GiB base in ${elapsed} ms, occupying $freshb bytes"
note "this is paid once per cold boot of an actor with no durable data yet"

echo
if [[ $fail -ne 0 ]]; then
  echo "qcow2 is NOT usable on this host; leave ATEOM_DURABLE_BACKEND unset"
  exit 1
fi
echo "qcow2 is usable on this host"
