# syntax=docker/dockerfile:1
#
# Builds cdampd as a static, pure-Go binary (modernc.org/sqlite needs no
# cgo, matching the project's own "one binary, no external services"
# design principle) and ships it in a minimal runtime image.
#
# Build:
#   docker build -t cdampd .
#
# Run (config and data live outside the container so they survive an
# image rebuild -- see README.md's Quick start for the full config
# shape). Two things differ from that config when running in a
# container:
#   - listen_addr must bind to 0.0.0.0, not 127.0.0.1 -- otherwise the
#     federation/local API is unreachable through the port mapping below,
#     since "localhost" inside the container is not the host's localhost.
#   - admin_bind_addr is deliberately left un-published (no -p for 8444):
#     the admin API/dashboard has no auth beyond a single bootstrap
#     cookie and is meant to stay off the public network, per
#     02-ARCHITECTURE.md's "bound to localhost by default" -- reach it
#     with `docker exec` + curl, or publish 8444 yourself if you
#     understand and accept that tradeoff.
#
#   docker run -d \
#     -p 8443:8443 \
#     -e CDAMPD_KEY_PASSPHRASE=<your passphrase> \
#     -v "$(pwd)/cdampd.yaml:/etc/cdampd/cdampd.yaml:ro" \
#     -v cdampd-data:/var/lib/cdampd \
#     cdampd

FROM golang:1.27-alpine AS builder

WORKDIR /src

# Cache module downloads in their own layer, separate from source changes.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/cdampd ./cmd/cdampd

FROM alpine:3.20

# Needed for the outbound HTTPS calls cdampd's Directory/Delivery adapters
# make when federating with other instances.
RUN apk add --no-cache ca-certificates && \
    adduser -D -H -u 10001 cdampd

COPY --from=builder /out/cdampd /usr/local/bin/cdampd

# The daemon's own default cdampd.yaml (see README.md) points sqlite_path
# here -- mount a volume over it to persist data across container
# restarts/rebuilds.
RUN mkdir -p /var/lib/cdampd && chown cdampd:cdampd /var/lib/cdampd
VOLUME ["/var/lib/cdampd"]

USER cdampd
WORKDIR /var/lib/cdampd

# listen_addr and admin_bind_addr, per the default config in README.md /
# install.sh.
EXPOSE 8443 8444

ENTRYPOINT ["cdampd"]
CMD ["--config", "/etc/cdampd/cdampd.yaml"]
