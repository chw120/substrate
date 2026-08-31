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

package tarutil

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent-substrate/substrate/internal/roottest"
	"golang.org/x/sys/unix"
)

func TestWriteFormat(t *testing.T) {
	tests := []struct {
		env  string
		want Format
	}{
		{env: "", want: FormatTar},
		{env: "tar", want: FormatTar},
		{env: "erofs", want: FormatErofs},
		{env: "EROFS", want: FormatErofs},
		{env: "  erofs  ", want: FormatErofs},
		// Anything unrecognized falls back rather than failing: a typo in a
		// node's config must not take the node out, it must leave it on the
		// behavior it had before the setting existed.
		{env: "squashfs", want: FormatTar},
		{env: "ero fs", want: FormatTar},
	}
	for _, tc := range tests {
		t.Run("env="+tc.env, func(t *testing.T) {
			t.Setenv(FormatEnvVar, tc.env)
			if got := WriteFormat(); got != tc.want {
				t.Errorf("WriteFormat() with %s=%q = %q, want %q", FormatEnvVar, tc.env, got, tc.want)
			}
		})
	}
}

// erofsHeader returns the first 1028 bytes of a file that sniffs as an erofs
// image: zeroed boot sector, then the superblock magic.
func erofsHeader() []byte {
	b := make([]byte, erofsSuperOffset+len(erofsMagic))
	copy(b[erofsSuperOffset:], erofsMagic)
	return b
}

func TestPreflight(t *testing.T) {
	// The tar default must never depend on an external binary or on a mountable
	// kernel: a node that has not opted in has to start on a host with no
	// erofs-utils at all. This case is the one that runs everywhere, including
	// unprivileged and on the developer laptop.
	t.Run("tar needs nothing", func(t *testing.T) {
		t.Setenv(FormatEnvVar, "")
		t.Setenv("PATH", t.TempDir())
		if err := Preflight(t.Context()); err != nil {
			t.Errorf("Preflight() with the default format = %v, want nil", err)
		}
	})

	t.Run("erofs without the tools is refused", func(t *testing.T) {
		t.Setenv(FormatEnvVar, string(FormatErofs))
		t.Setenv("PATH", t.TempDir())
		err := Preflight(t.Context())
		if err == nil {
			t.Fatal("Preflight() = nil, want an error; the opt-in must not start on a node that cannot honor it")
		}
		// The operator reads this in a crash-looping pod's logs and has to be
		// able to act on it without reading the source.
		for _, want := range []string{FormatEnvVar, mkfsErofs, "erofs-utils", "check-erofs-support.sh"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("Preflight() error %q does not mention %q", err, want)
			}
		}
	})

	// The whole point of the round trip is that it passes only where an image
	// can really be built, mounted, and read back with its xattrs, so the
	// success case needs a host that can do all of that.
	t.Run("erofs round trip", func(t *testing.T) {
		requireErofs(t)
		t.Setenv(FormatEnvVar, string(FormatErofs))
		if err := Preflight(t.Context()); err != nil {
			t.Errorf("Preflight() = %v, want nil", err)
		}
	})
}

