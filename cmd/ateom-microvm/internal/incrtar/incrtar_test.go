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
	"archive/tar"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"

	"github.com/agent-substrate/substrate/cmd/ateom-microvm/internal/tarutil"
	"github.com/agent-substrate/substrate/internal/roottest"
	"golang.org/x/sys/unix"
)

// chain drives a sequence of generations over one source directory, keeping the
// tars where Restore can find them. Tests mutate the source between snapshots.
type chain struct {
	t       *testing.T
	src     string
	tarDir  string
	tars    map[int]string
	gen     int
	current *Manifest
}

// newChain starts a chain over a fresh source directory.
func newChain(t *testing.T) *chain {
	t.Helper()
	return &chain{t: t, src: t.TempDir(), tarDir: t.TempDir(), tars: map[int]string{}}
}

// snap takes the next generation and returns what Create reported.
func (c *chain) snap() *CreateResult {
	c.t.Helper()
	c.gen++
	tarPath := filepath.Join(c.tarDir, "durable-"+string(rune('0'+c.gen))+".tar")
	res, err := Create(c.t.Context(), CreateOptions{
		SrcDir:     c.src,
		TarPath:    tarPath,
		Generation: c.gen,
		Previous:   c.current,
	})
	if err != nil {
		c.t.Fatalf("Create generation %d: %v", c.gen, err)
	}
	c.tars[c.gen] = tarPath
	c.current = res.Manifest
	return res
}

// restore rebuilds the latest generation into a new directory and returns it.
func (c *chain) restore() string {
	c.t.Helper()
	dst := c.t.TempDir()
	if err := Restore(RestoreOptions{DstDir: dst, Manifest: c.current, Tars: c.tars}); err != nil {
		c.t.Fatalf("Restore generation %d: %v", c.gen, err)
	}
	return dst
}

// write creates or replaces a file, creating parents as needed.
func (c *chain) write(rel, contents string, mode os.FileMode) {
	c.t.Helper()
	path := filepath.Join(c.src, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		c.t.Fatalf("creating parent of %q: %v", rel, err)
	}
	if err := os.WriteFile(path, []byte(contents), mode); err != nil {
		c.t.Fatalf("writing %q: %v", rel, err)
	}
	if err := os.Chmod(path, mode); err != nil {
		c.t.Fatalf("setting mode of %q: %v", rel, err)
	}
}

// rewrite replaces a file's contents while putting its modification time back,
// so what follows can only have noticed the change by reading the bytes. Every
// cycle starts from a fresh extraction, which is why the design refuses to
// trust timestamps at all; the tests hold it to that.
func (c *chain) rewrite(rel, contents string) {
	c.t.Helper()
	path := filepath.Join(c.src, filepath.FromSlash(rel))
	before, err := os.Stat(path)
	if err != nil {
		c.t.Fatalf("stat %q: %v", rel, err)
	}
	if err := os.WriteFile(path, []byte(contents), before.Mode().Perm()); err != nil {
		c.t.Fatalf("rewriting %q: %v", rel, err)
	}
	if err := os.Chtimes(path, before.ModTime(), before.ModTime()); err != nil {
		c.t.Fatalf("restoring times of %q: %v", rel, err)
	}
}

// path is the absolute location of rel in the source tree.
func (c *chain) path(rel string) string {
	return filepath.Join(c.src, filepath.FromSlash(rel))
}

// populate lays down a small tree with a subdirectory, used by most tests.
func (c *chain) populate() {
	c.t.Helper()
	c.write("a.txt", "alpha", 0o644)
	c.write("b.txt", "bravo", 0o600)
	c.write("sub/c.txt", "charlie", 0o644)
}

// tarNames lists an archive's entry names in the order they appear.
func tarNames(t *testing.T, tarPath string) []string {
	t.Helper()
	f, err := os.Open(tarPath)
	if err != nil {
		t.Fatalf("opening tar: %v", err)
	}
	defer f.Close()

	var names []string
	tr := tar.NewReader(f)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return names
		}
		if err != nil {
			t.Fatalf("reading tar: %v", err)
		}
		names = append(names, hdr.Name)
	}
}

