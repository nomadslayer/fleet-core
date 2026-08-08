# --- build stage: compile server + both agent arches ---
FROM golang:1.22-alpine AS build
WORKDIR /src
COPY . .
ENV CGO_ENABLED=0
RUN go build -trimpath -ldflags="-s -w" -o /out/fleet-server ./cmd/fleet-server && \
    GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o /out/binaries/fleet-agent-linux-amd64 ./cmd/fleet-agent && \
    GOARCH=arm64 go build -trimpath -ldflags="-s -w" -o /out/binaries/fleet-agent-linux-arm64 ./cmd/fleet-agent && \
    mkdir -p /out/state

# --- runtime stage: distroless, non-root ---
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/fleet-server /usr/local/bin/fleet-server
COPY --from=build /out/binaries /srv/binaries
# The state dir must exist in the image and be owned by nonroot (65532):
# Docker seeds a fresh named volume from the image path, ownership
# included. Without it the volume lands root-owned and CA init fails
# with "mkdir /var/lib/fleet-server/ca: permission denied".
COPY --from=build --chown=65532:65532 /out/state /var/lib/fleet-server
VOLUME /var/lib/fleet-server
EXPOSE 8443 8080
ENTRYPOINT ["/usr/local/bin/fleet-server"]
CMD ["--data","/var/lib/fleet-server","--agent-addr",":8443","--admin-addr","0.0.0.0:8080","--binaries","/srv/binaries"]
