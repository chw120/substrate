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
	"debug/elf"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

// The bytes have to be a loadable executable for the architecture they claim,
// and there is no build step that would have caught it if they were not. Parse
// what was emitted rather than comparing it to a golden blob: a golden blob
// tests that the code did not change, this tests that it is right.
func TestBuildSyncELF(t *testing.T) {
	for goarch, want := range map[string]elf.Machine{"amd64": elf.EM_X86_64, "arm64": elf.EM_AARCH64} {
		t.Run(goarch, func(t *testing.T) {
			b, err := buildSyncELF(goarch)
			if err != nil {
				t.Fatalf("buildSyncELF(%q) = %v", goarch, err)
			}
			f, err := elf.NewFile(readerAt(b))
			if err != nil {
				t.Fatalf("the emitted bytes do not parse as ELF: %v", err)
			}
			if f.Class != elf.ELFCLASS64 || f.Data != elf.ELFDATA2LSB {
				t.Errorf("class/data = %v/%v, want 64-bit little-endian", f.Class, f.Data)
			}
			if f.Type != elf.ET_EXEC {
				t.Errorf("type = %v, want ET_EXEC", f.Type)
			}
			if f.Machine != want {
				t.Errorf("machine = %v, want %v", f.Machine, want)
			}
			if len(f.Progs) != 1 {
				t.Fatalf("%d program headers, want exactly one PT_LOAD", len(f.Progs))
			}
			p := f.Progs[0]
			if p.Type != elf.PT_LOAD || p.Flags&elf.PF_X == 0 || p.Flags&elf.PF_R == 0 {
				t.Errorf("segment = %v %v, want a readable executable PT_LOAD", p.Type, p.Flags)
			}
			// execve rejects a segment whose file offset and virtual address
			// disagree modulo the alignment, and the whole file is in this one.
			if (p.Vaddr-p.Off)%p.Align != 0 {
				t.Errorf("vaddr %#x and offset %#x disagree modulo align %#x", p.Vaddr, p.Off, p.Align)
			}
			if p.Filesz != uint64(len(b)) || p.Memsz != uint64(len(b)) {
				t.Errorf("segment covers %d/%d bytes of a %d-byte file", p.Filesz, p.Memsz, len(b))
			}
			// The entry has to land on the code, and on an instruction boundary
			// — aarch64 faults on a misaligned one.
			if f.Entry <= p.Vaddr || f.Entry >= p.Vaddr+p.Memsz {
				t.Errorf("entry %#x is outside the loaded segment", f.Entry)
			}
			if f.Entry%4 != 0 {
				t.Errorf("entry %#x is not instruction-aligned", f.Entry)
			}
		})
	}

	if _, err := buildSyncELF("riscv64"); err == nil {
		t.Error("buildSyncELF() on an unsupported GOARCH = nil, want an error")
	}
}

// The one test that proves the machine code: run it. A helper that parses as an
// ELF but faults on its first instruction fails every suspend, and nothing
// upstream of the guest would tell us.
func TestGuestSyncHelperRuns(t *testing.T) {
	if _, ok := syncProgram[runtime.GOARCH]; !ok {
		t.Skipf("no guest sync helper for %s", runtime.GOARCH)
	}
	dir := t.TempDir()
	if err := stageGuestSync(dir); err != nil {
		t.Fatalf("stageGuestSync() = %v", err)
	}
	path := filepath.Join(dir, guestSyncName)
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o555 {
		t.Errorf("mode = %v, want 0555; the actor may not run as root", st.Mode().Perm())
	}

	out, err := exec.Command(path).CombinedOutput()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			t.Fatalf("the helper exited %d (%v); output %q", ee.ExitCode(), err, out)
		}
		t.Fatalf("running the helper: %v", err)
	}
	if len(out) != 0 {
		t.Errorf("the helper wrote %q; it must be silent", out)
	}

	// Staged again over itself, as every boot does — including over an inode a
	// previous guest may still hold open.
	if err := stageGuestSync(dir); err != nil {
		t.Errorf("stageGuestSync() a second time = %v", err)
	}
	if err := exec.Command(path).Run(); err != nil {
		t.Errorf("the restaged helper = %v", err)
	}
	if _, err := os.Stat(path + ".tmp"); err == nil {
		t.Error("stageGuestSync() left its temporary file in the actor's rootfs")
	}
}

// readerAt adapts a byte slice for debug/elf.
type readerAt []byte

func (r readerAt) ReadAt(p []byte, off int64) (int, error) {
	if off >= int64(len(r)) {
		return 0, errors.New("EOF")
	}
	return copy(p, r[off:]), nil
}
