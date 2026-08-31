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

package kata

import (
	"slices"
	"testing"

	"github.com/agent-substrate/substrate/internal/ocispec"
)

// The storage entry is what the guest agent acts on to mount the durable disk,
// and every field in it is load-bearing. The device is found by enumeration
// order rather than by any identifier the host controls, so it must match the
// position buildVMConfig gives the disk; the mount point must be the directory
// the container specs bind their durable volumes out of.
func TestDurableDiskStorage(t *testing.T) {
	s := durableDiskStorage()
	if s.GetDriver() != virtioBlkDriver {
		t.Errorf("Driver = %q, want %q", s.GetDriver(), virtioBlkDriver)
	}
	if s.GetSource() != DurableDiskDevice {
		t.Errorf("Source = %q, want %q", s.GetSource(), DurableDiskDevice)
	}
	if s.GetFstype() != typeExt4 {
		t.Errorf("Fstype = %q, want %q", s.GetFstype(), typeExt4)
	}
	if s.GetMountPoint() != ocispec.GuestDurableDir {
		t.Errorf("MountPoint = %q, want %q", s.GetMountPoint(), ocispec.GuestDurableDir)
	}
	if !slices.Equal(s.GetOptions(), DurableDiskMountOptions) {
		t.Errorf("Options = %v, want %v", s.GetOptions(), DurableDiskMountOptions)
	}

	// A read-only mount would fail the actor's first write, and commit=1 is what
	// bounds the data at risk in the window between the pre-pause flush and the
	// pause itself. Both are worth failing a test over if they are dropped.
	if slices.Contains(s.GetOptions(), "ro") {
		t.Error("the durable disk is mounted read-only; the actor cannot write to it")
	}
	if !slices.Contains(s.GetOptions(), "commit=1") {
		t.Error("the durable disk is mounted without commit=1; a suspend could lose seconds of writes")
	}
}
