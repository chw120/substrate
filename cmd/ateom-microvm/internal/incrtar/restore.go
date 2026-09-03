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
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"

	"github.com/agent-substrate/substrate/cmd/ateom-microvm/internal/tarutil"
)

// RestoreOptions configures the reconstruction of one snapshot.
type RestoreOptions struct {
	// DstDir is where the tree is rebuilt. It is created if absent and is
	// expected to be empty: anything already there that the manifest does not
	// describe makes the restore fail rather than survive into the actor's
	// durable directory.
	DstDir string
	// Manifest is the target state, in full.
	Manifest *Manifest
	// Tars maps a generation to the local path of its archive. It must cover
	// Manifest.NeededGenerations(); supplying more is harmless.
	Tars map[int]string
}

// Restore rebuilds opts.Manifest's tree under opts.DstDir from the generations
// in opts.Tars, then verifies the result against the manifest.
//
// Directories come from the manifest and files from the tars, each tar
// contributing only the paths the manifest attributes to it. Extracting the
// generations whole and letting later ones overwrite earlier ones would be
// simpler and much worse: every superseded file would be written and then
// thrown away, so a long chain would cost several times a full capture to
// restore and give back more than this scheme saves.
//
// Verification is not optional. A chain has more ways to be silently incomplete
// than a single archive does — one missing generation is enough — so every file
// is re-hashed against the manifest and any surplus or missing path is an
// error. Handing an actor a plausible-looking but wrong durable directory would
// corrupt its state invisibly.
func Restore(opts RestoreOptions) error {
	m := opts.Manifest
	if m == nil {
		return errors.New("restoring a snapshot needs a manifest")
	}
	if err := m.validate(); err != nil {
		return fmt.Errorf("refusing to restore from an unusable manifest: %w", err)
	}
	for _, gen := range m.NeededGenerations() {
		if opts.Tars[gen] == "" {
			return fmt.Errorf("restoring generation %d needs the archive of generation %d, which was not supplied: the chain is incomplete", m.Generation, gen)
		}
	}

	if err := os.MkdirAll(opts.DstDir, 0o755); err != nil {
		return fmt.Errorf("creating destination %q: %w", opts.DstDir, err)
	}
	root, err := os.OpenRoot(opts.DstDir)
	if err != nil {
		return fmt.Errorf("opening destination %q: %w", opts.DstDir, err)
	}
	defer root.Close()

	if err := createDirs(root, m); err != nil {
		return err
	}
	if err := extractGenerations(opts); err != nil {
		return err
	}
	if err := verifyTree(root, opts.DstDir, m); err != nil {
		return err
	}
	return restoreDirs(root, m)
}

// createDirs materializes the manifest's directories before any archive is
// read, since the archives carry no directory entries of their own.
//
// They are created owner-writable and -searchable whatever mode the manifest
// records, so files can be written into a directory the workload had made
// read-only; restoreDirs puts the recorded modes back at the end. Entries are
// sorted by path, which places a parent before its children.
func createDirs(root *os.Root, m *Manifest) error {
	for _, e := range m.Entries {
		if e.Type != TypeDir {
			continue
		}
		if err := root.Mkdir(e.Path, e.FileMode().Perm()|0o700); err != nil && !errors.Is(err, os.ErrExist) {
			return fmt.Errorf("creating directory %q: %w", e.Path, err)
		}
	}
	return nil
}

// extractGenerations unpacks each needed archive, taking from it only the paths
// the manifest says it owns.
func extractGenerations(opts RestoreOptions) error {
	owner := make(map[string]int, len(opts.Manifest.Entries))
	for _, e := range opts.Manifest.Entries {
		if e.OriginGen > 0 {
			owner[e.Path] = e.OriginGen
		}
	}
	for _, gen := range opts.Manifest.NeededGenerations() {
		keep := func(name string, _ *tar.Header) bool {
			return owner[filepath.ToSlash(name)] == gen
		}
		if err := tarutil.ExtractFiltered(opts.Tars[gen], opts.DstDir, keep); err != nil {
			return fmt.Errorf("while restoring generation %d: %w", gen, err)
		}
	}
	return nil
}

