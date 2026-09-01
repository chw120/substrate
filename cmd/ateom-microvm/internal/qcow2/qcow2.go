//go:build linux

// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package qcow2 wraps qemu-img so ateom can keep an actor's durable-dir data
// in a qcow2 disk image instead of a host directory.
//
// The arrangement it enables: one image holds one ext4 filesystem holding every
// durable-dir volume as a top-level directory, cloud-hypervisor attaches the
// image as a second virtio-blk disk, and the guest mounts it itself. The host
// never expands the data into files, so landing a snapshot stops costing a
// write of every byte the actor owns — the guest reads clusters out of the
// image on demand instead.
//
// Increments come from qcow2's backing-file chain: a suspend seals the layer
// the guest was writing to and the next activation starts a fresh one on top,
// so each layer holds only the clusters that changed while it was the top.
//
//	durable-dir.layer-0000.qcow2   base: an empty ext4, or a flattened chain
//	durable-dir.layer-0001.qcow2   backing: layer-0000
//	durable-dir.layer-0002.qcow2   backing: layer-0001   <- CH opens this one
//
// Every layer of a chain lives in ONE directory and records its backing file
// as a bare filename, never a path. qemu resolves a relative backing filename
// against the directory of the image that names it, so the whole chain can be
// moved — from the actor's state directory into a checkpoint, from a snapshot
// into a different actor on a different node — without rewriting a single
// header. That is what keeps a restore from having to repoint N layers; only
// cloud-hypervisor's own record of the top layer's absolute path needs
// rewriting (see rewriteSnapshotSocketPaths).
//
// What this package does NOT do is lazy loading. A chain must be complete and
// local before the guest can read from it; qcow2 has no way to say "this
// cluster is not here, fetch it from object storage". Shrinking what has to be
// transferred is the whole of the benefit.
package qcow2

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/agent-substrate/substrate/cmd/ateom-microvm/internal/reaper"
)

// The external tools this package shells out to. qemu-img ships in qemu-utils
// and mkfs.ext4 in e2fsprogs; hack/images/ateom-base/Dockerfile installs both,
// and hack/check-qcow2-support.sh checks a node for them.
const (
	qemuImg  = "qemu-img"
	mkfsExt4 = "mkfs.ext4"
)

// BackendEnvVar selects how ateom serves durable-dir volumes. Anything
// unrecognized — including unset — leaves the node on the virtio-fs directory
// arrangement, so no node changes behavior without an explicit opt-in.
const BackendEnvVar = "ATEOM_DURABLE_BACKEND"

// BackendQcow2 is the value of BackendEnvVar that turns this package on.
const BackendQcow2 = "qcow2"

// SizeEnvVar overrides the virtual size of a newly created durable image, in
// gibibytes. The image is sparse, so the number is a ceiling on how much an
// actor may store rather than a reservation: an empty 32 GiB image costs a few
// hundred kibibytes of ext4 metadata on disk.
//
// It cannot be changed for an actor that already has an image — the size is
// baked into the base layer's ext4 superblock — so raising it only affects
// actors created afterwards. Growing an existing one needs qemu-img resize
// plus an online resize2fs in the guest, which this POC does not implement.
const SizeEnvVar = "ATEOM_DURABLE_QCOW2_SIZE_GIB"

// defaultSizeGiB is the virtual size of a durable image when SizeEnvVar is
// unset. Generous on purpose: sparseness makes the unused part free, and an
// actor that fills its disk has no recourse short of a rebuild.
const defaultSizeGiB = 32

// MaxChainEnvVar caps how many layers a backing chain may reach before the
// next restore flattens it. Every extra layer is another indirection a guest
// read of a cold cluster may have to walk, and another file that must be
// present before the image will open at all.
//
// It is also the setting that decides what a benchmark measures, so set it
// deliberately when running one. A chain of depth N holds up to N copies of
// every cluster the actor has rewritten since its last flatten, so against a
// tar baseline — which holds exactly one copy — a workload that rewrites the
// same bytes every cycle makes the chain look N times larger while telling you
// nothing about the arrangement. Depth 1 flattens at every restore and gives
// the comparable number; the default is for production, where deduplication
// across cycles is the point.
const MaxChainEnvVar = "ATEOM_DURABLE_QCOW2_MAX_CHAIN"

