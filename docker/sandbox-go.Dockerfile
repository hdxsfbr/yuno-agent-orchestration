# syntax=docker/dockerfile:1.7
# Sandbox image for project repositories whose toolchain is go (DESIGN 5.16 rule 4): the
# Go toolchain (go build, go test, go mod) plus git and curl from the upstream image.
# Nothing project-specific is baked in; a run with sandbox network downloads the
# project's own modules. The root filesystem is read-only at runtime, so GOPATH and
# the build and module caches live in the agent's workspace (bind-mounted
# read-write at /workspace), in hidden directories the platform never copies forward
# or packages.
FROM golang:1.26-bookworm
# The gateway runs exec with its own PATH (the usual system directories) and HOME=/workspace
# [probed], so the toolchain must be reachable from /usr/local/bin.
RUN useradd -u 1000 -m -s /bin/bash sandbox \
    && ln -s /usr/local/go/bin/go /usr/local/bin/go && ln -s /usr/local/go/bin/gofmt /usr/local/bin/gofmt
ENV XDG_CACHE_HOME=/workspace/.cache \
    GOPATH=/workspace/.cache/go \
    GOMODCACHE=/workspace/.cache/go/pkg/mod \
    GOCACHE=/workspace/.cache/go/build \
    GOFLAGS=-buildvcs=false
USER sandbox
WORKDIR /workspace
CMD ["sleep", "infinity"]
