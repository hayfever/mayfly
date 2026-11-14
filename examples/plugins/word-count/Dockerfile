# Node strips the type annotations on load, so the image carries the source.
# Mayfly never runs the image entrypoint, so the final stage can be the base
# image; only /mayfly-plugin/ matters.
FROM node:26-bookworm-slim
WORKDIR /mayfly-plugin
COPY package.json tsconfig.json ./
COPY src ./src
