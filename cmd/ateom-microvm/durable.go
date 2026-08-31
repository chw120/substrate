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
// sets tarutil.FormatEnvVar. The two differ only in how restore lands them —
// the tar is unpacked into the host directory above, the image is mounted and
// given a writable overlay — and the format is detected from the file, not
// from the setting, so an image and a tar are equally restorable on any node
// (see tarutil.Sniff). Which of the two is in play at any moment is answered
// by durableOverlayActive: the presence of the image at durableImagePath.

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

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

// durableUpperWorkDirs are the overlay upperdir and workdir that go on top of
// the image: siblings on real disk, beside the rootfs uppers, for the reasons
// kata.UpperWorkDirs and rootfsupper.go record.
func durableUpperWorkDirs(actorUID string) (upper, work string) {
	base := ateompath.ActorPath(actorUID)
	return filepath.Join(base, "durable-upper"), filepath.Join(base, "durable-work")
}

// durableOverlayActive reports whether this activation serves its durable
// volumes from an image plus overlay rather than from the plain host directory.
func durableOverlayActive(actorUID string) bool {
	_, err := os.Stat(durableImagePath(actorUID))
	return err == nil
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
	if durableOverlayActive(actorUID) {
		return kata.DurableMergedDir(actorUID)
	}
	return ateompath.DurableDirVolumeMountsDir(actorUID)
}

// resetDurableOverlayState clears the image and overlay dirs so the actor
// starts from the plain-directory arrangement. Called before a cold boot and
// at the start of every restore, so a previous activation's image can never
// decide the current one's format, and so the upper is empty when
// kata.StageDurableOverlay demands it.
func resetDurableOverlayState(actorUID string) error {
	upper, work := durableUpperWorkDirs(actorUID)
	for _, p := range []string{durableImagePath(actorUID), upper, work} {
		if err := os.RemoveAll(p); err != nil {
			return fmt.Errorf("while clearing durable-dir overlay state %q: %w", p, err)
		}
	}
	return nil
}

// stageDurableVolumes exposes the actor's durable-dir volumes to the guest at
// SharedDir(actorUID)/durable, either as a bind of the host directory or, when
// the restore landed an image, as that image plus a writable overlay. The
// guest sees the same tree either way.
func (s *AteomService) stageDurableVolumes(ctx context.Context, actorUID string, containers []*ateompb.Container) error {
	src := ateompath.DurableDirVolumeMountsDir(actorUID)
	if durableOverlayActive(actorUID) {
		upper, work := durableUpperWorkDirs(actorUID)
		err := kata.StageDurableOverlay(ctx, durableImagePath(actorUID), actorUID, upper, work)
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
		// This node cannot mount an image another node wrote: no
		// CONFIG_EROFS_FS, no CONFIG_EROFS_FS_XATTR, or no free loop device.
		// The actor's own configuration has no say in that, so refusing here
		// would strand it for a property of whoever is restoring it. Unpack
		// instead and serve it the old way: landing goes back to O(data size)
		// and this restore gets none of the format's benefit, but the actor
		// comes back.
		slog.WarnContext(ctx, "Cannot mount the durable-dir image on this node; extracting it instead",
			slog.String("id", actorUID), slog.Any("err", err))
		if xerr := tarutil.ExtractImage(ctx, durableImagePath(actorUID), src); xerr != nil {
			return fmt.Errorf("while staging the durable-dir overlay: %w (extract fallback: %v)", err, xerr)
		}
		// Drop the image before falling through. Leaving it would keep
		// durableOverlayActive true for the rest of the activation, and the
		// next suspend would archive kata.DurableMergedDir — a directory
		// nothing ever mounted — silently checkpointing an empty durable dir
		// over all of the actor's data.
		if rerr := resetDurableOverlayState(actorUID); rerr != nil {
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
// already created, empty), which costs a write of every file. An image is
// only moved into place — the mount that turns it into a directory happens in
// stageDurableVolumes, because CleanupSandboxState runs between here and there
// and would sweep any mount made now. That deferral is the point of the whole
// format: landing stops scaling with the actor's data.
//
// The format is read from the file rather than from this node's setting, so a
// snapshot written by a differently-configured node still restores.
func landDurableVolumes(dir, snapshotDir, actorUID string) error {
	// Whichever way this restore goes, it must not inherit the last one's
	// image or its overlay upper.
	if err := resetDurableOverlayState(actorUID); err != nil {
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
	if format == tarutil.FormatTar {
		if err := tarutil.Extract(src, dir); err != nil {
			return fmt.Errorf("while restoring durable-dir volumes into %q: %w", dir, err)
		}
		return nil
	}
	if err := adoptDurableImage(src, durableImagePath(actorUID)); err != nil {
		return fmt.Errorf("while landing the durable-dir image: %w", err)
	}
	return nil
}

// adoptDurableImage moves the image out of the restore dir, which belongs to
// atelet and is reused by the next restore, into ateom's own per-actor
// location.
//
// A hardlink, because both are under ActorPath and so on one filesystem: the
// image is the one thing on this path that must not be copied, since copying
// it is the O(data size) write the image exists to avoid. Rename would do as
// well but leaves the restore dir short of a file atelet put there; a link
// leaves both views intact and costs the same.
func adoptDurableImage(src, dst string) error {
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
