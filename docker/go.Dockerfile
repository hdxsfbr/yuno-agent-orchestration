# syntax=docker/dockerfile:1.7
# The Go tool container (compose service `go`: tests, vet, tidy). The upstream alpine
# image carries no git, which the project repository tests need to clone, commit and
# push against a local bare repository (DESIGN 5.16 tests).
FROM golang:1.26-alpine
RUN apk add --no-cache git
