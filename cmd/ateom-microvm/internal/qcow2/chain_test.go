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
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// writeImage writes a file that IsImage accepts, padded to n bytes. Enough for
// the manifest checks, which are about names and sizes rather than clusters.
func writeImage(t *testing.T, path string, n int) {
	t.Helper()
	b := make([]byte, n)
	copy(b, magic)
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// Layers are ordered base first, so the top — the one cloud-hypervisor opens —
// is the last. Getting this backwards would hand the guest the empty base and
// silently lose every write the actor ever made.
func TestManifestTopIsTheLastLayer(t *testing.T) {
	m := Manifest{Layers: []Layer{
		{File: "durable-dir.layer-0000.qcow2", SizeBytes: 10},
		{File: "durable-dir.layer-0001.qcow2", SizeBytes: 20},
		{File: "durable-dir.layer-0002.qcow2", SizeBytes: 30},
	}}
	top, err := m.Top()
	if err != nil {
		t.Fatalf("Top() = %v", err)
	}
	if top.File != "durable-dir.layer-0002.qcow2" {
		t.Errorf("Top() = %q, want the last layer", top.File)
	}
	if got := m.TotalBytes(); got != 60 {
		t.Errorf("TotalBytes() = %d, want 60", got)
	}
	want := []string{
		"durable-dir.layer-0000.qcow2",
		"durable-dir.layer-0001.qcow2",
		"durable-dir.layer-0002.qcow2",
	}
	if got := m.LayerFiles(); !slices.Equal(got, want) {
		t.Errorf("LayerFiles() = %v, want %v", got, want)
	}
}

// An empty manifest has no top. It is what a failed seal leaves behind, and the
// caller must get an error rather than an empty filename it would then join
// onto a directory and hand to cloud-hypervisor.
func TestManifestTopRejectsAnEmptyChain(t *testing.T) {
	if _, err := (Manifest{}).Top(); err == nil {
		t.Fatal("Top() on an empty manifest = nil, want an error")
	}
}

// A manifest round-trips through the file, and the reader rejects the two
// shapes that would be dangerous downstream: no layers at all, and a layer name
// with a path in it (which a restore would join onto its own directory).
func TestManifestReadWrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "chain.json")
	m := Manifest{
		Layers:           []Layer{{File: "a.qcow2", SizeBytes: 1}, {File: "b.qcow2", SizeBytes: 2}},
		VirtualSizeBytes: 1 << 30,
	}
	if err := WriteManifest(path, m); err != nil {
		t.Fatalf("WriteManifest() = %v", err)
	}
	got, err := ReadManifest(path)
	if err != nil {
		t.Fatalf("ReadManifest() = %v", err)
	}
	if !slices.Equal(got.LayerFiles(), m.LayerFiles()) || got.VirtualSizeBytes != m.VirtualSizeBytes {
		t.Errorf("ReadManifest() = %+v, want %+v", got, m)
	}

	for name, body := range map[string]string{
		"no layers":       `{"layers":[],"virtual_size_bytes":1}`,
		"layer as a path": `{"layers":[{"file":"../elsewhere/a.qcow2"}]}`,
		"not json":        `{`,
	} {
		bad := filepath.Join(dir, "bad.json")
		if err := os.WriteFile(bad, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadManifest(bad); err == nil {
			t.Errorf("ReadManifest() with %s = nil, want an error", name)
		}
	}
}

