# The operator interface, built first.
#
# webui/embed.go embeds webui/dist into the binary, and the repository carries
# only a placeholder there — so a Go build with no UI build in front of it
# produces a binary that serves a page saying exactly that. In an image, that
# would be discovered by whoever opened it in production, so the image builds
# the interface itself rather than trusting whatever the build host happened to
# have lying around.
FROM node:22-alpine AS ui

WORKDIR /ui

# The lockfile is its own layer: a source change should not re-resolve every
# dependency.
COPY webui/package.json webui/package-lock.json ./
RUN npm ci

COPY webui/ ./
RUN npm run build

# Build stage.
FROM golang:1.26-alpine AS build

WORKDIR /src

# Dependencies are their own layer so a source change does not re-download the
# module cache.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
COPY --from=ui /ui/dist ./webui/dist

# Passed by `make docker` from the same values the Makefile stamps a local
# build with. They default to something a human can recognise as unstamped
# rather than to something that looks like a release.
ARG VERSION=unknown
ARG GIT_COMMIT=unknown
ARG BUILD_TIME=unknown
ARG TREE_STATE=unknown

# The ldflag path matters and used to be wrong: it set main.version, which
# nothing reads. buildinfo is what the binary reports, what /health returns,
# and what production checks — an unidentified build refuses to serve, so a
# mis-stamped image failed at start-up with a message about a build nobody
# could trace.
ENV BUILDINFO=flowed/internal/platform/buildinfo

# CGO is off so the result runs on a minimal base. The migrations are embedded
# into the binary, so the deployed artifact carries exactly the schema it was
# built and tested against.
RUN CGO_ENABLED=0 GOOS=linux go build \
        -trimpath \
        -ldflags "-s -w \
            -X ${BUILDINFO}.version=${VERSION} \
            -X ${BUILDINFO}.commit=${GIT_COMMIT} \
            -X ${BUILDINFO}.buildTime=${BUILD_TIME} \
            -X ${BUILDINFO}.treeState=${TREE_STATE}" \
        -o /out/api ./cmd/api \
 && CGO_ENABLED=0 GOOS=linux go build \
        -trimpath \
        -ldflags "-s -w \
            -X ${BUILDINFO}.version=${VERSION} \
            -X ${BUILDINFO}.commit=${GIT_COMMIT} \
            -X ${BUILDINFO}.buildTime=${BUILD_TIME} \
            -X ${BUILDINFO}.treeState=${TREE_STATE}" \
        -o /out/migrate ./cmd/migrate

# The image is built and then asked to identify itself, so a mis-stamped build
# fails here rather than in production.
RUN /out/api version

# Runtime stage.
FROM alpine:3.23

# ca-certificates for outbound TLS; tzdata because report boundaries and
# installment due dates are evaluated against Asia/Baghdad even though every
# timestamp is stored in UTC. postgresql-client so the backup and restore-drill
# scripts can run from this image rather than needing a second one on the host.
#
# The client major version must be at least the server's: pg_dump refuses to
# dump from a server newer than itself, so a client behind the deployed
# PostgreSQL turns the nightly backup into a nightly error. It tracks the
# server in deploy/docker-compose.prod.yml — 18 — and the base image is chosen
# for carrying that client, which is why this is not the same Alpine the build
# stage happens to use.
#
# bash because the operational scripts are bash and are shared with the systemd
# deployment, where it is always present. Without it the nightly backup and the
# weekly restore drill fail with "can't execute bash" — the backup job's own
# failure mode being invisible until the morning it is needed is exactly what
# those scripts exist to prevent.
RUN apk add --no-cache ca-certificates tzdata postgresql18-client bash \
 && adduser -D -u 10001 -h /app app

WORKDIR /app
COPY --from=build /out/api /app/api
COPY --from=build /out/migrate /app/migrate
COPY scripts/backup.sh scripts/restore-drill.sh scripts/lib.sh /app/scripts/

USER app
EXPOSE 8080

# The metrics listener is deliberately not exposed: it binds loopback by
# default and its bind address is its only access control. A deployment that
# scrapes from another host publishes it explicitly.

HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD ["/app/api", "healthcheck"]

ENTRYPOINT ["/app/api"]
