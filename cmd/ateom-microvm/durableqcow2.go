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

// Durable-dir volumes served from a qcow2 disk.
//
// The directory arrangement (durable.go) hands the guest a host directory over
// virtio-fs and captures it as a tar, so both landing and archiving cost a
// write of every byte the actor owns. This one hands the guest a BLOCK DEVICE
// — a qcow2 image cloud-hypervisor attaches as /dev/vdb — and lets the guest
// mount its own ext4 on it. The host stops touching the contents at all:
//
//	suspend   seal the layer the guest was writing to      O(metadata)
//	restore   put the chain's files in one directory       O(chain bytes)
//	          and point CH at the top layer
//
// What that buys and what it costs are both large. The archive leg stops
// scaling with the actor's data; in exchange the host can no longer read the
// data (it is inside a filesystem only the guest mounts), the write-through
// coherence the tar relied on is gone (see flushGuestFilesystems), and the
// chain has to be flattened periodically or reads degrade.
//
// Which arrangement an activation is on is NOT the node's setting. It is
// whether a layer directory exists for the actor — durableQcow2Active — which
// a cold boot creates only when this node has opted in, and which a restore
// creates only when the snapshot it landed was written this way. A node with
// the setting on still restores a tar snapshot as a directory, and a node with
// it off still restores a chain snapshot as a disk. Anything else would strand
// actors mid-rollout.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/agent-substrate/substrate/cmd/ateom-microvm/internal/kata"
	"github.com/agent-substrate/substrate/cmd/ateom-microvm/internal/qcow2"
	"github.com/agent-substrate/substrate/internal/ateompath"
)

// durableQcow2Dir holds one actor's backing chain: every layer file plus the
// manifest naming them.
//
// It is ateom's, not atelet's — atelet's actor-dir reset wipes
// DurableDirVolumeMountsDir, and these files must outlive both the restore dir
// they were adopted from and the guest that reads through them. It is on real
// disk rather than under VMDir, which is tmpfs: a gibibyte of durable data
// there would be a gibibyte of node RAM.
func durableQcow2Dir(actorUID string) string {
	return filepath.Join(actorPath(actorUID), durableQcow2DirName)
}

// actorPath is ateompath.ActorPath, indirected only so tests can put an actor's
// layers somewhere writable. Nothing in production replaces it.
var actorPath = ateompath.ActorPath

// durableQcow2DirName names that directory under every actor's path.
const durableQcow2DirName = "durable-qcow2"

// isDurableQcow2Path reports whether a path names a layer in some actor's chain
// — including an actor on another node, which is how a restore recognizes the
// durable disk in a snapshot's config among the content-addressed static ones.
func isDurableQcow2Path(p string) bool {
	return filepath.Base(filepath.Dir(p)) == durableQcow2DirName
}

// durableQcow2ManifestPath is the chain manifest inside that directory. Its
// presence is the marker for the whole arrangement, so it is written LAST when
// a chain is assembled and removed FIRST when one is torn down: a half-built
// chain must never look active.
func durableQcow2ManifestPath(actorUID string) string {
	return filepath.Join(durableQcow2Dir(actorUID), ateompath.DurableDirChainFile)
}

// durableQcow2Active reports whether this activation serves its durable
// volumes from a disk image rather than the host directory.
func durableQcow2Active(actorUID string) bool {
	_, err := os.Stat(durableQcow2ManifestPath(actorUID))
	return err == nil
}

// durableQcow2Top returns the path of the layer cloud-hypervisor should open,
// and the manifest it came from.
func durableQcow2Top(actorUID string) (string, qcow2.Manifest, error) {
	m, err := qcow2.ReadManifest(durableQcow2ManifestPath(actorUID))
	if err != nil {
		return "", m, err
	}
	top, err := m.Top()
	if err != nil {
		return "", m, err
	}
	return filepath.Join(durableQcow2Dir(actorUID), top.File), m, nil
}

// resetDurableQcow2State drops the whole layer directory so the actor starts
// from nothing. Called before a cold boot and at the start of every restore,
// so a previous activation's chain can never be mistaken for this one's.
func resetDurableQcow2State(actorUID string) error {
	dir := durableQcow2Dir(actorUID)
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("while clearing the durable-dir layer directory %q: %w", dir, err)
	}
	return nil
}

