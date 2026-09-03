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
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/agent-substrate/substrate/internal/ateompath"
)

// withIncrementalCapture turns incremental capture on and points the generation
// cache at a scratch directory, since the real one lives under /var/lib.
func withIncrementalCapture(t *testing.T) {
	t.Helper()
	t.Setenv(incrementalDurableDirEnv, "1")
	saved := ateompath.IncrementalDurableDirCacheDir
	ateompath.IncrementalDurableDirCacheDir = t.TempDir()
	t.Cleanup(func() { ateompath.IncrementalDurableDirCacheDir = saved })
}

// writeVolumeFile puts content in a durable-dir volume.
func writeVolumeFile(t *testing.T, dir, volume, name, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, volume), 0o700); err != nil {
		t.Fatalf("creating volume %q: %v", volume, err)
	}
	if err := os.WriteFile(filepath.Join(dir, volume, name), []byte(content), 0o644); err != nil {
		t.Fatalf("writing %s/%s: %v", volume, name, err)
	}
}

// suspend captures src into a fresh checkpoint directory and returns it.
func suspend(t *testing.T, actorUID, src string) string {
	t.Helper()
	checkpointDir := t.TempDir()
	if err := tarDurableVolumes(context.Background(), actorUID, src, checkpointDir); err != nil {
		t.Fatalf("tarDurableVolumes: %v", err)
	}
	return checkpointDir
}

// resume restores a checkpoint into a fresh directory and returns it.
func resume(t *testing.T, actorUID, checkpointDir string) string {
	t.Helper()
	dst := filepath.Join(t.TempDir(), "durable")
	if err := untarDurableVolumes(actorUID, dst, checkpointDir); err != nil {
		t.Fatalf("untarDurableVolumes: %v", err)
	}
	return dst
}

// wantVolumeFile asserts a restored file's content.
func wantVolumeFile(t *testing.T, dir, volume, name, want string) {
	t.Helper()
	got, err := os.ReadFile(filepath.Join(dir, volume, name))
	if err != nil {
		t.Errorf("reading restored %s/%s: %v", volume, name, err)
		return
	}
	if string(got) != want {
		t.Errorf("restored %s/%s = %q, want %q", volume, name, got, want)
	}
}

// sameFile reports whether two paths name the same inode.
func sameFile(t *testing.T, a, b string) bool {
	t.Helper()
	ai, err := os.Stat(a)
	if err != nil {
		t.Fatalf("stat %q: %v", a, err)
	}
	bi, err := os.Stat(b)
	if err != nil {
		t.Fatalf("stat %q: %v", b, err)
	}
	return os.SameFile(ai, bi)
}

// fileSize returns a checkpoint file's size, or -1 when it is absent.
func fileSize(t *testing.T, dir, name string) int64 {
	t.Helper()
	info, err := os.Stat(filepath.Join(dir, name))
	if err != nil {
		return -1
	}
	return info.Size()
}

func TestCaptureIsFullUnlessTheFlagIsSet(t *testing.T) {
	// The flag being off has to leave the checkpoint exactly as it was before
	// any of this existed, since that is what every deployment gets by default.
	src := t.TempDir()
	writeVolumeFile(t, src, "data", "a.txt", "hello")

	checkpointDir := suspend(t, "actor", src)
	if got := fileSize(t, checkpointDir, ateompath.DurableDirManifestFile); got != -1 {
		t.Errorf("a full capture wrote %s", ateompath.DurableDirManifestFile)
	}
	if got := fileSize(t, checkpointDir, ateompath.DurableDirTarFile); got <= 0 {
		t.Errorf("a full capture did not write %s", ateompath.DurableDirTarFile)
	}

	wantVolumeFile(t, resume(t, "actor", checkpointDir), "data", "a.txt", "hello")
}

