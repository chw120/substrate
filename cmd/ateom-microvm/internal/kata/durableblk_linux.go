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

package kata

// Durable-dir volumes served from a virtio-blk disk.
//
// On the virtio-fs arrangement the host assembles the durable tree and
// virtiofsd serves it; the guest mounts nothing of its own. On this one the
// host hands over a qcow2 image and the GUEST mounts the ext4 inside it,
// through the same kata-agent storage mechanism that mounts the shared
// filesystem. Every durable volume is a top-level directory in that one
// filesystem, so the tree the containers bind from is laid out exactly as it
// is under the share — only its origin differs.

import (
	"github.com/agent-substrate/substrate/cmd/ateom-microvm/internal/third_party/kata/agentpb"
	"github.com/agent-substrate/substrate/internal/ocispec"
)

const (
	// virtioBlkDriver is the agent storage driver for a virtio-blk disk. With a
	// Source that already starts with /dev the agent uses the device node as
	// given, rather than resolving a PCI path to one.
	virtioBlkDriver = "blk"
	// typeExt4 is the filesystem inside a durable image (see qcow2.CreateBase).
	typeExt4 = "ext4"
)

// DurableDiskDevice is the guest device node of the durable disk.
//
// cloud-hypervisor enumerates virtio-blk devices in VmConfig.Disks order, and
// ateom passes exactly two: the read-only kata guest image, then the actor's
// durable image. So the guest sees /dev/vda and /dev/vdb, and the second is
// this one. The coupling is real but small, and the alternative — having the
// guest find the disk by its ext4 label — trades it for a udev settle in the
// boot path.
const DurableDiskDevice = "/dev/vdb"

// DurableDiskMountOptions are how the guest mounts that filesystem.
//
// commit=1 is the concession this arrangement makes to losing write-through.
// On the virtio-fs path a completed guest write was on the host immediately;
// here it sits in guest page cache until ext4 commits, and a suspend that
// discards guest memory loses whatever has not. One second bounds that window
// without the per-write cost of a sync mount — it is a floor on the damage, not
// a substitute for the explicit flush a suspend does (see
// flushGuestFilesystems).
var DurableDiskMountOptions = []string{"rw", "noatime", "data=ordered", "commit=1", "errors=remount-ro"}

// CreateSandboxOpts are the pieces of sandbox setup that vary per actor.
type CreateSandboxOpts struct {
	SandboxID string
	Hostname  string
	// DurableDisk asks the agent to mount the durable-dir disk at
	// ocispec.GuestDurableDir as part of sandbox creation. False leaves the
	// actor on the virtio-fs arrangement, where durable volumes arrive through
	// the shared filesystem and the guest mounts nothing extra.
	DurableDisk bool
}

// durableDiskStorage is the agent storage entry that mounts it.
func durableDiskStorage() *agentpb.Storage {
	return &agentpb.Storage{
		Driver:     virtioBlkDriver,
		Source:     DurableDiskDevice,
		Fstype:     typeExt4,
		Options:    DurableDiskMountOptions,
		MountPoint: ocispec.GuestDurableDir,
	}
}
