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
	"strings"

	"github.com/agent-substrate/substrate/cmd/ateom-microvm/internal/reaper"
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
	defer os.RemoveAll(dir)

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

// MountImage mounts the erofs image at imagePath read-only at mountpoint,
// creating the mountpoint if needed. The caller owns the unmount.
//
// The loop device is set up by mount(8) rather than by a separate losetup, so
// it carries LO_FLAGS_AUTOCLEAR and is released when the mount goes away. That
// leaves only a stray MOUNT to clean up after a crash, which the sandbox sweep
// already handles — an explicitly allocated loop device would be ours to leak.
func MountImage(ctx context.Context, imagePath, mountpoint string) error {
	if err := os.MkdirAll(mountpoint, 0o755); err != nil {
		return fmt.Errorf("creating erofs mountpoint %q: %w", mountpoint, err)
	}
	cmd := exec.CommandContext(ctx, "mount", "-t", "erofs", "-o", "ro,loop", imagePath, mountpoint)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := reaper.Run(cmd); err != nil {
		return fmt.Errorf("mounting erofs image %q at %q: %w (%s)", imagePath, mountpoint, err, strings.TrimSpace(stderr.String()))
	}
	return nil
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
