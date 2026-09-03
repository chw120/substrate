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
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"slices"
	"sort"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// maxXattrBuf bounds the retry loop that sizes an xattr buffer, so a path whose
// attributes keep growing under us fails instead of looping forever.
const maxXattrBuf = 1 << 20

// inodeKey identifies an inode within one filesystem, so the paths that share
// one — a hardlink set — can be recognized while identical inode numbers on
// different devices are not conflated. It matches tarutil's own key.
type inodeKey struct {
	dev uint64
	ino uint64
}

// statOf returns the underlying stat, or ok=false where it is unavailable.
func statOf(info os.FileInfo) (*syscall.Stat_t, bool) {
	st, ok := info.Sys().(*syscall.Stat_t)
	return st, ok
}

// inodeOf returns info's inode identity and link count. ok is false where the
// platform does not supply them, in which case the caller treats the path as
// singly-linked.
func inodeOf(info os.FileInfo) (key inodeKey, nlink uint64, ok bool) {
	st, ok := statOf(info)
	if !ok {
		return inodeKey{}, 1, false
	}
	return inodeKey{dev: uint64(st.Dev), ino: uint64(st.Ino)}, uint64(st.Nlink), true
}

// ownerOf returns the on-disk uid and gid. os.FileInfo does not expose them and
// the workload writes under arbitrary ids, so reading them from stat is the
// only way the manifest can notice a chown.
func ownerOf(info os.FileInfo) (uid, gid int) {
	st, ok := statOf(info)
	if !ok {
		return 0, 0
	}
	return int(st.Uid), int(st.Gid)
}

// xattrDigest summarizes path's extended attributes as a hex SHA-256, or "" if
// it has none or the filesystem does not support them. Symlinks are not
// followed, so the digest belongs to the named path itself.
//
// Names are sorted and every name and value is length-prefixed, so no
// rearrangement or concatenation of attributes can collide with another set.
func xattrDigest(path string) (string, error) {
	names, err := listXattrs(path)
	if err != nil {
		return "", err
	}
	if len(names) == 0 {
		return "", nil
	}
	sort.Strings(names)

	h := sha256.New()
	for _, name := range names {
		value, err := getXattr(path, name)
		if err != nil {
			return "", err
		}
		fmt.Fprintf(h, "%d:%s:%d:", len(name), name, len(value))
		h.Write(value)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// devNumbersOf returns the major and minor numbers of a device node.
func devNumbersOf(info fs.FileInfo) (int64, int64, bool) {
	st, ok := statOf(info)
	if !ok {
		return 0, 0, false
	}
	dev := uint64(st.Rdev)
	return int64(unix.Major(dev)), int64(unix.Minor(dev)), true
}

// archivedXattr reports whether tarutil carries an attribute of this name. The
// scan records a digest of exactly these, so a change it reports is one a
// repack can actually reproduce (see Entry.XattrDigest).
func archivedXattr(name string) bool {
	return strings.HasPrefix(name, "user.") || strings.HasPrefix(name, "trusted.overlay.")
}

// listXattrs returns the names of the attributes of path that tarutil archives.
func listXattrs(path string) ([]string, error) {
	for size := 1024; size <= maxXattrBuf; size *= 2 {
		buf := make([]byte, size)
		n, err := unix.Llistxattr(path, buf)
		if errors.Is(err, unix.ERANGE) {
			continue
		}
		if err != nil {
			if unsupportedXattr(err) {
				return nil, nil
			}
			return nil, fmt.Errorf("listing xattrs of %q: %w", path, err)
		}
		return slices.DeleteFunc(splitNames(buf[:n]), func(name string) bool {
			return !archivedXattr(name)
		}), nil
	}
	return nil, fmt.Errorf("xattrs of %q exceed %d bytes of names", path, maxXattrBuf)
}

// getXattr reads one extended attribute's value.
func getXattr(path, name string) ([]byte, error) {
	for size := 1024; size <= maxXattrBuf; size *= 2 {
		buf := make([]byte, size)
		n, err := unix.Lgetxattr(path, name, buf)
		if errors.Is(err, unix.ERANGE) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("reading xattr %q of %q: %w", name, path, err)
		}
		return buf[:n], nil
	}
	return nil, fmt.Errorf("xattr %q of %q exceeds %d bytes", name, path, maxXattrBuf)
}

// unsupportedXattr reports whether err means the filesystem has no extended
// attributes rather than that something went wrong. A tmpfs or an overlay
// mounted without user_xattr answers this way, and a durable-dir volume on one
// is perfectly restorable — it simply has no attributes to record.
func unsupportedXattr(err error) bool {
	return errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.ENODATA) || errors.Is(err, unix.ENOSYS)
}

// splitNames splits llistxattr's NUL-terminated name list.
func splitNames(buf []byte) []string {
	var names []string
	for _, name := range bytes.Split(buf, []byte{0}) {
		if len(name) > 0 {
			names = append(names, string(name))
		}
	}
	return names
}
