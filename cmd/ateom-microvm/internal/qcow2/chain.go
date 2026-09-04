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

package qcow2

// The backing chain as a directory plus a manifest.
//
// A chain is already self-describing — each layer's header names the one
// behind it — so the manifest is not what makes the images readable. It exists
// for the two things the headers cannot say:
//
//   - which layer is the TOP. Headers point backwards, so a directory of
//     layers alone does not distinguish the one the guest should open from a
//     stale sibling left by a failed flatten.
//   - what SHOULD be here. A chain with a hole in it is the one failure that
//     must never be papered over: qemu will refuse to open it, but only after
//     the file is already gone, and "which layer went missing" is not a
//     question the surviving headers can answer.
//
// It also carries the per-layer byte counts, which is what makes the point of
// the whole arrangement measurable: how much of a suspend's upload is the new
// layer and how much is chain the actor is dragging along.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Manifest describes one actor's backing chain. It is written beside the
// layers, both in the actor's live layer directory and in a checkpoint.
type Manifest struct {
	// Layers are the chain's images, base FIRST and top LAST, by bare
	// filename. Order is the chain order, which happens to sort the same way
	// only because NextLayerName numbers them.
	Layers []Layer `json:"layers"`
	// VirtualSizeBytes is the base layer's virtual size, and so the ext4's.
	// Recorded so a restore can report a mismatch rather than have the guest
	// discover it.
	VirtualSizeBytes int64 `json:"virtual_size_bytes"`
}

// Layer is one image in a chain.
type Layer struct {
	// File is the layer's bare filename. The same name is used in the actor's
	// layer directory and in the checkpoint, so the backing-file headers stay
	// valid across the move.
	File string `json:"file"`
	// SizeBytes is the layer file's size on disk.
	//
	// A size and not a digest. Everything a manifest is read for — is the layer
	// here, is it the whole layer, which one is the top, how much did this
	// suspend add — a size answers, and a digest of the chain would cost a full
	// read of every byte in it inside the pause window this design exists to
	// keep short. Detecting bit rot is the object store's job, not the pause's.
	SizeBytes int64 `json:"size_bytes"`
}

// Top returns the manifest's top layer, the one to hand to cloud-hypervisor.
func (m Manifest) Top() (Layer, error) {
	if len(m.Layers) == 0 {
		return Layer{}, errors.New("durable-dir chain manifest lists no layers")
	}
	return m.Layers[len(m.Layers)-1], nil
}

// TotalBytes is the size of the whole chain: what a suspend uploads and a
// restore has to have locally before the guest can read anything.
func (m Manifest) TotalBytes() int64 {
	var n int64
	for _, l := range m.Layers {
		n += l.SizeBytes
	}
	return n
}

// ReadManifest loads a manifest from path.
func ReadManifest(path string) (Manifest, error) {
	var m Manifest
	b, err := os.ReadFile(path)
	if err != nil {
		return m, err
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return m, fmt.Errorf("parsing durable-dir chain manifest %q: %w", path, err)
	}
	if len(m.Layers) == 0 {
		return m, fmt.Errorf("durable-dir chain manifest %q lists no layers", path)
	}
	for _, l := range m.Layers {
		if l.File == "" || l.File != filepath.Base(l.File) {
			return m, fmt.Errorf("durable-dir chain manifest %q names a layer %q that is not a bare filename", path, l.File)
		}
	}
	return m, nil
}

// WriteManifest writes m to path.
//
// Through a temporary file and a rename, so what is at path is always a whole
// manifest. This is the file that says which layer is the top, and an in-place
// rewrite interrupted by a crash or a container kill leaves a truncated one
// that the next restore rejects as unparseable — with the layers themselves
// intact on disk and no record of how they stack.
func WriteManifest(path string, m Manifest) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return fmt.Errorf("writing durable-dir chain manifest %q: %w", path, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("installing durable-dir chain manifest %q: %w", path, err)
	}
	return nil
}

