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

# Per-employee OpenClaw actor image: stock public OpenClaw plus a fleet
# config, a fixed persona, and the test probe. Adapted from
# github.com/agent-substrate/always-on-agent build/actor.Dockerfile.
#
# The probe is compiled in (not a second container) because Substrate
# release-0.1 fails to FULL-restore multi-container actors — see the README
# gap list. Build context is the demo directory.
#
# The base is pinned by digest on purpose: a golden snapshot is only valid
# for the exact image it was taken from, so a floating tag would silently
# invalidate every snapshot the moment upstream pushes. To move to a newer
# OpenClaw, bump the digest and roll the fleet to a new ActorTemplate
# (rolling-update.sh).
#
#   crane digest ghcr.io/openclaw/openclaw:2026.8.2-slim
FROM golang:1.27-alpine AS probe-build
COPY probe/ /src/
RUN cd /src && go mod init fleet-probe >/dev/null 2>&1 && CGO_ENABLED=0 go build -o /fleet-probe .

FROM ghcr.io/openclaw/openclaw@sha256:5d25165995041caa6a7175bec82b25ad98c44eb269bb42435da8e27ec06e6be4
USER 0

# Enables OpenClaw's OpenAI-compatible HTTP API on port 80 (the primary actor
# port) and trusts atenet's link-local delivery address; see openclaw.json.
COPY build/openclaw.json /home/node/.openclaw/openclaw.json

# Fixed identity so OpenClaw does not run its persona-bootstrap flow on every
# employee's first activation.
RUN mkdir -p /home/node/.openclaw/workspace
COPY build/SOUL.md /home/node/.openclaw/workspace/SOUL.md
COPY build/IDENTITY.md /home/node/.openclaw/workspace/IDENTITY.md
RUN chown -R 1000:1000 /home/node/.openclaw

COPY --from=probe-build /fleet-probe /usr/local/bin/fleet-probe
COPY build/fleet-entrypoint.sh /usr/local/bin/fleet-entrypoint.sh
RUN chmod 0755 /usr/local/bin/fleet-entrypoint.sh /usr/local/bin/fleet-probe

USER 1000
# Keep tini as PID 1 (it is the base image's entrypoint) so the background
# probe is reaped; only the command changes.
ENTRYPOINT ["tini", "-s", "--"]
CMD ["/usr/local/bin/fleet-entrypoint.sh"]
