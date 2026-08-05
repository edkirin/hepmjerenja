# ── Stage 1: Build the Go binary ──
FROM golang:1.25-alpine AS builder
WORKDIR /src

COPY go.mod go.sum ./
COPY . .

RUN \
    apk add --no-cache ca-certificates && \
    go mod download && \
    # Install templ and generate template files. The CLI version must match
    # the templ runtime version pinned in go.mod, otherwise the generated code
    # may reference symbols that don't exist in the runtime.
    go install github.com/a-h/templ/cmd/templ@v0.3.1001 && \
    templ generate ./app/templates/ && \
    CGO_ENABLED=0 GOOS=linux go build -o /hepmjerenja ./app/.

# ── Stage 2: Minimal runtime image ──
FROM alpine:latest
WORKDIR /app

RUN apk add --no-cache tzdata ca-certificates

# Pre-create the data and log directories owned by the runtime user (33). A named
# volume mounted at either path inherits this ownership, so the app can create the
# SQLite database (plus its -wal and -shm files) and write its log files.
RUN mkdir -p /app/data /var/log/apps/hepmjerenja && \
    chown -R 33:33 /app/data /var/log/apps/hepmjerenja

COPY --from=builder /hepmjerenja .

EXPOSE 8000

USER 33

ENTRYPOINT ["./hepmjerenja"]
