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

package kata

// Durable-dir volumes served from an erofs image.
//
// The tar arrangement gives the guest a plain host directory bind-mounted into
// the share (BindIntoShare). The image arrangement gives it the same tree,
// assembled the way each container's rootfs already is: a read-only lower plus
// a writable host upper, merged by the host kernel and served over the one
// kataShared virtio-fs share. Nothing the guest sees differs — only how the
// host conjures the directory into existence, which stops costing a file-by-file
// write-out at restore.

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/agent-substrate/substrate/cmd/ateom-microvm/internal/reaper"
	"github.com/agent-substrate/substrate/cmd/ateom-microvm/internal/tarutil"
	"github.com/agent-substrate/substrate/internal/ocispec"
)

// DurableLowerDir is where the durable-dir erofs image is mounted read-only,
// to serve as the merged overlay's lower.
//
// It sits under VMDir, and the merged mount under SharedDir, because those are
// exactly the two directories CleanupSandboxState sweeps: put both mounts
// there and crash cleanup covers them with no change to the sweep. It is a
// MOUNTPOINT ONLY — VMDir is on tmpfs, so nothing that scales with the actor's
// data may be written into it (the image file itself lives on real disk).
func DurableLowerDir(id string) string { return filepath.Join(VMDir(id), "durable-ro") }

// DurableMergedDir is the merged, writable durable tree virtiofsd serves — the
// same path under the share that BindIntoShare targets on the tar path, so the
// guest sees no difference.
func DurableMergedDir(id string) string {
	return filepath.Join(SharedDir(id), ocispec.ShareDurable)
}

// StageDurableOverlay mounts imagePath read-only at DurableLowerDir(id) and
// overlays upper/work on top of it at DurableMergedDir(id), the location the
// guest reaches the durable volumes through. upper and work must be siblings
// on real disk (the kernel requires them on one filesystem and rejects a
// nested workdir) and must be EMPTY: the image already carries everything the
// previous suspend captured, so an upper left over from that activation would
// re-apply its whiteouts and delete files that legitimately exist again.
//
// Callers stage this before StartVirtiofsd, like every other share subtree.
func StageDurableOverlay(ctx context.Context, imagePath, id, upper, work string) error {
	lower := DurableLowerDir(id)
	merged := DurableMergedDir(id)
	// Drop any stale mounts first, innermost last (lazy if busy), then ensure
	// clean mountpoints.
	unmount(merged)
	unmount(lower)
	for _, d := range []string{merged, upper, work} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return fmt.Errorf("creating %q: %w", d, err)
		}
	}
	if err := tarutil.MountImage(ctx, imagePath, lower); err != nil {
		return fmt.Errorf("while mounting the durable-dir image: %w", err)
	}
	// metacopy=off,index=off: pinned for the same reason StageMergedRootfs
	// pins them, and harder. Both record file-handle references to LOWER
	// inodes in the upper, and this lower is rebuilt from a freshly downloaded
	// image on every restore — so every inode is new and any preserved handle
	// is stale, which turns the file silently unreadable after resume.
	//
	// No volatile, unlike StageMergedRootfs: that upper is throwaway, tarred
	// into the snapshot and deleted. This one holds the actor's durable data
	// between the resume and the next suspend, which is the entire meaning of
	// the word, so the syncs overlayfs wants to do are syncs we want done.
	opts := "lowerdir=" + lower + ",upperdir=" + upper + ",workdir=" + work +
		",metacopy=off,index=off"
	cmd := exec.CommandContext(ctx, "mount", "-t", "overlay", "overlay", "-o", opts, merged)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := reaper.Run(cmd); err != nil {
		tarutil.UnmountImage(lower)
		return fmt.Errorf("mounting durable-dir overlay at %q: %w (%s)", merged, err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// UnmountDurableOverlay drops both mounts, merged first. Best-effort like the
// rest of teardown; CleanupSandboxState sweeps whatever is left.
func UnmountDurableOverlay(id string) {
	unmount(DurableMergedDir(id))
	tarutil.UnmountImage(DurableLowerDir(id))
}

// unmount detaches path, lazily if it is busy, and ignores a path that was
// never mounted.
func unmount(path string) {
	if err := reaper.Run(exec.Command("umount", path)); err != nil {
		_ = reaper.Run(exec.Command("umount", "-l", path))
	}
}
