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

package tarutil

// Archive formats.
//
// A durable-dir snapshot can be written two ways: as a tar, which restore
// unpacks file by file, or as a read-only erofs image, which restore mounts.
// The tar's landing cost grows with the tree (a gibibyte of actor data takes
// well over half a second to write back out); the image's does not, because
// nothing is written back out at all — the guest reads through the mount and
// the kernel faults pages in on demand.
//
// The write side is chosen by FormatEnvVar and defaults to tar, so the
// behavior is unchanged until a node opts in. The READ side never consults the
// setting: a node restores archives written by other nodes, which during a
// rolling upgrade are running a differently-configured binary, so the format
// has to be a property of the file rather than of whoever is reading it. Sniff
// is that check, and it is why turning the setting on needs no migration.
//
// The setting is per-node but its consequences are fleet-wide: one node opting
// in means every node that might restore those actors has to be able to read an
// image. ExtractImage keeps that from being fatal — a host that cannot mount one
// unpacks it and gives up the speedup for that restore — but the arrangement
// only pays off where mounting works, so the unit to roll this out by is a node
// pool, whose nodes share a kernel and a node image, not an individual node.
//
// Mounting the image needs a loop device, and a loop device is a block device:
// the worker's device cgroup denies opening one whatever capabilities the pod
// holds, because the cgroup allow-list is not something a container can widen
// from inside. The worker gets one the same way it gets /dev/kvm — atelet
// advertises the node's loop devices to kubelet, and kubelet writes the allow
// rule for the one it reserves. So this costs a device grant, tied to the same
// opt-in, and not the pod's unprivileged status. Loop devices are a small fixed
// pool per node, which is the real ceiling on how many workers per node can
// serve a durable dir this way.
//
// Turning it off is not the mirror image, and a rollback plan that assumes it
// is will strand actors. Clearing the variable only changes what this node
// WRITES; every image already sitting in a snapshot store stays an image, and
// restoring one still needs a working mkfs-free read path — mount -t erofs, a
// loop device, and CONFIG_EROFS_FS_XATTR. If the reason for the rollback is
// that any of those is broken here, those actors cannot be restored on this
// node at all. Draining the format out means suspending every affected actor
// once more with the variable cleared, so each rewrites its snapshot as a tar;
// until that finishes, the read path has to keep working.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/agent-substrate/substrate/cmd/ateom-microvm/internal/reaper"
	"github.com/agent-substrate/substrate/internal/deviceplugin"
	"golang.org/x/sys/unix"
)

// Format names how an archive is written.
type Format string

const (
	// FormatTar is the default: an uncompressed tar, restored by Extract.
	FormatTar Format = "tar"
	// FormatErofs is a read-only erofs image, restored by MountImage.
	FormatErofs Format = "erofs"
)

// FormatEnvVar selects the format Create-side callers write. Anything
// unrecognized — including unset — means FormatTar, so no node changes
// behavior without an explicit opt-in.
const FormatEnvVar = "ATEOM_ARCHIVE_FORMAT"

// WriteFormat reports the format to write new archives in.
//
// Read from the environment on every call rather than cached at init: the
// callers each go on to move the whole of an actor's data, so one getenv is
// free, and tests (in this package and in ateom-microvm's) can drive the seam
// with t.Setenv instead of reaching into package state.
func WriteFormat() Format {
	if Format(strings.ToLower(strings.TrimSpace(os.Getenv(FormatEnvVar)))) == FormatErofs {
		return FormatErofs
	}
	return FormatTar
}

