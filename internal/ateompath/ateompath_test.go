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

package ateompath

import (
	"strings"
	"testing"
)

func TestAteomPath(t *testing.T) {
	podUID := "123e4567-e89b-12d3-a456-426614174000"

	path := AteomPath(podUID)
	expectedSuffix := "/ateoms/" + podUID
	if !strings.HasSuffix(path, expectedSuffix) {
		t.Errorf("expected path to end with %s, got %s", expectedSuffix, path)
	}
}

func TestAteomSocketPathLimits(t *testing.T) {
	podUID := "123e4567-e89b-12d3-a456-426614174000"

	sockPath := AteomSocketPath(podUID)

	// Unix domain socket path limit is 107 bytes (108 with NUL terminator)
	const maxUnixSocketLen = 107
	if len(sockPath) > maxUnixSocketLen {
		t.Errorf("socket path length %d exceeds max allowed length %d: %q", len(sockPath), maxUnixSocketLen, sockPath)
	}

	// Verify it is deterministic
	sockPath2 := AteomSocketPath(podUID)
	if sockPath != sockPath2 {
		t.Errorf("expected deterministic socket paths, got %q and %q", sockPath, sockPath2)
	}
}

func TestAteletOTLPSocketPath(t *testing.T) {
	sockPath := AteletOTLPSocketPath()

	// Unix domain socket path limit is 107 bytes (108 with NUL terminator)
	const maxUnixSocketLen = 107
	if len(sockPath) > maxUnixSocketLen {
		t.Errorf("socket path length %d exceeds max allowed length %d: %q", len(sockPath), maxUnixSocketLen, sockPath)
	}

	// It must sit under BasePath: that is the host directory already mounted at
	// the same path into atelet and into every ateom pod, which is the whole
	// reason the relay needs no new volume.
	if !strings.HasPrefix(sockPath, BasePath+"/") {
		t.Errorf("AteletOTLPSocketPath() = %q, want it under %q so ateom and atelet see the same file", sockPath, BasePath)
	}

	// Node-scoped, so it must not collide with any per-pod ateom socket.
	if other := AteomSocketPath("123e4567-e89b-12d3-a456-426614174000"); sockPath == other {
		t.Errorf("AteletOTLPSocketPath() collides with AteomSocketPath: %q", sockPath)
	}
}

func TestAteomPathUniqueness(t *testing.T) {
	uid1 := "123e4567-e89b-12d3-a456-426614174000"
	uid2 := "987f6543-e21b-32d1-b654-246614174111"

	path1 := AteomPath(uid1)
	path2 := AteomPath(uid2)

	if path1 == path2 {
		t.Errorf("expected different paths for different pod UIDs, got %q", path1)
	}
}

func TestActorPathUsesUID(t *testing.T) {
	uid1 := "123e4567-e89b-12d3-a456-426614174000"
	uid2 := "987f6543-e21b-32d1-b654-246614174111"

	path1 := ActorPath(uid1)
	path2 := ActorPath(uid2)
	if path1 == path2 {
		t.Fatalf("different actor UIDs produced the same path %q", path1)
	}
	if want := "/actors/" + uid1; !strings.HasSuffix(path1, want) {
		t.Errorf("ActorPath(%q) = %q, want suffix %q", uid1, path1, want)
	}
}

func TestDurableDirGeneration(t *testing.T) {
	for _, tc := range []struct {
		name    string
		wantGen int
		wantOK  bool
	}{
		{DurableDirGenTarFile(1), 1, true},
		{DurableDirGenTarFile(42), 42, true},
		{DurableDirTarFile, 0, false},
		{DurableDirManifestFile, 0, false},
		// A generation is numbered from one, so a zero or negative name is not
		// one this package ever wrote and must not be mistaken for a chain link.
		{"durable-dir.g0.tar", 0, false},
		{"durable-dir.g-1.tar", 0, false},
		{"durable-dir.g.tar", 0, false},
		{"durable-dir.gx.tar", 0, false},
		{"durable-dir.g1.tar.tmp", 0, false},
		{"state.json", 0, false},
	} {
		gen, ok := DurableDirGeneration(tc.name)
		if gen != tc.wantGen || ok != tc.wantOK {
			t.Errorf("DurableDirGeneration(%q) = (%d, %v), want (%d, %v)", tc.name, gen, ok, tc.wantGen, tc.wantOK)
		}
	}
}

func TestIsDurableDirFile(t *testing.T) {
	// atelet narrows a full capture to its durable data with this predicate, so
	// a durable file it misses is one a DATA snapshot is silently missing.
	for _, name := range []string{DurableDirTarFile, DurableDirManifestFile, DurableDirGenTarFile(3)} {
		if !IsDurableDirFile(name) {
			t.Errorf("IsDurableDirFile(%q) = false, want true", name)
		}
	}
	for _, name := range []string{"state.json", "config.json", "memory-ranges", "base-id", "rootfs-upper.tar"} {
		if IsDurableDirFile(name) {
			t.Errorf("IsDurableDirFile(%q) = true, want false", name)
		}
	}
}
