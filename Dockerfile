# SPDX-License-Identifier: Apache-2.0

FROM golang:1.19 AS builder

WORKDIR /subprovisioner

COPY go.mod go.sum ./
RUN go mod download

COPY cmd/ cmd/
COPY pkg/ pkg/

RUN go build -o bin/csi-plugin ./cmd/csi-plugin

# quay.io/centos/centos:stream9 doesn't package nbd-client
FROM fedora:37

RUN dnf install -qy jq nbd qemu-img && dnf clean all

WORKDIR /subprovisioner
COPY --from=builder /subprovisioner/bin/csi-plugin ./
COPY scripts/ ./

ENTRYPOINT [ "bash" ]
