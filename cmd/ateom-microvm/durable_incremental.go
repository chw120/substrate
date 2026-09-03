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

// Incremental durable-dir capture.
//
// A full capture writes the whole volume to local disk at every suspend, purely
// so the upload has a file to read. Incremental capture writes only the paths
// that changed since the previous snapshot and inherits the rest from archives
// already on the worker, which turns that cost into one proportional to the
// change.
//
// The snapshot stays self-contained: it carries the manifest, the new
// generation's archive, and the archives of every ancestor it still inherits
// from. Nothing above ateom has to learn that snapshots can reference each
// other. The saving is in the writes, not the uploads — the inherited archives
// are hard-linked out of a worker-local cache, so publishing them costs a
// directory entry rather than a copy of their contents.
//
// Turning it off is meant to be free: with the flag unset every path here is
// skipped and the checkpoint holds exactly the single tar it always did.
// Restore ignores the flag entirely and follows the snapshot instead, so a
// snapshot taken before any of this existed still restores the old way and one
// taken with the flag on still restores after the flag is turned off.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/agent-substrate/substrate/cmd/ateom-microvm/internal/incrtar"
	"github.com/agent-substrate/substrate/internal/ateompath"
)

const (
	// incrementalDurableDirEnv turns incremental durable-dir capture on. It is
	// off by default: the scheme trades a read of the whole volume (to hash it)
	// for a much smaller write, which is the right trade on the node's disk but
	// not obviously so on every workload shape.
	incrementalDurableDirEnv = "ATE_INCREMENTAL_DURABLE_DIR"

	// maxDurableDirChainEnv overrides how many generations a chain may reference
	// before the next capture is taken in full.
	maxDurableDirChainEnv = "ATE_INCREMENTAL_DURABLE_DIR_MAX_CHAIN"

	// defaultMaxDurableDirChain bounds the chain. Every referenced generation is
	// an archive the snapshot has to carry and the restore has to open, so a
	// chain left to grow without limit eventually costs more to ship and
	// reassemble than the writes it saves.
	defaultMaxDurableDirChain = 16
)

// cachedManifestFile is the manifest of the newest generation in the worker's
// cache, and cacheGenTarPrefix names the generation archives beside it.
const (
	cachedManifestFile = "manifest.json"
	cacheGenTarPrefix  = "g"
)

// incrementalDurableDirEnabled reports whether suspends should capture
// incrementally.
func incrementalDurableDirEnabled() bool {
	return truthyEnv(os.Getenv(incrementalDurableDirEnv))
}

// truthyEnv reads the handful of spellings a deployment is likely to use.
func truthyEnv(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "t", "true", "y", "yes", "on":
		return true
	}
	return false
}

// maxDurableDirChain returns the chain bound in force.
func maxDurableDirChain() int {
	n, err := strconv.Atoi(strings.TrimSpace(os.Getenv(maxDurableDirChainEnv)))
	if err != nil || n < 1 {
		return defaultMaxDurableDirChain
	}
	return n
}

// cacheGenTarPath is where the worker keeps one generation's archive.
func cacheGenTarPath(cacheDir string, gen int) string {
	return filepath.Join(cacheDir, fmt.Sprintf("%s%d.tar", cacheGenTarPrefix, gen))
}