// sameTree fails unless two directories are indistinguishable in everything the
// manifest records: contents, mode, ownership, times, link targets, extended
// attributes, and which paths share an inode.
func sameTree(t *testing.T, want, got string) {
	t.Helper()
	wantEntries, err := scan(t.Context(), want)
	if err != nil {
		t.Fatalf("scanning %q: %v", want, err)
	}
	gotEntries, err := scan(t.Context(), got)
	if err != nil {
		t.Fatalf("scanning %q: %v", got, err)
	}
	if len(wantEntries) != len(gotEntries) {
		t.Fatalf("restored tree has %d entries, want %d", len(gotEntries), len(wantEntries))
	}
	for i := range wantEntries {
		if !reflect.DeepEqual(wantEntries[i], gotEntries[i]) {
			t.Errorf("entry %d differs:\n got %+v\nwant %+v", i, gotEntries[i], wantEntries[i])
		}
	}
}

// inodeAt returns the inode identity of a path.
func inodeAt(t *testing.T, path string) inodeKey {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("stat %q: %v", path, err)
	}
	key, _, ok := inodeOf(info)
	if !ok {
		t.Fatalf("no inode for %q", path)
	}
	return key
}

func TestUnchangedTreePacksNothing(t *testing.T) {
	c := newChain(t)
	c.populate()
	c.snap()

	second := c.snap()
	if second.Packed != 0 || second.PackedBytes != 0 {
		t.Errorf("second generation packed %d entries / %d bytes, want nothing", second.Packed, second.PackedBytes)
	}
	if names := tarNames(t, c.tars[2]); len(names) != 0 {
		t.Errorf("second generation's tar holds %v, want an empty archive", names)
	}
	if gens := second.Manifest.NeededGenerations(); !reflect.DeepEqual(gens, []int{1}) {
		t.Errorf("needed generations = %v, want [1]", gens)
	}
	sameTree(t, c.src, c.restore())
}

func TestOnlyTheChangedFileIsPacked(t *testing.T) {
	c := newChain(t)
	c.populate()
	c.snap()

	c.rewrite("b.txt", "bravissimo")
	second := c.snap()

	if second.Packed != 1 {
		t.Errorf("packed %d entries, want 1", second.Packed)
	}
	if names := tarNames(t, c.tars[2]); !reflect.DeepEqual(names, []string{"b.txt"}) {
		t.Errorf("second generation's tar holds %v, want [b.txt]", names)
	}
	sameTree(t, c.src, c.restore())
}