// Preflight reports whether this process can actually use the format it is
// configured for, and is meant to be called once at startup. It does nothing
// unless the node has opted in.
//
// It is a real round trip — build a one-file image, mount it, read the file and
// an xattr back, unmount — rather than a check that the tools exist, for two
// reasons.
//
// The cheap check fails late and badly: the binary starts, actors boot and
// serve, and a missing mkfs.erofs only surfaces at the first suspend, by which
// point the actor is pinned to a worker that cannot check it out.
//
// And the expensive half is the part that cannot be established any other way.
// hack/check-erofs-support.sh infers CONFIG_EROFS_FS_XATTR from
// /boot/config-$(uname -r), which a container usually cannot read, and losing
// xattrs is the worst failure this format has: overlay whiteouts stop being
// preserved, so files the guest deleted come back after a resume, silently.
// Reading one back off a mounted image settles it.
//
// It fails rather than quietly falling back to tar. A silent fallback leaves
// the operator believing the setting took effect on a node that is still
// writing tars, which for a change whose entire purpose is to be measured is
// worse than not starting. Refusing is legitimate here in a way it would not be
// for a node that never opted in: this node declared it would write images and
// cannot, so it is misconfigured against its own declaration. Combined with
// rolling the opt-in out a node pool at a time, a pool that comes up is a pool
// in which every node has PROVEN it can mount an image with xattrs intact,
// which is what makes the read side safe without any capability reporting.
func Preflight(ctx context.Context) error {
	if err := preflightErofsFormat(ctx); err != nil {
		return err
	}
	return preflightTarfsLanding(ctx)
}

// preflightErofsFormat is the FormatErofs half of Preflight.
func preflightErofsFormat(ctx context.Context) error {
	if WriteFormat() != FormatErofs {
		return nil
	}
	for _, bin := range []string{mkfsErofs, fsckErofs} {
		if _, err := exec.LookPath(bin); err != nil {
			return fmt.Errorf("%s=%s but %s is not on PATH (the ateom image needs erofs-utils; see hack/check-erofs-support.sh): %w",
				FormatEnvVar, FormatErofs, bin, err)
		}
	}

	dir, err := os.MkdirTemp("", "erofs-preflight-")
	if err != nil {
		return fmt.Errorf("erofs preflight: %w", err)
	}
	defer func() { _ = os.RemoveAll(dir) }() // a probe tree; leaking one is not worth failing startup over

	// trusted.overlay.opaque on a directory, which is exactly what the durable
	// overlay's whiteouts rely on, rather than a user.* attribute: the two take
	// different paths through the kernel and only this one matters here.
	src := filepath.Join(dir, "src")
	if err := os.MkdirAll(src, 0o755); err != nil {
		return fmt.Errorf("erofs preflight: %w", err)
	}
	const probeData = "erofs-preflight"
	if err := os.WriteFile(filepath.Join(src, "probe"), []byte(probeData), 0o600); err != nil {
		return fmt.Errorf("erofs preflight: %w", err)
	}
	if err := unix.Lsetxattr(src, overlayOpaqueXattr, []byte("y"), 0); err != nil {
		return fmt.Errorf("erofs preflight: setting %s on the probe tree: %w", overlayOpaqueXattr, err)
	}

	img := filepath.Join(dir, "probe.erofs")
	if err := CreateImage(ctx, img, src); err != nil {
		return fmt.Errorf("erofs preflight: %w (see hack/check-erofs-support.sh)", err)
	}
	mnt := filepath.Join(dir, "mnt")
	if err := MountImage(ctx, img, mnt); err != nil {
		return fmt.Errorf("erofs preflight: %w (CONFIG_EROFS_FS, or no loop device available; see hack/check-erofs-support.sh)", err)
	}
	defer UnmountImage(mnt)

	got, err := os.ReadFile(filepath.Join(mnt, "probe"))
	if err != nil {
		return fmt.Errorf("erofs preflight: reading the probe file back: %w", err)
	}
	if string(got) != probeData {
		return fmt.Errorf("erofs preflight: probe file read back as %q, want %q", got, probeData)
	}

	sz, err := unix.Lgetxattr(mnt, overlayOpaqueXattr, make([]byte, 16))
	if err != nil {
		return fmt.Errorf("erofs preflight: reading %s back off the mounted image: %w "+
			"(CONFIG_EROFS_FS_XATTR is most likely off, which loses overlay whiteouts and silently resurrects deleted files after a resume; see hack/check-erofs-support.sh)",
			overlayOpaqueXattr, err)
	}
	if sz == 0 {
		return fmt.Errorf("erofs preflight: %s came back empty off the mounted image (CONFIG_EROFS_FS_XATTR?)", overlayOpaqueXattr)
	}
	return nil
}