func TestSniff(t *testing.T) {
	dir := t.TempDir()

	// A real tar, written the way a checkpoint writes one.
	tarPath := filepath.Join(dir, "real.tar")
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "a.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatalf("writing fixture: %v", err)
	}
	if err := Create(t.Context(), tarPath, src); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// An empty tar: 1024 zero bytes, so it carries neither magic. It must read
	// as a tar, because Extract is what should report it.
	emptyTar := filepath.Join(dir, "empty.tar")
	if err := Create(t.Context(), emptyTar, t.TempDir()); err != nil {
		t.Fatalf("Create on empty dir: %v", err)
	}

	// A tar whose ARCHIVED CONTENT holds the erofs magic exactly where an
	// image's superblock would be: 512 bytes of header, then 512 bytes of file
	// data, puts byte 1024 at data offset 512. Without the tar magic taking
	// priority this restores as an image and the actor loses its data.
	decoy := t.TempDir()
	body := append(bytes.Repeat([]byte("x"), 512), erofsMagic...)
	if err := os.WriteFile(filepath.Join(decoy, "a.bin"), body, 0o644); err != nil {
		t.Fatalf("writing decoy fixture: %v", err)
	}
	decoyTar := filepath.Join(dir, "decoy.tar")
	if err := Create(t.Context(), decoyTar, decoy); err != nil {
		t.Fatalf("Create decoy: %v", err)
	}
	if b, err := os.ReadFile(decoyTar); err != nil {
		t.Fatalf("reading decoy tar: %v", err)
	} else if !bytes.Equal(b[erofsSuperOffset:erofsSuperOffset+len(erofsMagic)], erofsMagic) {
		t.Fatalf("decoy tar does not carry the erofs magic at %d; the test proves nothing", erofsSuperOffset)
	}

	image := filepath.Join(dir, "image.erofs")
	if err := os.WriteFile(image, erofsHeader(), 0o644); err != nil {
		t.Fatalf("writing image: %v", err)
	}

	// Shorter than the superblock offset: cannot be an image, so it is a tar's
	// problem.
	short := filepath.Join(dir, "short.bin")
	if err := os.WriteFile(short, []byte("nowhere near long enough"), 0o644); err != nil {
		t.Fatalf("writing short file: %v", err)
	}

	tests := []struct {
		name string
		path string
		want Format
	}{
		{name: "tar", path: tarPath, want: FormatTar},
		{name: "empty tar", path: emptyTar, want: FormatTar},
		{name: "tar carrying the erofs magic in its payload", path: decoyTar, want: FormatTar},
		{name: "erofs image", path: image, want: FormatErofs},
		{name: "too short for either", path: short, want: FormatTar},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Sniff(tc.path)
			if err != nil {
				t.Fatalf("Sniff(%q): %v", tc.path, err)
			}
			if got != tc.want {
				t.Errorf("Sniff(%q) = %q, want %q", tc.path, got, tc.want)
			}
		})
	}

	t.Run("missing file", func(t *testing.T) {
		if _, err := Sniff(filepath.Join(dir, "absent")); err == nil {
			t.Error("Sniff on a missing file returned no error")
		}
	})
}

// TestExtractRejectsImage covers the mixed-fleet direction that fails: a node
// handed an image by a node that writes them. It must say so, not extract
// nothing — archive/tar reads a missing trailer as a clean EOF, so the silent
// outcome would be an empty durable-dir reported as a successful restore.
func TestExtractRejectsImage(t *testing.T) {
	image := filepath.Join(t.TempDir(), "durable-dir.tar")
	if err := os.WriteFile(image, erofsHeader(), 0o644); err != nil {
		t.Fatalf("writing image: %v", err)
	}
	dst := t.TempDir()
	err := Extract(image, dst)
	if err == nil {
		t.Fatal("Extract accepted an erofs image")
	}
	if !strings.Contains(err.Error(), string(FormatErofs)) {
		t.Errorf("Extract error does not name the format: %v", err)
	}
}

// requireErofs skips unless this machine can both build and mount an image:
// root, mkfs.erofs and fsck.erofs on PATH, and an erofs-capable kernel.
//
// Under CI it fails instead of skipping. Every condition below is a property of
// the machine rather than of the change under test, so on a developer laptop a
// skip is the only useful answer — but in CI the same skip would leave the whole
// fidelity gate reporting green while checking nothing, and the conditions are
// things the workflow installs and can therefore stop installing by accident.
func requireErofs(t *testing.T) {
	t.Helper()
	// GitHub Actions sets CI=true, as does essentially every other runner.
	inCI := os.Getenv("CI") != ""
	unavailable := func(format string, args ...any) {
		t.Helper()
		if inCI {
			t.Fatalf(format+" (CI must not skip this: install erofs-utils in the workflow)", args...)
		}
		t.Skipf(format, args...)
	}

	roottest.Require(t, "building and mounting an erofs image requires root")
	for _, bin := range []string{mkfsErofs, fsckErofs} {
		if _, err := exec.LookPath(bin); err != nil {
			unavailable("needs %s on PATH: %v", bin, err)
		}
	}
	probe := filepath.Join(t.TempDir(), "probe.erofs")
	if err := CreateImage(t.Context(), probe, t.TempDir()); err != nil {
		unavailable("cannot build an erofs image here: %v", err)
	}
	mnt := t.TempDir()
	if err := MountImage(t.Context(), probe, mnt); err != nil {
		unavailable("cannot mount erofs here (CONFIG_EROFS_FS, loop devices): %v", err)
	}
	UnmountImage(mnt)
}

