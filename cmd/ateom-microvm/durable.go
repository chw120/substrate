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

package main

// Durable-dir volumes for the micro-VM runtime.
//
// A durable-dir volume is a directory whose contents outlive the actor's process
// state: it survives suspend/resume and, under the Data snapshot scope, is the
// ONLY thing captured (the workload cold-starts on restore). The host side is
// owned by atelet, which creates one directory per volume under
// ateompath.DurableDirVolumeMountsDir(actorUID) and wipes them when the actor's
// directories are reset.
//
// ateom exposes that host directory to the guest under the single kataShared
// virtio-fs share at SharedDir(actorUID)/durable, where each container's bind
// is attached.
//
// Snapshots carry the contents as an archive of the whole per-actor directory,
// so every volume rides along and the layout is reproduced verbatim on restore.
// virtiofsd serves the share write-through (no --writeback), so once the guest
// is paused every completed guest write is already visible on the host and the
// archive is complete.
//
// The archive is a tar by default and a read-only erofs image when the node
// sets tarutil.FormatEnvVar. The format is detected from the file, not from the
// setting, so an image and a tar are equally restorable on any node (see
// tarutil.Sniff).
//
// There are three ways a restore can land what it was handed. An image is
// mounted and given a writable overlay. A tar is unpacked into the host
// directory above — or, on a node that sets tarutil.LandingEnvVar, indexed and
// mounted like the image, which is the tarfs arrangement. Which one an
// activation is on is answered by durableLowerKind, from the files the landing
// left behind.

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/agent-substrate/substrate/cmd/ateom-microvm/internal/kata"
	"github.com/agent-substrate/substrate/cmd/ateom-microvm/internal/tarutil"
	"github.com/agent-substrate/substrate/internal/ateompath"
	"github.com/agent-substrate/substrate/internal/ocispec"
	"github.com/agent-substrate/substrate/internal/proto/ateompb"
)

// durableTarFile is the snapshot file holding the tar of the actor's durable-dir
// volumes. Its entries are <volumeName>/... relative to
// ateompath.DurableDirVolumeMountsDir, so extraction restores the same layout.
// The name is shared with atelet, which uses it to carve durable data out of a
// FULL snapshot's file set when uploading a paused checkpoint as DATA.
const durableTarFile = ateompath.DurableDirTarFile

// hasDurableVolumes reports whether any container mounts a durable-dir volume.
func hasDurableVolumes(containers []*ateompb.Container) bool {
	for _, c := range containers {
		if len(c.GetDurableDirVolumeMounts()) > 0 {
			return true
		}
	}
	return false
}

// durableVolumeNames returns the distinct durable-dir volumes the containers
// declare, in declaration order.
func durableVolumeNames(containers []*ateompb.Container) []string {
	var names []string
	seen := map[string]bool{}
	for _, c := range containers {
		for _, m := range c.GetDurableDirVolumeMounts() {
			if n := m.GetVolumeName(); n != "" && !seen[n] {
				seen[n] = true
				names = append(names, n)
			}
		}
	}
	return names
}

// durableImagePath is where landDurableVolumes parks the erofs image a
// snapshot arrived as, and its presence is what puts the actor on the overlay
// path for the rest of the activation.
//
// It is ateom's, not atelet's: atelet's actor-dir reset wipes
// DurableDirVolumeMountsDir, and the image has to outlive the restore dir it
// came from because the loop mount reads from it for as long as the actor
// runs. It is on real disk rather than under VMDir, which is tmpfs — a
// gibibyte image there is a gibibyte of node RAM.
//
// It also costs disk that the plain-directory arrangement does not: the image
// stays whole for the actor's lifetime while the upper accumulates alongside
// it, so an actor that rewrites all of its data ends up storing it twice.
// Bounded by the actor's own data and reclaimed at teardown by
// resetDurableOverlayState, but node capacity planning should assume 2x rather
// than 1x for durable-dir volumes.
func durableImagePath(actorUID string) string {
	return filepath.Join(ateompath.ActorPath(actorUID), "durable-dir.erofs")
}

// durableTarPath is where landDurableVolumes parks a tar it is going to serve
// through tarfs instead of unpacking.
//
// It is the mount's data device, so it has to outlive the restore dir it came
// from for exactly as long as the image does on the other path: a loop device
// reads from it for the whole activation. Same reasoning as durableImagePath
// for why it is here and not under VMDir.
func durableTarPath(actorUID string) string {
	return filepath.Join(ateompath.ActorPath(actorUID), durableTarFile)
}

// durableIndexPath is the metadata-only erofs index CreateTarIndex builds over
// durableTarPath. Kilobytes, purely local, never uploaded.
func durableIndexPath(actorUID string) string {
	return filepath.Join(ateompath.ActorPath(actorUID), "durable-dir.erofs.idx")
}

