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
// first suspend ship a full image where every later one ships a delta.
func initDurableQcow2(ctx context.Context, actorUID string) error {
	if err := resetDurableQcow2State(actorUID); err != nil {
		return err
	}
	dir := durableQcow2Dir(actorUID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("while creating the durable-dir layer directory %q: %w", dir, err)
	}
	size := qcow2.SizeBytes()
	base := qcow2.NextLayerName(ateompath.DurableDirLayerPrefix, nil)
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
	name := qcow2.NextLayerName(ateompath.DurableDirLayerPrefix, layers)
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
	// Collapse a chain that has reached the cap before stacking on it, so that
	// what cloud-hypervisor is handed is never deeper than MaxChain.
	//
	// The suspend that sealed the chain normally collapsed it already
	// (collapseSealedQcow2), which is where the cost belongs. This is the
	// fallback for the snapshots that arrive deep anyway: one written before
	// that ran, or one whose collapse failed. It stays because the alternative
	// to flattening a chain at the cap is an actor that cannot boot.
	if len(layers) >= qcow2.MaxChain() {
		base, err := flattenDurableQcow2(ctx, actorUID, layers)
		if err != nil {
			// A failed flatten is a performance problem, not a correctness
			// one: the chain that arrived is still intact and still mounts.
			slog.WarnContext(ctx, "Failed to flatten the durable-dir chain; continuing on the deep chain",
				slog.String("id", actorUID), slog.Int("layers", len(layers)), slog.Any("err", err))
		} else {
			layers = []string{base}
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
	// Settle what this restore dirtied, so that the guest's pre-suspend flush
	// does not have to wait on it. In the background and deliberately: waiting
	// for it measured a straight trade -- 1 200 ms off suspend, 1 300 ms onto
	// resume -- which is the wrong direction, since resume is the latency an
	// actor's caller is sitting through.
	//
	// How much of it the guest's own cold boot hides depends on how much there
	// is. On a 128 MiB durable dir the drain took 1 216 ms against a 1 227 ms
	// boot and cost nothing; on a 512 MiB one it took 4 659 ms against a
	// 2 151 ms boot, so the back half of it ran while the guest was reading its
	// containers off the same device. Sizing this to what is actually dirty is
	// the open question here, and behind it the larger one of why a restore
	// dirties an order of magnitude more than it changed.
	//
	// Nothing waits for the result and nothing has to: a drain that fails, or
	// that a teardown outruns, leaves the pages dirty and the kernel writes
	// them back on its own schedule. The cost of losing the race is the flush
	// this was meant to spare, not correctness.
	go func() {
		tDrain := time.Now()
		if err := qcow2.Drain(dir); err != nil {
			slog.WarnContext(ctx, "Could not drain the durable-dir filesystem",
				slog.String("id", actorUID), slog.String("error", err.Error()))
			return
		}
		slog.InfoContext(ctx, "Drained the durable-dir filesystem",
			slog.String("id", actorUID), slog.Duration("took", time.Since(tDrain)))
	}()
	slog.InfoContext(ctx, "Landed the durable-dir chain", slog.String("id", actorUID),
		slog.Int("layers", len(layers)+1), slog.Int64("bytes", src.TotalBytes()))
	return nil
}

// flattenDurableQcow2 collapses the chain in an actor's live layer directory.
func flattenDurableQcow2(ctx context.Context, actorUID string, layers []string) (string, error) {
	return flattenChain(ctx, actorUID, durableQcow2Dir(actorUID), layers)
}

// flattenChain collapses layers — a chain, base first, all in dir — into a
// single self-contained image and returns the name of the one layer that
// survives.
//
// That name is the LAST of the layers given, not a fresh one and not the base's.
// Whatever is stacked on this chain records its backing file by name, so
// collapsing into the name at the top of what was collapsed leaves that header
// correct for free, whether it is written before or after. It is also why layer
// numbers no longer track chain depth (see qcow2.NextLayerName): the numbers
// climb, and a flatten removes the ones below the survivor rather than
// renumbering.
//
// Nothing is removed until the new image is in place under that name, and
// rename over it is atomic, so at no point is there a directory in which the
// layer is absent or half-written. Reversing the two would open a window in
// which the actor's data existed only under a dotfile name no other code path
// looks for.
//
// The layer files are shared by link between an actor's live directory and the
// checkpoints sealed from it, so both the rename and the removals must stay
// link-safe: they replace and unlink names, and never write through an inode a
// sealed checkpoint still refers to.
func flattenChain(ctx context.Context, actorUID, dir string, layers []string) (string, error) {
	if len(layers) == 0 {
		return "", errors.New("cannot flatten an empty durable-dir chain")
	}
	base := layers[len(layers)-1]
	tmp := filepath.Join(dir, ".flattened.qcow2")
	t := time.Now()
	if err := qcow2.Flatten(ctx, filepath.Join(dir, base), tmp); err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	if err := os.Rename(tmp, filepath.Join(dir, base)); err != nil {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("installing the flattened durable-dir base: %w", err)
	}
	for _, name := range layers[:len(layers)-1] {
		if err := os.Remove(filepath.Join(dir, name)); err != nil {
			return "", fmt.Errorf("removing flattened layer %q: %w", name, err)
		}
	}
	st, err := os.Stat(filepath.Join(dir, base))
	if err != nil {
		return "", err
	}
	slog.InfoContext(ctx, "Flattened the durable-dir chain", slog.String("id", actorUID),
		slog.Int("from_layers", len(layers)), slog.String("into", base),
		slog.Int64("bytes", st.Size()), slog.Duration("took", time.Since(t)))
	return base, nil
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

// collapseSealedQcow2 flattens a sealed checkpoint's chain in place, so that the
// restore reading it lands a shallow chain and never has to flatten one itself.
//
// The collapse has to happen somewhere: cloud-hypervisor refuses to open a chain
// nested deeper than qcow2.MaxNestingDepth, and the only way back under that
// ceiling is a qemu-img convert of the actor's data — 4.6 s at 512 MiB, the one
// operation in this arrangement that still scales with the data. Doing it here
// rather than on the restore path spends it where there is measured room for it:
// a suspend costs 0.42x of the directory arrangement's and a resume 1.5x, so the
// same work is roughly free on one side and the whole tail on the other. It also
// shrinks what atelet ships — a chain at the cap is 889 MB against a 512 MiB
// durable dir, and the flattened image is the durable dir.
//
// It runs after the guest is torn down: the checkpoint's files are hardlinks and
// so outlive the actor's directory, and holding the VM's memory and virtiofsd
// open across a convert buys nothing.
//
// A failure is not fatal. The chain that was sealed is intact and still restores
// — the restore path keeps its own flatten for exactly this case, and for
// snapshots written before this ran at all.
func collapseSealedQcow2(ctx context.Context, actorUID, checkpointDir string, m qcow2.Manifest) (qcow2.Manifest, error) {
	layers := m.LayerFiles()
	if len(layers) < qcow2.MaxChain() {
		return m, nil
	}
	survivor, err := flattenChain(ctx, actorUID, checkpointDir, layers)
	if err != nil {
		return m, err
	}
	// Re-derive from the headers rather than editing the manifest: the flatten
	// rewrote them, and the manifest is what the restore trusts to know which
	// files must be present.
	flat, err := qcow2.DescribeChain(ctx, filepath.Join(checkpointDir, survivor))
	if err != nil {
		return m, err
	}
	if err := qcow2.WriteManifest(filepath.Join(checkpointDir, ateompath.DurableDirChainFile), flat); err != nil {
		return m, err
	}
	return flat, nil
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