// defaultMaxChain is the chain depth that triggers a flatten. Deliberately
// small: the cost of being wrong in this direction is one slower restore,
// and in the other direction it is read amplification that grows without
// bound for the whole of a long-lived actor's life.
const defaultMaxChain = 8

// magic is the qcow2 file header's first four bytes ("QFI\xfb").
var magic = []byte{0x51, 0x46, 0x49, 0xfb}

// Enabled reports whether this node writes durable-dir data as a qcow2 image.
//
// It governs the WRITE side only. Whether a given activation is on the image
// arrangement is decided by what a restore landed, not by this setting — a
// node restoring an actor another node suspended has to read whatever it was
// given (see the ateom-side durableQcow2Active).
//
// Read from the environment on each call rather than cached, so tests can
// drive the seam with t.Setenv.
func Enabled() bool {
	return strings.ToLower(strings.TrimSpace(os.Getenv(BackendEnvVar))) == BackendQcow2
}

// SizeBytes is the virtual size to create new durable images at.
func SizeBytes() int64 {
	gib := defaultSizeGiB
	if v, err := strconv.Atoi(strings.TrimSpace(os.Getenv(SizeEnvVar))); err == nil && v > 0 {
		gib = v
	}
	return int64(gib) << 30
}

// MaxChain is the chain depth at which the next restore flattens.
func MaxChain() int {
	if v, err := strconv.Atoi(strings.TrimSpace(os.Getenv(MaxChainEnvVar))); err == nil && v > 0 {
		return v
	}
	return defaultMaxChain
}

