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

package qcow2

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// probeSizeBytes is the virtual size the tests build images at: above
// mkfs.ext4's floor for a journalled filesystem, and small enough that a whole
// chain costs a fraction of a second.
const probeSizeBytes = 64 << 20

// requireTools skips a test that needs the external binaries. They live in the
// ateom container image (hack/images/ateom-base/Dockerfile), not on a
// developer's laptop, so the tests that exercise a real chain have to be
// skippable — the ones above them that do not shell out are not.
func requireTools(t *testing.T) {
	t.Helper()
	for _, bin := range []string{qemuImg, mkfsExt4} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s is not on PATH", bin)
		}
	}
}

// The backend is off unless the node opts in by name. Everything else — unset,
// a typo, the empty string — has to leave durable data on the virtio-fs
// arrangement, because a node that half-converts is what strands actors.
func TestEnabled(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  bool
	}{
		{"", false},
		{"qcow2", true},
		{" QCOW2 ", true},
		{"qcow", false},
		{"erofs", false},
		{"true", false},
	} {
		t.Setenv(BackendEnvVar, tc.value)
		if got := Enabled(); got != tc.want {
			t.Errorf("Enabled() with %s=%q = %v, want %v", BackendEnvVar, tc.value, got, tc.want)
		}
	}
}

// The size and depth knobs fall back to their defaults for anything that is not
// a positive integer, rather than producing a zero-sized image or a chain that
// flattens on every restore.
func TestSizeAndMaxChainFallBack(t *testing.T) {
	for _, v := range []string{"", "0", "-4", "lots"} {
		t.Setenv(SizeEnvVar, v)
		if got, want := SizeBytes(), int64(defaultSizeGiB)<<30; got != want {
			t.Errorf("SizeBytes() with %s=%q = %d, want %d", SizeEnvVar, v, got, want)
		}
		t.Setenv(MaxChainEnvVar, v)
		if got := MaxChain(); got != defaultMaxChain {
			t.Errorf("MaxChain() with %s=%q = %d, want %d", MaxChainEnvVar, v, got, defaultMaxChain)
		}
	}
	t.Setenv(SizeEnvVar, "2")
	if got, want := SizeBytes(), int64(2)<<30; got != want {
		t.Errorf("SizeBytes() = %d, want %d", got, want)
	}
	t.Setenv(MaxChainEnvVar, "3")
	if got := MaxChain(); got != 3 {
		t.Errorf("MaxChain() = %d, want 3", got)
	}
}

// A backing file given as a path would produce a chain that breaks the first
// time it is moved to another node — which is the one thing this package
// exists to make cheap — so it is rejected at creation rather than discovered
// at restore.
func TestCreateDeltaRejectsAPathBacking(t *testing.T) {
	dir := t.TempDir()
	err := CreateDelta(context.Background(), filepath.Join(dir, "d.qcow2"), filepath.Join(dir, "b.qcow2"))
	if err == nil {
		t.Fatal("CreateDelta() = nil, want an error for a backing file given as a path")
	}
	if !strings.Contains(err.Error(), "bare filename") {
		t.Errorf("CreateDelta() = %v, want the error to say the backing file must be bare", err)
	}
}

// CreateDelta must not write a layer whose backing file is not there: the
// resulting image opens nowhere, and the failure surfaces at the guest rather
// than here.
func TestCreateDeltaRejectsAMissingBacking(t *testing.T) {
	dir := t.TempDir()
	if err := CreateDelta(context.Background(), filepath.Join(dir, "d.qcow2"), "nope.qcow2"); err == nil {
		t.Fatal("CreateDelta() = nil, want an error for a backing file that does not exist")
	}
}

// The magic sniff has to answer for the things a restore actually finds in a
// snapshot dir: a real image, a tar, and a file too short to be either.
func TestIsImage(t *testing.T) {
	requireTools(t)
	dir := t.TempDir()
	img := filepath.Join(dir, "base.qcow2")
	if err := CreateBase(context.Background(), img, probeSizeBytes); err != nil {
		t.Fatalf("CreateBase() = %v", err)
	}
	notImg := filepath.Join(dir, "durable-dir.tar")
	if err := os.WriteFile(notImg, []byte("not a qcow2 at all"), 0o600); err != nil {
		t.Fatal(err)
	}
	short := filepath.Join(dir, "short")
	if err := os.WriteFile(short, []byte{0x51}, 0o600); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]bool{img: true, notImg: false, short: false} {
		got, err := IsImage(path)
		if err != nil {
			t.Fatalf("IsImage(%q) = %v", path, err)
		}
		if got != want {
			t.Errorf("IsImage(%q) = %v, want %v", filepath.Base(path), got, want)
		}
	}
	if _, err := IsImage(filepath.Join(dir, "absent")); err == nil {
		t.Error("IsImage() on a missing file = nil, want an error")
	}
}