// initDurableQcow2 builds a fresh chain for an actor that has no durable data
// yet: an empty ext4 base with a writable delta on top.
//
// The delta is not optional. Writing into the base would make the actor's
// first suspend ship a full image where every later one ships a delta, and —
// once flattening starts producing bases — would write into a compressed
// image, which qcow2 does not allow.
func initDurableQcow2(ctx context.Context, actorUID string) error {
	if err := resetDurableQcow2State(actorUID); err != nil {
		return err
	}
	dir := durableQcow2Dir(actorUID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("while creating the durable-dir layer directory %q: %w", dir, err)
	}
	size := qcow2.SizeBytes()
	base := qcow2.NextLayerName(ateompath.DurableDirLayerPrefix, 0)
	t := time.Now()
	if err := qcow2.CreateBase(ctx, filepath.Join(dir, base), size); err != nil {
		return err
	}
	slog.InfoContext(ctx, "Created a durable-dir base image", slog.String("id", actorUID),
		slog.Int64("virtual_size_bytes", size), slog.Duration("took", time.Since(t)))
	return appendDurableQcow2Layer(ctx, actorUID, []string{base}, size)
}

// appendDurableQcow2Layer stacks an empty writable layer on top of the layers
// named (base first) and publishes the result by writing the manifest.
func appendDurableQcow2Layer(ctx context.Context, actorUID string, layers []string, virtualSize int64) error {
	if len(layers) == 0 {
		return errors.New("cannot stack a durable-dir layer on an empty chain")
	}
	dir := durableQcow2Dir(actorUID)
	name := qcow2.NextLayerName(ateompath.DurableDirLayerPrefix, len(layers))
	if err := qcow2.CreateDelta(ctx, filepath.Join(dir, name), layers[len(layers)-1]); err != nil {
		return err
	}
	m, err := qcow2.DescribeChain(ctx, filepath.Join(dir, name))
	if err != nil {
		return err
	}
	if virtualSize > 0 && m.VirtualSizeBytes != virtualSize {
		return fmt.Errorf("durable-dir chain for %s reports a virtual size of %d bytes; the base was made at %d",
			actorUID, m.VirtualSizeBytes, virtualSize)
	}
	return qcow2.WriteManifest(durableQcow2ManifestPath(actorUID), m)
}

// landDurableQcow2 brings a snapshot's chain down onto this node, ready for
// cloud-hypervisor to open. It must run before the VM is created, and before
// anything else can observe the actor's durable data.
//
// The layers are adopted out of the restore dir — which belongs to atelet and
// is reused by the next restore — rather than copied, so landing costs
// directory operations rather than a write of the actor's data. That is the
// whole point of the arrangement, and it is why the fresh top layer this adds
// is load-bearing rather than tidy: without it the guest's writes would go
// through the hardlink into the file atelet still has, corrupting the snapshot
// that was just restored from.
func landDurableQcow2(ctx context.Context, actorUID, snapshotDir string) error {
	if err := resetDurableQcow2State(actorUID); err != nil {
		return err
	}
	src, err := qcow2.ReadManifest(filepath.Join(snapshotDir, ateompath.DurableDirChainFile))
	if err != nil {
		return fmt.Errorf("while reading the durable-dir chain manifest: %w", err)
	}
	if err := src.VerifyPresent(snapshotDir); err != nil {
		// A chain with a hole in it must fail here rather than mount: the
		// guest would see a filesystem whose missing clusters read as zeroes,
		// which is silent corruption dressed up as a successful restore.
		return fmt.Errorf("while restoring durable-dir volumes: %w", err)
	}
	dir := durableQcow2Dir(actorUID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("while creating the durable-dir layer directory %q: %w", dir, err)
	}
	// The layer filenames are preserved across the move. Every layer records
	// its backing file as a bare name, so relocating the whole set keeps the
	// chain valid with no header rewriting at all.
	for _, name := range src.LayerFiles() {
		if err := adoptFile(filepath.Join(snapshotDir, name), filepath.Join(dir, name)); err != nil {
			return fmt.Errorf("while landing durable-dir layer %q: %w", name, err)
		}
	}

	layers := src.LayerFiles()
	// Flatten before stacking, so the depth check is against what the actor
	// arrived with and the flattened base is what the new top writes into.
	if len(layers) >= qcow2.MaxChain() {
		flattened, err := flattenDurableQcow2(ctx, actorUID, layers)
		if err != nil {
			// A failed flatten is a performance problem, not a correctness
			// one: the chain that arrived is still intact and still mounts.
			slog.WarnContext(ctx, "Failed to flatten the durable-dir chain; continuing on the deep chain",
				slog.String("id", actorUID), slog.Int("layers", len(layers)), slog.Any("err", err))
		} else {
			layers = flattened
		}
	}
	if err := appendDurableQcow2Layer(ctx, actorUID, layers, src.VirtualSizeBytes); err != nil {
		return err
	}
	top, _, err := durableQcow2Top(actorUID)
	if err != nil {
		return err
	}
	// Validate the assembled chain once, here, where the failure is still a
	// clean restore error. Past this point it becomes a guest that boots and
	// then cannot read its own data.
	if err := qcow2.Check(ctx, top); err != nil {
		return fmt.Errorf("while validating the restored durable-dir chain: %w", err)
	}
	slog.InfoContext(ctx, "Landed the durable-dir chain", slog.String("id", actorUID),
		slog.Int("layers", len(layers)+1), slog.Int64("bytes", src.TotalBytes()))
	return nil
}

