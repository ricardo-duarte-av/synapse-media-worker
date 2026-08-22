FROM golang:1.26-alpine AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY *.go ./

ARG TAG=dev
ARG COMMIT=unknown
ARG BUILD_TIME=unknown

# CGO is not needed: every image decoder and encoder used is pure Go, so the
# result is a static binary that runs on any base.
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags="-s -w \
      -X main.Version=${TAG} \
      -X main.Commit=${COMMIT} \
      -X main.BuildTime=${BUILD_TIME}" \
    -o /synapse-media-worker .

FROM alpine:3.21
# curl is here for the compose healthcheck, which probes the worker's own
# unix socket.
RUN apk add --no-cache ca-certificates tzdata curl

COPY --from=build /synapse-media-worker /usr/local/bin/synapse-media-worker

# The worker reads Synapse's media store and writes only its own cache. Running
# as a non-root user makes an accidental write to a bind mount fail loudly.
# The uid must be able to read media_store; override with `user:` in compose if
# Synapse's files are owned by something other than 991.
RUN adduser -D -u 991 -g '' mediaworker
USER mediaworker

ENTRYPOINT ["/usr/local/bin/synapse-media-worker"]
CMD ["-config", "/data/config.yaml"]
