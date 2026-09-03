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

// Package incrtar snapshots a directory tree as a chain of tar archives, each
// generation carrying only what changed since the last, plus a manifest that
// describes the tree in full.
//
// It exists to shrink the durable-dir tar a suspend writes to local disk before
// upload. That tar is pure overhead — it is written only so the upload has
// something to read — and at a full capture it costs one write of the whole
// volume every cycle. Archiving only the changed paths makes that cost
// proportional to the change instead.
//
// Change is decided by content, never by timestamps. Every cycle begins by
// extracting a snapshot into a fresh directory, which gives every file a brand
// new ctime and inode, so any scheme that reads those — GNU tar's
// --listed-incremental among them — would call the whole tree new on the very
// next suspend and degrade to a full capture every time.
//
// The archives are ordinary tarutil archives, so the fidelity guarantees the
// durable-dir path depends on (modes, ownership, times, symlinks, hardlinks,
// FIFOs, confinement on extraction) are exactly tarutil's, unchanged.
//
// Restore never walks the chain: the manifest names the target state outright,
// says which generation's tar holds each path, and each of those tars is
// extracted through a filter that takes only the paths attributed to it. A file
// is therefore written once, not once per generation that ever touched it —
// without the filter a long chain would cost more to restore than a full
// capture, which would defeat the point.
//
// Two consequences to know before using it. Losing any generation in the chain
// makes the snapshot unrestorable, so restore verifies every file against the
// manifest and fails loudly rather than handing back a partial tree. And a
// chain is only as cheap as it is short: something has to force a fresh full
// capture periodically and reclaim the generations nothing references any more.
package incrtar

import (
	"context"
	"fmt"
	"io/fs"

	"github.com/agent-substrate/substrate/cmd/ateom-microvm/internal/tarutil"
)

// CreateOptions configures one generation of a snapshot chain.
type CreateOptions struct {
	// SrcDir is the tree to snapshot. The caller must have quiesced whatever
	// writes to it: this reads it twice, once to hash and once to archive, and
	// a write in between would put contents in the tar that the manifest does
	// not describe.
	SrcDir string
	// TarPath is where this generation's archive is written.
	TarPath string
	// Generation numbers this snapshot and must be at least 1 and greater than
	// Previous's.
	Generation int
	// Previous is the manifest of the generation this one builds on. A nil
	// Previous makes this a full capture, which is what the first snapshot of
	// an actor is — and what any later one should fall back to when the
	// previous manifest cannot be fetched. Falling back costs a generation's
	// worth of writes; failing costs the actor its suspend.
	Previous *Manifest
}

// CreateResult reports what Create produced.
type CreateResult struct {
	// Manifest describes SrcDir in full and must be stored alongside the tar:
	// without it the tar cannot be placed in the chain.
	Manifest *Manifest
	// Packed and PackedBytes count what went into this generation's tar,
	// against Total and TotalBytes for the whole tree. Their ratio is the delta
	// the proposal calls δ — the thing worth reporting from a real workload,
	// since the saving is bounded by it.
	Packed      int
	PackedBytes int64
	Total       int
	TotalBytes  int64
}

// Create snapshots opts.SrcDir into a tar holding only the paths that differ
// from opts.Previous, and returns the manifest describing the whole tree.
//
// A path counts as changed when its contents, mode, ownership, modification
// time, symlink target, extended attributes, or hardlink grouping differ — not
// just its contents. tar cannot express "same bytes, new header", so a file
// that was only chmod'ed is repacked whole; that wastes a copy on a large file,
// but the alternative is restoring it with the wrong mode.
//
// Directories are never archived. Their metadata lives in the manifest and
// Restore materializes them from it, which keeps an unchanged tree's tar
// genuinely empty and spares the chain a header per directory per generation.
func Create(ctx context.Context, opts CreateOptions) (*CreateResult, error) {
	if opts.Generation < 1 {
		return nil, fmt.Errorf("generation %d is not positive", opts.Generation)
	}
	if opts.Previous != nil && opts.Previous.Generation >= opts.Generation {
		return nil, fmt.Errorf("previous manifest is generation %d, not older than %d", opts.Previous.Generation, opts.Generation)
	}

	entries, err := scan(ctx, opts.SrcDir)
	if err != nil {
		return nil, fmt.Errorf("while scanning %q: %w", opts.SrcDir, err)
	}

	previous := map[string]Entry{}
	if opts.Previous != nil {
		previous = opts.Previous.byPath()
	}
	pack := changedPaths(entries, previous)

	result := &CreateResult{Total: len(entries)}
	for i, e := range entries {
		result.TotalBytes += e.Size
		switch {
		case e.Type == TypeDir:
			entries[i].OriginGen = 0
		case pack[e.Path]:
			entries[i].OriginGen = opts.Generation
			result.Packed++
			result.PackedBytes += e.Size
		default:
			entries[i].OriginGen = previous[e.Path].OriginGen
		}
	}

	sel := func(rel string, d fs.DirEntry) bool { return pack[rel] }
	if err := tarutil.CreateSelected(ctx, opts.TarPath, opts.SrcDir, sel); err != nil {
		return nil, fmt.Errorf("while archiving generation %d of %q: %w", opts.Generation, opts.SrcDir, err)
	}

	result.Manifest = &Manifest{
		Version:    ManifestVersion,
		Generation: opts.Generation,
		Entries:    entries,
	}
	if err := result.Manifest.validate(); err != nil {
		return nil, fmt.Errorf("generation %d of %q produced an unusable manifest: %w", opts.Generation, opts.SrcDir, err)
	}
	return result, nil
}

// changedPaths returns the non-directory paths this generation has to archive.
//
// A path is in the set when it is new, when any recorded field differs, or when
// the generation that used to hold it is unknown. The set is then closed over
// hardlink sets: tarutil resolves links within a single archive, so archiving
// one member of a set without the others would restore them as unrelated files
// with duplicated contents.
func changedPaths(entries []Entry, previous map[string]Entry) map[string]bool {
	changed := map[string]bool{}
	for _, e := range entries {
		if e.Type == TypeDir {
			continue
		}
		prev, ok := previous[e.Path]
		if !ok || prev.OriginGen == 0 || !e.sameAs(prev) {
			changed[e.Path] = true
		}
	}

	sets := map[string][]string{}
	dirty := map[string]bool{}
	for _, e := range entries {
		if e.LinkSet == "" {
			continue
		}
		sets[e.LinkSet] = append(sets[e.LinkSet], e.Path)
		if changed[e.Path] {
			dirty[e.LinkSet] = true
		}
	}
	for set, paths := range sets {
		if !dirty[set] {
			continue
		}
		for _, path := range paths {
			changed[path] = true
		}
	}
	return changed
}
