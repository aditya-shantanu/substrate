#!/bin/sh
# Single-container entrypoint: the workspace probe runs beside OpenClaw
# because Substrate release-0.1 cannot FULL-restore an actor with more than
# one application container (gVisor "inconsistent private memory files on
# restore"); see the demo README's gap list. tini (the image's entrypoint)
# reaps the background probe.
/usr/local/bin/fleet-probe --port=8080 --workspace=/workspace --rev="${FLEET_TEMPLATE_REV:-v1}" &
exec node openclaw.mjs gateway