// flattenDurableQcow2 collapses the chain into a single compressed base and
// returns the layer list that replaces it.
//
// Nothing is removed until the new base is in place under its final name, and
// the manifest — the marker for the whole arrangement — is not written until
// after this returns. So an interrupted flatten leaves a directory that does
// not look active, which the retried restore clears and re-adopts from the
// checkpoint it is landing.
//
// It runs at restore rather than at suspend because suspend is the measured
// window and this is the one operation here that still scales with the actor's
// data. That makes one restore in every MaxChain() slower, which is a poor
// trade for a latency-sensitive resume and the reason the flatten policy is
// still an open question rather than a settled one — moving it to a background
// job needs an answer for what happens when the actor suspends mid-flatten.
func flattenDurableQcow2(ctx context.Context, actorUID string, layers []string) ([]string, error) {
	dir := durableQcow2Dir(actorUID)
	tmp := filepath.Join(dir, ".flattened.qcow2")
	t := time.Now()
	if err := qcow2.Flatten(ctx, filepath.Join(dir, layers[len(layers)-1]), tmp); err != nil {
		_ = os.Remove(tmp)
		return nil, err
	}
	// Install first, then drop what it replaced. The new base takes the old
	// one's name, and rename over it is atomic, so at no point is there a
	// directory in which the base is absent or half-written. Reversing these
	// two would open a window in which the actor's data existed only under a
	// dotfile name no other code path looks for.
	base := qcow2.NextLayerName(ateompath.DurableDirLayerPrefix, 0)
	if err := os.Rename(tmp, filepath.Join(dir, base)); err != nil {
		return nil, fmt.Errorf("installing the flattened durable-dir base: %w", err)
	}
	for _, name := range layers {
		if name == base {
			continue // Replaced in place by the rename above.
		}
		if err := os.Remove(filepath.Join(dir, name)); err != nil {
			return nil, fmt.Errorf("removing flattened layer %q: %w", name, err)
		}
	}
	st, err := os.Stat(filepath.Join(dir, base))
	if err != nil {
		return nil, err
	}
	slog.InfoContext(ctx, "Flattened the durable-dir chain", slog.String("id", actorUID),
		slog.Int("from_layers", len(layers)), slog.Int64("bytes", st.Size()),
		slog.Duration("took", time.Since(t)))
	return []string{base}, nil
}

// sealDurableQcow2 captures the actor's durable data into checkpointDir. The
// caller must have paused the guest and flushed its filesystems first (see
// flushGuestFilesystems): what is in the image is what the guest has actually
// written back, not what it has in its page cache.
//
// This is the operation the whole proposal turns on. It hardlinks the layer
// files and writes a manifest — no walk of the actor's data, no compression,
// no copy — so the durable half of a suspend costs the same for a gibibyte as
// for a kilobyte.
func sealDurableQcow2(ctx context.Context, actorUID, checkpointDir string) (qcow2.Manifest, error) {
	top, _, err := durableQcow2Top(actorUID)
	if err != nil {
		return qcow2.Manifest{}, fmt.Errorf("while sealing durable-dir volumes: %w", err)
	}
	// Re-derive the chain from the headers rather than reusing the manifest
	// on disk: the headers are what cloud-hypervisor actually read through,
	// and a flatten that failed halfway would have left the two disagreeing.
	m, err := qcow2.DescribeChain(ctx, top)
	if err != nil {
		return m, fmt.Errorf("while describing the durable-dir chain: %w", err)
	}
	dir := durableQcow2Dir(actorUID)
	for _, name := range m.LayerFiles() {
		if err := adoptFile(filepath.Join(dir, name), filepath.Join(checkpointDir, name)); err != nil {
			return m, fmt.Errorf("while sealing durable-dir layer %q: %w", name, err)
		}
	}
	if err := qcow2.WriteManifest(filepath.Join(checkpointDir, ateompath.DurableDirChainFile), m); err != nil {
		return m, err
	}
	return m, nil
}