// Preflight reports whether this process can do what the node has configured
// it to do, and is meant to run once at startup. It is a no-op unless the node
// has opted in.
//
// Like the erofs preflight it is a real round trip rather than a check that
// the binaries exist: build a tiny base, stack a delta on it, read the chain
// back and check it. The failure it is really guarding against is a qemu-img
// too old to take -F/--backing-chain or to write a v3 image, which a
// LookPath would not catch and which would otherwise first surface at an
// actor's first suspend — by which point the actor is pinned to a worker that
// cannot check it out.
//
// It fails rather than silently reverting to virtio-fs: a node that declared
// it would serve durable data from images and cannot is misconfigured against
// its own declaration, and for a change whose entire purpose is to be measured,
// a quiet fallback is worse than not starting.
func Preflight(ctx context.Context) error {
	if !Enabled() {
		return nil
	}
	for _, bin := range []string{qemuImg, mkfsExt4} {
		if _, err := exec.LookPath(bin); err != nil {
			return fmt.Errorf("%s=%s but %s is not on PATH (the ateom image needs qemu-utils and e2fsprogs; see hack/check-qcow2-support.sh): %w",
				BackendEnvVar, BackendQcow2, bin, err)
		}
	}

	dir, err := os.MkdirTemp("", "qcow2-preflight-")
	if err != nil {
		return fmt.Errorf("qcow2 preflight: %w", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()

	// 64 MiB clears mkfs.ext4's floor for a journalled filesystem, so this
	// exercises exactly the code path a real base takes, at a size that costs
	// nothing.
	base := filepath.Join(dir, "base.qcow2")
	if err := CreateBase(ctx, base, 64<<20); err != nil {
		return fmt.Errorf("qcow2 preflight: %w", err)
	}
	delta := filepath.Join(dir, "delta.qcow2")
	if err := CreateDelta(ctx, delta, filepath.Base(base)); err != nil {
		return fmt.Errorf("qcow2 preflight: %w", err)
	}
	chain, err := BackingChain(ctx, delta)
	if err != nil {
		return fmt.Errorf("qcow2 preflight: %w", err)
	}
	if len(chain) != 2 {
		return fmt.Errorf("qcow2 preflight: %s reported a %d-image backing chain for a delta over a base; want 2 (is qemu-img too old to follow relative backing files?)",
			qemuImg, len(chain))
	}
	if err := Check(ctx, delta); err != nil {
		return fmt.Errorf("qcow2 preflight: %w", err)
	}
	return nil
}

// CreateBase writes an empty durable image at path: a qcow2 of sizeBytes
// virtual size holding a freshly made ext4.
//
// mkfs cannot write into a qcow2, so the filesystem is made in a sparse raw
// file first and converted. Both halves are cheap because both are sparse —
// what is actually written is ext4's metadata, and the conversion turns the
// untouched remainder into unallocated clusters rather than zeroes.
//
// lazy_itable_init=0 and lazy_journal_init=0 pay for that metadata HERE, once,
// instead of leaving the guest kernel to zero the inode tables in the
// background after the first mount. Left lazy, that background work would
// land in whichever delta layer happened to be on top, adding tens of
// mebibytes of pure zeroing to the actor's first increment.
func CreateBase(ctx context.Context, path string, sizeBytes int64) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("creating %q: %w", dir, err)
	}
	raw, err := os.CreateTemp(dir, ".mkfs-*.raw")
	if err != nil {
		return fmt.Errorf("creating a scratch raw image: %w", err)
	}
	rawPath := raw.Name()
	defer func() { _ = os.Remove(rawPath) }()
	if err := raw.Truncate(sizeBytes); err != nil {
		_ = raw.Close()
		return fmt.Errorf("sizing the scratch raw image %q: %w", rawPath, err)
	}
	if err := raw.Close(); err != nil {
		return fmt.Errorf("closing the scratch raw image %q: %w", rawPath, err)
	}

	// -m 0: no reserved-for-root blocks. Nothing in this filesystem runs as
	// root-needs-headroom; the reserve would only be capacity the actor paid
	// for and cannot use.
	if out, err := run(ctx, mkfsExt4,
		"-F", "-q", "-L", FilesystemLabel, "-m", "0",
		"-E", "lazy_itable_init=0,lazy_journal_init=0", rawPath); err != nil {
		return fmt.Errorf("making the durable ext4: %w (%s)", err, out)
	}
	if out, err := run(ctx, qemuImg, "convert", "-f", "raw", "-O", "qcow2", rawPath, path); err != nil {
		return fmt.Errorf("converting the durable ext4 to qcow2 %q: %w (%s)", path, err, out)
	}
	return nil
}

// FilesystemLabel is the ext4 label on a durable image, so an operator who
// loop-mounts one can tell what they are looking at.
const FilesystemLabel = "ate-durable"

// CreateDelta writes an empty layer at path backed by backing, which must be a
// BARE FILENAME of an image in the same directory. Passing a path would defeat
// the relocatability the whole chain depends on; CreateDelta rejects one
// rather than writing a chain that breaks the first time it is moved.
func CreateDelta(ctx context.Context, path, backing string) error {
	if backing != filepath.Base(backing) {
		return fmt.Errorf("backing file %q must be a bare filename in the layer directory, not a path", backing)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(path), backing)); err != nil {
		return fmt.Errorf("backing file for %q: %w", path, err)
	}
	// -F qcow2 states the backing format explicitly. Without it qemu probes,
	// which it warns about and which recent versions refuse outright.
	if out, err := run(ctx, qemuImg, "create", "-q", "-f", "qcow2", "-F", "qcow2", "-b", backing, path); err != nil {
		return fmt.Errorf("creating delta layer %q over %q: %w (%s)", path, backing, err, out)
	}
	return nil
}

