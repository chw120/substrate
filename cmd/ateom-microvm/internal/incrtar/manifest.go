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
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"sort"
	"time"
)

// ManifestVersion is the format version written into every manifest. Readers
// refuse a version they do not know rather than guessing at unfamiliar fields,
// because a misread manifest restores a subtly wrong tree instead of failing.
const ManifestVersion = 1

// EntryType is the kind of filesystem object an entry describes. Sockets are
// absent by design: they carry no data and tarutil skips them.
type EntryType string

const (
	// TypeDir is a directory. Directories live in the manifest alone — see
	// Entry.OriginGen.
	TypeDir EntryType = "dir"
	// TypeRegular is a regular file, the only kind with contents to hash.
	TypeRegular EntryType = "regular"
	// TypeSymlink is a symbolic link; its target is in Entry.Linkname.
	TypeSymlink EntryType = "symlink"
	// TypeFifo is a named pipe.
	TypeFifo EntryType = "fifo"
	// TypeChar is a character device and TypeBlock a block device. They are
	// described because tarutil archives them: an overlay upper records every
	// deleted lower-layer file as a 0:0 character-device whiteout, and dropping
	// one silently resurrects deleted content after a resume.
	TypeChar  EntryType = "char"
	TypeBlock EntryType = "block"
)

// Entry describes one path in a snapshot. Every field except OriginGen
// participates in change detection, so a file that is byte-identical but was
// chmod'ed, chown'ed, or re-timestamped still counts as changed (§6b of the
// proposal: tar has no way to express "same contents, new header", so such a
// file is repacked whole).
type Entry struct {
	// Path is slash-separated and relative to the snapshot root.
	Path string    `json:"path"`
	Type EntryType `json:"type"`
	// Size is the content length of a regular file and zero otherwise.
	Size int64 `json:"size,omitempty"`
	// Mode holds the permission bits plus setuid, setgid, and sticky — the
	// same set tarutil restores.
	Mode uint32 `json:"mode"`
	UID  int    `json:"uid"`
	GID  int    `json:"gid"`
	// ModTimeNano is the modification time in Unix nanoseconds, rounded to the
	// second that a tar archive can carry (see archivedModTime), or zero for a
	// symlink, whose own times tarutil does not restore.
	ModTimeNano int64 `json:"mtime_nano,omitempty"`
	// Linkname is a symlink's target, verbatim.
	Linkname string `json:"linkname,omitempty"`
	// LinkSet groups the paths that share one inode, naming the lexically
	// first of them; it is empty for a path with a single link. Change
	// detection promotes any change within a set to the whole set, so a set is
	// always archived together and its members' links survive the round trip
	// (§6a: tarutil's hardlink detection only reaches within one archive).
	LinkSet string `json:"link_set,omitempty"`
	// XattrDigest summarizes the extended attributes tarutil archives — user.*
	// and trusted.overlay.* — and is empty when there are none.
	//
	// It exists so a setxattr with no content change is still detected. It
	// covers only the archived namespaces on purpose, for the same reason
	// ModTimeNano holds a rounded time: the manifest is a promise about what
	// restore will produce, and recording a security.* attribute that no
	// archive can carry would report a change repacking could not reproduce.
	XattrDigest string `json:"xattr_digest,omitempty"`
	// ContentHash is the hex SHA-256 of a regular file's contents, empty for
	// every other type.
	ContentHash string `json:"content_hash,omitempty"`
	// Devmajor and Devminor are a device node's numbers, zero otherwise.
	Devmajor int64 `json:"devmajor,omitempty"`
	Devminor int64 `json:"devminor,omitempty"`
	// OriginGen is the generation whose tar carries this entry, and zero for a
	// directory, which restore materializes from the manifest instead. Making
	// generations start at 1 leaves zero free to mean "in no tar".
	OriginGen int `json:"origin_gen,omitempty"`
}

// Manifest is the complete state of one snapshot generation — complete, not a
// delta, even though the tar beside it holds only what changed.
//
// Keeping it whole is what makes the format cheap to reason about: a deleted
// path is simply one that stopped appearing, restore never walks the chain to
// learn the target state, and OriginGen says exactly which generations' tars
// have to be fetched.
type Manifest struct {
	Version int `json:"version"`
	// Generation numbers this snapshot. It is at least 1 (see Entry.OriginGen).
	Generation int `json:"generation"`
	// Entries is sorted by Path, so two manifests of the same tree serialize
	// identically.
	Entries []Entry `json:"entries"`
}

// ModTime returns the entry's modification time.
func (e Entry) ModTime() time.Time {
	return time.Unix(0, e.ModTimeNano)
}