// overlayOpaqueXattr marks a directory in an overlay upper as replacing, rather
// than merging with, the one below it. Losing it un-deletes a directory the
// guest emptied, so the preflight round trip checks for this one specifically.
const overlayOpaqueXattr = "trusted.overlay.opaque"

const (
	// erofsSuperOffset is where an erofs superblock starts: the first 1024
	// bytes are left for a boot sector.
	erofsSuperOffset = 1024
	// tarMagicOffset is where a POSIX ustar / GNU tar header carries its
	// "ustar" magic, in the first 512-byte block.
	tarMagicOffset = 257
)

// erofsMagic is EROFS_SUPER_MAGIC_V1 (0xE0F5E1E2) as it appears on disk,
// little-endian, at erofsSuperOffset.
var erofsMagic = []byte{0xe2, 0xe1, 0xf5, 0xe0}

// tarMagic is the ustar magic shared by the POSIX and GNU header formats
// ("ustar\x00" and "ustar  \x00"); the common prefix is all we need.
var tarMagic = []byte("ustar")

// Sniff reports how archivePath was written, by inspecting the file rather
// than the configured format.
//
// The tar magic is checked FIRST and wins. Both magics sit at fixed offsets,
// and the erofs one lands 1024 bytes in — which in a tar is ordinary file
// content, so a tar whose first archived file happens to hold those four bytes
// there would otherwise be misread as an image. An erofs image cannot fake the
// tar magic in return: byte 257 is inside the boot sector mkfs.erofs zeroes.
//
// Content that matches neither is reported as FormatTar, which is what keeps
// every archive written before this existed readable.
func Sniff(archivePath string) (Format, error) {
	f, err := os.Open(archivePath)
	if err != nil {
		return "", fmt.Errorf("opening archive %q: %w", archivePath, err)
	}
	defer f.Close()
	return sniffFile(f, archivePath)
}

// sniffFile is Sniff on an already-open archive. It reads by offset and leaves
// the file position alone, so a caller that is about to read the file itself
// can sniff first.
func sniffFile(f *os.File, archivePath string) (Format, error) {
	buf := make([]byte, erofsSuperOffset+len(erofsMagic))
	n, err := f.ReadAt(buf, 0)
	// A short file is not an error here: it is simply too small to be an erofs
	// image, and an empty or truncated tar is Extract's problem to report.
	if err != nil && !errors.Is(err, io.EOF) {
		return "", fmt.Errorf("reading archive header of %q: %w", archivePath, err)
	}
	buf = buf[:n]
	if len(buf) >= tarMagicOffset+len(tarMagic) &&
		bytes.Equal(buf[tarMagicOffset:tarMagicOffset+len(tarMagic)], tarMagic) {
		return FormatTar, nil
	}
	if len(buf) >= erofsSuperOffset+len(erofsMagic) &&
		bytes.Equal(buf[erofsSuperOffset:], erofsMagic) {
		return FormatErofs, nil
	}
	return FormatTar, nil
}

// mkfsErofs is the image builder. It is an external binary and must be present
// in the ateom container image.
const mkfsErofs = "mkfs.erofs"

