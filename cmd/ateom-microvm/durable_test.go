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
	"path/filepath"
	"slices"
	"testing"

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

// The names drive the directories the guest creates inside the durable image,
// so two containers sharing a volume must not produce it twice, and the order
// has to be stable for the storage list to be.
func TestDurableVolumeNames(t *testing.T) {
	got := durableVolumeNames([]*ateompb.Container{
		{Name: "app", DurableDirVolumeMounts: []*ateompb.DurableDirVolumeMount{
			{VolumeName: "state", MountPath: "/var/state"},
			{VolumeName: "data", MountPath: "/var/data"},
		}},
		{Name: "sidecar", DurableDirVolumeMounts: []*ateompb.DurableDirVolumeMount{
			{VolumeName: "data", MountPath: "/mnt/data"},
		}},
		{Name: "plain"},
	})
	if want := []string{"data", "state"}; !slices.Equal(got, want) {
		t.Errorf("durableVolumeNames() = %v, want %v", got, want)
	}
	if got := durableVolumeNames(nil); len(got) != 0 {
		t.Errorf("durableVolumeNames(nil) = %v, want empty", got)
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

func TestDurableVolumesRoundTrip(t *testing.T) {
	// Checkpoint: every volume the actor has, archived while the guest is paused.
	src := durableDirWith(t, []string{"data", "cache"}, false)
	for vol, content := range map[string]string{"data": "42", "cache": "7"} {
		if err := os.WriteFile(filepath.Join(src, vol, "a.txt"), []byte(content), 0o644); err != nil {
			t.Fatalf("writing %q content: %v", vol, err)
		}
	}
	checkpointDir := t.TempDir()
	if err := tarDurableVolumes(t.Context(), src, checkpointDir); err != nil {
		t.Fatalf("tarDurableVolumes: %v", err)
	}
	if _, err := os.Stat(filepath.Join(checkpointDir, durableTarFile)); err != nil {
		t.Fatalf("checkpoint is missing %s: %v", durableTarFile, err)
	}

	// Restore: onto the empty directory atelet re-creates for the actor.
	dst := t.TempDir()
	if err := untarDurableVolumes(dst, checkpointDir); err != nil {
		t.Fatalf("untarDurableVolumes: %v", err)
	}
	// Both volumes come back, each under its own name: the names are what the
	// guest mount paths are built from after a restore onto another node.
	for vol, want := range map[string]string{"data": "42", "cache": "7"} {
		got, err := os.ReadFile(filepath.Join(dst, vol, "a.txt"))
		if err != nil {
			t.Errorf("reading restored %q content: %v", vol, err)
			continue
		}
		if string(got) != want {
			t.Errorf("restored %q content = %q, want %q", vol, got, want)
		}
	}
}