// durableUpperWorkDirs are the overlay upperdir and workdir that go on top of
// the lower: siblings on real disk, beside the rootfs uppers, for the reasons
// kata.UpperWorkDirs and rootfsupper.go record.
func durableUpperWorkDirs(actorUID string) (upper, work string) {
	base := ateompath.ActorPath(actorUID)
	return filepath.Join(base, "durable-upper"), filepath.Join(base, "durable-work")
}

// durableLower names what this activation's durable overlay reads through, if
// it has one at all.
type durableLower int

const (
	// durableLowerNone is the plain host directory: no overlay, no lower.
	durableLowerNone durableLower = iota
	// durableLowerImage is a read-only erofs image the snapshot arrived as.
	durableLowerImage
	// durableLowerTarfs is a tar the snapshot arrived as, mounted through an
	// erofs index built over it.
	durableLowerTarfs
)

// durableLowerKind reports which arrangement landDurableVolumes chose, by
// looking for the file that arrangement leaves behind. The two are mutually
// exclusive: a restore lands one or the other, never both, and a fallback
// clears whichever it had.
func durableLowerKind(actorUID string) durableLower {
	if fileExists(durableImagePath(actorUID)) {
		return durableLowerImage
	}
	// The index, not the tar: the tar alone is what a half-finished landing
	// leaves, and serving that as a lower is impossible. The index exists only
	// once the pair is ready.
	if fileExists(durableIndexPath(actorUID)) {
		return durableLowerTarfs
	}
	return durableLowerNone
}

// fileExists reports whether path is there, treating any stat error as absent:
// the callers all go on to do something best-effort, and a path they cannot
// stat is one they cannot act on either.
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// String names the arrangement for logs.
func (k durableLower) String() string {
	switch k {
	case durableLowerImage:
		return "erofs-image"
	case durableLowerTarfs:
		return "tarfs"
	default:
		return "none"
	}
}

// durableArchiveDir is the directory a checkpoint must archive to capture the
// actor's durable volumes.
//
// On the overlay path that is the MERGED tree, not the image's lower and not
// the upper alone: reading through overlayfs applies the whiteouts and opaque
// markers the guest's deletions left in the upper, so what mkfs.erofs sees is
// the materialized tree and the new image is self-contained. Each suspend
// therefore writes a full image and stays O(data size) — this path makes
// restore cheap, not suspend.
func durableArchiveDir(actorUID string) string {
	if durableLowerKind(actorUID) != durableLowerNone {
		return kata.DurableMergedDir(actorUID)
	}
	return ateompath.DurableDirVolumeMountsDir(actorUID)
}

// durableReset is how long each part of resetDurableOverlayState took.
//
// Teardown reports it because reclaiming these trees is the entire cost an
// overlay path carries over the plain-directory path: the lower stays whole for
// the actor's lifetime while the upper accumulates beside it, so a suspend has
// to free both where a plain directory frees nothing. Which of them is
// expensive is not something the byte counts predict — freeing one large file
// and freeing the same bytes as an overlay upper are different work for the
// filesystem — so they are timed apart.
type durableReset struct {
	Image time.Duration
	Tar   time.Duration
	Index time.Duration
	Upper time.Duration
	Work  time.Duration
}

// resetDurableOverlayState clears every lower and overlay dir so the actor
// starts from the plain-directory arrangement. Called before a cold boot and
// at the start of every restore, so a previous activation's lower can never
// decide the current one's arrangement, and so the upper is empty when
// kata.StageDurableOverlay demands it.
//
// It clears both arrangements' files unconditionally rather than dispatching on
// durableLowerKind. An activation only ever has one of them, but the point of
// this call is to be sure of that, and asking first would trust the very state
// it is here to discard.
//
// The timings are always returned; callers that only need the reset ignore them.
func resetDurableOverlayState(ctx context.Context, actorUID string) (durableReset, error) {
	upper, work := durableUpperWorkDirs(actorUID)
	// A tarfs mount holds the tar through a loop device that, unlike the one
	// mount(8) sets up for the index, carries no autoclear. Unlinking the file
	// under a live binding frees no disk and spends a device the whole node
	// shares, so the binding goes first. Skipped unless there is a tar to hold,
	// which is every activation that is not on the tarfs path.
	if tarPath := durableTarPath(actorUID); fileExists(tarPath) {
		tarutil.ReleaseLoopDevices(ctx, tarPath)
	}
	var d durableReset
	for _, e := range []struct {
		path string
		into *time.Duration
	}{
		{durableImagePath(actorUID), &d.Image},
		{durableTarPath(actorUID), &d.Tar},
		{durableIndexPath(actorUID), &d.Index},
		{upper, &d.Upper},
		{work, &d.Work},
	} {
		t := time.Now()
		err := os.RemoveAll(e.path)
		*e.into = time.Since(t)
		if err != nil {
			return d, fmt.Errorf("while clearing durable-dir overlay state %q: %w", e.path, err)
		}
	}
	return d, nil
}

