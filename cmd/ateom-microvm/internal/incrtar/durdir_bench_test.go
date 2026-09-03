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
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/agent-substrate/substrate/cmd/ateom-microvm/internal/tarutil"
)

// These benchmarks stand in for the cluster DurDir scenarios while incrtar is
// still a standalone library and nothing calls it from suspend or resume. They
// run the same shapes the harness does, against local disk, and report the one
// number a cluster run cannot produce any earlier: δ, the fraction of the volume
// a cycle actually has to archive.
//
// The saving on the wire is bounded by δ, so δ decides whether wiring this into
// the suspend path is worth doing at all. Everything else the cluster measures —
// what the avoided writes are worth against the node's disk ceiling, how the
// corridor behaves under a real suspend — needs the real thing and is not
// approximated here.
//
// Run them with:
//
//	go test ./cmd/ateom-microvm/internal/incrtar -run xxx -bench Durdir -benchtime 20x
//
// Fixing the iteration count matters: -benchtime defaults to wall-clock, and
// these write hundreds of megabytes per iteration.

// durdirCases mirror the DurDir scenarios. Each partial case pairs with the
// sweep case above it: the volume is the same and only the per-cycle change
// differs, which is exactly the axis δ lives on.
var durdirCases = []struct {
	name string
	// files is how many files the directory holds, one of which each cycle
	// rewrites. A count of 1 is the size sweep, which rewrites the volume.
	files int
	size  int64
}{
	{"sweep_128mb", 1, 128 << 20},
	{"partial_128mb", 8, 16 << 20},
	{"sweep_512mb", 1, 512 << 20},
	{"partial_512mb", 8, 64 << 20},
}

// BenchmarkDurdirIncrementalCycle measures one suspend's worth of incremental
// capture: hash the tree, archive what changed, write the manifest.
//
// ns/op covers the capture alone — the workload's own rewrite is charged to
// nobody, the way a suspend does not pay for the writes that preceded it.
func BenchmarkDurdirIncrementalCycle(b *testing.B) {
	for _, tc := range durdirCases {
		b.Run(tc.name, func(b *testing.B) {
			ctx := b.Context()
			src := newDurdirTree(b, tc.files, tc.size)
			tars := b.TempDir()
			b.SetBytes(int64(tc.files) * tc.size)

			gen := 0
			var prev *Manifest
			capture := func() *CreateResult {
				gen++
				tarPath := filepath.Join(tars, fmt.Sprintf("gen-%d.tar", gen))
				res, err := Create(ctx, CreateOptions{
					SrcDir:     src,
					TarPath:    tarPath,
					Generation: gen,
					Previous:   prev,
				})
				if err != nil {
					b.Fatalf("Create generation %d: %v", gen, err)
				}
				prev = res.Manifest
				// Nothing here restores, so the archive is dead the moment it is
				// measured. Keeping the chain would cost more disk than the
				// benchmark is worth.
				if err := os.Remove(tarPath); err != nil {
					b.Fatalf("removing generation %d: %v", gen, err)
				}
				return res
			}

			// Generation 1 is the bootstrap capture. It is a full one by
			// definition and is not what these cycles are about.
			capture()

			var packed, total int64
			rng := newDurdirRNG(2)
			b.ResetTimer()
			for i := range b.N {
				b.StopTimer()
				writeDurdirFile(b, src, i%tc.files, tc.size, rng)
				b.StartTimer()

				res := capture()
				packed += res.PackedBytes
				total += res.TotalBytes
			}
			b.StopTimer()

			reportDurdir(b, packed, total)
		})
	}
}

// BenchmarkDurdirFullCycle is the same workload captured the way suspend
// captures it today, so the two benchmarks' MiB/s and archived bytes are
// directly comparable.
func BenchmarkDurdirFullCycle(b *testing.B) {
	for _, tc := range durdirCases {
		b.Run(tc.name, func(b *testing.B) {
			ctx := b.Context()
			src := newDurdirTree(b, tc.files, tc.size)
			tarPath := filepath.Join(b.TempDir(), "durable.tar")
			b.SetBytes(int64(tc.files) * tc.size)

			var packed int64
			rng := newDurdirRNG(2)
			b.ResetTimer()
			for i := range b.N {
				b.StopTimer()
				writeDurdirFile(b, src, i%tc.files, tc.size, rng)
				b.StartTimer()

				if err := tarutil.Create(ctx, tarPath, src); err != nil {
					b.Fatalf("tarutil.Create: %v", err)
				}

				b.StopTimer()
				info, err := os.Stat(tarPath)
				if err != nil {
					b.Fatalf("stat archive: %v", err)
				}
				packed += info.Size()
				b.StartTimer()
			}
			b.StopTimer()

			reportDurdir(b, packed, int64(b.N)*int64(tc.files)*tc.size)
		})
	}
}