// topLayerBytes is the size of the layer the actor was writing to — the part of
// a sealed chain that a delta-aware upload would be the only thing to ship.
// Zero for an empty manifest, which only a failed seal produces.
func topLayerBytes(m qcow2.Manifest) int64 {
	top, err := m.Top()
	if err != nil {
		return 0
	}
	return top.SizeBytes
}

// adoptFile links src to dst, falling back to a rename.
//
// A hardlink, because both ends are under ActorPath and so on one filesystem:
// the layers are the one thing on this path that must not be copied, since
// copying them is the O(data) write the format exists to avoid. Rename would
// do as well but leaves the source directory short of a file its owner put
// there; a link leaves both views intact and costs the same.
func adoptFile(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return fmt.Errorf("creating %q: %w", filepath.Dir(dst), err)
	}
	_ = os.Remove(dst)
	if err := os.Link(src, dst); err != nil {
		if rerr := os.Rename(src, dst); rerr != nil {
			return fmt.Errorf("linking %q to %q: %w (rename fallback: %v)", src, dst, err, rerr)
		}
	}
	return nil
}

// flushGuestFilesystems asks the guest to write its page cache back to the
// durable disk, and reports how long that took.
//
// The directory arrangement never needed this: virtiofsd serves the share
// write-through, so a completed guest write was already on the host and
// pausing was enough to make the tar coherent. A block device has no such
// property. The guest's dirty pages and its ext4 journal live in guest memory,
// and under a DATA-scope snapshot that memory is discarded — so without a
// flush, a suspend loses every write the guest had not yet committed, and can
// leave the ext4 needing a journal replay that will never happen.
//
// There is no kata-agent RPC for "sync". What there is, is the ability to run
// a process in a container, and sync(2) is not namespaced — it flushes every
// filesystem the guest kernel has mounted, whichever container asks. So this
// execs a helper and waits for it. The helper is ateom's own (see
// guestsync.go), not the image's: most actor images are distroless and have no
// sync in them at all.
//
// The helper lives in the actor's own rootfs, which the actor can write to, so
// it re-stages before each attempt and works down the containers rather than
// betting the suspend on the first one. That covers an actor that removed it —
// an image whose entrypoint tidies /, most likely. It does NOT cover one that
// removes it in a loop: the file is unlinkable between the restage and the
// exec, and closing that needs it to be a read-only bind mount instead, where
// the unlink returns EBUSY. That is the fix this wants before production; it
// needs a guest to test against, since nothing else in the micro-VM path binds
// a file rather than a directory.
//
// Best-effort by return value, not by intent: the caller decides whether a
// failed flush should fail the suspend. An image is not corrupted by a missed
// flush (ext4 recovers on the next mount from the journal, IF the journal
// made it out), but data is lost, so this must be loud when it fails.
//
// Its cost is the top unmeasured quantity in this proposal, which is why
// the duration comes back rather than being logged in here: the suspend log
// puts it beside the memory snapshot it has to compete with.
func flushGuestFilesystems(ctx context.Context, ac *kata.AgentClient, actorUID string, containerIDs []string) (time.Duration, error) {
	if ac == nil {
		return 0, errors.New("no kata-agent connection to flush the guest through")
	}
	if len(containerIDs) == 0 {
		return 0, errors.New("no running container to flush the guest through")
	}
	t := time.Now()
	var errs []error
	for _, cid := range containerIDs {
		if err := stageGuestSync(kata.MergedRootfsPath(actorUID, cid)); err != nil {
			errs = append(errs, err)
			continue
		}
		if err := ac.RunToCompletion(ctx, cid, "ate-durable-sync", []string{guestSyncPath}); err != nil {
			errs = append(errs, fmt.Errorf("in container %q: %w", cid, err))
			continue
		}
		return time.Since(t), nil
	}
	return time.Since(t), fmt.Errorf("while flushing guest filesystems: %w", errors.Join(errs...))
}
