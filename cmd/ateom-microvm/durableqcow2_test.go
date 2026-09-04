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

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/agent-substrate/substrate/cmd/ateom-microvm/internal/qcow2"
	"github.com/agent-substrate/substrate/internal/ateompath"
	"github.com/agent-substrate/substrate/internal/proto/ateompb"
)

// smallDurableDisk points the durable image size at something a test can build
// in a moment, and roots actor paths in a temp dir. Everything the chain code
// does is size-independent, so 64 MiB exercises the same paths a 32 GiB image
// would.
func smallDurableDisk(t *testing.T) {
	t.Helper()
	root := t.TempDir()
	old := actorPath
	actorPath = func(actorUID string) string { return filepath.Join(root, actorUID) }
	t.Cleanup(func() { actorPath = old })
	t.Setenv(qcow2.SizeEnvVar, "1")
}

// requireQemuImg skips a test that builds a real chain. The tools live in the
// ateom container image, not on a developer's laptop.
func requireQemuImg(t *testing.T) {
	t.Helper()
	for _, bin := range []string{"qemu-img", "mkfs.ext4"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s is not on PATH", bin)
		}
	}
}

// A snapshot's durable disk is recognized by the directory it sits in, so that
// a restore can pick it out of a config.json written by another actor on
// another node. Nothing else CH holds a path to may match.
func TestIsDurableQcow2Path(t *testing.T) {
	for path, want := range map[string]bool{
		"/var/lib/ateom-gvisor/actors/abc/durable-qcow2/durable-dir.layer-0000.qcow2":   true,
		"/var/lib/ateom-gvisor/actors/other/durable-qcow2/durable-dir.layer-0007.qcow2": true,
		"/opt/kata/share/kata-containers/rootfs.img":                                    false,
		"/opt/kata/share/kata-containers/vmlinux":                                       false,
		"/var/lib/ateom-gvisor/actors/abc/durable-qcow2":                                false,
		"": false,
	} {
		if got := isDurableQcow2Path(path); got != want {
			t.Errorf("isDurableQcow2Path(%q) = %v, want %v", path, got, want)
		}
	}
}

// topLayerBytes reports the newest layer's size — the part of a suspend a
// delta-aware upload would be the only thing to ship — and must not panic on
// the empty manifest a failed seal leaves behind.
func TestTopLayerBytes(t *testing.T) {
	m := qcow2.Manifest{Layers: []qcow2.Layer{{File: "a", SizeBytes: 900}, {File: "b", SizeBytes: 7}}}
	if got := topLayerBytes(m); got != 7 {
		t.Errorf("topLayerBytes() = %d, want 7", got)
	}
	if got := topLayerBytes(qcow2.Manifest{}); got != 0 {
		t.Errorf("topLayerBytes() on an empty manifest = %d, want 0", got)
	}
}

