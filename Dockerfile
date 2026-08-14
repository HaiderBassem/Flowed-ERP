# Build stage.
FROM golang:1.26-alpine AS build

WORKDIR /src

# Dependencies are their own layer so a source change does not re-download the
# module cache.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG VERSION=dev
# CGO is off so the result runs on a scratch base. The migrations are embedded
# into the binary, so the deployed artifact carries exactly the schema it was
# built and tested against.
RUN CGO_ENABLED=0 GOOS=linux go build \
        -trimpath \
        -ldflags "-s -w -X main.version=${VERSION}" \
        -o /out/api ./cmd/api \
 && CGO_ENABLED=0 GOOS=linux go build \
        -trimpath \
        -ldflags "-s -w -X main.version=${VERSION}" \
        -o /out/migrate ./cmd/migrate

# Runtime stage.
FROM alpine:3.20

# ca-certificates for outbound TLS; tzdata because report boundaries and
# installment due dates are evaluated against Asia/Baghdad even though every
# timestamp is stored in UTC.
RUN apk add --no-cache ca-certificates tzdata \
 && adduser -D -u 10001 -h /app app

WORKDIR /app
COPY --from=build /out/api /app/api
COPY --from=build /out/migrate /app/migrate

USER app
EXPOSE 8080

HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD ["/app/api", "healthcheck"]

ENTRYPOINT ["/app/api"]