// fidelityTree builds a source tree holding one instance of every property the
// package doc promises the round trip preserves, and returns the modification
// time stamped on all of it.
func fidelityTree(t *testing.T) (dir string, mtime time.Time) {
	t.Helper()
	dir = t.TempDir()
	join := func(rel string) string { return filepath.Join(dir, rel) }

	if err := os.Mkdir(join("vol"), 0o2775); err != nil {
		t.Fatalf("mkdir vol: %v", err)
	}
	if err := os.Chmod(join("vol"), 0o2775); err != nil {
		t.Fatalf("chmod vol: %v", err)
	}
	if err := os.WriteFile(join("vol/file"), []byte("payload"), 0o640); err != nil {
		t.Fatalf("writing file: %v", err)
	}
	// Two names, one inode: erofs must keep them sharing rather than
	// duplicating, or an actor's data doubles across a suspend.
	if err := os.Link(join("vol/file"), join("vol/hardlink")); err != nil {
		t.Fatalf("hardlink: %v", err)
	}
	if err := os.Symlink("file", join("vol/symlink")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	if err := unix.Mkfifo(join("vol/fifo"), 0o600); err != nil {
		t.Fatalf("mkfifo: %v", err)
	}
	// A 0:0 char device is how overlayfs records a deleted file. Losing it
	// resurrects the file on the next resume, silently.
	if err := unix.Mknod(join("vol/whiteout"), unix.S_IFCHR|0o000, int(unix.Mkdev(0, 0))); err != nil {
		t.Fatalf("mknod whiteout: %v", err)
	}
	// A socket: skipped, like Create skips it. Agents leave these lying around
	// and mkfs.erofs refuses to archive one.
	ln, err := net.Listen("unix", join("vol/agent.sock"))
	if err != nil {
		t.Fatalf("creating socket: %v", err)
	}
	t.Cleanup(func() { ln.Close() })

	// Ownership above the 16-bit range a compact inode can hold. Sandboxed
	// workloads run under arbitrary uids, so this is not a corner case.
	for _, rel := range []string{"vol", "vol/file", "vol/fifo", "vol/whiteout"} {
		if err := os.Chown(join(rel), bigUID, bigGID); err != nil {
			t.Fatalf("chown %q: %v", rel, err)
		}
	}
	if err := unix.Lchown(join("vol/symlink"), bigUID, bigGID); err != nil {
		t.Fatalf("lchown symlink: %v", err)
	}
	if err := unix.Lsetxattr(join("vol"), "trusted.overlay.opaque", []byte("y"), 0); err != nil {
		t.Fatalf("setting trusted.overlay.opaque: %v", err)
	}
	if err := unix.Lsetxattr(join("vol/file"), "user.checkpoint", []byte("kept"), 0); err != nil {
		t.Fatalf("setting user.checkpoint: %v", err)
	}

	// Whole seconds, and distinctly not "now", so a build timestamp standing in
	// for the real mtime is unmistakable.
	mtime = time.Date(2001, 2, 3, 4, 5, 6, 0, time.UTC)
	for _, rel := range []string{"vol/file", "vol/hardlink", "vol/fifo", "vol/whiteout", "vol"} {
		if err := os.Chtimes(join(rel), mtime, mtime); err != nil {
			t.Fatalf("chtimes %q: %v", rel, err)
		}
	}
	return dir, mtime
}

// bigUID / bigGID exceed the 16 bits a compact erofs inode carries, which is
// what force-inode-extended exists to handle.
const (
	bigUID = 100000
	bigGID = 100001
)

// TestImageFidelity is the gate the erofs format has to clear before it is
// worth anything: an image that quietly drops mtimes or truncates uids is
// worse than a slower tar, because the actor resumes and looks fine.
func TestImageFidelity(t *testing.T) {
	requireErofs(t)
	src, mtime := fidelityTree(t)

	img := filepath.Join(t.TempDir(), "durable-dir.tar")
	if err := CreateImage(t.Context(), img, src); err != nil {
		t.Fatalf("CreateImage: %v", err)
	}
	if got, err := Sniff(img); err != nil || got != FormatErofs {
		t.Fatalf("Sniff(image) = %q, %v; want %q, nil", got, err, FormatErofs)
	}

	mnt := t.TempDir()
	if err := MountImage(t.Context(), img, mnt); err != nil {
		t.Fatalf("MountImage: %v", err)
	}
	t.Cleanup(func() { UnmountImage(mnt) })

	checkFidelity(t, mnt, mtime)
}

// TestExtractImageFidelity runs the same checks over ExtractImage's output.
//
// The fallback is only worth having if what it produces is the tree the actor
// had: it is reached exactly when a node cannot mount the image, so nothing
// downstream will notice a lossy unpack, and the loss would be of durable user
// data. Whether fsck.erofs restores ownership, mtimes, hardlinks and xattrs the
// way mounting does is a property of the tool, not something the code above can
// arrange, so it has to be measured rather than assumed.
func TestExtractImageFidelity(t *testing.T) {
	requireErofs(t)
	src, mtime := fidelityTree(t)

	img := filepath.Join(t.TempDir(), "durable-dir.tar")
	if err := CreateImage(t.Context(), img, src); err != nil {
		t.Fatalf("CreateImage: %v", err)
	}
	dst := t.TempDir()
	if err := ExtractImage(t.Context(), img, dst); err != nil {
		t.Fatalf("ExtractImage: %v", err)
	}
	checkFidelity(t, dst, mtime)
}

// checkFidelity asserts that root holds the tree fidelityTree built, whether it
// got there by being mounted or by being unpacked.
func checkFidelity(t *testing.T, mnt string, mtime time.Time) {
	t.Helper()

	statAt := func(rel string) *unix.Stat_t {
		t.Helper()
		var st unix.Stat_t
		if err := unix.Lstat(filepath.Join(mnt, rel), &st); err != nil {
			t.Fatalf("lstat %q in image: %v", rel, err)
		}
		return &st
	}

	t.Run("contents", func(t *testing.T) {
		got, err := os.ReadFile(filepath.Join(mnt, "vol/file"))
		if err != nil {
			t.Fatalf("reading vol/file: %v", err)
		}
		if string(got) != "payload" {
			t.Errorf("vol/file = %q, want %q", got, "payload")
		}
	})

	t.Run("mtime", func(t *testing.T) {
		for _, rel := range []string{"vol", "vol/file", "vol/fifo", "vol/whiteout"} {
			if got := time.Unix(statAt(rel).Mtim.Unix()).UTC(); !got.Equal(mtime) {
				t.Errorf("%s mtime = %s, want %s (a compact inode has no mtime field: check -E force-inode-extended)",
					rel, got, mtime)
			}
		}
	})

	t.Run("ownership", func(t *testing.T) {
		for _, rel := range []string{"vol", "vol/file", "vol/symlink", "vol/fifo", "vol/whiteout"} {
			st := statAt(rel)
			if st.Uid != bigUID || st.Gid != bigGID {
				t.Errorf("%s owner = %d:%d, want %d:%d (a compact inode holds 16-bit ids)",
					rel, st.Uid, st.Gid, bigUID, bigGID)
			}
		}
	})

	t.Run("modes", func(t *testing.T) {
		// Permission bits plus setgid. The setgid case is the one that bites:
		// the workload relies on group inheritance for the files its different
		// uids create, and losing the bit only surfaces later, when the next
		// file lands with the wrong group.
		for _, tc := range []struct {
			rel  string
			want uint32
		}{
			{"vol", 0o2775},
			{"vol/file", 0o640},
			{"vol/fifo", 0o600},
		} {
			if got := statAt(tc.rel).Mode & 0o7777; got != tc.want {
				t.Errorf("%s mode = %#o, want %#o", tc.rel, got, tc.want)
			}
		}
	})

	t.Run("hardlink", func(t *testing.T) {
		a, b := statAt("vol/file"), statAt("vol/hardlink")
		if a.Ino != b.Ino {
			t.Errorf("vol/file and vol/hardlink have distinct inodes (%d, %d): the link was duplicated", a.Ino, b.Ino)
		}
		if a.Nlink != 2 {
			t.Errorf("vol/file nlink = %d, want 2", a.Nlink)
		}
	})

	t.Run("symlink", func(t *testing.T) {
		got, err := os.Readlink(filepath.Join(mnt, "vol/symlink"))
		if err != nil {
			t.Fatalf("readlink: %v", err)
		}
		if got != "file" {
			t.Errorf("vol/symlink -> %q, want %q", got, "file")
		}
	})

	t.Run("fifo", func(t *testing.T) {
		if st := statAt("vol/fifo"); st.Mode&unix.S_IFMT != unix.S_IFIFO {
			t.Errorf("vol/fifo mode = %#o, want a FIFO", st.Mode)
		}
	})

	t.Run("whiteout device", func(t *testing.T) {
		st := statAt("vol/whiteout")
		if st.Mode&unix.S_IFMT != unix.S_IFCHR {
			t.Errorf("vol/whiteout mode = %#o, want a character device", st.Mode)
		}
		if major, minor := unix.Major(uint64(st.Rdev)), unix.Minor(uint64(st.Rdev)); major != 0 || minor != 0 {
			t.Errorf("vol/whiteout device = %d:%d, want 0:0", major, minor)
		}
	})

	t.Run("xattrs", func(t *testing.T) {
		for _, tc := range []struct{ rel, attr, want string }{
			{"vol", "trusted.overlay.opaque", "y"},
			{"vol/file", "user.checkpoint", "kept"},
		} {
			got, err := getxattr(filepath.Join(mnt, tc.rel), tc.attr)
			if err != nil {
				t.Errorf("reading %s of %s: %v (CONFIG_EROFS_FS_XATTR?)", tc.attr, tc.rel, err)
				continue
			}
			if got != tc.want {
				t.Errorf("%s of %s = %q, want %q", tc.attr, tc.rel, got, tc.want)
			}
		}
	})

	t.Run("socket skipped", func(t *testing.T) {
		// The point is that CreateImage SUCCEEDED above with a socket in the
		// tree; that it is absent from the image just confirms how.
		if _, err := os.Lstat(filepath.Join(mnt, "vol/agent.sock")); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("vol/agent.sock is present in the image (err = %v); Create skips sockets and so must CreateImage", err)
		}
	})
}

