# SPDX-License-Identifier: Apache-2.0

FROM quay.io/centos/centos:stream9

RUN dnf install -qy diffutils qemu-img util-linux-core && dnf clean all

WORKDIR /test

ENTRYPOINT [ "bash" ]