// tarDurableVolumesIncrementally captures dir as the next generation of the
// actor's chain and publishes the whole snapshot into checkpointDir.
//
// It never fails the suspend over a cache problem. A missing, unreadable, or
// incomplete cache costs this cycle its saving and nothing else: the capture
// falls back to a full one, which is what the actor's first suspend on a worker
// does anyway. Losing a suspend would cost the actor its state.
func tarDurableVolumesIncrementally(ctx context.Context, actorUID, dir, checkpointDir string) error {
	cacheDir := ateompath.IncrementalDurableDirActorCacheDir(actorUID)
	if err := os.MkdirAll(cacheDir, 0o700); err != nil {
		return fmt.Errorf("while creating the durable-dir generation cache %q: %w", cacheDir, err)
	}

	previous := usableCachedManifest(cacheDir)
	generation := 1
	if previous != nil {
		generation = previous.Generation + 1
	}

	tarPath := cacheGenTarPath(cacheDir, generation)
	result, err := incrtar.Create(ctx, incrtar.CreateOptions{
		SrcDir:     dir,
		TarPath:    tarPath,
		Generation: generation,
		Previous:   previous,
	})
	if err != nil {
		return fmt.Errorf("while incrementally archiving durable-dir volumes from %q: %w", dir, err)
	}

	if err := publishDurableChain(cacheDir, checkpointDir, result.Manifest); err != nil {
		return err
	}
	if err := writeCachedManifest(cacheDir, result.Manifest); err != nil {
		return err
	}
	pruneGenerationCache(cacheDir, result.Manifest)
	return nil
}

// usableCachedManifest returns the manifest the next generation should build
// on, or nil to capture in full.
//
// It declines a chain that has reached its bound and one whose archives are not
// all present. The second check matters because inheriting a path means
// promising that some archive still holds it: a manifest that names a
// generation the cache has lost would produce a snapshot that cannot be
// restored, and the failure would not surface until the resume.
func usableCachedManifest(cacheDir string) *incrtar.Manifest {
	m, err := readCachedManifest(cacheDir)
	if err != nil || m == nil {
		return nil
	}
	needed := m.NeededGenerations()
	if len(needed) >= maxDurableDirChain() {
		return nil
	}
	for _, gen := range needed {
		if _, err := os.Stat(cacheGenTarPath(cacheDir, gen)); err != nil {
			return nil
		}
	}
	return m
}

// readCachedManifest loads the cache's manifest, returning nil when there is
// none.
func readCachedManifest(cacheDir string) (*incrtar.Manifest, error) {
	raw, err := os.ReadFile(filepath.Join(cacheDir, cachedManifestFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var m incrtar.Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// writeCachedManifest records the new newest generation, replacing the file
// atomically so an interrupted suspend leaves the previous manifest intact
// rather than a truncated one the next cycle would have to discard.
func writeCachedManifest(cacheDir string, m *incrtar.Manifest) error {
	raw, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("while encoding the durable-dir manifest: %w", err)
	}
	tmp := filepath.Join(cacheDir, cachedManifestFile+".tmp")
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return fmt.Errorf("while writing the cached durable-dir manifest: %w", err)
	}
	if err := os.Rename(tmp, filepath.Join(cacheDir, cachedManifestFile)); err != nil {
		return fmt.Errorf("while installing the cached durable-dir manifest: %w", err)
	}
	return nil
}

// publishDurableChain places the manifest and every archive it references into
// checkpointDir, so the snapshot atelet uploads restores on its own.
//
// The newest generation is published under the name a full capture uses, which
// is what keeps a one-generation chain indistinguishable from a full capture to
// everything downstream.
func publishDurableChain(cacheDir, checkpointDir string, m *incrtar.Manifest) error {
	raw, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("while encoding the durable-dir manifest: %w", err)
	}
	manifestPath := filepath.Join(checkpointDir, ateompath.DurableDirManifestFile)
	if err := os.WriteFile(manifestPath, raw, 0o600); err != nil {
		return fmt.Errorf("while writing %q: %w", manifestPath, err)
	}

	for _, gen := range m.NeededGenerations() {
		name := ateompath.DurableDirGenTarFile(gen)
		if gen == m.Generation {
			name = ateompath.DurableDirTarFile
		}
		if err := linkOrCopy(cacheGenTarPath(cacheDir, gen), filepath.Join(checkpointDir, name)); err != nil {
			return fmt.Errorf("while publishing durable-dir generation %d: %w", gen, err)
		}
	}
	return nil
}

