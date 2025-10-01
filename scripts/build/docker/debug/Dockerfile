# Copyright (c) 2024 The Jaeger Authors.
# SPDX-License-Identifier: Apache-2.0

FROM golang:1.25.1-alpine@sha256:b6ed3fd0452c0e9bcdef5597f29cc1418f61672e9d3a2f55bf02e7222c014abd AS build
ARG TARGETARCH
ENV GOPATH /go
RUN apk add --update --no-cache ca-certificates make git build-base mailcap

COPY go.mod go.sum /go/src/debug-delve/ 

# TODO: Remove s390x once go-delve adds support for it (https://github.com/go-delve/delve/issues/2883)
# TODO: Remove ppc64le once support is released (https://github.com/go-delve/delve/issues/1564) - not yet as of delve@v1.22.1
RUN if [[ "$TARGETARCH" == "s390x" ||  "$TARGETARCH" == "ppc64le" ]] ; then \
	touch /go/bin/dlv; \
    else \
        cd /go/src/debug-delve && go mod download && go build -o /go/bin/dlv github.com/go-delve/delve/cmd/dlv; \
    fi 

FROM golang:1.25.1-alpine@sha256:b6ed3fd0452c0e9bcdef5597f29cc1418f61672e9d3a2f55bf02e7222c014abd
COPY --from=build /go/bin/dlv /go/bin/dlv
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /etc/mime.types /etc/mime.types