func TestIncrementalCaptureArchivesOnlyTheChange(t *testing.T) {
	withIncrementalCapture(t)

	// A volume large enough that carrying it a second time would be obvious in
	// the tar's size, beside a small one that is the only thing to change.
	src := t.TempDir()
	writeVolumeFile(t, src, "data", "big.bin", strings.Repeat("x", 512<<10))
	writeVolumeFile(t, src, "cache", "small.txt", "one")

	first := suspend(t, "actor", src)
	// The first capture has nothing to inherit, so it is a full one under
	// another name: one archive, published as the file a full capture writes.
	if got := fileSize(t, first, ateompath.DurableDirGenTarFile(1)); got != -1 {
		t.Errorf("the first generation was published as an inherited archive")
	}
	fullSize := fileSize(t, first, ateompath.DurableDirTarFile)
	if fullSize < 512<<10 {
		t.Fatalf("the first capture's tar is %d bytes, too small to hold the volume", fullSize)
	}

	// The actor resumes and rewrites only the small volume.
	restored := resume(t, "actor", first)
	wantVolumeFile(t, restored, "data", "big.bin", strings.Repeat("x", 512<<10))
	writeVolumeFile(t, restored, "cache", "small.txt", "two")

	second := suspend(t, "actor", restored)
	deltaSize := fileSize(t, second, ateompath.DurableDirTarFile)
	if deltaSize <= 0 {
		t.Fatalf("the second capture wrote no tar")
	}
	if deltaSize > fullSize/8 {
		t.Errorf("the second capture's tar is %d bytes against a full %d; it did not archive only the change", deltaSize, fullSize)
	}

	// The unchanged volume is inherited, so its archive rides along and the
	// snapshot stays restorable on its own.
	if got := fileSize(t, second, ateompath.DurableDirGenTarFile(1)); got != fullSize {
		t.Errorf("generation 1's archive is %d bytes in the second snapshot, want the original %d", got, fullSize)
	}
	if got := fileSize(t, second, ateompath.DurableDirManifestFile); got <= 0 {
		t.Errorf("the second capture wrote no manifest")
	}

	// Both volumes come back, each from the generation that holds it.
	final := resume(t, "actor", second)
	wantVolumeFile(t, final, "data", "big.bin", strings.Repeat("x", 512<<10))
	wantVolumeFile(t, final, "cache", "small.txt", "two")
}

func TestInheritedArchivesAreHardlinkedNotCopied(t *testing.T) {
	withIncrementalCapture(t)

	// Rewriting an inherited generation into every snapshot would spend exactly
	// the disk writes this scheme exists to avoid, so the link is the feature.
	src := t.TempDir()
	writeVolumeFile(t, src, "data", "big.bin", strings.Repeat("x", 256<<10))

	first := suspend(t, "actor", src)
	restored := resume(t, "actor", first)
	writeVolumeFile(t, restored, "data", "note.txt", "changed")
	second := suspend(t, "actor", restored)

	cached := filepath.Join(ateompath.IncrementalDurableDirActorCacheDir("actor"), "g1.tar")
	published := filepath.Join(second, ateompath.DurableDirGenTarFile(1))
	if !sameFile(t, cached, published) {
		t.Errorf("generation 1 was copied into the snapshot rather than linked")
	}
}

func TestARestoredSnapshotSeedsTheNextIncrementalCapture(t *testing.T) {
	withIncrementalCapture(t)

	// Without the seeding, an actor that migrates to a new worker would keep
	// recapturing in full: the chain has to be re-derivable from the snapshot.
	src := t.TempDir()
	writeVolumeFile(t, src, "data", "big.bin", strings.Repeat("x", 256<<10))
	first := suspend(t, "actor", src)

	// A different actor UID stands in for a worker that has never seen this
	// chain, so the only thing the capture can build on is the restore.
	restored := resume(t, "fresh", first)
	writeVolumeFile(t, restored, "data", "note.txt", "changed")
	second := suspend(t, "fresh", restored)

	if got, want := fileSize(t, second, ateompath.DurableDirTarFile), int64(256<<10); got >= want {
		t.Errorf("the capture after a restore wrote %d bytes; it recaptured the volume instead of inheriting it", got)
	}
	final := resume(t, "fresh", second)
	wantVolumeFile(t, final, "data", "big.bin", strings.Repeat("x", 256<<10))
	wantVolumeFile(t, final, "data", "note.txt", "changed")
}