// restoreDirs applies the manifest's directory modes, ownership, and times,
// deepest first: a child's path is always longer than its parent's, so
// length-descending order finishes with a directory before its mode can make
// the ones inside it unreachable. Each is checked immediately after it is
// applied, while its ancestors are still searchable.
func restoreDirs(root *os.Root, m *Manifest) error {
	dirs := make([]Entry, 0, len(m.Entries))
	for _, e := range m.Entries {
		if e.Type == TypeDir {
			dirs = append(dirs, e)
		}
	}
	sort.Slice(dirs, func(i, j int) bool { return len(dirs[i].Path) > len(dirs[j].Path) })

	for _, e := range dirs {
		if err := applyMeta(root, e); err != nil {
			return err
		}
		info, err := root.Lstat(e.Path)
		if err != nil {
			return fmt.Errorf("re-reading restored directory %q: %w", e.Path, err)
		}
		if err := checkMeta(e, info); err != nil {
			return err
		}
	}
	return nil
}

// applyMeta restores one entry's ownership, mode, and modification time.
// Ownership goes first because a chown clears setuid and setgid, which the
// chmod then puts back.
func applyMeta(root *os.Root, e Entry) error {
	if err := lchown(root, e); err != nil {
		return err
	}
	if err := root.Chmod(e.Path, e.restoredMode()); err != nil {
		return fmt.Errorf("restoring mode on %q: %w", e.Path, err)
	}
	if e.ModTimeNano != 0 {
		if err := root.Chtimes(e.Path, e.ModTime(), e.ModTime()); err != nil {
			return fmt.Errorf("restoring times on %q: %w", e.Path, err)
		}
	}
	return nil
}

// lchown restores ownership without following symlinks, tolerating the refusal
// an unprivileged process gets. ateom runs as root in its worker pod, where
// ownership is restored faithfully and an EPERM means something real; tests and
// local tooling do not, and refusing to work there would make the package
// untestable outside a root context. This mirrors tarutil.
func lchown(root *os.Root, e Entry) error {
	err := root.Lchown(e.Path, e.UID, e.GID)
	if err == nil || (errors.Is(err, os.ErrPermission) && os.Geteuid() != 0) {
		return nil
	}
	return fmt.Errorf("restoring ownership of %q to %d:%d: %w", e.Path, e.UID, e.GID, err)
}

// verifyTree checks the restored tree against the manifest in one walk: every
// path it finds must be described, and every path described must be found.
// Directory modes and times are excluded because restoreDirs has not applied
// them yet — it checks its own work.
func verifyTree(root *os.Root, dstDir string, m *Manifest) error {
	want := m.byPath()
	found := make(map[string]bool, len(want))
	// Inode identity per hardlink set, so the members can be checked against
	// each other as they are encountered.
	setInode := map[string]inodeKey{}

	err := filepath.WalkDir(dstDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dstDir, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		name := filepath.ToSlash(rel)
		e, ok := want[name]
		if !ok {
			return fmt.Errorf("restored tree has %q, which the manifest does not describe", name)
		}
		found[name] = true

		info, err := d.Info()
		if err != nil {
			return err
		}
		if e.Type != TypeDir {
			if err := checkMeta(e, info); err != nil {
				return err
			}
		}
		if err := checkContents(root, e, info); err != nil {
			return err
		}
		return checkLinkSet(e, info, setInode)
	})
	if err != nil {
		return fmt.Errorf("verifying restored tree in %q: %w", dstDir, err)
	}

	for _, e := range m.Entries {
		if !found[e.Path] {
			return fmt.Errorf("verifying restored tree in %q: %q is missing; generation %d supplied nothing for it", dstDir, e.Path, e.OriginGen)
		}
	}
	return nil
}

