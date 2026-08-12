# Copyright 2026 Google LLC
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

"""Owner of --glutton-ram-bytes, the guest memory water level.

Unlike the other boomer-tunable flags, this one has no Python-side consumer:
GluttonUser load is driven entirely by the boomer-Go worker, which reads the
value over /boomer-config and issues WriteRAM to its actor each iteration.
Registration still has to happen here so locust accepts the flag on the
master's argv and renders it in the web UI form.

A zero value (the default) means "no WriteRAM call", which is the behavior
every test had before this knob existed.
"""

from locust import events
from locust.argument_parser import LocustArgumentParser

_initialized = False


def init_glutton_ram() -> None:
    """Register the --glutton-ram-bytes command line flag."""
    global _initialized
    if _initialized:
        return

    @events.init_command_line_parser.add_listener
    def on_init_parser(parser: LocustArgumentParser) -> None:
        parser.add_argument(
            "--glutton-ram-bytes",
            type=float,
            default=0.0,
            env_var="LOCUST_GLUTTON_RAM_BYTES",
            help=(
                "Bytes of RAM each glutton actor is told to hold resident, "
                "rewritten every iteration. 0 disables the WriteRAM call. "
                "Must fit in an int32 (max 2147483647)."
            ),
            include_in_web_ui=True,
        )

    _initialized = True