func TestChainLengthIsBounded(t *testing.T) {
	withIncrementalCapture(t)
	t.Setenv(maxDurableDirChainEnv, "2")

	// Each cycle adds a volume, so every generation stays referenced and the
	// chain grows until the bound forces a fresh full capture.
	src := t.TempDir()
	writeVolumeFile(t, src, "v0", "a.txt", "0")

	dir, lengths := src, []int{}
	for i := 1; i <= 4; i++ {
		checkpointDir := suspend(t, "actor", dir)
		m, err := readSnapshotManifest(checkpointDir)
		if err != nil {
			t.Fatalf("reading the manifest of cycle %d: %v", i, err)
		}
		if m == nil {
			t.Fatalf("cycle %d wrote no manifest", i)
		}
		lengths = append(lengths, len(m.NeededGenerations()))

		dir = resume(t, "actor", checkpointDir)
		writeVolumeFile(t, dir, "v"+string(rune('0'+i)), "a.txt", "1")
	}

	for i, got := range lengths {
		if got > 2 {
			t.Errorf("cycle %d referenced %d generations, above the bound of 2", i+1, got)
		}
	}
	// The bound has to actually bite, or the test would pass on a chain that
	// simply never grew. Only the first cycle is a full capture for want of
	// anything to inherit; a later one of length 1 is the bound at work.
	if !slices.Contains(lengths[1:], 1) {
		t.Errorf("chain lengths were %v; the bound never forced a full recapture", lengths)
	}
}

func TestACaptureAfterTheFlagIsClearedStillRestores(t *testing.T) {
	withIncrementalCapture(t)

	src := t.TempDir()
	writeVolumeFile(t, src, "data", "a.txt", "hello")
	checkpointDir := suspend(t, "actor", src)

	// Restore reads the scheme off the snapshot, not the environment, so an
	// operator turning the flag off does not strand the chains already taken.
	t.Setenv(incrementalDurableDirEnv, "0")
	wantVolumeFile(t, resume(t, "actor", checkpointDir), "data", "a.txt", "hello")
}

func TestPruningDropsUnreferencedGenerations(t *testing.T) {
	withIncrementalCapture(t)

	// One file rewritten every cycle: each generation supersedes the last, so
	// nothing but the newest stays referenced and the cache must not grow.
	dir := t.TempDir()
	writeVolumeFile(t, dir, "data", "a.txt", "0")
	for i := 1; i <= 3; i++ {
		checkpointDir := suspend(t, "actor", dir)
		dir = resume(t, "actor", checkpointDir)
		writeVolumeFile(t, dir, "data", "a.txt", string(rune('0'+i)))
	}

	entries, err := os.ReadDir(ateompath.IncrementalDurableDirActorCacheDir("actor"))
	if err != nil {
		t.Fatalf("reading the generation cache: %v", err)
	}
	tars := 0
	for _, e := range entries {
		if _, ok := cacheGeneration(e.Name()); ok {
			tars++
		}
	}
	if tars != 1 {
		t.Errorf("the cache holds %d generation archives, want 1: superseded ones were not pruned", tars)
	}
}

func TestALostGenerationFallsBackToAFullCapture(t *testing.T) {
	withIncrementalCapture(t)

	// A snapshot that names an archive nobody has is unrestorable, and the
	// failure would not surface until the resume — so a gap in the cache has to
	// cost the saving rather than the actor's data.
	src := t.TempDir()
	writeVolumeFile(t, src, "data", "big.bin", strings.Repeat("x", 256<<10))
	first := suspend(t, "actor", src)

	dir := resume(t, "actor", first)
	writeVolumeFile(t, dir, "data", "note.txt", "changed")
	if err := os.Remove(filepath.Join(ateompath.IncrementalDurableDirActorCacheDir("actor"), "g1.tar")); err != nil {
		t.Fatalf("removing the cached generation: %v", err)
	}

	second := suspend(t, "actor", dir)
	m, err := readSnapshotManifest(second)
	if err != nil {
		t.Fatalf("reading the manifest: %v", err)
	}
	if got := m.NeededGenerations(); len(got) != 1 {
		t.Errorf("the capture referenced generations %v after losing one; it should have recaptured in full", got)
	}
	final := resume(t, "actor", second)
	wantVolumeFile(t, final, "data", "big.bin", strings.Repeat("x", 256<<10))
	wantVolumeFile(t, final, "data", "note.txt", "changed")
}
