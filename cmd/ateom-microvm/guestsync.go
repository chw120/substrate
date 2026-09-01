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

// The sync helper the durable-disk suspend runs in the guest.
//
// Suspending a durable DISK has to flush the guest's page cache first, and the
// only way to run anything in the guest is kata-agent's ExecProcess, which runs
// a binary from the ACTOR's container rootfs. Depending on the actor's image
// for that would be wrong twice over: most images are distroless and have no
// sync at all (the benchmark's own glutton is built on
// gcr.io/distroless/static-debian13), so the suspend would fail outright; and an
// image that did have one would make a substrate-internal operation depend on
// what the user chose to package.
//
// So ateom supplies the binary. sync(2) is not namespaced — it flushes every
// filesystem the guest kernel has mounted, whichever process calls it — so this
// only has to be A process in the guest, not a privileged or special one.
//
// The binary is written into the container's merged rootfs on the host before
// virtiofsd starts, which is the same route the OCI bundle itself takes into the
// guest. It is emitted here rather than shipped in the ateom image because the
// whole program is two syscalls: assembling it costs a few hundred bytes of
// table and no build, image, or supply-chain surface at all. The guest runs the
// host's architecture, so runtime.GOARCH is the one to emit.

import (
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

// guestSyncName is the helper's path inside the actor's container. A dotfile at
// the root of the rootfs: the guest's ExecProcess needs an absolute path in the
// container, and there is no directory an arbitrary image is guaranteed to have.
const guestSyncName = ".ate-sync"

// guestSyncPath is that path as the guest agent must be given it.
const guestSyncPath = "/" + guestSyncName

// syncProgram is the machine code, per GOARCH: call sync(2), then exit(0).
//
//	amd64                      arm64
//	mov  eax, 162  (sync)      mov x8, #81   (sync)
//	syscall                    svc #0
//	mov  eax, 60   (exit)      mov x8, #93   (exit)
//	xor  edi, edi              mov x0, #0
//	syscall                    svc #0
var syncProgram = map[string][]byte{
	"amd64": {
		0xb8, 0xa2, 0x00, 0x00, 0x00,
		0x0f, 0x05,
		0xb8, 0x3c, 0x00, 0x00, 0x00,
		0x31, 0xff,
		0x0f, 0x05,
	},
	"arm64": {
		0x28, 0x0a, 0x80, 0xd2,
		0x01, 0x00, 0x00, 0xd4,
		0xa8, 0x0b, 0x80, 0xd2,
		0x00, 0x00, 0x80, 0xd2,
		0x01, 0x00, 0x00, 0xd4,
	},
}

// elfMachine is e_machine for each GOARCH we can emit.
var elfMachine = map[string]uint16{"amd64": 62, "arm64": 183}

const (
	elfHeaderSize  = 64
	progHeaderSize = 56
	// loadAddr is where the whole file is mapped. Any address above the mmap
	// minimum does; this is the conventional static-executable base, and the
	// program is position-dependent only in that its entry point is fixed.
	loadAddr = 0x400000
)

// buildSyncELF returns a static ELF64 executable for goarch that calls sync(2)
// and exits 0.
//
// One PT_LOAD segment covering the file from offset 0, so the headers are mapped
// along with the code and no section table is needed — the kernel's loader reads
// neither. p_offset and p_vaddr agree modulo the page size, which is the one
// alignment rule execve enforces.
func buildSyncELF(goarch string) ([]byte, error) {
	code, ok := syncProgram[goarch]
	if !ok {
		return nil, fmt.Errorf("no guest sync helper for GOARCH %q", goarch)
	}
	machine := elfMachine[goarch]

	const codeOff = elfHeaderSize + progHeaderSize
	total := uint64(codeOff + len(code))
	b := make([]byte, total)

	copy(b, []byte{0x7f, 'E', 'L', 'F', 2, 1, 1, 0}) // 64-bit, little-endian, SysV
	binary.LittleEndian.PutUint16(b[16:], 2)         // e_type: ET_EXEC
	binary.LittleEndian.PutUint16(b[18:], machine)   // e_machine
	binary.LittleEndian.PutUint32(b[20:], 1)         // e_version
	binary.LittleEndian.PutUint64(b[24:], loadAddr+codeOff)
	binary.LittleEndian.PutUint64(b[32:], elfHeaderSize) // e_phoff
	binary.LittleEndian.PutUint16(b[52:], elfHeaderSize) // e_ehsize
	binary.LittleEndian.PutUint16(b[54:], progHeaderSize)
	binary.LittleEndian.PutUint16(b[56:], 1) // e_phnum

	ph := b[elfHeaderSize:]
	binary.LittleEndian.PutUint32(ph[0:], 1) // p_type: PT_LOAD
	binary.LittleEndian.PutUint32(ph[4:], 5) // p_flags: R+X
	binary.LittleEndian.PutUint64(ph[8:], 0) // p_offset
	binary.LittleEndian.PutUint64(ph[16:], loadAddr)
	binary.LittleEndian.PutUint64(ph[24:], loadAddr)
	binary.LittleEndian.PutUint64(ph[32:], total)  // p_filesz
	binary.LittleEndian.PutUint64(ph[40:], total)  // p_memsz
	binary.LittleEndian.PutUint64(ph[48:], 0x1000) // p_align

	copy(b[codeOff:], code)
	return b, nil
}

// stageGuestSync writes the helper into a container's merged rootfs, so the
// guest sees it at guestSyncPath.
//
// Through the overlay's merged mount rather than into its upper directly:
// modifying an upper that is already mounted is not something overlayfs
// supports, and this runs after the mount. Rewritten on every boot rather than
// only when absent — the upper is re-materialized from a snapshot tar on
// restore, and a helper from an older ateom is not what this one wants to exec.
func stageGuestSync(rootfs string) error {
	elf, err := buildSyncELF(runtime.GOARCH)
	if err != nil {
		return err
	}
	path := filepath.Join(rootfs, guestSyncName)
	// Replace rather than truncate in place: the previous activation's guest may
	// still hold the old inode open, and a partially rewritten executable is one
	// the next flush would fail on.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, elf, 0o555); err != nil {
		return fmt.Errorf("writing the guest sync helper to %q: %w", tmp, err)
	}
	// Whatever is at the destination goes, including a directory: rename over
	// one fails, and an actor that made a directory there would otherwise have
	// made itself permanently unsuspendable with a single mkdir.
	if err := os.RemoveAll(path); err != nil {
		return fmt.Errorf("clearing %q for the guest sync helper: %w", path, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("installing the guest sync helper at %q: %w", path, err)
	}
	return nil
}