// stageDurableVolumes exposes the actor's durable-dir volumes to the guest at
// SharedDir(actorUID)/durable, either as a bind of the host directory or, when
// the restore landed an image, as that image plus a writable overlay. The
// guest sees the same tree either way.
func (s *AteomService) stageDurableVolumes(ctx context.Context, actorUID string, containers []*ateompb.Container) error {
	src := ateompath.DurableDirVolumeMountsDir(actorUID)
	if kind := durableLowerKind(actorUID); kind != durableLowerNone {
		upper, work := durableUpperWorkDirs(actorUID)
		var err error
		switch kind {
		case durableLowerImage:
			err = kata.StageDurableOverlay(ctx, durableImagePath(actorUID), actorUID, upper, work)
		case durableLowerTarfs:
			err = kata.StageDurableTarfsOverlay(ctx, durableIndexPath(actorUID), durableTarPath(actorUID), actorUID, upper, work)
		}
		if err == nil {
			// A volume the image predates has no directory in the lower, and
			// atelet's mkdir went into ITS directory, which this arrangement
			// does not serve. Create the missing ones in the merged tree, where
			// they land in the upper: the guest's bind source has to exist
			// before the container starts, exactly as on the bind path.
			for _, name := range durableVolumeNames(containers) {
				dir := filepath.Join(kata.DurableMergedDir(actorUID), name)
				if err := os.MkdirAll(dir, 0o700); err != nil {
					return fmt.Errorf("while creating durable-dir volume %q: %w", name, err)
				}
			}
			return nil
		}
		// This node cannot mount what another node wrote: no CONFIG_EROFS_FS,
		// no CONFIG_EROFS_FS_XATTR, a kernel too old for a 512-byte block, or
		// too few free loop devices. The actor's own configuration has no say
		// in that, so refusing here would strand it for a property of whoever
		// is restoring it. Unpack instead and serve it the old way: landing
		// goes back to O(data size) and this restore gets none of the benefit,
		// but the actor comes back.
		//
		// On the tarfs path the way back is Extract over the very same tar,
		// which is to say the arrangement this node would have used had the
		// setting never been on. That symmetry is the point of landing as a
		// read-side choice: falling back costs the speedup and nothing else.
		slog.WarnContext(ctx, "Cannot mount the durable-dir lower on this node; extracting it instead",
			slog.String("id", actorUID), slog.String("lower", kind.String()), slog.Any("err", err))
		var xerr error
		switch kind {
		case durableLowerImage:
			xerr = tarutil.ExtractImage(ctx, durableImagePath(actorUID), src)
		case durableLowerTarfs:
			xerr = tarutil.Extract(durableTarPath(actorUID), src)
		}
		if xerr != nil {
			return fmt.Errorf("while staging the durable-dir overlay: %w (extract fallback: %v)", err, xerr)
		}
		// Drop the lower before falling through. Leaving it would keep
		// durableLowerKind non-none for the rest of the activation, and the
		// next suspend would archive kata.DurableMergedDir — a directory
		// nothing ever mounted — silently checkpointing an empty durable dir
		// over all of the actor's data.
		if _, rerr := resetDurableOverlayState(ctx, actorUID); rerr != nil {
			return rerr
		}
	}
	if _, err := os.Stat(src); err != nil {
		return fmt.Errorf("while checking durable-dir volumes dir %q: %w", src, err)
	}
	if err := kata.BindIntoShare(ctx, src, actorUID, ocispec.ShareDurable); err != nil {
		return fmt.Errorf("while binding durable-dir volumes into the shared tree: %w", err)
	}
	return nil
}

// archiveDurableVolumes archives the actor's durable-dir volumes (dir) into the
// checkpoint directory, in the format this node is configured to write. The
// caller must have paused the guest first: virtiofsd is write-through, so a
// completed guest write is on the host by then, but a running guest could still
// add more after the walk.
//
// Sockets the workload left behind are skipped rather than archived (both
// writers log them); they hold no data and the workload recreates them on start.
func archiveDurableVolumes(ctx context.Context, dir, checkpointDir string) error {
	// The filename does not change with the format. atelet uses it to carve the
	// durable data out of a FULL snapshot's file set (see
	// ateompath.DurableDirTarFile), so renaming it by format would break that;
	// the reader sniffs the contents instead.
	dst := filepath.Join(checkpointDir, durableTarFile)
	var err error
	if tarutil.WriteFormat() == tarutil.FormatErofs {
		err = tarutil.CreateImage(ctx, dst, dir)
	} else {
		err = tarutil.Create(ctx, dst, dir)
	}
	if err != nil {
		return fmt.Errorf("while archiving durable-dir volumes from %q: %w", dir, err)
	}
	return nil
}