// DescribeChain builds a manifest for the chain whose top layer is topPath, by
// walking the backing links rather than trusting any existing manifest. This
// is what a suspend records: the headers are the source of truth for what the
// guest was actually reading through.
func DescribeChain(ctx context.Context, topPath string) (Manifest, error) {
	var m Manifest
	chain, err := BackingChain(ctx, topPath)
	if err != nil {
		return m, err
	}
	dir := filepath.Dir(topPath)
	// BackingChain returns top first; a manifest is base first.
	for i := len(chain) - 1; i >= 0; i-- {
		// Every layer must back onto a sibling. That is what lets the chain be
		// moved between nodes without rewriting headers, and a chain that has
		// lost the property still WORKS in place — it breaks on the next node
		// instead — so it has to be caught here.
		//
		// A bare name is what this package writes; an absolute path into the
		// same directory is accepted because some qemu-img versions report the
		// resolved name here rather than the recorded one.
		if b := chain[i].BackingFilename; b != "" && filepath.Dir(b) != "." && filepath.Dir(b) != dir {
			return m, fmt.Errorf("chain layer %q backs onto %q, which is outside its own directory %q; the chain cannot be relocated",
				chain[i].Filename, b, dir)
		}
		name := filepath.Base(chain[i].Filename)
		path := filepath.Join(dir, name)
		st, err := os.Stat(path)
		if err != nil {
			return m, fmt.Errorf("stat-ing chain layer %q: %w", path, err)
		}
		m.Layers = append(m.Layers, Layer{File: name, SizeBytes: st.Size()})
	}
	m.VirtualSizeBytes = chain[len(chain)-1].VirtualSize
	return m, nil
}

// VerifyPresent checks that every layer the manifest names exists in dir, is a
// qcow2, and is the recorded size; it reports the FIRST one that is not.
//
// The alternative — letting qemu discover it — reports "could not open backing
// file" for whichever layer names the missing one, which is the layer in front
// of the hole rather than the hole. Since the recovery for a missing layer is
// to re-fetch it, naming the right file is the whole value of the check.
func (m Manifest) VerifyPresent(dir string) error {
	for _, l := range m.Layers {
		path := filepath.Join(dir, l.File)
		st, err := os.Stat(path)
		if err != nil {
			return fmt.Errorf("durable-dir chain layer %q is missing: %w", l.File, err)
		}
		if l.SizeBytes != 0 && st.Size() != l.SizeBytes {
			return fmt.Errorf("durable-dir chain layer %q is %d bytes; the manifest records %d", l.File, st.Size(), l.SizeBytes)
		}
		// A layer that is the right size and not an image at all is what a
		// download that fetched an error page looks like.
		img, err := IsImage(path)
		if err != nil {
			return fmt.Errorf("reading durable-dir chain layer %q: %w", l.File, err)
		}
		if !img {
			return fmt.Errorf("durable-dir chain layer %q is not a qcow2 image", l.File)
		}
	}
	return nil
}

// LayerFiles returns the manifest's layer filenames, base first.
func (m Manifest) LayerFiles() []string {
	out := make([]string, 0, len(m.Layers))
	for _, l := range m.Layers {
		out = append(out, l.File)
	}
	return out
}

// NextLayerName returns the filename for the layer that goes on top of the
// chain made of existing, given by bare filename in any order. Sequence-
// numbered rather than content-addressed so that the order is legible in a
// directory listing and in a checkpoint's file set — which matters because an
// operator looking at either is usually asking "how deep has this got".
//
// One past the highest number in use, rather than the number of layers. The two
// agree until a flatten runs, and a flatten collapses layers WITHOUT renaming
// the one that survives — it has to, since the layer above records its backing
// file by name and is open in a running guest. Numbering by count after that
// would hand a new layer the name of one a live header still points at.
//
// Numbers therefore climb for the life of an actor rather than tracking its
// depth, and past 9999 the names grow a digit and stop sorting with their
// predecessors. Nothing reads chain order out of the names — the headers carry
// it — so that costs legibility in a directory listing, not correctness.
func NextLayerName(prefix string, existing []string) string {
	next := 0
	for _, name := range existing {
		if n, ok := layerNumber(prefix, name); ok && n >= next {
			next = n + 1
		}
	}
	return fmt.Sprintf("%s%04d.qcow2", prefix, next)
}

// layerNumber pulls the sequence number back out of a layer filename, and
// reports whether the name was one of this package's at all.
func layerNumber(prefix, name string) (int, bool) {
	s, ok := strings.CutPrefix(name, prefix)
	if !ok {
		return 0, false
	}
	s, ok = strings.CutSuffix(s, ".qcow2")
	if !ok {
		return 0, false
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}
