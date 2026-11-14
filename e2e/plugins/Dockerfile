# Fixture plugin images for the end-to-end suite. The server is plain Node, so
# the agent image (which ships Node 22) can run it inline, and the same tree
# runs behind the shim as a sidecar or a service.
#
# plugin: a valid plugin image with everything under /mayfly-plugin.
# missing: the same image without /mayfly-plugin, so the copy init container
#          fails and the session must fail at start.

FROM node:22-bookworm-slim AS plugin
COPY server.js /mayfly-plugin/server.js
RUN chmod 0644 /mayfly-plugin/server.js \
 && chmod 0755 /mayfly-plugin

FROM scratch AS missing
COPY README /not-a-plugin-dir/README