// checkMeta compares an entry's recorded mode, ownership, and modification time
// with what is on disk. Ownership is only checked as root, for the reason
// lchown tolerates EPERM elsewhere.
func checkMeta(e Entry, info os.FileInfo) error {
	if got, want := info.Mode().Type(), e.FileMode().Type(); got != want {
		return fmt.Errorf("%q was restored as %v, not %v", e.Path, got, want)
	}
	// A symlink's mode and times belong to whatever it points at, and tarutil
	// restores neither, so there is nothing here to compare.
	if e.Type == TypeSymlink {
		return nil
	}
	if got, want := info.Mode()&(fs.ModePerm|fs.ModeSetuid|fs.ModeSetgid|fs.ModeSticky), e.restoredMode(); got != want {
		return fmt.Errorf("%q was restored with mode %v, not %v", e.Path, got, want)
	}
	if os.Geteuid() == 0 {
		if uid, gid := ownerOf(info); uid != e.UID || gid != e.GID {
			return fmt.Errorf("%q was restored owned by %d:%d, not %d:%d", e.Path, uid, gid, e.UID, e.GID)
		}
	}
	if got := info.ModTime().UnixNano(); got != e.ModTimeNano {
		return fmt.Errorf("%q was restored with modification time %d, not %d", e.Path, got, e.ModTimeNano)
	}
	// A whiteout that came back pointing at the wrong device would un-delete
	// exactly the content it exists to hide, and nothing else here would notice:
	// the mode and times of a 0:0 char device match any other char device.
	if e.Type == TypeChar || e.Type == TypeBlock {
		major, minor, ok := devNumbersOf(info)
		if !ok {
			return fmt.Errorf("cannot read the device numbers of restored %q", e.Path)
		}
		if major != e.Devmajor || minor != e.Devminor {
			return fmt.Errorf("%q was restored as device %d:%d, not %d:%d", e.Path, major, minor, e.Devmajor, e.Devminor)
		}
	}
	return nil
}

// checkContents re-reads a restored file and compares it with the manifest.
// This is the check that catches a missing generation in the chain, so it hashes
// the bytes rather than trusting the size.
func checkContents(root *os.Root, e Entry, info os.FileInfo) error {
	switch e.Type {
	case TypeRegular:
		if info.Size() != e.Size {
			return fmt.Errorf("%q was restored with %d bytes, not %d", e.Path, info.Size(), e.Size)
		}
		f, err := openForHashing(root, e)
		if err != nil {
			return fmt.Errorf("re-reading restored file %q: %w", e.Path, err)
		}
		defer f.Close()
		h := sha256.New()
		if _, err := io.Copy(h, f); err != nil {
			return fmt.Errorf("re-hashing restored file %q: %w", e.Path, err)
		}
		if got := hex.EncodeToString(h.Sum(nil)); got != e.ContentHash {
			return fmt.Errorf("%q was restored with contents hashing to %s, not %s", e.Path, got, e.ContentHash)
		}
	case TypeSymlink:
		target, err := root.Readlink(e.Path)
		if err != nil {
			return fmt.Errorf("re-reading restored symlink %q: %w", e.Path, err)
		}
		if target != e.Linkname {
			return fmt.Errorf("%q was restored pointing at %q, not %q", e.Path, target, e.Linkname)
		}
	}
	return nil
}

// openForHashing opens a restored file for reading, widening its mode if it has
// to.
//
// A workload may leave a file unreadable — 0o000 on a secret is not unusual —
// and the mode is already back in place by the time verification runs. ateom is
// root and reads it regardless; elsewhere, rather than skipping the check that
// catches a broken chain, the mode is widened just long enough to get a
// descriptor and then restored. Permission is settled at open, so the deferred
// chmod does not disturb the read that follows.
func openForHashing(root *os.Root, e Entry) (*os.File, error) {
	f, err := root.Open(e.Path)
	if err == nil || !errors.Is(err, os.ErrPermission) {
		return f, err
	}
	if chmodErr := root.Chmod(e.Path, 0o400); chmodErr != nil {
		return nil, err
	}
	defer func() { _ = root.Chmod(e.Path, e.restoredMode()) }()
	return root.Open(e.Path)
}

// checkLinkSet confirms that the paths the manifest grouped by inode really do
// share one after restore. Nothing else would notice the failure: the members
// have identical contents, so a set that came back as separate copies passes
// every other check while quietly doubling the space it occupies and breaking
// the workload's assumption that a write through one path is visible from all.
func checkLinkSet(e Entry, info os.FileInfo, setInode map[string]inodeKey) error {
	if e.LinkSet == "" {
		return nil
	}
	key, _, ok := inodeOf(info)
	if !ok {
		return nil
	}
	first, seen := setInode[e.LinkSet]
	if !seen {
		setInode[e.LinkSet] = key
		return nil
	}
	if first != key {
		return fmt.Errorf("%q and the rest of link set %q were restored as separate inodes", e.Path, e.LinkSet)
	}
	return nil
}
