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

# Base image for cmd/ateom-microvm, adding erofs-utils to what .ko.yaml
# otherwise uses directly.
#
# ateom serves durable-dir volumes from an erofs image when
# ATEOM_ARCHIVE_FORMAT=erofs, which needs two binaries the base image does not
# carry: mkfs.erofs to build one at suspend, and fsck.erofs to unpack one on a
# host that cannot mount it. Both ship in erofs-utils. ko builds from a base
# image rather than a Dockerfile, so they cannot be installed by ko itself.
#
# Build and push, then point ko at it without editing the checked-in config:
#
#   docker buildx build --platform linux/amd64 --push \
#     -t "$KO_DOCKER_REPO/ateom-base:dev" hack/images/ateom-base
#   tmp=$(mktemp -d)
#   sed "s|debian:stable-slim|$KO_DOCKER_REPO/ateom-base:dev|" .ko.yaml > "$tmp/.ko.yaml"
#   KO_CONFIG_PATH="$tmp" ./hack/install-ate.sh ...
FROM debian:stable-slim

RUN apt-get update \
  && apt-get install -y --no-install-recommends erofs-utils \
  && rm -rf /var/lib/apt/lists/*
