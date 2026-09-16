#!/bin/sh
# Single-container entrypoint: the workspace probe runs beside OpenClaw and
# tini (the image's entrypoint) reaps it.
#
# Substrate runs the container as uid 0 with cwd "/" — it honors the image's
# ENTRYPOINT/CMD and ENV but NOT its USER or WORKDIR. So: absolute paths only
# (a relative "openclaw.mjs" is "Cannot find module" from "/"), HOME must be
# set explicitly (uid 0 would resolve /root and miss the baked config), and
# --allow-unconfigured keeps the gateway's start guard from exiting 78 if
# config resolution ever lands elsewhere. Getting any of these wrong makes
# OpenClaw exit within seconds; Substrate does not notice, checkpoints the
# empty sandbox as the golden, and every restore then fails.
export HOME="${HOME:-/home/node}"
/usr/local/bin/fleet-probe --port=8080 --workspace=/workspace --rev="${FLEET_TEMPLATE_REV:-v1}" &
exec node /app/openclaw.mjs gateway --allow-unconfigured --bind lan --port 80
