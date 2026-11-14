# The sidecar and service containers run this image directly. It holds one
# directory, /mayfly-plugin/, with the static binary.
FROM golang:1.27-bookworm AS build
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /out/notify .

FROM scratch
COPY --from=build /out/notify /mayfly-plugin/bin/notify