// durdirChainDepth is how many cycles pile up before the restore benchmarks
// rebuild the tree. A chain is only as cheap as it is short, and the cost of
// letting one grow is the other half of what a delta is worth.
const durdirChainDepth = 8

// BenchmarkDurdirIncrementalRestore measures a resume against a chain that deep.
//
// The filtered extraction is what this is really testing: each generation's tar
// contributes only the paths the manifest attributes to it, so a file is written
// once no matter how many generations touched it. If that holds, restoring a
// chain costs about what restoring a full capture costs, and the depth is free.
func BenchmarkDurdirIncrementalRestore(b *testing.B) {
	for _, tc := range durdirCases {
		b.Run(tc.name, func(b *testing.B) {
			ctx := b.Context()
			src := newDurdirTree(b, tc.files, tc.size)
			tarDir := b.TempDir()
			tars := map[int]string{}

			var prev *Manifest
			rng := newDurdirRNG(2)
			for gen := 1; gen <= durdirChainDepth; gen++ {
				if gen > 1 {
					writeDurdirFile(b, src, (gen-2)%tc.files, tc.size, rng)
				}
				tarPath := filepath.Join(tarDir, fmt.Sprintf("gen-%d.tar", gen))
				res, err := Create(ctx, CreateOptions{
					SrcDir:     src,
					TarPath:    tarPath,
					Generation: gen,
					Previous:   prev,
				})
				if err != nil {
					b.Fatalf("Create generation %d: %v", gen, err)
				}
				prev, tars[gen] = res.Manifest, tarPath
			}
			b.SetBytes(int64(tc.files) * tc.size)

			b.ResetTimer()
			for range b.N {
				b.StopTimer()
				dst := b.TempDir()
				b.StartTimer()

				if err := Restore(RestoreOptions{DstDir: dst, Manifest: prev, Tars: tars}); err != nil {
					b.Fatalf("Restore: %v", err)
				}

				b.StopTimer()
				if err := os.RemoveAll(dst); err != nil {
					b.Fatalf("clearing restore target: %v", err)
				}
				b.StartTimer()
			}
			// Reported after the loop: ResetTimer drops user metrics.
			b.ReportMetric(float64(len(prev.NeededGenerations())), "tars/restore")
		})
	}
}

// BenchmarkDurdirFullRestore is the resume side of today's arrangement: one tar
// holding the whole volume, extracted whole.
func BenchmarkDurdirFullRestore(b *testing.B) {
	for _, tc := range durdirCases {
		b.Run(tc.name, func(b *testing.B) {
			src := newDurdirTree(b, tc.files, tc.size)
			tarPath := filepath.Join(b.TempDir(), "durable.tar")
			if err := tarutil.Create(b.Context(), tarPath, src); err != nil {
				b.Fatalf("tarutil.Create: %v", err)
			}
			b.SetBytes(int64(tc.files) * tc.size)

			b.ResetTimer()
			for range b.N {
				b.StopTimer()
				dst := b.TempDir()
				b.StartTimer()

				if err := tarutil.Extract(tarPath, dst); err != nil {
					b.Fatalf("tarutil.Extract: %v", err)
				}

				b.StopTimer()
				if err := os.RemoveAll(dst); err != nil {
					b.Fatalf("clearing restore target: %v", err)
				}
				b.StartTimer()
			}
		})
	}
}

// reportDurdir adds the two numbers the ns/op does not carry: δ as a percentage,
// and the archived megabytes a cycle costs.
func reportDurdir(b *testing.B, packed, total int64) {
	b.Helper()
	if total > 0 {
		b.ReportMetric(float64(packed)/float64(total)*100, "delta%")
	}
	b.ReportMetric(float64(packed)/float64(b.N)/(1<<20), "MiB/cycle")
}

// newDurdirTree builds the directory the workload starts a run with: count files
// of size bytes each, which is the steady size the cluster scenarios hold.
func newDurdirTree(tb testing.TB, count int, size int64) string {
	tb.Helper()
	dir := filepath.Join(tb.TempDir(), "durable")
	if err := os.Mkdir(dir, 0o755); err != nil {
		tb.Fatalf("creating durable dir: %v", err)
	}
	rng := newDurdirRNG(1)
	for i := range count {
		writeDurdirFile(tb, dir, i, size, rng)
	}
	return dir
}