// pruneGenerationCache drops the archives the newest manifest no longer
// references. They are unreachable once it is the manifest a resume will use,
// and keeping them would grow the worker's cache without bound.
//
// Failures are ignored: the cache is disposable and a suspend that already
// succeeded should not be failed over disk it could not tidy up.
func pruneGenerationCache(cacheDir string, m *incrtar.Manifest) {
	keep := map[int]bool{}
	for _, gen := range m.NeededGenerations() {
		keep[gen] = true
	}
	entries, err := os.ReadDir(cacheDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		gen, ok := cacheGeneration(e.Name())
		if !ok || keep[gen] {
			continue
		}
		os.Remove(filepath.Join(cacheDir, e.Name()))
	}
}

// cacheGeneration returns the generation a cache file holds, if it holds one.
func cacheGeneration(name string) (int, bool) {
	rest, ok := strings.CutPrefix(name, cacheGenTarPrefix)
	if !ok {
		return 0, false
	}
	digits, ok := strings.CutSuffix(rest, ".tar")
	if !ok {
		return 0, false
	}
	gen, err := strconv.Atoi(digits)
	if err != nil || gen < 1 {
		return 0, false
	}
	return gen, true
}

// untarDurableVolumesFromChain rebuilds dir from an incremental snapshot and
// leaves the worker's cache holding that snapshot's generations, so the actor's
// next suspend can inherit from them instead of recapturing the volume whole.
func untarDurableVolumesFromChain(actorUID, dir, snapshotDir string, m *incrtar.Manifest) error {
	tars := map[int]string{}
	for _, gen := range m.NeededGenerations() {
		name := ateompath.DurableDirGenTarFile(gen)
		if gen == m.Generation {
			name = ateompath.DurableDirTarFile
		}
		tars[gen] = filepath.Join(snapshotDir, name)
	}
	if err := incrtar.Restore(incrtar.RestoreOptions{DstDir: dir, Manifest: m, Tars: tars}); err != nil {
		return fmt.Errorf("while restoring durable-dir volumes into %q: %w", dir, err)
	}
	seedGenerationCache(actorUID, m, tars)
	return nil
}

// seedGenerationCache copies the restored snapshot's chain into the worker's
// cache. It is best-effort: without it the next suspend simply captures in
// full, which is correct, just not cheap.
//
// The cache is cleared first so it describes this snapshot and nothing else.
// Leaving a newer generation behind — from an earlier incarnation of the same
// actor on this worker — would let the next capture number itself below one
// that already exists, and Create rejects that.
func seedGenerationCache(actorUID string, m *incrtar.Manifest, tars map[int]string) {
	cacheDir := ateompath.IncrementalDurableDirActorCacheDir(actorUID)
	if err := os.RemoveAll(cacheDir); err != nil {
		return
	}
	if err := os.MkdirAll(cacheDir, 0o700); err != nil {
		return
	}
	// A half-populated cache is worse than none: the next capture would inherit
	// from generations it cannot produce. Discard the whole thing instead.
	for gen, src := range tars {
		if err := linkOrCopy(src, cacheGenTarPath(cacheDir, gen)); err != nil {
			_ = os.RemoveAll(cacheDir)
			return
		}
	}
	if err := writeCachedManifest(cacheDir, m); err != nil {
		_ = os.RemoveAll(cacheDir)
	}
}

// readSnapshotManifest returns the manifest of an incremental durable-dir
// snapshot, or nil when the snapshot is a plain full capture.
func readSnapshotManifest(snapshotDir string) (*incrtar.Manifest, error) {
	path := filepath.Join(snapshotDir, ateompath.DurableDirManifestFile)
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("while reading %q: %w", path, err)
	}
	var m incrtar.Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("while decoding %q: %w", path, err)
	}
	return &m, nil
}

// linkOrCopy hardlinks src to dst, copying only if it cannot. The link is the
// point: an inherited generation must reach the snapshot without its contents
// being written again, since those writes are exactly what this scheme exists
// to avoid. A copy still produces a correct snapshot, so a cache that has ended
// up on a different filesystem degrades rather than fails.
func linkOrCopy(src, dst string) error {
	if err := os.Remove(dst); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.Link(src, dst); err == nil {
		return nil
	}

	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