// adoptFile must LINK rather than copy: that a sealed layer costs no data write
// is the whole reason the arrangement exists. It also has to overwrite, since a
// retried restore lands into a directory a previous attempt may have touched.
func TestAdoptFileLinks(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.qcow2")
	dst := filepath.Join(dir, "sub", "dst.qcow2")
	if err := os.WriteFile(src, []byte("payload"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := adoptFile(src, dst); err != nil {
		t.Fatalf("adoptFile() = %v", err)
	}
	si, err := os.Stat(src)
	if err != nil {
		t.Fatal(err)
	}
	di, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(si, di) {
		t.Error("adoptFile() copied instead of linking; sealing a chain would cost a write of the actor's data")
	}
	// The source must survive: atelet still owns the restore dir it came from.
	if _, err := os.Stat(src); err != nil {
		t.Errorf("adoptFile() removed the source: %v", err)
	}
	if err := adoptFile(src, dst); err != nil {
		t.Errorf("adoptFile() over an existing destination = %v", err)
	}
}

// A guest that cannot be flushed cannot be suspended: skipping the flush would
// lose whatever the actor had not written back, and losing it silently is the
// one outcome worse than failing. Both ways of having nothing to flush through
// have to be errors.
func TestFlushGuestFilesystemsNeedsSomewhereToRun(t *testing.T) {
	if _, err := flushGuestFilesystems(context.Background(), nil, "actor", []string{"app"}); err == nil {
		t.Error("flushGuestFilesystems() with no agent = nil, want an error")
	}
	if _, err := flushGuestFilesystems(context.Background(), nil, "actor", nil); err == nil {
		t.Error("flushGuestFilesystems() with no containers = nil, want an error")
	}
}

// A cold boot builds a two-layer chain — an ext4 base plus an empty writable
// delta — and publishes it by writing the manifest. Writing into the base
// instead would make the actor's first suspend ship a full image.
func TestInitDurableQcow2(t *testing.T) {
	requireQemuImg(t)
	smallDurableDisk(t)
	ctx := context.Background()
	const uid = "actor-init"

	if durableQcow2Active(uid) {
		t.Fatal("durableQcow2Active() = true before anything was created")
	}
	if err := initDurableQcow2(ctx, uid); err != nil {
		t.Fatalf("initDurableQcow2() = %v", err)
	}
	if !durableQcow2Active(uid) {
		t.Fatal("durableQcow2Active() = false after initDurableQcow2()")
	}

	top, m, err := durableQcow2Top(uid)
	if err != nil {
		t.Fatalf("durableQcow2Top() = %v", err)
	}
	first := qcow2.NextLayerName(ateompath.DurableDirLayerPrefix, nil)
	want := []string{first, qcow2.NextLayerName(ateompath.DurableDirLayerPrefix, []string{first})}
	if !slices.Equal(m.LayerFiles(), want) {
		t.Errorf("chain layers = %v, want %v", m.LayerFiles(), want)
	}
	if filepath.Base(top) != want[1] {
		t.Errorf("top = %q, want the delta %q", filepath.Base(top), want[1])
	}
	if err := qcow2.Check(ctx, top); err != nil {
		t.Errorf("Check() on a fresh chain = %v", err)
	}

	// The whole image is a rounding error on disk, which is what makes handing
	// an actor a large virtual disk free.
	st, err := os.Stat(top)
	if err != nil {
		t.Fatal(err)
	}
	if st.Size() > 1<<20 {
		t.Errorf("the fresh top layer is %d bytes, want it near-empty", st.Size())
	}

	if err := resetDurableQcow2State(uid); err != nil {
		t.Fatalf("resetDurableQcow2State() = %v", err)
	}
	if durableQcow2Active(uid) {
		t.Error("durableQcow2Active() = true after resetDurableQcow2State()")
	}
}

// The suspend/restore cycle: seal a chain into a checkpoint, then land it under
// a DIFFERENT actor, as a restore onto another node does. The layers must
// arrive by name with their backing links intact, and landing must stack a
// fresh top so the guest's writes cannot reach back into the files the
// snapshot still holds.
func TestSealAndLandAcrossActors(t *testing.T) {
	requireQemuImg(t)
	smallDurableDisk(t)
	ctx := context.Background()
	const src, dst = "actor-src", "actor-dst"

	if err := initDurableQcow2(ctx, src); err != nil {
		t.Fatalf("initDurableQcow2() = %v", err)
	}
	checkpoint := t.TempDir()
	sealed, err := sealDurableQcow2(ctx, src, checkpoint)
	if err != nil {
		t.Fatalf("sealDurableQcow2() = %v", err)
	}
	if len(sealed.Layers) != 2 {
		t.Fatalf("sealed %d layers, want 2", len(sealed.Layers))
	}
	if err := sealed.VerifyPresent(checkpoint); err != nil {
		t.Fatalf("the sealed chain is not complete in the checkpoint: %v", err)
	}
	if _, err := os.Stat(filepath.Join(checkpoint, ateompath.DurableDirChainFile)); err != nil {
		t.Fatalf("sealDurableQcow2() wrote no manifest: %v", err)
	}
	// Sealing must not disturb the running actor's own chain.
	if !durableQcow2Active(src) {
		t.Error("sealDurableQcow2() left the source actor without a chain")
	}

	if err := landDurableQcow2(ctx, dst, checkpoint); err != nil {
		t.Fatalf("landDurableQcow2() = %v", err)
	}
	top, landed, err := durableQcow2Top(dst)
	if err != nil {
		t.Fatalf("durableQcow2Top() = %v", err)
	}
	if len(landed.Layers) != len(sealed.Layers)+1 {
		t.Errorf("landed %d layers, want the snapshot's %d plus a fresh top", len(landed.Layers), len(sealed.Layers))
	}
	if err := qcow2.Check(ctx, top); err != nil {
		t.Errorf("Check() on the landed chain = %v", err)
	}

	// The guest writes into the fresh top, so the layer the snapshot holds must
	// NOT be the one CH is handed.
	sealedTop, err := sealed.Top()
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(top) == sealedTop.File {
		t.Error("landDurableQcow2() handed the guest the snapshot's own top layer; its writes would corrupt the snapshot")
	}
}

// A chain with a layer missing must fail the restore rather than mount. What a
// hole produces is a filesystem whose absent clusters read as zeroes — silent
// corruption dressed up as a successful restore — and the error has to name the
// layer that is gone, because re-fetching it is the recovery.
func TestLandDurableQcow2RefusesAHole(t *testing.T) {
	requireQemuImg(t)
	smallDurableDisk(t)
	ctx := context.Background()

	if err := initDurableQcow2(ctx, "actor-src"); err != nil {
		t.Fatalf("initDurableQcow2() = %v", err)
	}
	checkpoint := t.TempDir()
	sealed, err := sealDurableQcow2(ctx, "actor-src", checkpoint)
	if err != nil {
		t.Fatalf("sealDurableQcow2() = %v", err)
	}
	base := sealed.Layers[0].File
	if err := os.Remove(filepath.Join(checkpoint, base)); err != nil {
		t.Fatal(err)
	}

	err = landDurableQcow2(ctx, "actor-dst", checkpoint)
	if err == nil {
		t.Fatal("landDurableQcow2() with a missing base = nil, want an error")
	}
	if !strings.Contains(err.Error(), base) {
		t.Errorf("landDurableQcow2() = %v, want the error to name the missing %q", err, base)
	}
	if durableQcow2Active("actor-dst") {
		t.Error("landDurableQcow2() left a chain active after failing; the guest must not be able to mount it")
	}
}

// deepChain writes a chain of n layers plus its manifest into dir: the shape a
// snapshot arrives in from an actor that has suspended n-1 times without ever
// being flattened.
func deepChain(t *testing.T, ctx context.Context, dir string, n int) {
	t.Helper()
	names := []string{qcow2.NextLayerName(ateompath.DurableDirLayerPrefix, nil)}
	if err := qcow2.CreateBase(ctx, filepath.Join(dir, names[0]), qcow2.SizeBytes()); err != nil {
		t.Fatalf("CreateBase() = %v", err)
	}
	for len(names) < n {
		name := qcow2.NextLayerName(ateompath.DurableDirLayerPrefix, names)
		if err := qcow2.CreateDelta(ctx, filepath.Join(dir, name), names[len(names)-1]); err != nil {
			t.Fatalf("CreateDelta() = %v", err)
		}
		names = append(names, name)
	}
	m, err := qcow2.DescribeChain(ctx, filepath.Join(dir, names[len(names)-1]))
	if err != nil {
		t.Fatalf("DescribeChain() = %v", err)
	}
	if err := qcow2.WriteManifest(filepath.Join(dir, ateompath.DurableDirChainFile), m); err != nil {
		t.Fatalf("WriteManifest() = %v", err)
	}
}

// The collapse moved to the suspend side is only worth anything if the restore
// that follows lands a shallow chain without flattening. A checkpoint sealed at
// the cap must therefore arrive as one layer, and the restore must still be able
// to read every byte of it.
func TestCollapseSealedQcow2LeavesTheRestoreNothingToFlatten(t *testing.T) {
	requireQemuImg(t)
	smallDurableDisk(t)
	t.Setenv(qcow2.MaxChainEnvVar, "3")
	ctx := context.Background()
	const uid = "actor-collapse"

	checkpoint := t.TempDir()
	deepChain(t, ctx, checkpoint, 3)
	sealed, err := qcow2.ReadManifest(filepath.Join(checkpoint, ateompath.DurableDirChainFile))
	if err != nil {
		t.Fatalf("ReadManifest() = %v", err)
	}

	collapsed, err := collapseSealedQcow2(ctx, uid, checkpoint, sealed)
	if err != nil {
		t.Fatalf("collapseSealedQcow2() = %v", err)
	}
	if len(collapsed.Layers) != 1 {
		t.Fatalf("collapseSealedQcow2() left %d layers, want 1", len(collapsed.Layers))
	}
	// The manifest on disk is what the restore trusts, so it has to agree with
	// the directory: a layer it still names but the collapse removed would fail
	// VerifyPresent and strand the actor.
	onDisk, err := qcow2.ReadManifest(filepath.Join(checkpoint, ateompath.DurableDirChainFile))
	if err != nil {
		t.Fatalf("ReadManifest() after the collapse = %v", err)
	}
	if err := onDisk.VerifyPresent(checkpoint); err != nil {
		t.Errorf("the rewritten manifest does not describe the checkpoint: %v", err)
	}
	if len(onDisk.Layers) != 1 {
		t.Errorf("the manifest on disk names %d layers, want 1", len(onDisk.Layers))
	}

	if err := landDurableQcow2(ctx, uid, checkpoint); err != nil {
		t.Fatalf("landDurableQcow2() = %v", err)
	}
	_, landed, err := durableQcow2Top(uid)
	if err != nil {
		t.Fatalf("durableQcow2Top() = %v", err)
	}
	if len(landed.Layers) != 2 {
		t.Errorf("landed %d layers, want the collapsed base plus a fresh top", len(landed.Layers))
	}
}

// Below the cap the collapse must not run: it is a convert of the actor's data,
// and paying it on every suspend rather than every MaxChain-th would cost more
// than the restore path it was moved off.
func TestCollapseSealedQcow2LeavesAShallowChainAlone(t *testing.T) {
	requireQemuImg(t)
	smallDurableDisk(t)
	t.Setenv(qcow2.MaxChainEnvVar, "8")
	ctx := context.Background()

	checkpoint := t.TempDir()
	deepChain(t, ctx, checkpoint, 3)
	sealed, err := qcow2.ReadManifest(filepath.Join(checkpoint, ateompath.DurableDirChainFile))
	if err != nil {
		t.Fatalf("ReadManifest() = %v", err)
	}
	collapsed, err := collapseSealedQcow2(ctx, "actor-shallow", checkpoint, sealed)
	if err != nil {
		t.Fatalf("collapseSealedQcow2() = %v", err)
	}
	if len(collapsed.Layers) != 3 {
		t.Errorf("collapseSealedQcow2() collapsed a chain of 3 under a cap of 8, leaving %d layers", len(collapsed.Layers))
	}
}

// The checkpoint's layers are hardlinks to the live actor's, so a collapse that
// wrote through an inode instead of replacing a name would reach back into the
// actor that is still running on that chain.
func TestCollapseSealedQcow2DoesNotTouchTheLiveChain(t *testing.T) {
	requireQemuImg(t)
	smallDurableDisk(t)
	t.Setenv(qcow2.MaxChainEnvVar, "2")
	ctx := context.Background()
	const uid = "actor-live"

	if err := initDurableQcow2(ctx, uid); err != nil {
		t.Fatalf("initDurableQcow2() = %v", err)
	}
	checkpoint := t.TempDir()
	sealed, err := sealDurableQcow2(ctx, uid, checkpoint)
	if err != nil {
		t.Fatalf("sealDurableQcow2() = %v", err)
	}
	if _, err := collapseSealedQcow2(ctx, uid, checkpoint, sealed); err != nil {
		t.Fatalf("collapseSealedQcow2() = %v", err)
	}

	_, live, err := durableQcow2Top(uid)
	if err != nil {
		t.Fatalf("durableQcow2Top() = %v", err)
	}
	if len(live.Layers) != len(sealed.Layers) {
		t.Errorf("the live chain has %d layers after the collapse, want the %d it had", len(live.Layers), len(sealed.Layers))
	}
	if err := live.VerifyPresent(durableQcow2Dir(uid)); err != nil {
		t.Errorf("the collapse removed a layer the live actor is running on: %v", err)
	}
}

// A chain that arrives at the depth cap is collapsed before it is stacked on,
// so the chain handed to cloud-hypervisor never gets deeper than MaxChain.
// The suspend side normally collapses first; this is the fallback for the
// snapshots that arrive deep anyway.
func TestLandDurableQcow2FlattensADeepChain(t *testing.T) {
	requireQemuImg(t)
	smallDurableDisk(t)
	t.Setenv(qcow2.MaxChainEnvVar, "3")
	ctx := context.Background()
	const uid = "actor-flatten"

	checkpoint := t.TempDir()
	deepChain(t, ctx, checkpoint, 3)
	if err := landDurableQcow2(ctx, uid, checkpoint); err != nil {
		t.Fatalf("landDurableQcow2() = %v", err)
	}
	_, m, err := durableQcow2Top(uid)
	if err != nil {
		t.Fatalf("durableQcow2Top() = %v", err)
	}
	if len(m.Layers) != 2 {
		t.Fatalf("landDurableQcow2() left %d layers, want the flattened base plus the writable top", len(m.Layers))
	}
	if err := m.VerifyPresent(durableQcow2Dir(uid)); err != nil {
		t.Errorf("the manifest does not describe what is on disk: %v", err)
	}
	top, _, err := durableQcow2Top(uid)
	if err != nil {
		t.Fatal(err)
	}
	if err := qcow2.Check(ctx, top); err != nil {
		t.Errorf("Check() after flattening = %v", err)
	}
}

// The depth cap is what keeps the chain inside cloud-hypervisor's nesting
// limit, so a node configured past that limit must still land a chain CH will
// open. Getting this wrong does not cost one slow restore: a boot that fails
// lands no layer and collapses none, so the actor never starts again.
func TestLandDurableQcow2StaysInsideTheNestingLimit(t *testing.T) {
	requireQemuImg(t)
	smallDurableDisk(t)
	t.Setenv(qcow2.MaxChainEnvVar, "999")
	ctx := context.Background()
	const uid = "actor-nesting"

	checkpoint := t.TempDir()
	deepChain(t, ctx, checkpoint, qcow2.MaxChain())
	if err := landDurableQcow2(ctx, uid, checkpoint); err != nil {
		t.Fatalf("landDurableQcow2() = %v", err)
	}
	_, m, err := durableQcow2Top(uid)
	if err != nil {
		t.Fatalf("durableQcow2Top() = %v", err)
	}
	if len(m.Layers) > qcow2.MaxChain() {
		t.Errorf("landDurableQcow2() handed cloud-hypervisor %d layers, want no more than %d",
			len(m.Layers), qcow2.MaxChain())
	}
}

// The flatten collapses into the name of the layer it collapsed UP TO, not a
// fresh one and not the base's. Anything stacked on the chain records that name
// in its own header, and cloud-hypervisor is writing to it, so reusing the name
// is what lets the whole thing happen without a second writer on that header.
func TestFlattenDurableQcow2KeepsTheNameTheTopBacksOnto(t *testing.T) {
	requireQemuImg(t)
	smallDurableDisk(t)
	ctx := context.Background()
	const uid = "actor-flatten-name"

	dir := durableQcow2Dir(uid)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	deepChain(t, ctx, dir, 3)
	m, err := qcow2.ReadManifest(filepath.Join(dir, ateompath.DurableDirChainFile))
	if err != nil {
		t.Fatal(err)
	}
	layers := m.LayerFiles()

	base, err := flattenDurableQcow2(ctx, uid, layers)
	if err != nil {
		t.Fatalf("flattenDurableQcow2() = %v", err)
	}
	if want := layers[len(layers)-1]; base != want {
		t.Errorf("flattenDurableQcow2() = %q, want the chain's own top layer %q", base, want)
	}
	chain, err := qcow2.BackingChain(ctx, filepath.Join(dir, base))
	if err != nil {
		t.Fatalf("BackingChain() = %v", err)
	}
	if len(chain) != 1 {
		t.Errorf("the flattened layer still backs onto %d others, want it self-contained", len(chain)-1)
	}
	for _, name := range layers[:len(layers)-1] {
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Errorf("flattenDurableQcow2() left the collapsed layer %q behind", name)
		}
	}
}

// A chain with nothing in it is a caller bug, and flattening it would produce
// an empty directory that still looked like a chain.
func TestFlattenDurableQcow2RejectsAnEmptyChain(t *testing.T) {
	smallDurableDisk(t)
	if _, err := flattenDurableQcow2(context.Background(), "actor-empty", nil); err == nil {
		t.Error("flattenDurableQcow2() on an empty chain = nil, want an error")
	}
}

// prepareDurableVolumes places a boot on one arrangement or the other, and the
// order of its cases is the policy: what the actor already has beats what the
// node is configured to do, because converting between them means rewriting the
// actor's data.
func TestPrepareDurableVolumes(t *testing.T) {
	withDurable := []*ateompb.Container{{Name: "app", DurableDirVolumeMounts: []*ateompb.DurableDirVolumeMount{
		{VolumeName: "data", MountPath: "/home/counter"},
	}}}

	t.Run("no durable volumes stays on the directory", func(t *testing.T) {
		smallDurableDisk(t)
		t.Setenv(qcow2.BackendEnvVar, qcow2.BackendQcow2)
		got, err := prepareDurableVolumes(context.Background(), actorBootParams{
			actorUID: "actor-none", containers: []*ateompb.Container{{Name: "app"}},
		})
		if err != nil || got {
			t.Errorf("prepareDurableVolumes() = %v, %v; want false, nil", got, err)
		}
	})

	t.Run("backend off stays on the directory", func(t *testing.T) {
		smallDurableDisk(t)
		t.Setenv(qcow2.BackendEnvVar, "")
		got, err := prepareDurableVolumes(context.Background(), actorBootParams{
			actorUID: "actor-off", containers: withDurable,
		})
		if err != nil || got {
			t.Errorf("prepareDurableVolumes() = %v, %v; want false, nil", got, err)
		}
	})

	// The case that matters most: a tar snapshot was just extracted into the
	// host directory. Building a chain now would hand the guest an empty disk
	// and strand the data that was restored.
	t.Run("a restored tar is not converted", func(t *testing.T) {
		smallDurableDisk(t)
		t.Setenv(qcow2.BackendEnvVar, qcow2.BackendQcow2)
		got, err := prepareDurableVolumes(context.Background(), actorBootParams{
			actorUID: "actor-tar", containers: withDurable, durableRestored: true,
		})
		if err != nil || got {
			t.Errorf("prepareDurableVolumes() = %v, %v; want false, nil", got, err)
		}
	})

	t.Run("backend on builds a chain", func(t *testing.T) {
		requireQemuImg(t)
		smallDurableDisk(t)
		t.Setenv(qcow2.BackendEnvVar, qcow2.BackendQcow2)
		got, err := prepareDurableVolumes(context.Background(), actorBootParams{
			actorUID: "actor-new", containers: withDurable,
		})
		if err != nil || !got {
			t.Fatalf("prepareDurableVolumes() = %v, %v; want true, nil", got, err)
		}
		if !durableQcow2Active("actor-new") {
			t.Error("prepareDurableVolumes() reported a disk but built no chain")
		}
	})

	// A node with the backend OFF still has to serve a chain a restore landed,
	// or an actor is stranded the moment it lands on a node mid-rollout.
	t.Run("a landed chain is served with the backend off", func(t *testing.T) {
		requireQemuImg(t)
		smallDurableDisk(t)
		t.Setenv(qcow2.BackendEnvVar, qcow2.BackendQcow2)
		if err := initDurableQcow2(context.Background(), "actor-landed"); err != nil {
			t.Fatal(err)
		}
		t.Setenv(qcow2.BackendEnvVar, "")
		got, err := prepareDurableVolumes(context.Background(), actorBootParams{
			actorUID: "actor-landed", containers: withDurable, durableRestored: true,
		})
		if err != nil || !got {
			t.Errorf("prepareDurableVolumes() = %v, %v; want true, nil", got, err)
		}
	})
}

// restoreDurableVolumes reads the arrangement off the SNAPSHOT rather than the
// node's setting, so an actor keeps working when it lands on a node configured
// the other way.
func TestRestoreDurableVolumesFollowsTheSnapshot(t *testing.T) {
	t.Run("a tar restores as a directory even with the backend on", func(t *testing.T) {
		smallDurableDisk(t)
		t.Setenv(qcow2.BackendEnvVar, qcow2.BackendQcow2)
		snapshot := t.TempDir()
		src := durableDirWith(t, []string{"data"}, false)
		if err := tarDurableVolumes(context.Background(), src, snapshot); err != nil {
			t.Fatalf("tarDurableVolumes() = %v", err)
		}

		dst := filepath.Join(t.TempDir(), "durable")
		if err := restoreDurableVolumes(context.Background(), "actor-tar", dst, snapshot); err != nil {
			t.Fatalf("restoreDurableVolumes() = %v", err)
		}
		if _, err := os.Stat(filepath.Join(dst, "data")); err != nil {
			t.Errorf("the tar was not extracted: %v", err)
		}
		if durableQcow2Active("actor-tar") {
			t.Error("restoreDurableVolumes() built a chain for a tar snapshot")
		}
	})

	t.Run("a chain restores as a disk", func(t *testing.T) {
		requireQemuImg(t)
		smallDurableDisk(t)
		t.Setenv(qcow2.BackendEnvVar, "")
		ctx := context.Background()

		t.Setenv(qcow2.BackendEnvVar, qcow2.BackendQcow2)
		if err := initDurableQcow2(ctx, "actor-src"); err != nil {
			t.Fatal(err)
		}
		snapshot := t.TempDir()
		if _, err := sealDurableQcow2(ctx, "actor-src", snapshot); err != nil {
			t.Fatal(err)
		}
		t.Setenv(qcow2.BackendEnvVar, "")

		dst := filepath.Join(t.TempDir(), "durable")
		if err := restoreDurableVolumes(ctx, "actor-dst", dst, snapshot); err != nil {
			t.Fatalf("restoreDurableVolumes() = %v", err)
		}
		if !durableQcow2Active("actor-dst") {
			t.Error("restoreDurableVolumes() did not land the chain on a node with the backend off")
		}
	})

	// A chain landing over an actor that previously ran on the directory — and
	// the reverse — must leave no trace of the other arrangement.
	t.Run("a tar clears a stale chain", func(t *testing.T) {
		requireQemuImg(t)
		smallDurableDisk(t)
		ctx := context.Background()
		t.Setenv(qcow2.BackendEnvVar, qcow2.BackendQcow2)
		if err := initDurableQcow2(ctx, "actor-mixed"); err != nil {
			t.Fatal(err)
		}
		snapshot := t.TempDir()
		src := durableDirWith(t, []string{"data"}, false)
		if err := tarDurableVolumes(ctx, src, snapshot); err != nil {
			t.Fatal(err)
		}
		dst := filepath.Join(t.TempDir(), "durable")
		if err := restoreDurableVolumes(ctx, "actor-mixed", dst, snapshot); err != nil {
			t.Fatalf("restoreDurableVolumes() = %v", err)
		}
		if durableQcow2Active("actor-mixed") {
			t.Error("restoreDurableVolumes() left the previous activation's chain in place")
		}
	})
}
