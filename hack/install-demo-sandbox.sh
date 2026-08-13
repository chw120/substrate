#!/usr/bin/env bash

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
#
# This is sourced as part of install-ate.sh. Do not run directly.

ATE_DEMOS+=(demo-sandbox) # register demo-sandbox

# The matched action is recorded in DEMO_ACTION rather than run here: the
# dispatch loop in install-ate.sh (which sources this file) runs it with
# errexit in effect, so a failing deploy actually fails the script.
# shellcheck disable=SC2034 # DEMO_ACTION is read by install-ate.sh
demo-sandbox_cmdline() {
  case "${1}" in
    --deploy-demo-sandbox) DEMO_ACTION=(demo-sandbox_deploy) ;;
    --delete-demo-sandbox) DEMO_ACTION=(demo-sandbox_delete) ;;
    *)
      return 1
      ;;
  esac
  return 0
}

demo-sandbox_deploy() {
  log_step "demo-sandbox_deploy"
  ensure_crds
  sed "s|\${BUCKET_NAME}|${BUCKET_NAME}|g" demos/sandbox/sandbox.yaml.tmpl \
    | run_ko apply -f -
}

demo-sandbox_delete() {
  log_step "demo-sandbox_delete"
  delete_demo_actors ate-demo-sandbox sandbox-template
  sed "s|\${BUCKET_NAME}|${BUCKET_NAME}|g" demos/sandbox/sandbox.yaml.tmpl \
    | run_kubectl delete --ignore-not-found -f -
}