// ImageInfo is the subset of `qemu-img info` this package reads.
type ImageInfo struct {
	Filename    string `json:"filename"`
	VirtualSize int64  `json:"virtual-size"`
	ActualSize  int64  `json:"actual-size"`
	// BackingFilename is the backing file exactly as the header records it,
	// which for a chain this package built is always a bare filename.
	BackingFilename string `json:"backing-filename"`
}

// BackingChain returns the image at path and every image behind it, top first.
// It fails if any layer is missing, which is exactly the check a restore wants
// before handing the top of a chain to cloud-hypervisor: a chain with a hole
// in it must not mount, because what the guest would see is a filesystem whose
// missing clusters read as zeroes.
//
// -U because a sealing suspend reads the chain of a disk cloud-hypervisor still
// has open — pausing the guest does not drop CH's write lock on the image, and
// without it qemu-img refuses with "Failed to lock byte 201". Safe here: this
// only reads headers, and the guest is paused, so what it reads cannot change
// under it.
func BackingChain(ctx context.Context, path string) ([]ImageInfo, error) {
	out, err := run(ctx, qemuImg, "info", "-U", "--output=json", "--backing-chain", "-f", "qcow2", path)
	if err != nil {
		return nil, fmt.Errorf("reading the qcow2 backing chain of %q: %w (%s)", path, err, out)
	}
	var chain []ImageInfo
	if err := json.Unmarshal([]byte(out), &chain); err != nil {
		return nil, fmt.Errorf("parsing the qcow2 backing chain of %q: %w", path, err)
	}
	if len(chain) == 0 {
		return nil, fmt.Errorf("qcow2 backing chain of %q is empty", path)
	}
	return chain, nil
}

// Flatten collapses the chain ending at src into a single self-contained image
// at dst. O(allocated clusters) rather than O(virtual size) — unallocated
// clusters stay unallocated — but still the one operation here that scales
// with the actor's data, which is why what triggers it (MaxChain) is a policy
// question rather than a detail.
//
// -c compresses the result. Compressed clusters are read-only in qcow2, so
// this is only ever right for something that will be a BASE: a delta stacked
// on top takes the writes.
func Flatten(ctx context.Context, src, dst string) error {
	if out, err := run(ctx, qemuImg, "convert", "-f", "qcow2", "-O", "qcow2", "-c", src, dst); err != nil {
		return fmt.Errorf("flattening the qcow2 chain at %q into %q: %w (%s)", src, dst, err, out)
	}
	return nil
}

// Check validates an image's qcow2 metadata (L1/L2 tables, refcounts) and, for
// a chain, that every layer behind it is present and readable.
//
// The failure it exists for is the one the format adds over a tar: a suspend
// that catches the image mid-metadata-update yields a file that will not open
// at all, which is a worse outcome than losing the last few writes.
func Check(ctx context.Context, path string) error {
	// qemu-img check exits 3 for "leaked clusters", which is a repairable
	// bookkeeping wart rather than data loss, and 0 for clean. Anything else
	// means the image is not usable.
	out, err := run(ctx, qemuImg, "check", "-f", "qcow2", path)
	if err == nil {
		return nil
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) && ee.ExitCode() == 3 {
		return nil
	}
	return fmt.Errorf("qcow2 check failed for %q: %w (%s)", path, err, out)
}

// IsImage reports whether path starts with the qcow2 magic. Used to tell a
// durable snapshot's layers apart from a tar written by a node that had not
// opted in.
func IsImage(path string) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer func() { _ = f.Close() }()
	buf := make([]byte, len(magic))
	n, err := f.Read(buf)
	if err != nil && n < len(magic) {
		return false, nil //nolint:nilerr // too short to be an image is an answer, not a failure
	}
	return bytes.Equal(buf, magic), nil
}

// run executes one tool synchronously through the reaper (so the child reaper
// cannot eat its exit status) and returns its combined output.
func run(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	out, err := reaper.RunCombined(cmd)
	return strings.TrimSpace(string(out)), err
}