// The whole arrangement in one test: build a base, stack two deltas, and read
// the chain back. This is what a cold boot plus two suspend/resume cycles
// leaves behind, and it is the shape every other operation assumes.
func TestChainRoundTrip(t *testing.T) {
	requireTools(t)
	ctx := context.Background()
	dir := t.TempDir()

	base := filepath.Join(dir, "durable-dir.layer-0000.qcow2")
	if err := CreateBase(ctx, base, probeSizeBytes); err != nil {
		t.Fatalf("CreateBase() = %v", err)
	}
	// An empty ext4 in a sparse qcow2 must not cost anything like its virtual
	// size — that is the property the size story rests on.
	st, err := os.Stat(base)
	if err != nil {
		t.Fatal(err)
	}
	if st.Size() >= probeSizeBytes {
		t.Errorf("base layer is %d bytes for a %d byte disk; it is not sparse", st.Size(), probeSizeBytes)
	}

	names := []string{filepath.Base(base)}
	for i := 1; i <= 2; i++ {
		name := NextLayerName("durable-dir.layer-", i)
		if err := CreateDelta(ctx, filepath.Join(dir, name), names[len(names)-1]); err != nil {
			t.Fatalf("CreateDelta(%q) = %v", name, err)
		}
		names = append(names, name)
	}
	top := filepath.Join(dir, names[len(names)-1])

	chain, err := BackingChain(ctx, top)
	if err != nil {
		t.Fatalf("BackingChain() = %v", err)
	}
	if len(chain) != len(names) {
		t.Fatalf("BackingChain() returned %d images, want %d", len(chain), len(names))
	}
	if got, want := filepath.Base(chain[0].Filename), names[len(names)-1]; got != want {
		t.Errorf("BackingChain()[0] = %q, want the top layer %q", got, want)
	}
	if err := Check(ctx, top); err != nil {
		t.Errorf("Check() = %v", err)
	}

	// Sealing a suspend reads the chain of a disk cloud-hypervisor still holds
	// open — pausing the guest does not drop its image lock. Byte 201 is the one
	// qemu names in "Failed to lock byte 201", so holding it reproduces exactly
	// what a live VM does to a chain walk.
	f, err := os.OpenFile(top, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	lock := unix.Flock_t{Type: unix.F_WRLCK, Whence: io.SeekStart, Start: 201, Len: 1}
	if err := unix.FcntlFlock(f.Fd(), unix.F_OFD_SETLK, &lock); err != nil {
		t.Fatalf("locking byte 201 of %q: %v", top, err)
	}
	if _, err := BackingChain(ctx, top); err != nil {
		t.Errorf("BackingChain() on an image a running VM holds open = %v", err)
	}
}

// Moving a whole chain to a different directory must keep it readable with no
// header rewriting. This is the property that lets a restore land a snapshot's
// layers under a different actor on a different node and only repoint
// cloud-hypervisor at the top.
func TestChainSurvivesRelocation(t *testing.T) {
	requireTools(t)
	ctx := context.Background()
	src, dst := t.TempDir(), filepath.Join(t.TempDir(), "elsewhere")
	if err := os.MkdirAll(dst, 0o700); err != nil {
		t.Fatal(err)
	}

	base := "durable-dir.layer-0000.qcow2"
	delta := "durable-dir.layer-0001.qcow2"
	if err := CreateBase(ctx, filepath.Join(src, base), probeSizeBytes); err != nil {
		t.Fatalf("CreateBase() = %v", err)
	}
	if err := CreateDelta(ctx, filepath.Join(src, delta), base); err != nil {
		t.Fatalf("CreateDelta() = %v", err)
	}
	m, err := DescribeChain(ctx, filepath.Join(src, delta))
	if err != nil {
		t.Fatalf("DescribeChain() = %v", err)
	}

	for _, name := range m.LayerFiles() {
		b, err := os.ReadFile(filepath.Join(src, name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dst, name), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.VerifyPresent(dst); err != nil {
		t.Fatalf("VerifyPresent() after relocation = %v", err)
	}
	if err := Check(ctx, filepath.Join(dst, delta)); err != nil {
		t.Errorf("Check() after relocation = %v", err)
	}
}

// Flatten collapses a chain into one image that still reads the same, and the
// result stands alone — a base with no backing file, which is what makes it
// safe to delete the layers it replaced.
func TestFlattenProducesASelfContainedBase(t *testing.T) {
	requireTools(t)
	ctx := context.Background()
	dir := t.TempDir()

	base := "durable-dir.layer-0000.qcow2"
	delta := "durable-dir.layer-0001.qcow2"
	if err := CreateBase(ctx, filepath.Join(dir, base), probeSizeBytes); err != nil {
		t.Fatalf("CreateBase() = %v", err)
	}
	if err := CreateDelta(ctx, filepath.Join(dir, delta), base); err != nil {
		t.Fatalf("CreateDelta() = %v", err)
	}

	flat := filepath.Join(dir, "flat.qcow2")
	if err := Flatten(ctx, filepath.Join(dir, delta), flat); err != nil {
		t.Fatalf("Flatten() = %v", err)
	}
	chain, err := BackingChain(ctx, flat)
	if err != nil {
		t.Fatalf("BackingChain() = %v", err)
	}
	if len(chain) != 1 {
		t.Errorf("the flattened image reports a %d-image chain, want 1 (it still has a backing file)", len(chain))
	}
	// The originals must survive the flatten: nothing may be removed until the
	// replacement is complete, or an interrupted flatten loses the actor's data.
	for _, name := range []string{base, delta} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("Flatten() removed %q: %v", name, err)
		}
	}
}

// allocationRE and compressionRE pull the two figures this file cares about out
// of `qemu-img check`, whose summary line reads
// "128/1024 = 12.50% allocated, 0.00% fragmented, 0.00% compressed clusters".
var (
	allocationRE  = regexp.MustCompile(`(\d+)/\d+ = [\d.]+% allocated`)
	compressionRE = regexp.MustCompile(`([\d.]+)% compressed clusters`)
)

// The flatten must not compress. It runs on the restore path, where its cost is
// resume latency: compressing a durable-dir chain measured 6.6x slower for an
// image of identical size, since the data an actor writes does not generally
// deflate. Assert on the image rather than on the argv so the property survives
// someone rewriting how the convert is invoked.
func TestFlattenLeavesTheBaseUncompressed(t *testing.T) {
	requireTools(t)
	ctx := context.Background()
	dir := t.TempDir()

	base := "durable-dir.layer-0000.qcow2"
	delta := "durable-dir.layer-0001.qcow2"
	if err := CreateBase(ctx, filepath.Join(dir, base), probeSizeBytes); err != nil {
		t.Fatalf("CreateBase() = %v", err)
	}
	if err := CreateDelta(ctx, filepath.Join(dir, delta), base); err != nil {
		t.Fatalf("CreateDelta() = %v", err)
	}

	flat := filepath.Join(dir, "flat.qcow2")
	if err := Flatten(ctx, filepath.Join(dir, delta), flat); err != nil {
		t.Fatalf("Flatten() = %v", err)
	}

	out, err := exec.CommandContext(ctx, qemuImg, "check", "-f", "qcow2", flat).CombinedOutput()
	if err != nil {
		t.Fatalf("qemu-img check on the flattened image = %v (%s)", err, out)
	}
	// A wholly unallocated image reports 0% compressed too, which would pass
	// this test without proving anything. The base carries an ext4, so it has
	// clusters; check that before reading the compression figure.
	alloc := allocationRE.FindSubmatch(out)
	if alloc == nil {
		t.Fatalf("qemu-img check reported no allocation figure: %s", out)
	}
	if string(alloc[1]) == "0" {
		t.Fatalf("the flattened image has no allocated clusters, so its compression says nothing: %s", out)
	}
	compressed := compressionRE.FindSubmatch(out)
	if compressed == nil {
		t.Fatalf("qemu-img check reported no compression figure: %s", out)
	}
	if got := string(compressed[1]); got != "0.00" {
		t.Errorf("the flattened image is %s%% compressed clusters, want 0.00%%: %s", got, out)
	}
}

// Preflight is a no-op on a node that has not opted in — it must not require
// tools the node has no reason to carry.
func TestPreflightSkippedWhenDisabled(t *testing.T) {
	t.Setenv(BackendEnvVar, "")
	if err := Preflight(context.Background()); err != nil {
		t.Errorf("Preflight() with the backend off = %v, want nil", err)
	}
}

// With the backend on, Preflight runs a real round trip. A node that declared
// it would serve durable data from images and cannot must not start.
func TestPreflightRunsARoundTrip(t *testing.T) {
	requireTools(t)
	t.Setenv(BackendEnvVar, BackendQcow2)
	if err := Preflight(context.Background()); err != nil {
		t.Errorf("Preflight() = %v", err)
	}
}

// A missing tool has to be named, and the error has to point at the fix. This
// is the failure an operator sees when the ateom image was not rebuilt from
// hack/images/ateom-base.
func TestPreflightReportsAMissingTool(t *testing.T) {
	t.Setenv(BackendEnvVar, BackendQcow2)
	t.Setenv("PATH", t.TempDir())
	err := Preflight(context.Background())
	if err == nil {
		t.Fatal("Preflight() with an empty PATH = nil, want an error")
	}
	if !strings.Contains(err.Error(), qemuImg) {
		t.Errorf("Preflight() = %v, want the error to name %s", err, qemuImg)
	}
}