// VerifyPresent names the FIRST layer that is wrong, and it must catch all
// three ways a chain arrives broken. A hole here means a guest that mounts a
// filesystem whose missing clusters read as zeroes, so nothing may proceed on a
// chain that does not pass.
func TestVerifyPresent(t *testing.T) {
	dir := t.TempDir()
	writeImage(t, filepath.Join(dir, "a.qcow2"), 100)
	writeImage(t, filepath.Join(dir, "b.qcow2"), 200)
	m := Manifest{Layers: []Layer{{File: "a.qcow2", SizeBytes: 100}, {File: "b.qcow2", SizeBytes: 200}}}
	if err := m.VerifyPresent(dir); err != nil {
		t.Fatalf("VerifyPresent() on an intact chain = %v", err)
	}

	t.Run("missing", func(t *testing.T) {
		d := t.TempDir()
		writeImage(t, filepath.Join(d, "b.qcow2"), 200)
		err := m.VerifyPresent(d)
		if err == nil || !strings.Contains(err.Error(), "a.qcow2") {
			t.Errorf("VerifyPresent() = %v, want it to name the missing a.qcow2", err)
		}
	})
	t.Run("truncated", func(t *testing.T) {
		d := t.TempDir()
		writeImage(t, filepath.Join(d, "a.qcow2"), 100)
		writeImage(t, filepath.Join(d, "b.qcow2"), 12)
		err := m.VerifyPresent(d)
		if err == nil || !strings.Contains(err.Error(), "b.qcow2") {
			t.Errorf("VerifyPresent() = %v, want it to name the truncated b.qcow2", err)
		}
	})
	t.Run("not an image", func(t *testing.T) {
		d := t.TempDir()
		writeImage(t, filepath.Join(d, "a.qcow2"), 100)
		if err := os.WriteFile(filepath.Join(d, "b.qcow2"), make([]byte, 200), 0o600); err != nil {
			t.Fatal(err)
		}
		err := m.VerifyPresent(d)
		if err == nil || !strings.Contains(err.Error(), "not a qcow2") {
			t.Errorf("VerifyPresent() = %v, want it to reject b.qcow2 as not an image", err)
		}
	})
}

// A manifest records sizes and no content digest, and this is a guard on that
// rather than a description of it. DescribeChain runs inside the pause window
// of every suspend; anything added here that has to read a layer's bytes turns
// the O(metadata) seal the arrangement exists for back into a full read of the
// actor's data, and it would do so silently.
func TestManifestCarriesNoContentDigest(t *testing.T) {
	b, err := json.Marshal(Manifest{Layers: []Layer{{File: "a.qcow2", SizeBytes: 1}}})
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"sha256", "sha512", "md5", "digest", "checksum"} {
		if strings.Contains(string(b), field) {
			t.Errorf("the manifest encodes a %q field: computing it would read every byte of the chain during the pause", field)
		}
	}
}

// The layer names are what a directory listing and a checkpoint's file set are
// read through, so they have to sort in chain order.
func TestNextLayerNameSortsInChainOrder(t *testing.T) {
	var names []string
	for i := 0; i < 12; i++ {
		names = append(names, NextLayerName("durable-dir.layer-", i))
	}
	if !slices.IsSorted(names) {
		t.Errorf("NextLayerName() produced %v, which does not sort in chain order", names)
	}
	if got, want := names[0], "durable-dir.layer-0000.qcow2"; got != want {
		t.Errorf("NextLayerName(_, 0) = %q, want %q", got, want)
	}
}

// DescribeChain reads the chain out of the HEADERS rather than any manifest
// beside them — that is what makes a seal record what the guest was actually
// reading through — and it records the layers base first with their sizes.
func TestDescribeChain(t *testing.T) {
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

	m, err := DescribeChain(ctx, filepath.Join(dir, delta))
	if err != nil {
		t.Fatalf("DescribeChain() = %v", err)
	}
	if want := []string{base, delta}; !slices.Equal(m.LayerFiles(), want) {
		t.Errorf("DescribeChain() layers = %v, want %v", m.LayerFiles(), want)
	}
	if m.VirtualSizeBytes != probeSizeBytes {
		t.Errorf("DescribeChain() virtual size = %d, want %d", m.VirtualSizeBytes, probeSizeBytes)
	}
	for _, l := range m.Layers {
		if l.SizeBytes == 0 {
			t.Errorf("DescribeChain() layer %q = %+v, want a size", l.File, l)
		}
	}
	if err := m.VerifyPresent(dir); err != nil {
		t.Errorf("VerifyPresent() on the described chain = %v", err)
	}
}
