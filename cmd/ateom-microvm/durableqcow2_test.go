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

// A guest with no agent connection cannot be flushed, and a suspend that
// silently skipped the flush would lose the actor's most recent writes. It has
// to be an error.
func TestFlushGuestFilesystemsNeedsAnAgent(t *testing.T) {
	if _, err := flushGuestFilesystems(context.Background(), nil, "app"); err == nil {
		t.Fatal("flushGuestFilesystems() with no agent = nil, want an error")
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
	want := []string{
		qcow2.NextLayerName(ateompath.DurableDirLayerPrefix, 0),
		qcow2.NextLayerName(ateompath.DurableDirLayerPrefix, 1),
	}
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

// A chain that has reached the depth cap is flattened as it lands, so reads
// stop walking an ever-growing stack of indirections. The result must still be
// the same disk, and must still have a writable layer on top.
func TestLandDurableQcow2FlattensADeepChain(t *testing.T) {
	requireQemuImg(t)
	smallDurableDisk(t)
	t.Setenv(qcow2.MaxChainEnvVar, "3")
	ctx := context.Background()

	// Build up a chain by sealing and landing repeatedly, as suspend/resume
	// cycles do, until it is deep enough to trip the cap.
	const uid = "actor-deep"
	if err := initDurableQcow2(ctx, uid); err != nil {
		t.Fatalf("initDurableQcow2() = %v", err)
	}
	var landed qcow2.Manifest
	for i := 0; i < 3; i++ {
		checkpoint := t.TempDir()
		if _, err := sealDurableQcow2(ctx, uid, checkpoint); err != nil {
			t.Fatalf("sealDurableQcow2() = %v", err)
		}
		if err := landDurableQcow2(ctx, uid, checkpoint); err != nil {
			t.Fatalf("landDurableQcow2() = %v", err)
		}
		var err error
		if _, landed, err = durableQcow2Top(uid); err != nil {
			t.Fatalf("durableQcow2Top() = %v", err)
		}
	}
	if len(landed.Layers) > 3 {
		t.Errorf("chain reached %d layers with a cap of 3; it is not being flattened", len(landed.Layers))
	}
	top, _, err := durableQcow2Top(uid)
	if err != nil {
		t.Fatal(err)
	}
	if err := qcow2.Check(ctx, top); err != nil {
		t.Errorf("Check() after flattening = %v", err)
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
