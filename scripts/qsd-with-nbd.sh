#!/bin/bash
# SPDX-License-Identifier: Apache-2.0

set -o errexit -o pipefail -o nounset -o xtrace

qcow2_file_path="$1"
out_dev_path="$2"
readonly="$3"  # must be "true" or "false"

case "${readonly}" in
    true)
        extra_qsd_export_options=writable=off
        extra_nbd_connect_flags=-readonly
        ;;
    false)
        extra_qsd_export_options=writable=on
        extra_nbd_connect_flags=
        ;;
    *)
        exit 2
        ;;
esac

# launch qemu-storage-daemon

function qsd() {
    qemu-storage-daemon \
        --blockdev driver=file,node-name=file,filename="${qcow2_file_path}","$1" \
        --blockdev driver=qcow2,node-name=qcow2,file=file \
        --nbd-server addr.type=unix,addr.path=qsd.sock \
        --export type=nbd,id=export,name=default,node-name=qcow2,"${extra_qsd_export_options}" \
        --daemonize \
        --pidfile qsd.pid
}

qsd cache.direct=on ||
    qsd cache.direct=off  # some file systems don't support O_DIRECT (e.g., tmpfs)

qsd_pid="$( cat qsd.pid )"

function stop_qsd() {
    # Attempt graceful termination. If this blocks, we'll eventually be killed.
    kill "${qsd_pid}" || true
    while kill -0 "${qsd_pid}" 2>/dev/null; do sleep 1; done
}
trap stop_qsd EXIT

# configure NBD client

function nbd_dev_is_connected() {
    local exit_code
    exit_code=0
    nbd-client -c "$1" || exit_code="$?"
    case "${exit_code}" in
        0) return 0 ;;
        1) return 1 ;;
        *) exit 1   ;; # failed to determine whether device is connected
    esac
}

function setup_device() {
    # shellcheck disable=SC2044
    for dev in $( find /dev -regex '/dev/nbd[0-9]+' | shuf ); do
        if ! nbd_dev_is_connected "${dev}"; then
            # $dev isn't connected, so we try to take it. This is racy, as
            # someone else might try to do the same at the same time. If this
            # happens, our nbd-client invocation can actually undo their
            # configuration and cause the device to be disconnected, or vice
            # versa (but our nbd-client invocation shouldn't fail even in those
            # cases). We thus subsequently check that we actually succeeded to
            # connect the device, otherwise we assume that someone else is
            # trying to configure it and move on to try the next device.

            nbd-client \
                -unix qsd.sock "${dev}" \
                -name default -connections 1 -no-optgo -nonetlink \
                ${extra_nbd_connect_flags}

            if nbd_dev_is_connected "${dev}"; then
                return 0
            fi
        fi
    done

    # couldn't find any available device
    return 1
}

setup_device

[[ $( blockdev --getsize64 "${dev}" ) != 0 ]]  # sanity check

trap 'nbd-client -nonetlink -d "${dev}"; stop_qsd' EXIT

# expose device at the target path

rmdir "${out_dev_path}" || true  # Kubernetes might place a directory there
[[ ! -d "${out_dev_path}" ]]  # must not exist or not be a directory

cp -fpR "${dev}" "${out_dev_path}"

# wait until the container is asked to terminate

# If we simply invoked sleep, we wouldn't be able to react to SIGTERM, even if
# we installed the trap beforehand, because we are the init process (PID 1).
sleep infinity &
trap "kill %1" TERM
wait || true