// landDurableVolumes brings a snapshot's durable-dir volumes down onto this
// node, ready for stageDurableVolumes to expose them. It must run before the
// durable share's virtiofsd starts, so the guest never observes the volumes
// mid-restore.
//
// A tar is unpacked into the actor's host directory (dir, which atelet has
// already created, empty), which costs a write of every file — unless this node
// lands tars through tarfs, in which case the tar is only moved into place and
// an index is built over it. An image is likewise only moved into place. Either
// way the mount that turns the file into a directory happens in
// stageDurableVolumes, because CleanupSandboxState runs between here and there
// and would sweep any mount made now, while it has no reason to touch a plain
// file. That split is what makes both overlay arrangements work: produce files
// here, produce mounts there.
//
// The archive format is read from the file rather than from this node's
// setting, so a snapshot written by a differently-configured node still
// restores. The landing mode is this node's own choice, because unlike the
// format it leaves no trace anyone else has to read.
func landDurableVolumes(ctx context.Context, dir, snapshotDir, actorUID string) error {
	// Whichever way this restore goes, it must not inherit the last one's
	// lower or its overlay upper.
	if _, err := resetDurableOverlayState(ctx, actorUID); err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("while creating durable-dir volumes dir %q: %w", dir, err)
	}
	src := filepath.Join(snapshotDir, durableTarFile)
	format, err := tarutil.Sniff(src)
	if err != nil {
		return fmt.Errorf("while identifying the durable-dir archive: %w", err)
	}
	if format == tarutil.FormatErofs {
		if err := adoptDurableArchive(src, durableImagePath(actorUID)); err != nil {
			return fmt.Errorf("while landing the durable-dir image: %w", err)
		}
		return nil
	}
	if tarutil.Landing() == tarutil.LandingTarfs {
		err := landDurableTarfs(ctx, src, actorUID)
		if err == nil {
			return nil
		}
		// Nothing about the actor is wrong, only this node's ability to index
		// the tar, so unpacking it is the same result the node would have
		// produced with the setting off.
		slog.WarnContext(ctx, "Cannot build a tarfs index for the durable-dir tar; extracting it instead",
			slog.String("id", actorUID), slog.Any("err", err))
		if _, rerr := resetDurableOverlayState(ctx, actorUID); rerr != nil {
			return rerr
		}
	}
	if err := tarutil.Extract(src, dir); err != nil {
		return fmt.Errorf("while restoring durable-dir volumes into %q: %w", dir, err)
	}
	return nil
}

// landDurableTarfs parks the tar where the mount can reach it and builds the
// index beside it. Both files together are what durableLowerKind reads as
// durableLowerTarfs, and the index is written second so a failure halfway
// leaves the actor on the plain-directory path rather than on a path whose
// lower cannot be mounted.
func landDurableTarfs(ctx context.Context, src, actorUID string) error {
	tarPath := durableTarPath(actorUID)
	if err := adoptDurableArchive(src, tarPath); err != nil {
		return fmt.Errorf("while landing the durable-dir tar: %w", err)
	}
	if err := tarutil.CreateTarIndex(ctx, durableIndexPath(actorUID), tarPath); err != nil {
		return fmt.Errorf("while indexing the durable-dir tar: %w", err)
	}
	return nil
}

// adoptDurableArchive moves the archive out of the restore dir, which belongs
// to atelet and is reused by the next restore, into ateom's own per-actor
// location.
//
// A hardlink, because both are under ActorPath and so on one filesystem: the
// archive is the one thing on this path that must not be copied, since copying
// it is the O(data size) write these arrangements exist to avoid. Rename would
// do as well but leaves the restore dir short of a file atelet put there; a
// link leaves both views intact and costs the same.
func adoptDurableArchive(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return fmt.Errorf("creating %q: %w", filepath.Dir(dst), err)
	}
	if err := os.Link(src, dst); err != nil {
		// A filesystem that refuses hardlinks, or a layout that put the two
		// dirs on different ones: rename still avoids the copy in the case
		// that matters, and reports the original failure if it cannot.
		if rerr := os.Rename(src, dst); rerr != nil {
			return fmt.Errorf("linking %q to %q: %w (rename fallback: %v)", src, dst, err, rerr)
		}
	}
	return nil
}
