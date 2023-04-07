#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0

set -o errexit -o pipefail -o nounset

start_time="$( date +%s.%N )"
script_dir="$( realpath -e "$0" | xargs dirname )"
repo_root="$( realpath -e "${script_dir}/.." )"

# parse usage

fail_fast=0
unset k3s_image
k3s_image=rancher/k3s:v1.26.2-k3s1
pause_on_failure=0
pause_on_stage=0
num_agents=1
tests=()

while (( $# > 0 )); do
    case "$1" in
        --fail-fast)
            fail_fast=1
            ;;
        --k3s-image)
            shift
            k3s_image="$1"
            ;;
        --nodes)
            shift
            num_agents="$(( "$1" - 1 ))"
            ;;
        --pause-on-failure)
            pause_on_failure=1
            ;;
        --pause-on-stage)
            pause_on_stage=1
            ;;
        *)
            tests+=( "$1" )
            ;;
    esac
    shift
done

if (( "${#tests[@]}" == 0 )); then
    >&2 echo -n "\
Usage: $0 [<options...>] <tests...>
       $0 [<options...>] all

Run each given test against a temporary k3d cluster.

If invoked with a single \`all\` argument, all .sh files under t/ are run as
tests.

Options:
   --fail-fast          Cancel remaining tests after a test fails.
   --k3s-image <tag>    Use the given k3s image.
   --nodes              Number of nodes in the cluster, including the control
                        plane node, which can also run pods (default is 2).
   --pause-on-failure   Launch an interactive shell after a test fails.
   --pause-on-stage     Launch an interactive shell before each stage in a test.
"
    exit 2
fi

if (( "${#tests[@]}" == 1 )) && [[ "${tests[0]}" = all ]]; then
    tests=()
    for f in "${script_dir}"/t/*.sh; do
        tests+=( "$f" )
    done
fi

for test in "${tests[@]}"; do
    if [[ ! -e "${test}" ]]; then
        >&2 echo "Test file does not exist: ${test}"
        exit 1
    fi
done

# private definitions

# Usage: __elapsed
function __elapsed() {
    bc -l <<< "$( date +%s.%N ) - ${start_time}"
}

# Usage: __big_log <color> <format> <args...>
function __big_log() {
    local text term_cols sep_len
    text="$( printf "${@:2}" )"
    term_cols="$( tput cols 2> /dev/null )" || term_cols=80
    sep_len="$(( term_cols - ${#text} - 16 ))"
    printf "\033[%sm--- [%6.1f] %s " "$1" "$( __elapsed )" "${text}"
    printf '%*s\033[0m\n' "$(( sep_len < 0 ? 0 : sep_len ))" '' | tr ' ' -
}

# Usage: __log <color> <format> <args...>
function __log() {
    # shellcheck disable=SC2059
    printf "\033[%sm--- [%6.1f] %s\033[0m\n" \
        "$1" "$( __elapsed )" "$( printf "${@:2}" )"
}

# Usage: __log_red <format> <args...>
function __log_red() {
    __log 31 "$@"
}

# Usage: __log_yellow <format> <args...>
function __log_yellow() {
    __log 33 "$@"
}

# Usage: __log_cyan <format> <args...>
function __log_cyan() {
    __log 36 "$@"
}

# Usage: __failure <format> <args...>
function __failure() {
    __log_red "$@"

    if (( pause_on_failure )); then
        __log_red "Use the following to point kubectl at the k3d cluster:"
        __log_red "   export KUBECONFIG=${KUBECONFIG}"
        __log_red "Starting interactive shell with that kubeconfig..."
        ( cd "${temp_dir}" && "${BASH}" ) || true
    fi
}

# definitions shared with test scripts

export REPO_ROOT="${repo_root}"

# Usage: __stage <format> <args...>
function __stage() {
    (
        set -o errexit -o pipefail -o nounset +o xtrace

        # shellcheck disable=SC2059
        text="$( printf "$@" )"
        text_lower="${text,,}"

        if (( pause_on_stage )); then
            __log_yellow "Pausing before ${text_lower::1}${text:1}"
            __log_yellow "Use the following to point kubectl at the k3d cluster:"
            __log_yellow "   export KUBECONFIG=${KUBECONFIG}"
            __log_yellow "Starting interactive shell with that kubeconfig..."
            ( cd "${temp_dir}" && "${BASH}" )
        fi

        printf "\033[36m--- [%6.1f] %s\033[0m\n" "$( __elapsed )" "${text}"
    )
}

# Usage: __poll <retry_delay> <max_tries> <command>
function __poll() {
    (
        set -o errexit -o pipefail -o nounset +o xtrace

        for (( i = 1; i < "$2"; ++i )); do
            if eval "${*:3}"; then return 0; fi
            sleep "$1"
        done

        if eval "${*:3}"; then return 0; fi

        return 1
    )
}

# Usage: __pod_is_running <timeout_seconds> [-n=<pod_namespace>] <pod_name>
function __pod_is_running() {
    [[ "$( kubectl get pod "$@" -o=jsonpath='{.status.phase}' )" = Running ]]
}

# Usage: __wait_for_pod_to_succeed <timeout_seconds> [-n=<pod_namespace>] <pod_name>
function __wait_for_pod_to_succeed() {
    __poll 1 "$1" "[[ \"\$( kubectl get pod ${*:2} -o=jsonpath='{.status.phase}' )\" =~ ^Succeeded|Failed$ ]]"
    # shellcheck disable=SC2048,SC2086
    [[ "$( kubectl get pod ${*:2} -o=jsonpath='{.status.phase}' )" = Succeeded ]]
}

# Usage: __wait_for_pod_to_start_running <timeout_seconds> [-n=<pod_namespace>] <pod_name>
function __wait_for_pod_to_start_running() {
    __poll 1 "$1" "[[ \"\$( kubectl get pod ${*:2} -o=jsonpath='{.status.phase}' )\" =~ ^Running|Succeeded|Failed$ ]]"
}

# Usage: __wait_for_pvc_to_be_bound <timeout_seconds> [-n=<pvc_namespace>] <pvc_name>
function __wait_for_pvc_to_be_bound() {
    __poll 1 "$1" "[[ \"\$( kubectl get pvc ${*:2} -o=jsonpath='{.status.phase}' )\" = Bound ]]"
}

# Usage: __wait_for_vs_to_be_ready <timeout_seconds> [-n=<vs_namespace>] <vs_name>
function __wait_for_vs_to_be_ready() {
    __poll 1 "$1" "[[ \"\$( kubectl get vs ${*:2} -o=jsonpath='{.status.readyToUse}' )\" = true ]]"
}

# build Subprovisioner image

__log_cyan "Building Subprovisioner image (subprovisioner/subprovisioner:test)..."
docker image build -t "subprovisioner/subprovisioner:test" "${repo_root}"

__log_cyan "Building test image (subprovisioner/test:test)..."
docker image build -t subprovisioner/test:test "${script_dir}/image"

# create temporary directory

temp_dir="$( mktemp -d )"
trap 'rm -fr "${temp_dir}"' EXIT

# run tests

test_i=0
num_succeeded=0
num_failed=0

canceled=0
trap 'canceled=1' SIGINT

for test in "${tests[@]}"; do

    test_name="$( realpath --relative-to=. "${test}" )"
    test_resolved="$( realpath -e "${test}" )"

    __big_log 33 'Running test %s (%d of %d)...' \
        "${test_name}" "$(( ++test_i ))" "${#tests[@]}"

    __log_cyan 'Creating k3d cluster...'

    # This seems necessary to ensure kubelet can always configure loop devices.
    volumes=( "/dev:/dev@server:0;agent:*" )

    # Directory serving as the backing, shared, file system volume. It is
    # mounted on all k3d nodes.
    mkdir "${temp_dir}/backing"
    volumes+=( "${temp_dir}/backing:/var/backing@server:0;agent:*" )

    # Create ourselves the directory inside the backing volume in which volumes
    # will be stored, so that it doesn't end up being owned by root and we can
    # actually clean it up.
    mkdir "${temp_dir}/backing/volumes"

    trap '{
        k3d cluster delete subprovisioner-test
        rm -fr "${temp_dir}"
        }' EXIT

    # shellcheck disable=SC2068
    k3d cluster create \
        --agents "${num_agents}" \
        ${k3s_image:+"--image=${k3s_image}"} \
        --k3s-arg "--disable=metrics-server@server:0" \
        --kubeconfig-switch-context=false \
        --kubeconfig-update-default=false \
        --no-lb \
        ${volumes[@]/#/--volume } \
        subprovisioner-test

    k3d kubeconfig get subprovisioner-test > "${temp_dir}/kubeconfig"
    export KUBECONFIG="${temp_dir}/kubeconfig"

    __log_cyan 'Importing Subprovisioner images into k3d cluster...'
    # shellcheck disable=SC2046
    k3d image import --cluster=subprovisioner-test --mode=direct \
        subprovisioner/subprovisioner:test subprovisioner/test:test

    set +o errexit
    (
        set -o errexit -o pipefail -o nounset +o xtrace

        __log_cyan "Installing Subprovisioner..."
        sed -E 's|subprovisioner/([a-z-]+):[0-9+\.]+|subprovisioner/\1:test|g' \
            "${repo_root}/deployment.yaml" | kubectl create -f -
        # kubectl rollout status -n=subprovisioner --timeout=60s \
        #     deployment/csi-controller-plugin daemonset/csi-node-plugin

        __log_cyan "Enabling volume snapshot support in the cluster..."
        base_url=https://raw.githubusercontent.com/kubernetes-csi/external-snapshotter/v6.2.1
        kubectl create \
            -f "${base_url}/client/config/crd/snapshot.storage.k8s.io_volumesnapshotclasses.yaml" \
            -f "${base_url}/client/config/crd/snapshot.storage.k8s.io_volumesnapshotcontents.yaml" \
            -f "${base_url}/client/config/crd/snapshot.storage.k8s.io_volumesnapshots.yaml" \
            -f "${base_url}/deploy/kubernetes/snapshot-controller/rbac-snapshot-controller.yaml" \
            -f "${base_url}/deploy/kubernetes/snapshot-controller/setup-snapshot-controller.yaml"
        unset base_url

        __log_cyan "Creating common objects..."
        kubectl create -f "${script_dir}/common.yaml"
        __wait_for_pvc_to_be_bound 45 -n=backing backing-pvc

        set -o xtrace
        cd "$( dirname "${test_resolved}" )"
        # shellcheck disable=SC1090
        source "${test_resolved}"
    )
    exit_code="$?"
    set -o errexit

    if (( exit_code == 0 )); then

        __log_cyan "Uninstalling Subprovisioner..."
        kubectl delete --ignore-not-found --timeout=45s \
            -f "${repo_root}/deployment.yaml" \
            || exit_code="$?"

        if (( exit_code != 0 )); then
            __failure 'Failed to uninstall Subprovisioner.'
        fi

    else

        __failure 'Test %s failed.' "${test_name}"

    fi

    __log_cyan 'Deleting k3d cluster...'
    k3d cluster delete subprovisioner-test
    rm -fr "${temp_dir}/backing" "${temp_dir}/kubeconfig"

    trap 'rm -fr "${temp_dir}"' EXIT

    if (( canceled )); then
        break
    elif (( exit_code == 0 )); then
        : $(( num_succeeded++ ))
    else
        : $(( num_failed++ ))
        if (( fail_fast )); then
            break
        fi
    fi

done

# print summary

num_canceled="$(( ${#tests[@]} - num_succeeded - num_failed ))"

if (( num_failed > 0 )); then
    color=31
elif (( num_canceled > 0 )); then
    color=33
else
    color=32
fi

__big_log "${color}" '%d succeeded, %d failed, %d canceled' \
    "${num_succeeded}" "${num_failed}" "${num_canceled}"

(( num_succeeded == ${#tests[@]} ))
