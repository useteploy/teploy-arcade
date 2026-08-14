# Build
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath \
      -ldflags="-s -w -X main.version=${VERSION}" \
      -o /out/teploy-arcade ./cmd/teploy-arcade

# Run
FROM alpine:3.20
RUN apk add --no-cache ca-certificates docker-cli tzdata
WORKDIR /app
# The frontend is embedded in the binary, so nothing else ships.
COPY --from=build /out/teploy-arcade /app/teploy-arcade

# State lives on a bind mount, deliberately NOT a named volume: the panel starts
# sibling game containers, and their bind mounts are resolved by the daemon on
# the host. Mount a host directory at the same path in here -
#   -v /var/teploy-arcade:/var/teploy-arcade
# - or pass -data-host so the panel can translate. Otherwise game servers get
# empty directories and boot blank worlds.
EXPOSE 3457

# The agent drives game containers through the host's Docker socket, so it is a
# sibling of the containers it manages, not their parent. The socket has to be
# mounted in for the docker runtime to work:
#   -v /var/run/docker.sock:/var/run/docker.sock
#
# Bind to 0.0.0.0 inside the container; Caddy terminates TLS in front of it.
ENTRYPOINT ["/app/teploy-arcade"]
CMD ["-host", "0.0.0.0", "-port", "3457", "-data", "/var/teploy-arcade"]