// writeDurdirFile truncates and rewrites one file with fresh bytes, which is
// what a DurdirUser cycle does. The contents are incompressible and never
// repeat, so nothing but a genuine change can be mistaken for one.
func writeDurdirFile(tb testing.TB, dir string, index int, size int64, rng *durdirRNG) {
	tb.Helper()
	path := filepath.Join(dir, fmt.Sprintf("file-%02d.dat", index))
	f, err := os.Create(path)
	if err != nil {
		tb.Fatalf("creating %q: %v", path, err)
	}
	defer f.Close()

	buf := make([]byte, 1<<20)
	for remaining := size; remaining > 0; {
		n := int64(len(buf))
		if n > remaining {
			n = remaining
		}
		rng.fill(buf[:n])
		if _, err := f.Write(buf[:n]); err != nil {
			tb.Fatalf("writing %q: %v", path, err)
		}
		remaining -= n
	}
	if err := f.Close(); err != nil {
		tb.Fatalf("closing %q: %v", path, err)
	}
}

// durdirRNG is an xorshift64 generator. The benchmark needs bytes that do not
// compress and do not collide, not bytes that are unpredictable, and this fills
// a megabyte far faster than crypto/rand — fast enough that the workload's write
// stays a write benchmark rather than a random-number one.
type durdirRNG struct{ state uint64 }

func newDurdirRNG(seed uint64) *durdirRNG { return &durdirRNG{state: seed} }

func (r *durdirRNG) next() uint64 {
	x := r.state
	x ^= x << 13
	x ^= x >> 7
	x ^= x << 17
	r.state = x
	return x
}

func (r *durdirRNG) fill(buf []byte) {
	for len(buf) >= 8 {
		binary.LittleEndian.PutUint64(buf, r.next())
		buf = buf[8:]
	}
	if len(buf) > 0 {
		var tail [8]byte
		binary.LittleEndian.PutUint64(tail[:], r.next())
		copy(buf, tail[:])
	}
}

// TestDurdirCasesAreCoherent keeps the benchmark honest without running it: a
// partial case that did not actually hold a volume the same size as its sweep
// case would compare two different things and read as a saving that is really a
// smaller directory.
func TestDurdirCasesAreCoherent(t *testing.T) {
	volumes := map[int64][]string{}
	for _, tc := range durdirCases {
		if tc.files < 1 {
			t.Errorf("case %s has %d files, want at least 1", tc.name, tc.files)
		}
		volumes[int64(tc.files)*tc.size] = append(volumes[int64(tc.files)*tc.size], tc.name)
	}
	for volume, names := range volumes {
		if len(names) < 2 {
			t.Errorf("volume %d MiB is measured only by %v, so it has nothing to compare against", volume>>20, names)
		}
	}
}

// TestDurdirCycleReportsTheExpectedDelta runs the partial-rewrite shape at a
// size that fits in a unit test, and asserts the delta a cycle archives is the
// one file it rewrote. This is the claim the benchmark reports as a number, and
// it is worth failing over rather than only observing.
func TestDurdirCycleReportsTheExpectedDelta(t *testing.T) {
	const (
		files = 8
		size  = 64 << 10
	)
	ctx := t.Context()
	src := newDurdirTree(t, files, size)
	tars := t.TempDir()

	first, err := Create(ctx, CreateOptions{
		SrcDir:     src,
		TarPath:    filepath.Join(tars, "gen-1.tar"),
		Generation: 1,
	})
	if err != nil {
		t.Fatalf("bootstrap capture: %v", err)
	}
	if first.PackedBytes != files*size {
		t.Errorf("bootstrap packed %d bytes, want the whole volume, %d", first.PackedBytes, files*size)
	}

	writeDurdirFile(t, src, 3, size, newDurdirRNG(99))
	cycle, err := Create(ctx, CreateOptions{
		SrcDir:     src,
		TarPath:    filepath.Join(tars, "gen-2.tar"),
		Generation: 2,
		Previous:   first.Manifest,
	})
	if err != nil {
		t.Fatalf("cycle capture: %v", err)
	}
	if cycle.Packed != 1 {
		t.Errorf("cycle packed %d entries, want just the rewritten file", cycle.Packed)
	}
	if cycle.PackedBytes != size {
		t.Errorf("cycle packed %d bytes, want one file's %d", cycle.PackedBytes, size)
	}
	if cycle.TotalBytes != files*size {
		t.Errorf("cycle reports a total of %d bytes, want the whole volume, %d", cycle.TotalBytes, files*size)
	}
}
