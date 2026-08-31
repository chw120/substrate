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
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/agent-substrate/substrate/cmd/ateom-microvm/internal/kata"
	"github.com/agent-substrate/substrate/cmd/ateom-microvm/internal/qcow2"
)

// writeSnapshotConfig writes a config.json holding the given fs devices (plus the
// vsock and console entries every snapshot has) into a fresh snapshot dir. The
// serial device is the debug-mode one, present here so the rewrite is exercised
// against a snapshot that has both.
func writeSnapshotConfig(t *testing.T, fsDevices []map[string]any) string {
	t.Helper()
	dir := t.TempDir()
	cfg := map[string]any{
		"vsock":   map[string]any{"cid": 3, "socket": "/run/vc/vm/golden/clh.sock"},
		"console": map[string]any{"mode": "File", "file": "/run/vc/vm/golden/console.log"},
		"serial":  map[string]any{"mode": "File", "file": "/run/vc/vm/golden/serial.log"},
		"fs":      fsDevices,
	}
	b, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshaling config: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), b, 0o600); err != nil {
		t.Fatalf("writing config.json: %v", err)
	}
	return dir
}

// readFsSockets returns the rewritten config's fs sockets keyed by device tag.
func readFsSockets(t *testing.T, dir string) map[string]string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatalf("reading config.json: %v", err)
	}
	var cfg struct {
		Vsock  map[string]any `json:"vsock"`
		Serial map[string]any `json:"serial"`
		Fs     []struct {
			Tag    string `json:"tag"`
			Socket string `json:"socket"`
		} `json:"fs"`
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		t.Fatalf("parsing rewritten config: %v", err)
	}
	got := map[string]string{}
	for _, f := range cfg.Fs {
		got[f.Tag] = f.Socket
	}
	return got
}

func TestRewriteSnapshotSocketPaths(t *testing.T) {
	const id = "actor-uid"

	t.Run("overlay lower only", func(t *testing.T) {
		dir := writeSnapshotConfig(t, []map[string]any{
			{"tag": kata.FsTag, "socket": "/run/vc/vm/golden/virtiofsd.sock"},
		})
		if err := rewriteSnapshotSocketPaths(dir, id); err != nil {
			t.Fatalf("rewriteSnapshotSocketPaths: %v", err)
		}
		if got, want := readFsSockets(t, dir)[kata.FsTag], kata.VirtiofsdSocketPath(id); got != want {
			t.Errorf("%s socket = %q, want %q", kata.FsTag, got, want)
		}
	})

	t.Run("unknown tag is an error", func(t *testing.T) {
		// "ateUpper", "ateDurable", and "ateCSI" are retired multi-share tags:
		// they never appear in snapshots this code produces, and one showing up
		// must fail loudly rather than be silently repointed.
		for _, tag := range []string{"somethingElse", "ateUpper", "ateDurable", "ateCSI"} {
			dir := writeSnapshotConfig(t, []map[string]any{
				{"tag": tag, "socket": "/run/vc/vm/golden/other.sock"},
			})
			if err := rewriteSnapshotSocketPaths(dir, id); err == nil {
				t.Fatalf("rewriteSnapshotSocketPaths accepted fs tag %q, want an error", tag)
			}
		}
	})

	t.Run("vsock and console devices are repointed", func(t *testing.T) {
		dir := writeSnapshotConfig(t, []map[string]any{
			{"tag": kata.FsTag, "socket": "/run/vc/vm/golden/virtiofsd.sock"},
		})
		if err := rewriteSnapshotSocketPaths(dir, id); err != nil {
			t.Fatalf("rewriteSnapshotSocketPaths: %v", err)
		}
		b, err := os.ReadFile(filepath.Join(dir, "config.json"))
		if err != nil {
			t.Fatalf("reading config.json: %v", err)
		}
		var cfg struct {
			Vsock   struct{ Socket string } `json:"vsock"`
			Console struct{ File string }   `json:"console"`
			Serial  struct{ File string }   `json:"serial"`
		}
		if err := json.Unmarshal(b, &cfg); err != nil {
			t.Fatalf("parsing rewritten config: %v", err)
		}
		if want := kata.VsockSocketPath(id); cfg.Vsock.Socket != want {
			t.Errorf("vsock socket = %q, want %q", cfg.Vsock.Socket, want)
		}
		if want := kata.ConsoleLogPath(id); cfg.Console.File != want {
			t.Errorf("console file = %q, want %q", cfg.Console.File, want)
		}
		if want := kata.SerialLogPath(id); cfg.Serial.File != want {
			t.Errorf("serial file = %q, want %q", cfg.Serial.File, want)
		}
	})
}