// getxattr reads one extended attribute as a string.
func getxattr(path, attr string) (string, error) {
	sz, err := unix.Lgetxattr(path, attr, nil)
	if err != nil {
		return "", err
	}
	buf := make([]byte, sz)
	if sz, err = unix.Lgetxattr(path, attr, buf); err != nil {
		return "", err
	}
	return string(buf[:sz]), nil
}

// TestCreateImageOverwrites covers the re-suspend case: mkfs.erofs appends to
// an existing file rather than truncating it, so a second checkpoint over the
// first one's image would produce something unmountable.
func TestCreateImageOverwrites(t *testing.T) {
	requireErofs(t)
	img := filepath.Join(t.TempDir(), "durable-dir.tar")

	first := t.TempDir()
	if err := os.WriteFile(filepath.Join(first, "big"), bytes.Repeat([]byte("x"), 1<<20), 0o644); err != nil {
		t.Fatalf("writing first fixture: %v", err)
	}
	if err := CreateImage(t.Context(), img, first); err != nil {
		t.Fatalf("first CreateImage: %v", err)
	}

	second := t.TempDir()
	if err := os.WriteFile(filepath.Join(second, "small"), []byte("2"), 0o644); err != nil {
		t.Fatalf("writing second fixture: %v", err)
	}
	if err := CreateImage(t.Context(), img, second); err != nil {
		t.Fatalf("second CreateImage: %v", err)
	}

	mnt := t.TempDir()
	if err := MountImage(t.Context(), img, mnt); err != nil {
		t.Fatalf("MountImage after overwrite: %v", err)
	}
	t.Cleanup(func() { UnmountImage(mnt) })
	if _, err := os.Lstat(filepath.Join(mnt, "big")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the first image's contents survived the second CreateImage (err = %v)", err)
	}
	if _, err := os.Lstat(filepath.Join(mnt, "small")); err != nil {
		t.Errorf("the second image's contents are missing: %v", err)
	}
}

