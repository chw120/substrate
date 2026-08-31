# Copyright 2026 Google LLC
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#      http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# Base image for cmd/ateom-microvm, adding the qcow2 tooling to what .ko.yaml
# otherwise uses directly.
#
# ateom serves durable-dir volumes from a qcow2 disk image when
# ATEOM_DURABLE_BACKEND=qcow2, which needs two binaries the base image does not
# carry: qemu-img, to create, stack, flatten and check the backing chain, and
# mkfs.ext4, to make the filesystem inside the base layer once. They ship in
# qemu-utils and e2fsprogs. ko builds from a base image rather than a
# Dockerfile, so they cannot be installed by ko itself.
#
# Neither is on the guest's IO path: cloud-hypervisor reads and writes the qcow2
# itself, and these run only at the boundaries (cold boot, restore, suspend).
#
# Build and push, then point ko at it without editing the checked-in config:
#
#   docker buildx build --platform linux/amd64 --push \
#     -t "$KO_DOCKER_REPO/ateom-base:dev" hack/images/ateom-base
#   tmp=$(mktemp -d)
#   sed "s|debian:stable-slim|$KO_DOCKER_REPO/ateom-base:dev|" .ko.yaml > "$tmp/.ko.yaml"
#   KO_CONFIG_PATH="$tmp" ./hack/install-ate.sh ...
FROM debian:stable-slim

# qemu-utils pulls in a good deal of qemu's block layer; --no-install-recommends
# keeps it to that rather than the full emulator.
RUN apt-get update \
  && apt-get install -y --no-install-recommends qemu-utils e2fsprogs \
  && rm -rf /var/lib/apt/lists/*
