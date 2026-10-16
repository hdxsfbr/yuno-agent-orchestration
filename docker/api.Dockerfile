# syntax=docker/dockerfile:1.7
FROM golang:1.26-alpine AS build
WORKDIR /src
ENV CGO_ENABLED=0 GOFLAGS=-buildvcs=false
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY cmd ./cmd
COPY internal ./internal
COPY api ./api
COPY templates ./templates
# The commit the image is built from (make run and make build pass it); /api/status and
# the Status page show it. Declared just before the build, so a new version reruns only
# that step.
ARG VERSION=dev
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go build -trimpath -ldflags="-s -w -X yuno/internal/config.Version=${VERSION}" -o /out/yuno ./cmd/yuno

FROM alpine:3.21 AS runtime
RUN apk add --no-cache ca-certificates tzdata wget && adduser -D -u 1000 app \
    && mkdir -p /data && chown app:app /data
WORKDIR /src
COPY --from=build /out/yuno /usr/local/bin/yuno
COPY --from=build /src/templates ./templates
# /data is the deploytarget release volume; the named volume inherits this ownership
# when it is first created.
USER app
ENTRYPOINT ["yuno"]
CMD ["api"]