// writeSnapshotConfigWithDisks writes a config.json carrying a disk list, as a
// snapshot of a guest with a durable-dir disk does.
func writeSnapshotConfigWithDisks(t *testing.T, disks []map[string]any) string {
	t.Helper()
	dir := writeSnapshotConfig(t, []map[string]any{
		{"tag": kata.FsTag, "socket": "/run/vc/vm/golden/virtiofsd.sock"},
	})
	path := filepath.Join(dir, "config.json")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading config.json: %v", err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(b, &cfg); err != nil {
		t.Fatalf("parsing config.json: %v", err)
	}
	cfg["disks"] = disks
	if b, err = json.Marshal(cfg); err != nil {
		t.Fatalf("marshaling config: %v", err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatalf("writing config.json: %v", err)
	}
	return dir
}

// readDiskPaths returns the rewritten config's disk paths, in order.
func readDiskPaths(t *testing.T, dir string) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatalf("reading config.json: %v", err)
	}
	var cfg struct {
		Disks []struct {
			Path string `json:"path"`
		} `json:"disks"`
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		t.Fatalf("parsing rewritten config: %v", err)
	}
	var out []string
	for _, d := range cfg.Disks {
		out = append(out, d.Path)
	}
	return out
}

// fakeChain writes a manifest naming layers that exist, without building real
// images. Enough for the paths that only read the chain's shape.
func fakeChain(t *testing.T, actorUID string, layers ...string) {
	t.Helper()
	dir := durableQcow2Dir(actorUID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	m := qcow2.Manifest{VirtualSizeBytes: 1 << 30}
	for _, l := range layers {
		if err := os.WriteFile(filepath.Join(dir, l), []byte("\x51\x46\x49\xfb"), 0o600); err != nil {
			t.Fatal(err)
		}
		m.Layers = append(m.Layers, qcow2.Layer{File: l, SizeBytes: 4})
	}
	if err := qcow2.WriteManifest(durableQcow2ManifestPath(actorUID), m); err != nil {
		t.Fatal(err)
	}
}

// A snapshot's durable-dir disk points at the path it had on the node that took
// it, which does not exist here — the restore has to repoint it at the layer
// this node landed. Resuming against the old path is a guest that comes back
// with someone else's disk, or none.
func TestRewriteSnapshotDurableDisk(t *testing.T) {
	const id = "actor-uid"
	const rootfs = "/opt/kata/share/kata-containers/rootfs.img"

	t.Run("the durable disk is repointed and the rootfs is left alone", func(t *testing.T) {
		smallDurableDisk(t)
		fakeChain(t, id, "durable-dir.layer-0000.qcow2", "durable-dir.layer-0001.qcow2")
		dir := writeSnapshotConfigWithDisks(t, []map[string]any{
			{"path": rootfs, "readonly": true},
			{"path": "/var/lib/ateom-gvisor/actors/other-node-actor/durable-qcow2/durable-dir.layer-0003.qcow2"},
		})
		if err := rewriteSnapshotSocketPaths(dir, id); err != nil {
			t.Fatalf("rewriteSnapshotSocketPaths: %v", err)
		}
		top, _, err := durableQcow2Top(id)
		if err != nil {
			t.Fatal(err)
		}
		want := []string{rootfs, top}
		if got := readDiskPaths(t, dir); !slices.Equal(got, want) {
			t.Errorf("disk paths = %v, want %v", got, want)
		}
	})

	// The two arrangements have to match on both sides. A snapshot taken with a
	// disk, resumed against an actor whose data is a host directory, is a guest
	// that comes back mounting a device that is not attached — so it must fail
	// here, before the VM is created, rather than inside the guest.
	t.Run("a disk with no landed chain is an error", func(t *testing.T) {
		smallDurableDisk(t)
		dir := writeSnapshotConfigWithDisks(t, []map[string]any{
			{"path": rootfs, "readonly": true},
			{"path": "/var/lib/ateom-gvisor/actors/other/durable-qcow2/durable-dir.layer-0000.qcow2"},
		})
		if err := rewriteSnapshotSocketPaths(dir, id); err == nil {
			t.Fatal("rewriteSnapshotSocketPaths accepted a durable disk with no chain, want an error")
		}
	})

	// And the reverse: a chain was landed, but the snapshot's guest has no disk
	// to attach it to. Booting it would strand the restored data silently.
	t.Run("a landed chain with no disk is an error", func(t *testing.T) {
		smallDurableDisk(t)
		fakeChain(t, id, "durable-dir.layer-0000.qcow2")
		dir := writeSnapshotConfigWithDisks(t, []map[string]any{
			{"path": rootfs, "readonly": true},
		})
		if err := rewriteSnapshotSocketPaths(dir, id); err == nil {
			t.Fatal("rewriteSnapshotSocketPaths accepted a landed chain the snapshot has no disk for, want an error")
		}
	})

	t.Run("a snapshot with no disks at all is unchanged", func(t *testing.T) {
		smallDurableDisk(t)
		dir := writeSnapshotConfig(t, []map[string]any{
			{"tag": kata.FsTag, "socket": "/run/vc/vm/golden/virtiofsd.sock"},
		})
		if err := rewriteSnapshotSocketPaths(dir, id); err != nil {
			t.Errorf("rewriteSnapshotSocketPaths: %v", err)
		}
	})
}