// CreateImage writes srcDir as a read-only erofs image at imagePath. Entry
// names are relative to srcDir, so mounting the image reproduces the tree.
//
// It is the FormatErofs counterpart of Create and preserves the same
// properties: modes, ownership, modification times, symlinks, hardlinks,
// FIFOs, device nodes, and extended attributes. Sockets are skipped, matching
// Create — see collectSockets.
func CreateImage(ctx context.Context, imagePath, srcDir string) error {
	excludes, err := collectSockets(ctx, srcDir)
	if err != nil {
		return err
	}
	// mkfs.erofs appends to, rather than truncates, a file that is already
	// there, so a re-run over a previous image would produce a corrupt one.
	if err := os.Remove(imagePath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("clearing stale erofs image %q: %w", imagePath, err)
	}

	args := make([]string, 0, len(excludes)+4)
	// force-inode-extended: without it mkfs.erofs picks the 32-byte compact
	// inode wherever the values fit, and a compact inode has NO mtime field
	// (readers report the superblock's build timestamp instead) and only 16
	// bits of uid/gid. Sandboxed workloads run under arbitrary uids and their
	// build systems care about mtimes, so both losses are real and both are
	// silent: the tree restores, it just restores wrong. The extended inode
	// costs 32 more bytes per file.
	args = append(args, "-E", "force-inode-extended")
	for _, rel := range excludes {
		args = append(args, "--exclude-path="+rel)
	}
	args = append(args, imagePath, srcDir)

	cmd := exec.CommandContext(ctx, mkfsErofs, args...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := reaper.Run(cmd); err != nil {
		return fmt.Errorf("building erofs image %q from %q: %w (%s)", imagePath, srcDir, err, strings.TrimSpace(stderr.String()))
	}

	// mkfs.erofs issues no fsync of its own (zero calls under strace), and the
	// image is handed to atelet for upload as soon as we return — the same
	// requirement CreateFiltered meets with f.Sync(). The tool will not meet
	// it for us, so do it here; on a gibibyte this is a few hundred
	// milliseconds and it is not optional.
	f, err := os.Open(imagePath)
	if err != nil {
		return fmt.Errorf("opening erofs image %q to sync: %w", imagePath, err)
	}
	defer f.Close()
	if err := f.Sync(); err != nil {
		return fmt.Errorf("syncing erofs image %q: %w", imagePath, err)
	}
	return nil
}

// collectSockets returns the srcDir-relative paths of every socket in the
// tree, for mkfs.erofs to exclude.
//
// writeTree skips sockets for a reason that applies unchanged here: a socket
// carries no data and is meaningless without the process that was listening on
// it, and agents leave them lying around in their data directories (ssh-agent,
// gpg-agent, language servers). mkfs.erofs refuses to archive one, so without
// this pass a single stray socket would fail every checkpoint and strand the
// actor on its worker with no way to suspend it.
//
// The extra walk is a stat pass over a tree mkfs.erofs is about to read in
// full, so it does not change what archiving costs.
func collectSockets(ctx context.Context, srcDir string) ([]string, error) {
	var sockets []string
	err := filepath.WalkDir(srcDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type()&os.ModeSocket == 0 {
			return nil
		}
		rel, err := filepath.Rel(srcDir, path)
		if err != nil {
			return err
		}
		slog.WarnContext(ctx, "Skipping socket while archiving directory",
			slog.String("path", path), slog.String("root", srcDir))
		sockets = append(sockets, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("scanning %q for sockets: %w", srcDir, err)
	}
	return sockets, nil
}

// loopDeviceGlob matches the loop devices this process has been granted. The
// worker's /dev holds only what the runtime and kubelet's device manager put
// there, so whatever this matches is exactly the set atelet's device plugin
// reserved for this container — no other worker holds them and nothing else on
// the node will take them while we do.
//
// A var only so tests can point it at a directory they control; nothing
// reassigns it in production.
var loopDeviceGlob = "/dev/loop[0-9]*"

// GrantedLoopDevices lists the loop devices this worker may use, in a stable
// order.
//
// A worker mounts one image at a time, so one device is enough; more than one
// only matters if a grant is stale (a previous mount that outlived its unmount)
// and the first choice is busy.
func GrantedLoopDevices() ([]string, error) {
	devs, err := filepath.Glob(loopDeviceGlob)
	if err != nil {
		// Glob only errors on a malformed pattern, which is a constant here.
		return nil, fmt.Errorf("listing loop devices: %w", err)
	}
	sort.Strings(devs)
	return devs, nil
}

// MountImage mounts the erofs image at imagePath read-only at mountpoint,
// creating the mountpoint if needed. The caller owns the unmount.
//
// The loop device comes from the ate.dev/loop grant rather than from the
// kernel's free-device search: a worker is not privileged, so /dev/loop-control
// is not open to it and it may only touch the nodes kubelet put in its /dev.
// mount(8) still sets the device up, via -o loop=<dev>, which is what keeps
// LO_FLAGS_AUTOCLEAR on it — the device is released when the mount goes away,
// so a crash leaves only a stray MOUNT for the sandbox sweep and never a bound
// loop device that would be ours to leak. Loop devices are a small fixed pool
// per node (max_loop, commonly 8), so leaking one is not a private cost.
func MountImage(ctx context.Context, imagePath, mountpoint string) error {
	if err := os.MkdirAll(mountpoint, 0o755); err != nil {
		return fmt.Errorf("creating erofs mountpoint %q: %w", mountpoint, err)
	}
	devs, err := GrantedLoopDevices()
	if err != nil {
		return err
	}
	if len(devs) == 0 {
		return fmt.Errorf("mounting erofs image %q at %q: no loop device in this worker's /dev (the pod needs a %s request)",
			imagePath, mountpoint, deviceplugin.ResourceLoop)
	}
	var errs []error
	for _, dev := range devs {
		cmd := exec.CommandContext(ctx, "mount", "-t", "erofs", "-o", "ro,loop="+dev, imagePath, mountpoint)
		var stderr strings.Builder
		cmd.Stderr = &stderr
		if err := reaper.Run(cmd); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w (%s)", dev, err, strings.TrimSpace(stderr.String())))
			continue
		}
		return nil
	}
	return fmt.Errorf("mounting erofs image %q at %q: %w", imagePath, mountpoint, errors.Join(errs...))
}

// fsckErofs unpacks an image without mounting it. It ships in the same
// erofs-utils package as mkfsErofs, so a node that can write an image can
// always read one back this way.
const fsckErofs = "fsck.erofs"

// ExtractImage unpacks an erofs image into destDir, which must already exist.
//
// This is the way back when the kernel cannot mount the image — no
// CONFIG_EROFS_FS, no CONFIG_EROFS_FS_XATTR, no free loop device. A node can be
// handed an image written by any other node regardless of its own configuration,
// so the read path has to survive a host that cannot mount one; unpacking costs
// the whole benefit of the format for that one restore but keeps the actor
// restorable, which the alternative does not.
//
// The result is the same directory layout Extract produces from a tar, which is
// what lets the caller fall back onto the existing bind path unchanged.
func ExtractImage(ctx context.Context, imagePath, destDir string) error {
	cmd := exec.CommandContext(ctx, fsckErofs, "--extract="+destDir, "--overwrite", imagePath)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := reaper.Run(cmd); err != nil {
		return fmt.Errorf("extracting erofs image %q into %q: %w (%s)", imagePath, destDir, err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// UnmountImage drops a MountImage mount, lazily if it is busy. Best-effort,
// like the rest of teardown: the sandbox-state sweep catches stragglers.
func UnmountImage(mountpoint string) {
	if err := reaper.Run(exec.Command("umount", mountpoint)); err != nil {
		_ = reaper.Run(exec.Command("umount", "-l", mountpoint))
	}
}

// Landing modes.
//
// FormatErofs above changes what a suspend WRITES, and everything awkward about
// it follows from that: the bytes in the snapshot store are a different file
// format, so every node that might restore the actor has to be able to read
// one, the read side has to Sniff which of the two it was handed, and clearing
// the setting does not undo anything already written — the images drain only
// once each affected actor has been suspended again.
//
// A landing mode changes what a restore DOES with a tar. The archive stays a
// tar, byte for byte, so none of that applies: the setting is a property of one
// node, its effect ends when that node's actors are gone, and turning it off
// takes effect on the next restore with nothing to migrate. It is the same
// trade in the other direction — LandingTarfs buys the mount-instead-of-unpack
// restore that FormatErofs buys, without changing the wire format — and it is
// therefore the safer of the two to roll out, at the cost of a second loop
// device per worker.
//
// The two are independent and may both be on. A node writing images and landing
// tars via tarfs simply exercises both paths, since Sniff still decides which
// arrangement an incoming archive gets.

// LandingMode decides what a restore does with a durable-dir tar.
type LandingMode string

const (
	// LandingExtract is the default: Extract writes the tree out file by file.
	LandingExtract LandingMode = "extract"
	// LandingTarfs builds a metadata-only erofs index over the tar and mounts
	// the pair, with the tar as the data device and a writable overlay on top.
	// Landing stops scaling with the actor's data.
	LandingTarfs LandingMode = "tarfs"
)

// LandingEnvVar selects the landing mode. Anything unrecognized — including
// unset — means LandingExtract, so no node changes behavior without an explicit
// opt-in.
const LandingEnvVar = "ATEOM_DURABLE_LANDING"

// Landing reports how this node should land a durable-dir tar.
//
// Read from the environment on every call, for the reason WriteFormat gives.
func Landing() LandingMode {
	if LandingMode(strings.ToLower(strings.TrimSpace(os.Getenv(LandingEnvVar)))) == LandingTarfs {
		return LandingTarfs
	}
	return LandingExtract
}

// tarfsBlockSize is the only block size a tarfs index can use, and it is a
// constraint of the layout rather than a tuning knob: the index addresses file
// data at its offset inside the tar, and tar members are aligned to 512 bytes.
// A 512-byte erofs block needs Linux 6.4 or newer, which is where the kernel
// stopped assuming the block size was the page size.
const tarfsBlockSize = "512"

// CreateTarIndex writes a metadata-only erofs index for tarPath at indexPath.
//
// The index holds no file data: each inode addresses its bytes where they
// already are, inside the tar, so mounting it requires the tar alongside as a
// data device. That is what makes this cheap — a few kilobytes and a few
// milliseconds for a gibibyte of tar, because nothing is copied.
//
// The tar must be one Create wrote, or equivalent: mkfs.erofs reads the member
// headers to build the inodes, so a compressed or otherwise wrapped stream will
// not do.
func CreateTarIndex(ctx context.Context, indexPath, tarPath string) error {
	// mkfs.erofs appends to, rather than truncates, a file that is already
	// there, so a re-run over a previous index would produce a corrupt one.
	if err := os.Remove(indexPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("clearing stale tarfs index %q: %w", indexPath, err)
	}
	// No force-inode-extended, unlike CreateImage: tar mode already writes
	// extended inodes, so mtimes and 32-bit ids survive without it (measured —
	// the flag changes neither the metadata read back nor the index size).
	cmd := exec.CommandContext(ctx, mkfsErofs, "--tar=i", "-b", tarfsBlockSize, indexPath, tarPath)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := reaper.Run(cmd); err != nil {
		return fmt.Errorf("building tarfs index %q over %q: %w (%s)", indexPath, tarPath, err, strings.TrimSpace(stderr.String()))
	}
	// No fsync, unlike CreateImage. That one syncs because atelet uploads the
	// image the moment it returns; this index is never uploaded and never
	// leaves the node — it is read back through a loop device on this same
	// host, which the page cache serves whether or not it reached the platter.
	//
	// Nor is there a socket-exclusion pass. The input is a tar, and Create
	// already dropped the sockets when it wrote it.
	return nil
}

// MountTarfs mounts the tarfs pair read-only at mountpoint: indexPath supplies
// the metadata, tarPath the data. The caller owns UnmountTarfs.
//
// Two loop devices, and they are set up differently on purpose. The index is
// the mounted device, so mount(8) can attach it via -o loop=<dev> and thereby
// keep LO_FLAGS_AUTOCLEAR on it — that binding is released with the mount even
// if this process dies. The tar is not the mounted device; mount(8) will not
// attach it for us, so we do it ourselves, and a loop device attached that way
// has no autoclear. UnmountTarfs is what releases it, and CleanupSandboxState
// is the backstop, because a leaked loop device is a cost to the whole node
// rather than to this actor: the pool is max_loop, commonly 8.
func MountTarfs(ctx context.Context, indexPath, tarPath, mountpoint string) error {
	if err := os.MkdirAll(mountpoint, 0o755); err != nil {
		return fmt.Errorf("creating tarfs mountpoint %q: %w", mountpoint, err)
	}
	devs, err := GrantedLoopDevices()
	if err != nil {
		return err
	}
	if len(devs) < 2 {
		return fmt.Errorf("mounting tarfs %q at %q: this worker's /dev holds %d loop device(s) and tarfs needs 2, one for the index and one for the tar (the pod needs a %s request of 2)",
			indexPath, mountpoint, len(devs), deviceplugin.ResourceLoop)
	}

	var errs []error
	for i, dataDev := range devs {
		if err := attachLoop(ctx, dataDev, tarPath); err != nil {
			errs = append(errs, fmt.Errorf("attaching %q to %s: %w", tarPath, dataDev, err))
			continue
		}
		for j, indexDev := range devs {
			if j == i {
				continue
			}
			cmd := exec.CommandContext(ctx, "mount", "-t", "erofs",
				"-o", "ro,loop="+indexDev+",device="+dataDev, indexPath, mountpoint)
			var stderr strings.Builder
			cmd.Stderr = &stderr
			if err := reaper.Run(cmd); err != nil {
				errs = append(errs, fmt.Errorf("index on %s over data on %s: %w (%s)", indexDev, dataDev, err, strings.TrimSpace(stderr.String())))
				continue
			}
			return nil
		}
		// No index device worked against this data device, so it is ours to
		// release before trying the next one.
		detachLoop(ctx, dataDev)
	}
	return fmt.Errorf("mounting tarfs %q at %q: %w", indexPath, mountpoint, errors.Join(errs...))
}

// UnmountTarfs drops a MountTarfs mount and releases the loop device holding
// the tar. Best-effort, like the rest of teardown.
//
// The tar's device is found by asking losetup what is bound to the file rather
// than by remembering what MountTarfs chose: teardown also runs after a crash,
// or in a process that never did the mount, and the binding is the only record
// that survives either.
func UnmountTarfs(ctx context.Context, mountpoint, tarPath string) {
	if err := reaper.Run(exec.Command("umount", mountpoint)); err != nil {
		_ = reaper.Run(exec.Command("umount", "-l", mountpoint))
	}
	ReleaseLoopDevices(ctx, tarPath)
}

// ReleaseLoopDevices unbinds every loop device currently backed by path.
//
// Teardown calls this before deleting the file, and so does the reset at the
// start of an activation: a binding left by a crashed predecessor would
// otherwise hold the inode alive after the unlink, so the disk would not come
// back and the device would stay spent for the whole node.
//
// Best-effort and quiet about a device that is still busy. losetup turns that
// case into a deferred detach — the binding drops when the last user closes it
// — which is the outcome we want anyway.
func ReleaseLoopDevices(ctx context.Context, path string) {
	// Detached from the caller's cancellation. This runs on teardown paths
	// whose context is often already done, and skipping it there would leak a
	// device the whole node shares rather than merely abandoning this actor's
	// work. The commands are two losetup invocations, so there is nothing to
	// bound. The context is kept for its values, which is what the logging
	// below reads.
	ctx = context.WithoutCancel(ctx)
	devs, err := loopDevicesBackedBy(ctx, path)
	if err != nil {
		slog.WarnContext(ctx, "Could not look up the loop devices backing a file",
			slog.String("path", path), slog.Any("err", err))
		return
	}
	for _, dev := range devs {
		detachLoop(ctx, dev)
	}
}

// attachLoop binds dev to path. The device must be one kubelet granted this
// worker: /dev/loop-control is not open to an unprivileged pod, so there is no
// asking the kernel for a free one.
func attachLoop(ctx context.Context, dev, path string) error {
	cmd := exec.CommandContext(ctx, "losetup", dev, path)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := reaper.Run(cmd); err != nil {
		return fmt.Errorf("%w (%s)", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// detachLoop releases dev. Best-effort: a device that was never attached, or
// was already released by an autoclear, is not a problem to report.
func detachLoop(ctx context.Context, dev string) {
	_ = reaper.Run(exec.CommandContext(ctx, "losetup", "-d", dev))
}

// loopDevicesBackedBy lists the loop devices currently bound to path.
//
// It parses `losetup -j`'s default output — "/dev/loopN: [id]:ino (path)", one
// per line — rather than asking for a single column with -O, which is a newer
// option than the rest of what this package relies on.
func loopDevicesBackedBy(ctx context.Context, path string) ([]string, error) {
	cmd := exec.CommandContext(ctx, "losetup", "-j", path)
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := reaper.Run(cmd); err != nil {
		return nil, fmt.Errorf("listing loop devices for %q: %w (%s)", path, err, strings.TrimSpace(stderr.String()))
	}
	var devs []string
	for _, line := range strings.Split(stdout.String(), "\n") {
		dev, _, ok := strings.Cut(strings.TrimSpace(line), ":")
		if ok && dev != "" {
			devs = append(devs, dev)
		}
	}
	return devs, nil
}

// preflightTarfsLanding is the LandingTarfs half of Preflight, and it is a
// round trip for the reasons Preflight gives — most of all the xattr, since a
// tarfs lower that dropped trusted.overlay.opaque would silently resurrect
// directories the guest emptied.
//
// It needs no separate version checks for the two things tarfs requires beyond
// plain erofs — mkfs.erofs new enough for --tar=i, and a kernel that will mount
// a 512-byte-block image. Each fails this round trip loudly at the step that
// needs it, which is more reliable than parsing a version out of a
// /boot/config file a container usually cannot read.
func preflightTarfsLanding(ctx context.Context) error {
	if Landing() != LandingTarfs {
		return nil
	}
	if _, err := exec.LookPath(mkfsErofs); err != nil {
		return fmt.Errorf("%s=%s but %s is not on PATH (the ateom image needs erofs-utils): %w",
			LandingEnvVar, LandingTarfs, mkfsErofs, err)
	}

	dir, err := os.MkdirTemp("", "tarfs-preflight-")
	if err != nil {
		return fmt.Errorf("tarfs preflight: %w", err)
	}
	defer func() { _ = os.RemoveAll(dir) }() // a probe tree; leaking one is not worth failing startup over

	// One level down, not at the root of the probe tree: Create archives a
	// directory's CONTENTS, so the root itself is never an entry and an xattr
	// set on it would never reach the tar. The real thing has the same shape —
	// the durable dir holds one subdirectory per volume — so this is what the
	// mode has to carry anyway.
	src := filepath.Join(dir, "src")
	vol := filepath.Join(src, "vol")
	if err := os.MkdirAll(vol, 0o755); err != nil {
		return fmt.Errorf("tarfs preflight: %w", err)
	}
	const probeData = "tarfs-preflight"
	if err := os.WriteFile(filepath.Join(vol, "probe"), []byte(probeData), 0o600); err != nil {
		return fmt.Errorf("tarfs preflight: %w", err)
	}
	if err := unix.Lsetxattr(vol, overlayOpaqueXattr, []byte("y"), 0); err != nil {
		return fmt.Errorf("tarfs preflight: setting %s on the probe tree: %w", overlayOpaqueXattr, err)
	}

	tarPath := filepath.Join(dir, "probe.tar")
	if err := Create(ctx, tarPath, src); err != nil {
		return fmt.Errorf("tarfs preflight: %w", err)
	}
	idxPath := filepath.Join(dir, "probe.erofs.idx")
	if err := CreateTarIndex(ctx, idxPath, tarPath); err != nil {
		return fmt.Errorf("tarfs preflight: %w (erofs-utils 1.6 or newer is needed for --tar=i)", err)
	}
	mnt := filepath.Join(dir, "mnt")
	if err := MountTarfs(ctx, idxPath, tarPath, mnt); err != nil {
		return fmt.Errorf("tarfs preflight: %w (CONFIG_EROFS_FS, a kernel older than 6.4, or fewer than two loop devices granted)", err)
	}
	defer UnmountTarfs(ctx, mnt, tarPath)

	got, err := os.ReadFile(filepath.Join(mnt, "vol", "probe"))
	if err != nil {
		return fmt.Errorf("tarfs preflight: reading the probe file back: %w", err)
	}
	if string(got) != probeData {
		return fmt.Errorf("tarfs preflight: probe file read back as %q, want %q", got, probeData)
	}
	sz, err := unix.Lgetxattr(filepath.Join(mnt, "vol"), overlayOpaqueXattr, make([]byte, 16))
	if err != nil {
		return fmt.Errorf("tarfs preflight: reading %s back off the mounted index: %w "+
			"(CONFIG_EROFS_FS_XATTR is most likely off, which loses overlay whiteouts and silently resurrects deleted files after a resume)",
			overlayOpaqueXattr, err)
	}
	if sz == 0 {
		return fmt.Errorf("tarfs preflight: %s came back empty off the mounted index (CONFIG_EROFS_FS_XATTR?)", overlayOpaqueXattr)
	}
	return nil
}
