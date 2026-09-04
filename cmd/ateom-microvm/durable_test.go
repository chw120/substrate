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
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/agent-substrate/substrate/cmd/ateom-microvm/internal/kata"
	"github.com/agent-substrate/substrate/cmd/ateom-microvm/internal/tarutil"
	"github.com/agent-substrate/substrate/internal/ateompath"
	"github.com/agent-substrate/substrate/internal/proto/ateompb"
)

func TestHasDurableVolumes(t *testing.T) {
	tests := []struct {
		name       string
		containers []*ateompb.Container
		want       bool
	}{
		{name: "no containers"},
		{
			name:       "container without durable volumes",
			containers: []*ateompb.Container{{Name: "app"}},
		},
		{
			name: "one of several containers has a durable volume",
			containers: []*ateompb.Container{
				{Name: "sidecar"},
				{Name: "app", DurableDirVolumeMounts: []*ateompb.DurableDirVolumeMount{
					{VolumeName: "data", MountPath: "/home/counter"},
				}},
			},
			want: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := hasDurableVolumes(tc.containers); got != tc.want {
				t.Errorf("hasDurableVolumes() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestDurableVolumeNames(t *testing.T) {
	// Two containers sharing a volume: the pair is one directory, and asking
	// for it twice would be a second MkdirAll of the same path.
	containers := []*ateompb.Container{
		{Name: "app", DurableDirVolumeMounts: []*ateompb.DurableDirVolumeMount{
			{VolumeName: "data", MountPath: "/home/counter"},
			{VolumeName: "cache", MountPath: "/var/cache"},
		}},
		{Name: "sidecar", DurableDirVolumeMounts: []*ateompb.DurableDirVolumeMount{
			{VolumeName: "data", MountPath: "/mnt/data"},
		}},
		{Name: "plain"},
	}
	got := durableVolumeNames(containers)
	want := []string{"data", "cache"}
	if !slices.Equal(got, want) {
		t.Errorf("durableVolumeNames() = %v, want %v", got, want)
	}
	if names := durableVolumeNames(nil); len(names) != 0 {
		t.Errorf("durableVolumeNames(nil) = %v, want empty", names)
	}
}

// durableDirWith returns a durable-dir volumes directory laid out the way atelet
// prepares one: a subdirectory per volume, plus optionally a stray regular file.
func durableDirWith(t *testing.T, volumes []string, strayFile bool) string {
	t.Helper()
	dir := t.TempDir()
	for _, v := range volumes {
		if err := os.Mkdir(filepath.Join(dir, v), 0o700); err != nil {
			t.Fatalf("creating volume dir %q: %v", v, err)
		}
	}
	if strayFile {
		if err := os.WriteFile(filepath.Join(dir, "not-a-volume"), nil, 0o600); err != nil {
			t.Fatalf("creating stray file: %v", err)
		}
	}
	return dir
}

// useTempActorsDir roots the per-actor state ateom owns — the durable-dir
// image and its overlay dirs — in a temp directory. Not parallel-safe: the
// path is process-global.
func useTempActorsDir(t *testing.T) {
	t.Helper()
	orig := ateompath.ActorsDir
	ateompath.ActorsDir = filepath.Join(t.TempDir(), "actors")
	t.Cleanup(func() { ateompath.ActorsDir = orig })
}

// volumeContents is the fixture every round-trip case restores and re-reads.
var volumeContents = map[string]string{"data": "42", "cache": "7"}

// durableFixture returns a durable-dir directory holding volumeContents.
func durableFixture(t *testing.T) string {
	t.Helper()
	src := durableDirWith(t, []string{"data", "cache"}, false)
	for vol, content := range volumeContents {
		if err := os.WriteFile(filepath.Join(src, vol, "a.txt"), []byte(content), 0o644); err != nil {
			t.Fatalf("writing %q content: %v", vol, err)
		}
	}
	return src
}

// checkVolumes asserts every fixture volume came back under its own name: the
// names are what the guest mount paths are built from after a restore onto
// another node.
func checkVolumes(t *testing.T, dir string) {
	t.Helper()
	for vol, want := range volumeContents {
		got, err := os.ReadFile(filepath.Join(dir, vol, "a.txt"))
		if err != nil {
			t.Errorf("reading restored %q content: %v", vol, err)
			continue
		}
		if string(got) != want {
			t.Errorf("restored %q content = %q, want %q", vol, got, want)
		}
	}
}

func TestDurableVolumesRoundTrip(t *testing.T) {
	useTempActorsDir(t)
	const actorUID = "actor-tar"

	// Checkpoint: every volume the actor has, archived while the guest is paused.
	src := durableFixture(t)
	checkpointDir := t.TempDir()
	if err := archiveDurableVolumes(t.Context(), src, checkpointDir); err != nil {
		t.Fatalf("archiveDurableVolumes: %v", err)
	}
	if _, err := os.Stat(filepath.Join(checkpointDir, durableTarFile)); err != nil {
		t.Fatalf("checkpoint is missing %s: %v", durableTarFile, err)
	}

	// Restore: onto the empty directory atelet re-creates for the actor.
	dst := t.TempDir()
	if err := landDurableVolumes(dst, checkpointDir, actorUID); err != nil {
		t.Fatalf("landDurableVolumes: %v", err)
	}
	checkVolumes(t, dst)
	// A tar leaves the actor on the plain-directory arrangement: nothing to
	// mount, so stageDurableVolumes must go on binding the directory.
	if durableOverlayActive(actorUID) {
		t.Error("a tar restore put the actor on the overlay path")
	}
	if got, want := durableArchiveDir(actorUID), ateompath.DurableDirVolumeMountsDir(actorUID); got != want {
		t.Errorf("durableArchiveDir = %q, want %q", got, want)
	}
}

// TestLandDurableVolumesImage covers the erofs half of the landing branch
// without needing a kernel that can mount one: what landing does with an image
// is adopt it, and the mount is stageDurableVolumes' job precisely because
// CleanupSandboxState runs in between.
func TestLandDurableVolumesImage(t *testing.T) {
	useTempActorsDir(t)
	const actorUID = "actor-erofs"

	snapshotDir := t.TempDir()
	image := filepath.Join(snapshotDir, durableTarFile)
	// The magic alone is enough: nothing on this path reads further.
	content := make([]byte, 1024+4)
	copy(content[1024:], []byte{0xe2, 0xe1, 0xf5, 0xe0})
	if err := os.WriteFile(image, content, 0o644); err != nil {
		t.Fatalf("writing image: %v", err)
	}
	if got, err := tarutil.Sniff(image); err != nil || got != tarutil.FormatErofs {
		t.Fatalf("Sniff(fixture) = %q, %v; the fixture is not an image", got, err)
	}

	dst := t.TempDir()
	if err := landDurableVolumes(dst, snapshotDir, actorUID); err != nil {
		t.Fatalf("landDurableVolumes: %v", err)
	}
	if !durableOverlayActive(actorUID) {
		t.Fatalf("landing an image did not put the actor on the overlay path (no image at %q)",
			durableImagePath(actorUID))
	}
	// Adopted, not copied: the image is the one thing on this path that must
	// not be rewritten, since rewriting it is the cost the format exists to
	// remove. Same inode as the snapshot's copy proves it.
	if !sameFile(t, image, durableImagePath(actorUID)) {
		t.Error("the landed image is a copy of the snapshot's, not a link to it")
	}
	// Landing must not also unpack anything: an image restore writes no files.
	if entries, err := os.ReadDir(dst); err != nil {
		t.Fatalf("reading durable dir: %v", err)
	} else if len(entries) != 0 {
		t.Errorf("landing an image wrote %d entries into the durable dir, want 0", len(entries))
	}
	// A checkpoint of this actor reads the merged overlay, not atelet's
	// directory: the merged tree is where the guest's writes and deletions
	// resolve, so it is the only self-contained view.
	if got, want := durableArchiveDir(actorUID), kata.DurableMergedDir(actorUID); got != want {
		t.Errorf("durableArchiveDir = %q, want %q", got, want)
	}
}

// TestLandDurableVolumesClearsStaleImage covers a worker that resumes an
// image-format actor and then an ordinary tar one. The stale image would
// otherwise decide the second actor's format and serve the FIRST actor's data.
func TestLandDurableVolumesClearsStaleImage(t *testing.T) {
	useTempActorsDir(t)
	const actorUID = "actor-reused"

	if err := os.MkdirAll(ateompath.ActorPath(actorUID), 0o755); err != nil {
		t.Fatalf("creating actor dir: %v", err)
	}
	upper, work := durableUpperWorkDirs(actorUID)
	if err := os.WriteFile(durableImagePath(actorUID), []byte("stale"), 0o644); err != nil {
		t.Fatalf("planting stale image: %v", err)
	}
	for _, d := range []string{upper, work} {
		if err := os.MkdirAll(filepath.Join(d, "data"), 0o755); err != nil {
			t.Fatalf("planting stale overlay dir: %v", err)
		}
	}

	checkpointDir := t.TempDir()
	if err := archiveDurableVolumes(t.Context(), durableFixture(t), checkpointDir); err != nil {
		t.Fatalf("archiveDurableVolumes: %v", err)
	}
	dst := t.TempDir()
	if err := landDurableVolumes(dst, checkpointDir, actorUID); err != nil {
		t.Fatalf("landDurableVolumes: %v", err)
	}
	checkVolumes(t, dst)
	if durableOverlayActive(actorUID) {
		t.Error("a stale image survived a tar restore")
	}
	for _, d := range []string{upper, work} {
		if _, err := os.Stat(d); !os.IsNotExist(err) {
			t.Errorf("stale overlay dir %q survived the restore (err = %v)", d, err)
		}
	}
}

// TestResetDurableOverlayStateTimings checks that the reset reports one timing
// per tree it clears, and that the trees are gone. Teardown attributes seconds
// of suspend latency to these three numbers, so a reset that silently stopped
// filling one of them would read as "that tree is free" rather than as a bug.
func TestResetDurableOverlayStateTimings(t *testing.T) {
	useTempActorsDir(t)
	const actorUID = "actor-reclaim"

	if err := os.MkdirAll(ateompath.ActorPath(actorUID), 0o755); err != nil {
		t.Fatalf("creating actor dir: %v", err)
	}
	upper, work := durableUpperWorkDirs(actorUID)
	if err := os.WriteFile(durableImagePath(actorUID), []byte("image"), 0o644); err != nil {
		t.Fatalf("planting image: %v", err)
	}
	for _, d := range []string{upper, work} {
		if err := os.MkdirAll(filepath.Join(d, "data"), 0o755); err != nil {
			t.Fatalf("planting overlay dir: %v", err)
		}
	}

	got, err := resetDurableOverlayState(actorUID)
	if err != nil {
		t.Fatalf("resetDurableOverlayState: %v", err)
	}
	for name, d := range map[string]time.Duration{
		"image": got.Image, "upper": got.Upper, "work": got.Work,
	} {
		if d < 0 {
			t.Errorf("%s timing is negative: %v", name, d)
		}
	}
	for _, p := range []string{durableImagePath(actorUID), upper, work} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%q survived the reset (err = %v)", p, err)
		}
	}

	// A second reset is the cold-boot case: nothing to remove, no error.
	if _, err := resetDurableOverlayState(actorUID); err != nil {
		t.Fatalf("resetDurableOverlayState on an already-clear actor: %v", err)
	}
}

// TestArchiveDurableVolumesFormat checks the write-side seam both ways: the
// env var picks the format, and the file it produces is one the read side
// identifies as that format under its unchanged name.
func TestArchiveDurableVolumesFormat(t *testing.T) {
	tests := []struct {
		env  string
		want tarutil.Format
	}{
		{env: "", want: tarutil.FormatTar},
		{env: "erofs", want: tarutil.FormatErofs},
	}
	for _, tc := range tests {
		t.Run("env="+tc.env, func(t *testing.T) {
			if tc.want == tarutil.FormatErofs {
				if _, err := exec.LookPath("mkfs.erofs"); err != nil {
					t.Skipf("needs mkfs.erofs on PATH: %v", err)
				}
			}
			t.Setenv(tarutil.FormatEnvVar, tc.env)
			checkpointDir := t.TempDir()
			if err := archiveDurableVolumes(t.Context(), durableFixture(t), checkpointDir); err != nil {
				t.Fatalf("archiveDurableVolumes: %v", err)
			}
			// The name never changes with the format: atelet uses it to carve
			// the durable data out of a FULL snapshot's file set.
			got, err := tarutil.Sniff(filepath.Join(checkpointDir, durableTarFile))
			if err != nil {
				t.Fatalf("Sniff: %v", err)
			}
			if got != tc.want {
				t.Errorf("archived format = %q, want %q", got, tc.want)
			}
		})
	}
}

// sameFile reports whether two paths name one inode.
func sameFile(t *testing.T, a, b string) bool {
	t.Helper()
	sa, err := os.Stat(a)
	if err != nil {
		t.Fatalf("stat %q: %v", a, err)
	}
	sb, err := os.Stat(b)
	if err != nil {
		t.Fatalf("stat %q: %v", b, err)
	}
	return os.SameFile(sa, sb)
}
