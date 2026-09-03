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
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// scan walks srcDir and describes every path in it, returning the entries
// sorted by path with OriginGen left unset.
//
// The walk mirrors tarutil.writeTree exactly — same order, same treatment of
// sockets and device nodes — so what the manifest claims about a tree and what
// an archive of that tree contains cannot drift apart.
func scan(ctx context.Context, srcDir string) ([]Entry, error) {
	var entries []Entry
	// Hashing is per inode, not per path: the members of a hardlink set are the
	// same bytes and reading them once is the whole point of noticing the set.
	hashes := map[inodeKey]string{}
	// Paths sharing an inode, in walk order. The first is the one tarutil
	// archives with contents; the rest become tar hardlink entries.
	members := map[inodeKey][]string{}

	err := filepath.WalkDir(srcDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(srcDir, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}

		// Skipped for the reasons tarutil skips them: a socket carries no data,
		// cannot be represented in tar at all, and workloads leave them lying
		// around. Failing here would strand an actor that cannot be suspended.
		if info.Mode()&os.ModeSocket != 0 {
			slog.WarnContext(ctx, "Skipping socket while scanning directory",
				slog.String("path", path), slog.String("root", srcDir))
			return nil
		}

		name := filepath.ToSlash(rel)
		uid, gid := ownerOf(info)
		entry := Entry{
			Path:        name,
			Mode:        uint32(info.Mode() & (fs.ModePerm | fs.ModeSetuid | fs.ModeSetgid | fs.ModeSticky)),
			UID:         uid,
			GID:         gid,
			ModTimeNano: archivedModTime(info.ModTime()).UnixNano(),
		}

		digest, err := xattrDigest(path)
		if err != nil {
			return err
		}
		entry.XattrDigest = digest

		switch {
		case info.Mode().IsRegular():
			entry.Type = TypeRegular
			entry.Size = info.Size()
			key, nlink, ok := inodeOf(info)
			shared := ok && nlink > 1
			if shared {
				members[key] = append(members[key], name)
			}
			hash, cached := "", false
			if shared {
				hash, cached = hashes[key]
			}
			if !cached {
				if hash, err = hashFile(path); err != nil {
					return err
				}
				if shared {
					hashes[key] = hash
				}
			}
			entry.ContentHash = hash

		case d.IsDir():
			entry.Type = TypeDir

		case info.Mode()&os.ModeSymlink != 0:
			entry.Type = TypeSymlink
			if entry.Linkname, err = os.Readlink(path); err != nil {
				return fmt.Errorf("reading symlink %q: %w", path, err)
			}
			// A symlink's own times are not restored by tarutil (lchown is all
			// it applies), so recording them would report a change no restore
			// could reproduce.
			entry.ModTimeNano = 0

		case info.Mode()&os.ModeNamedPipe != 0:
			entry.Type = TypeFifo

		case info.Mode()&os.ModeDevice != 0:
			// Archived rather than rejected, because tarutil archives them and
			// the two walks must agree: a durable dir holding a whiteout would
			// otherwise suspend under a full capture and fail under an
			// incremental one.
			entry.Type = TypeBlock
			if info.Mode()&os.ModeCharDevice != 0 {
				entry.Type = TypeChar
			}
			major, minor, ok := devNumbersOf(info)
			if !ok {
				return fmt.Errorf("reading device numbers of %q", path)
			}
			entry.Devmajor, entry.Devminor = major, minor

		default:
			return fmt.Errorf("unsupported file type %v at %q", info.Mode().Type(), path)
		}

		entries = append(entries, entry)
		return nil
	})
	if err != nil {
		return nil, err
	}

	assignLinkSets(entries, members)
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	return entries, nil
}

// archivedModTime is the modification time a path will actually have after a
// round trip through tarutil: the nearest second.
//
// archive/tar rounds ModTime to a whole second unless the header names PAX
// explicitly, and tar.FileInfoHeader leaves the format unspecified — so this is
// what the durable-dir tar has always stored, incremental or not. The manifest
// records the same value rather than the filesystem's nanoseconds, because a
// manifest is a promise about what restore will produce: recording a precision
// the archive cannot carry would fail verification on every single file.
//
// The cost is that a change confined to the sub-second part of a timestamp goes
// unnoticed. Nothing is lost by that — repacking the file could not reproduce
// it either.
func archivedModTime(t time.Time) time.Time {
	return t.Round(time.Second)
}

// assignLinkSets labels the entries that share an inode with another path in
// the same tree. An inode with a high link count but only one path here is left
// unlabeled: its other links are outside the snapshot, and tarutil archives it
// as an ordinary file.
func assignLinkSets(entries []Entry, members map[inodeKey][]string) {
	setOf := map[string]string{}
	for _, paths := range members {
		if len(paths) < 2 {
			continue
		}
		for _, path := range paths {
			setOf[path] = paths[0]
		}
	}
	for i := range entries {
		entries[i].LinkSet = setOf[entries[i].Path]
	}
}

// hashFile returns the hex SHA-256 of path's contents.
//
// This read is not new work. A full capture already reads every byte to copy it
// into the tar; here the same read feeds the hash, and only the files that turn
// out to have changed are read a second time — by then from the page cache.
//
// SHA-256 is the standard library's, which keeps the package dependency-free.
// The proposal asks for at least 128 bits and suggests blake3 or xxh128; both
// would be faster per byte and neither is vendored today, so switching is a
// contained change if the hash ever shows up in a profile.
func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("opening %q: %w", path, err)
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("hashing %q: %w", path, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
