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

# Report whether this host can serve durable-dir volumes from an erofs image,
# i.e. whether ATEOM_ARCHIVE_FORMAT=erofs is safe to set on it.
#
# The metadata fidelity of the round trip is covered by a real test —
# TestImageFidelity in cmd/ateom-microvm/internal/tarutil, run under
# hack/run-root-tests.sh — so this script only answers the questions that are
# about the MACHINE rather than about the code:
#
#   1. Is mkfs.erofs installed, and which version? It is an external binary
#      that has to be in the ateom container image.
#   2. Can the kernel mount erofs (CONFIG_EROFS_FS) with xattrs
#      (CONFIG_EROFS_FS_XATTR)? Without xattrs, overlay whiteouts and opaque
#      markers do not survive and deleted files come back after a resume.
#   3. Can a loop device be allocated from HERE? mount -o loop goes through
#      /dev/loop-control, and a container gets its own /dev. A node that passes
#      every other check can still fail this one inside the pod.
#   4. Does mkfs.erofs still issue no fsync of its own? tarutil.CreateImage
#      syncs the finished image because the tool does not, and that sync is
#      several hundred milliseconds per gibibyte. If a future version starts
#      syncing, the extra one is pure cost and should be dropped.
#
# Run it as root INSIDE the ateom-microvm container, not on the node's host
# namespace: ateom runs in a pod, and checks 1 and 3 answer a question about
# that container rather than about the machine. Running it on the host answers
# checks 2 and 4, which are the kernel's, but will report a loop-device and
# mkfs.erofs verdict the pod does not necessarily share.
#
# Exits non-zero if erofs is unusable here.

set -euo pipefail

fail=0
note() { printf '  %s\n' "$*"; }
ok() { printf 'ok    %s\n' "$*"; }
bad() {
  printf 'FAIL  %s\n' "$*"
  fail=1
}
skip() { printf 'skip  %s\n' "$*"; }

if [[ $EUID -ne 0 ]]; then
  echo "must run as root (mounting requires it)" >&2
  exit 2
fi

echo "== 1. mkfs.erofs =="
if ! command -v mkfs.erofs >/dev/null; then
  bad "mkfs.erofs is not on PATH; the ateom image needs erofs-utils"
else
  ok "mkfs.erofs at $(command -v mkfs.erofs)"
  note "$(mkfs.erofs --version 2>&1 | head -1)"
fi

# fsck.erofs --extract is the read path's escape hatch: a node that cannot
# mount an image unpacks it instead (tarutil.ExtractImage). It arrives in the
# same package, but --extract is newer than the package itself, so an older
# erofs-utils can have the binary and not the flag.
if ! command -v fsck.erofs >/dev/null; then
  bad "fsck.erofs is not on PATH; a node that cannot mount an image has no way back"
elif fsck.erofs --help 2>&1 | grep -q -- '--extract'; then
  ok "fsck.erofs supports --extract"
else
  bad "this fsck.erofs has no --extract; erofs-utils is too old for the extract fallback"
fi

echo
echo "== 2. kernel =="
if [[ -r /proc/filesystems ]] && grep -qw erofs /proc/filesystems; then
  ok "erofs is already registered"
elif modprobe erofs 2>/dev/null; then
  ok "erofs loaded via modprobe (CONFIG_EROFS_FS=m)"
else
  bad "cannot load the erofs module (CONFIG_EROFS_FS)"
fi

config="/boot/config-$(uname -r)"
if [[ -r $config ]]; then
  if grep -q '^CONFIG_EROFS_FS_XATTR=y' "$config"; then
    ok "CONFIG_EROFS_FS_XATTR=y"
  else
    bad "CONFIG_EROFS_FS_XATTR is not set: trusted.overlay.* markers will be lost, which silently resurrects deleted files after a resume"
  fi
else
  skip "no readable $config; TestImageFidelity's xattrs case is the real check"
fi

echo
echo "== 3. loop devices =="
# In a container this is the check most likely to fail on its own: everything
# else is the kernel's, and the kernel is shared, but /dev is not.
if [[ -e /dev/loop-control ]]; then
  ok "/dev/loop-control is present"
  if free="$(losetup --find 2>&1)"; then
    ok "losetup --find returned $free"
  else
    bad "cannot allocate a loop device here: $free"
  fi
else
  bad "/dev/loop-control is missing; mount -o loop cannot allocate a device (a container needs it exposed)"
fi

echo
echo "== 4. build + mount a probe image =="
if [[ $fail -ne 0 ]]; then
  skip "earlier checks failed"
  exit 1
fi

work="$(mktemp -d)"
mnt="$work/mnt"
src="$work/src"
mkdir -p "$mnt" "$src/vol"
echo payload >"$src/vol/file"

strace_log="$work/strace.log"
if command -v strace >/dev/null; then
  strace -f -e trace=fsync,fdatasync,sync,syncfs -o "$strace_log" \
    mkfs.erofs -E force-inode-extended "$work/probe.erofs" "$src" >/dev/null
else
  mkfs.erofs -E force-inode-extended "$work/probe.erofs" "$src" >/dev/null
fi
ok "built $(stat -c %s "$work/probe.erofs") byte image"

cleanup() {
  umount "$mnt" 2>/dev/null || umount -l "$mnt" 2>/dev/null || true
  rm -rf "$work"
}
trap cleanup EXIT

if mount -t erofs -o ro,loop "$work/probe.erofs" "$mnt"; then
  ok "mounted read-only"
else
  bad "mount -t erofs failed"
fi
if [[ "$(cat "$mnt/vol/file" 2>/dev/null)" == payload ]]; then
  ok "contents read back"
else
  bad "contents did not read back"
fi

# mount -o loop sets LO_FLAGS_AUTOCLEAR, so umount must release the device.
# If it does not, every suspend/resume cycle strands one and the node runs out.
umount "$mnt"
if losetup --noheadings --output BACK-FILE --list | grep -qxF "$work/probe.erofs"; then
  bad "the loop device survived umount; LO_FLAGS_AUTOCLEAR is not in effect and orphans need sweeping"
else
  ok "umount released the loop device"
fi

echo
echo "== 5. does mkfs.erofs sync? =="
if [[ -r $strace_log ]]; then
  n="$(grep -cE '\b(fsync|fdatasync|syncfs|sync)\(' "$strace_log" || true)"
  if [[ $n -eq 0 ]]; then
    ok "no sync calls: tarutil.CreateImage's own f.Sync() is required, as documented"
  else
    note "$n sync call(s) — this version DOES sync; CreateImage's extra f.Sync() may now be redundant cost"
  fi
else
  skip "strace not installed"
fi

echo
if [[ $fail -ne 0 ]]; then
  echo "erofs is NOT usable on this host; leave ATEOM_ARCHIVE_FORMAT unset"
  exit 1
fi
echo "erofs is usable on this host"
