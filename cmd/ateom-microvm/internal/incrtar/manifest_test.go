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

package incrtar

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// sampleManifest is a small, valid manifest the tests bend out of shape.
func sampleManifest() *Manifest {
	return &Manifest{
		Version:    ManifestVersion,
		Generation: 3,
		Entries: []Entry{
			{Path: "a.txt", Type: TypeRegular, Size: 5, Mode: 0o644, ContentHash: "aa", OriginGen: 1},
			{Path: "b.txt", Type: TypeRegular, Size: 5, Mode: 0o600, ContentHash: "bb", OriginGen: 3},
			{Path: "sub", Type: TypeDir, Mode: 0o755},
			{Path: "sub/c.txt", Type: TypeRegular, Size: 7, Mode: 0o644, ContentHash: "cc", OriginGen: 1},
		},
	}
}

func TestManifestRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "durable.manifest")
	want := sampleManifest()
	if err := WriteManifest(path, want); err != nil {
		t.Fatalf("WriteManifest: %v", err)
	}
	got, err := ReadManifest(path)
	if err != nil {
		t.Fatalf("ReadManifest: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("round trip changed the manifest:\n got %+v\nwant %+v", got, want)
	}
}

func TestReadManifestRejectsAnUnknownVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "durable.manifest")
	if err := os.WriteFile(path, []byte(`{"version":99,"generation":1,"entries":[]}`), 0o600); err != nil {
		t.Fatalf("writing manifest: %v", err)
	}
	_, err := ReadManifest(path)
	if err == nil {
		t.Fatal("ReadManifest accepted version 99, want an error")
	}
	if !strings.Contains(err.Error(), "unsupported manifest version") {
		t.Errorf("error = %v, want it to name the version", err)
	}
}

func TestManifestValidation(t *testing.T) {
	tests := []struct {
		name string
		bend func(*Manifest)
		want string
	}{
		{
			name: "generation below one",
			bend: func(m *Manifest) { m.Generation = 0 },
			want: "not positive",
		},
		{
			name: "duplicate path",
			bend: func(m *Manifest) { m.Entries = append(m.Entries, m.Entries[0]) },
			want: "twice",
		},
		{
			name: "empty path",
			bend: func(m *Manifest) { m.Entries[0].Path = "" },
			want: "empty path",
		},
		{
			name: "entry from a future generation",
			bend: func(m *Manifest) { m.Entries[0].OriginGen = 9 },
			want: "past this manifest's",
		},
		{
			name: "directory attributed to an archive",
			bend: func(m *Manifest) { m.Entries[2].OriginGen = 1 },
			want: "directories and only directories",
		},
		{
			name: "file attributed to no archive",
			bend: func(m *Manifest) { m.Entries[0].OriginGen = 0 },
			want: "directories and only directories",
		},
		{
			name: "link set naming an absent path",
			bend: func(m *Manifest) { m.Entries[0].LinkSet = "gone.txt" },
			want: "not in the manifest",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := sampleManifest()
			tc.bend(m)
			err := m.validate()
			if err == nil {
				t.Fatalf("validate accepted %s, want an error", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestNeededGenerations(t *testing.T) {
	m := sampleManifest()
	if got, want := m.NeededGenerations(), []int{1, 3}; !reflect.DeepEqual(got, want) {
		t.Errorf("NeededGenerations() = %v, want %v", got, want)
	}

	// A tree whose only change was a chmod on a directory needs no archive at
	// all, which is the point of keeping directories out of them.
	dirsOnly := &Manifest{
		Version:    ManifestVersion,
		Generation: 2,
		Entries:    []Entry{{Path: "sub", Type: TypeDir, Mode: 0o700}},
	}
	if got := dirsOnly.NeededGenerations(); len(got) != 0 {
		t.Errorf("NeededGenerations() = %v, want none", got)
	}
}

func TestSameAsIgnoresOnlyTheOriginGeneration(t *testing.T) {
	base := Entry{Path: "a.txt", Type: TypeRegular, Size: 5, Mode: 0o644, UID: 1, GID: 2, ModTimeNano: 42, ContentHash: "aa", OriginGen: 1}

	moved := base
	moved.OriginGen = 7
	if !base.sameAs(moved) {
		t.Error("entries differing only in origin generation compared as different")
	}

	for _, tc := range []struct {
		name string
		bend func(*Entry)
	}{
		{"contents", func(e *Entry) { e.ContentHash = "zz" }},
		{"size", func(e *Entry) { e.Size = 6 }},
		{"mode", func(e *Entry) { e.Mode = 0o600 }},
		{"uid", func(e *Entry) { e.UID = 9 }},
		{"gid", func(e *Entry) { e.GID = 9 }},
		{"modification time", func(e *Entry) { e.ModTimeNano = 43 }},
		{"xattrs", func(e *Entry) { e.XattrDigest = "dd" }},
		{"link set", func(e *Entry) { e.LinkSet = "a.txt" }},
		{"link target", func(e *Entry) { e.Linkname = "elsewhere" }},
		{"type", func(e *Entry) { e.Type = TypeFifo }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			other := base
			tc.bend(&other)
			if base.sameAs(other) {
				t.Errorf("a change of %s compared as unchanged", tc.name)
			}
		})
	}
}