func TestDeletedFileDoesNotComeBack(t *testing.T) {
	c := newChain(t)
	c.populate()
	c.snap()

	if err := os.Remove(c.path("b.txt")); err != nil {
		t.Fatalf("removing b.txt: %v", err)
	}
	second := c.snap()

	for _, e := range second.Manifest.Entries {
		if e.Path == "b.txt" {
			t.Fatal("manifest still lists b.txt after it was deleted")
		}
	}
	dst := c.restore()
	if _, err := os.Lstat(filepath.Join(dst, "b.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("stat of deleted b.txt = %v, want it to not exist", err)
	}
	sameTree(t, c.src, dst)
}

func TestHardlinkSetIsRepackedWhole(t *testing.T) {
	c := newChain(t)
	c.populate()
	if err := os.Link(c.path("a.txt"), c.path("sub/link-to-a.txt")); err != nil {
		t.Fatalf("creating hardlink: %v", err)
	}
	first := c.snap()

	for _, e := range first.Manifest.Entries {
		if e.Path == "a.txt" || e.Path == "sub/link-to-a.txt" {
			if e.LinkSet != "a.txt" {
				t.Errorf("%q is in link set %q, want a.txt", e.Path, e.LinkSet)
			}
		}
	}

	// Written through one member; both paths are the same inode, so both
	// changed and both have to be in the same archive for the link to survive.
	c.rewrite("a.txt", "alpha prime")
	if second := c.snap(); second.Packed != 2 {
		t.Errorf("packed %d entries, want both members of the link set", second.Packed)
	}

	names := tarNames(t, c.tars[2])
	if !reflect.DeepEqual(names, []string{"a.txt", "sub/link-to-a.txt"}) {
		t.Errorf("second generation's tar holds %v, want both members of the link set", names)
	}

	dst := c.restore()
	if got, want := inodeAt(t, filepath.Join(dst, "sub/link-to-a.txt")), inodeAt(t, filepath.Join(dst, "a.txt")); got != want {
		t.Errorf("restored link members have inodes %+v and %+v, want one inode", got, want)
	}
	sameTree(t, c.src, dst)
}

func TestChmodAloneIsDetected(t *testing.T) {
	c := newChain(t)
	c.populate()
	c.snap()

	// chmod moves ctime, never mtime, and leaves the contents alone: nothing a
	// content hash or a timestamp would catch.
	if err := os.Chmod(c.path("a.txt"), 0o400); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	if second := c.snap(); second.Packed != 1 {
		t.Errorf("packed %d entries, want 1", second.Packed)
	}

	if names := tarNames(t, c.tars[2]); !reflect.DeepEqual(names, []string{"a.txt"}) {
		t.Errorf("second generation's tar holds %v, want [a.txt]", names)
	}
	dst := c.restore()
	info, err := os.Stat(filepath.Join(dst, "a.txt"))
	if err != nil {
		t.Fatalf("stat restored a.txt: %v", err)
	}
	if info.Mode().Perm() != 0o400 {
		t.Errorf("restored mode = %v, want 0400", info.Mode().Perm())
	}
	sameTree(t, c.src, dst)
}

func TestThreeGenerationChainMatchesAFullCapture(t *testing.T) {
	c := newChain(t)
	c.populate()
	c.snap()

	c.rewrite("b.txt", "bravo two")
	c.write("sub/d.txt", "delta", 0o644)
	c.snap()

	c.rewrite("b.txt", "bravo three")
	if err := os.Remove(c.path("sub/c.txt")); err != nil {
		t.Fatalf("removing sub/c.txt: %v", err)
	}
	third := c.snap()

	// b.txt is in all three archives. The filter has to take it from the third
	// and ignore the earlier copies, or restoring a long chain would cost more
	// writes than the full capture it replaces.
	for gen := 1; gen <= 3; gen++ {
		names := tarNames(t, c.tars[gen])
		var found bool
		for _, n := range names {
			if n == "b.txt" {
				found = true
			}
		}
		if !found {
			t.Fatalf("generation %d's tar %v does not hold b.txt, so this test is not exercising the filter", gen, names)
		}
	}
	if gens := third.Manifest.NeededGenerations(); !reflect.DeepEqual(gens, []int{1, 2, 3}) {
		t.Errorf("needed generations = %v, want [1 2 3]", gens)
	}

	// The yardstick: what a plain full capture of the final tree restores to.
	fullTar := filepath.Join(t.TempDir(), "full.tar")
	if err := tarutil.Create(t.Context(), fullTar, c.src); err != nil {
		t.Fatalf("full Create: %v", err)
	}
	full := t.TempDir()
	if err := tarutil.Extract(fullTar, full); err != nil {
		t.Fatalf("full Extract: %v", err)
	}
	sameTree(t, full, c.restore())
}

func TestNoPreviousManifestCapturesEverything(t *testing.T) {
	c := newChain(t)
	c.populate()
	first := c.snap()

	if first.Packed != 3 {
		t.Errorf("packed %d entries, want all 3 files", first.Packed)
	}
	if names := tarNames(t, c.tars[1]); !reflect.DeepEqual(names, []string{"a.txt", "b.txt", "sub/c.txt"}) {
		t.Errorf("first generation's tar holds %v, want every file", names)
	}
	// Directories are the manifest's business, not the archive's.
	for _, e := range first.Manifest.Entries {
		if e.Type == TypeDir && e.OriginGen != 0 {
			t.Errorf("directory %q claims generation %d, want 0", e.Path, e.OriginGen)
		}
	}
	sameTree(t, c.src, c.restore())
}

func TestRestoreRefusesAnIncompleteChain(t *testing.T) {
	c := newChain(t)
	c.populate()
	c.snap()
	c.rewrite("b.txt", "bravo two")
	c.snap()

	err := Restore(RestoreOptions{
		DstDir:   t.TempDir(),
		Manifest: c.current,
		Tars:     map[int]string{2: c.tars[2]},
	})
	if err == nil {
		t.Fatal("Restore succeeded without generation 1, want an error")
	}
	if !strings.Contains(err.Error(), "chain is incomplete") {
		t.Errorf("error = %v, want it to say the chain is incomplete", err)
	}
}

func TestRestoreRejectsAnUndescribedPath(t *testing.T) {
	c := newChain(t)
	c.populate()
	c.snap()

	dst := t.TempDir()
	if err := os.WriteFile(filepath.Join(dst, "stowaway.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("planting a file: %v", err)
	}
	err := Restore(RestoreOptions{DstDir: dst, Manifest: c.current, Tars: c.tars})
	if err == nil {
		t.Fatal("Restore succeeded over a stray file, want an error")
	}
	if !strings.Contains(err.Error(), "stowaway.txt") {
		t.Errorf("error = %v, want it to name the undescribed path", err)
	}
}

func TestRestoreDetectsATamperedArchive(t *testing.T) {
	c := newChain(t)
	c.populate()
	c.snap()

	// Stand in for a truncated or wrong-generation object: an archive whose
	// bytes no longer match what the manifest promises.
	swapped := filepath.Join(t.TempDir(), "swapped.tar")
	other := t.TempDir()
	if err := os.WriteFile(filepath.Join(other, "a.txt"), []byte("not alpha"), 0o644); err != nil {
		t.Fatalf("writing decoy: %v", err)
	}
	if err := tarutil.Create(t.Context(), swapped, other); err != nil {
		t.Fatalf("creating decoy tar: %v", err)
	}

	err := Restore(RestoreOptions{DstDir: t.TempDir(), Manifest: c.current, Tars: map[int]string{1: swapped}})
	if err == nil {
		t.Fatal("Restore succeeded from a mismatched archive, want an error")
	}
	if !strings.Contains(err.Error(), "a.txt") {
		t.Errorf("error = %v, want it to name the path whose contents do not match", err)
	}
}

func TestFidelityOfSpecialFiles(t *testing.T) {
	c := newChain(t)
	c.populate()

	if err := os.Symlink("a.txt", c.path("link")); err != nil {
		t.Fatalf("creating symlink: %v", err)
	}
	if err := os.Symlink("/nowhere", c.path("dangling")); err != nil {
		t.Fatalf("creating dangling symlink: %v", err)
	}
	if err := unix.Mkfifo(c.path("pipe"), 0o644); err != nil {
		t.Fatalf("creating fifo: %v", err)
	}
	// A mostly-hole file. Whether the filesystem actually punches holes is not
	// this test's business — the contents have to survive either way.
	sparse, err := os.Create(c.path("sparse.bin"))
	if err != nil {
		t.Fatalf("creating sparse file: %v", err)
	}
	if err := sparse.Truncate(1 << 20); err != nil {
		t.Fatalf("truncating sparse file: %v", err)
	}
	if _, err := sparse.WriteAt([]byte("end"), (1<<20)-3); err != nil {
		t.Fatalf("writing sparse tail: %v", err)
	}
	if err := sparse.Close(); err != nil {
		t.Fatalf("closing sparse file: %v", err)
	}
	// Sticky, setgid and setuid, which a plain Perm() comparison would drop.
	if err := os.Chmod(c.path("sub"), 0o2775); err != nil {
		t.Fatalf("setting setgid on sub: %v", err)
	}
	if err := os.Chmod(c.path("b.txt"), 0o4600); err != nil {
		t.Fatalf("setting setuid on b.txt: %v", err)
	}

	c.snap()
	sameTree(t, c.src, c.restore())

	// And again across a generation boundary, where only one file moved.
	c.rewrite("a.txt", "alpha prime")
	second := c.snap()
	if second.Packed != 1 {
		t.Errorf("packed %d entries, want only the rewritten file", second.Packed)
	}
	sameTree(t, c.src, c.restore())
}

func TestXattrChangeIsDetected(t *testing.T) {
	c := newChain(t)
	c.populate()
	if err := unix.Lsetxattr(c.path("a.txt"), "user.substrate", []byte("one"), 0); err != nil {
		t.Skipf("filesystem does not support user xattrs: %v", err)
	}
	c.snap()

	if err := unix.Lsetxattr(c.path("a.txt"), "user.substrate", []byte("two"), 0); err != nil {
		t.Fatalf("changing xattr: %v", err)
	}
	second := c.snap()

	// The attribute changed and nothing else did, so the file is repacked. Note
	// what is NOT asserted: that the attribute survives. tarutil does not carry
	// xattrs, so it does not — see Entry.XattrDigest.
	if names := tarNames(t, c.tars[2]); !reflect.DeepEqual(names, []string{"a.txt"}) {
		t.Errorf("second generation's tar holds %v, want [a.txt]", names)
	}
	if second.Packed != 1 {
		t.Errorf("packed %d entries, want 1", second.Packed)
	}
}

func TestDeviceNodeSurvivesAGeneration(t *testing.T) {
	roottest.Require(t, "creating a device node requires root")

	c := newChain(t)
	c.populate()
	// A 0:0 character device is what the host kernel's overlayfs writes to
	// record a deleted lower-layer file. tarutil archives it, so incrtar has to
	// describe and restore it: losing one resurrects the deleted file.
	if err := unix.Mknod(c.path("whiteout"), unix.S_IFCHR|0o600, int(unix.Mkdev(0, 0))); err != nil {
		t.Fatalf("creating whiteout device node: %v", err)
	}
	first := c.snap()

	var got Entry
	for _, e := range first.Manifest.Entries {
		if e.Path == "whiteout" {
			got = e
		}
	}
	if got.Type != TypeChar {
		t.Fatalf("whiteout described as type %q, want %q", got.Type, TypeChar)
	}
	if got.Devmajor != 0 || got.Devminor != 0 {
		t.Errorf("whiteout described as device %d:%d, want 0:0", got.Devmajor, got.Devminor)
	}

	// Restore verifies the device numbers itself, so a clean restore is the
	// assertion. A second generation then has to leave it alone.
	c.restore()
	if second := c.snap(); second.Packed != 0 {
		t.Errorf("an untouched tree repacked %d entries, want none", second.Packed)
	}
	c.restore()
}

func TestOwnershipSurvivesAGeneration(t *testing.T) {
	roottest.Require(t, "restoring file ownership requires CAP_CHOWN")

	const uid, gid = 1234, 5678
	c := newChain(t)
	c.populate()
	if err := os.Chown(c.path("a.txt"), uid, gid); err != nil {
		t.Fatalf("chown: %v", err)
	}
	c.snap()

	// A chown with no content change: caught by the manifest's uid/gid, not by
	// the hash.
	if err := os.Chown(c.path("b.txt"), uid, gid); err != nil {
		t.Fatalf("chown: %v", err)
	}
	c.snap()
	if names := tarNames(t, c.tars[2]); !reflect.DeepEqual(names, []string{"b.txt"}) {
		t.Errorf("second generation's tar holds %v, want [b.txt]", names)
	}

	dst := c.restore()
	info, err := os.Stat(filepath.Join(dst, "b.txt"))
	if err != nil {
		t.Fatalf("stat restored b.txt: %v", err)
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatal("stat has no syscall.Stat_t")
	}
	if st.Uid != uid || st.Gid != gid {
		t.Errorf("restored ownership = %d:%d, want %d:%d", st.Uid, st.Gid, uid, gid)
	}
	sameTree(t, c.src, dst)
}

func TestUnreadableFileIsStillVerified(t *testing.T) {
	c := newChain(t)
	c.populate()
	if err := os.Chmod(c.path("b.txt"), 0o000); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	c.snap()

	dst := c.restore()
	info, err := os.Stat(filepath.Join(dst, "b.txt"))
	if err != nil {
		t.Fatalf("stat restored b.txt: %v", err)
	}
	if info.Mode().Perm() != 0 {
		t.Errorf("restored mode = %v, want 0000: verification must put the mode back", info.Mode().Perm())
	}
}