// TestUnmountImageReleasesLoopDevice checks the property the mount -o loop
// choice buys: umount releases the loop device, so there is no orphan for
// anyone to sweep. Without LO_FLAGS_AUTOCLEAR every suspend/resume cycle would
// strand one and the node would run out.
func TestUnmountImageReleasesLoopDevice(t *testing.T) {
	requireErofs(t)
	img := filepath.Join(t.TempDir(), "durable-dir.tar")
	if err := CreateImage(t.Context(), img, t.TempDir()); err != nil {
		t.Fatalf("CreateImage: %v", err)
	}
	mnt := t.TempDir()
	if err := MountImage(t.Context(), img, mnt); err != nil {
		t.Fatalf("MountImage: %v", err)
	}
	UnmountImage(mnt)
	if attached, err := loopDeviceFor(img); err != nil {
		t.Skipf("cannot enumerate loop devices: %v", err)
	} else if attached != "" {
		t.Errorf("loop device %s is still backed by %s after umount", attached, img)
	}
}

// loopDeviceFor returns the loop device currently backing path, or "".
func loopDeviceFor(path string) (string, error) {
	out, err := exec.Command("losetup", "--noheadings", "--output", "NAME,BACK-FILE", "--list").Output()
	if err != nil {
		return "", fmt.Errorf("listing loop devices: %w", err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[1] == path {
			return fields[0], nil
		}
	}
	return "", nil
}