// FileMode returns the entry's mode with the type bits Go expects, so it can be
// handed to os.Chmod and friends.
func (e Entry) FileMode() fs.FileMode {
	mode := fs.FileMode(e.Mode) & (fs.ModePerm | fs.ModeSetuid | fs.ModeSetgid | fs.ModeSticky)
	switch e.Type {
	case TypeDir:
		mode |= fs.ModeDir
	case TypeSymlink:
		mode |= fs.ModeSymlink
	case TypeFifo:
		mode |= fs.ModeNamedPipe
	case TypeChar:
		mode |= fs.ModeDevice | fs.ModeCharDevice
	case TypeBlock:
		mode |= fs.ModeDevice
	}
	return mode
}

// restoredMode is the mode an entry is restored with: the permission bits plus
// setuid, setgid, and sticky, and no type bits. It is the same set tarutil
// carries — dropping setgid in particular would break a data directory that
// relies on group inheritance, and only later, when a file lands in the wrong
// group.
func (e Entry) restoredMode() fs.FileMode {
	return e.FileMode() & (fs.ModePerm | fs.ModeSetuid | fs.ModeSetgid | fs.ModeSticky)
}

// sameAs reports whether e and prev describe an identical path, in which case e
// needs no new copy in this generation's tar. OriginGen is excluded: it records
// where the bytes came from, not what they are.
func (e Entry) sameAs(prev Entry) bool {
	e.OriginGen, prev.OriginGen = 0, 0
	return e == prev
}

// NeededGenerations returns the generations whose tars restore has to read, in
// ascending order. Directories contribute nothing: they are restored from the
// manifest, so a tree that only gained a chmod on a directory needs no download
// at all.
func (m *Manifest) NeededGenerations() []int {
	seen := map[int]bool{}
	for _, e := range m.Entries {
		if e.OriginGen > 0 {
			seen[e.OriginGen] = true
		}
	}
	gens := make([]int, 0, len(seen))
	for gen := range seen {
		gens = append(gens, gen)
	}
	sort.Ints(gens)
	return gens
}

// byPath indexes the entries for lookup during change detection.
func (m *Manifest) byPath() map[string]Entry {
	index := make(map[string]Entry, len(m.Entries))
	for _, e := range m.Entries {
		index[e.Path] = e
	}
	return index
}

// validate rejects a manifest that cannot be restored faithfully, so a corrupt
// or hand-edited one fails before anything is written to disk.
func (m *Manifest) validate() error {
	if m.Version != ManifestVersion {
		return fmt.Errorf("unsupported manifest version %d (want %d)", m.Version, ManifestVersion)
	}
	if m.Generation < 1 {
		return fmt.Errorf("manifest generation %d is not positive", m.Generation)
	}
	seen := make(map[string]bool, len(m.Entries))
	for _, e := range m.Entries {
		if e.Path == "" {
			return fmt.Errorf("manifest has an entry with an empty path")
		}
		if seen[e.Path] {
			return fmt.Errorf("manifest lists %q twice", e.Path)
		}
		seen[e.Path] = true
		if e.OriginGen > m.Generation {
			return fmt.Errorf("entry %q claims generation %d, past this manifest's %d", e.Path, e.OriginGen, m.Generation)
		}
		if (e.Type == TypeDir) != (e.OriginGen == 0) {
			return fmt.Errorf("entry %q of type %q has origin generation %d: directories and only directories carry zero", e.Path, e.Type, e.OriginGen)
		}
	}
	for _, e := range m.Entries {
		if e.LinkSet != "" && !seen[e.LinkSet] {
			return fmt.Errorf("entry %q names link set %q, which is not in the manifest", e.Path, e.LinkSet)
		}
	}
	return nil
}

// WriteManifest serializes m to path. The manifest is small — a path and a hash
// per file, single-digit megabytes at a hundred thousand files — so it is
// written whole rather than streamed.
func WriteManifest(path string, m *Manifest) error {
	if err := m.validate(); err != nil {
		return fmt.Errorf("refusing to write manifest %q: %w", path, err)
	}
	data, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("encoding manifest %q: %w", path, err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("writing manifest %q: %w", path, err)
	}
	return nil
}

// ReadManifest loads and validates the manifest at path.
func ReadManifest(path string) (*Manifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading manifest %q: %w", path, err)
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("decoding manifest %q: %w", path, err)
	}
	if err := m.validate(); err != nil {
		return nil, fmt.Errorf("manifest %q is unusable: %w", path, err)
	}
	return &m, nil
}
