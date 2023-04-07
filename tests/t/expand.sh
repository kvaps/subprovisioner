# SPDX-License-Identifier: Apache-2.0

__stage "Provisioning volume..."

kubectl create -f - <<EOF
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: image-pvc
spec:
  accessModes:
    - ReadWriteOnce
  resources:
    requests:
      storage: 128Mi
  volumeMode: Block
  storageClassName: storage-class
EOF

__wait_for_pvc_to_be_bound 45 image-pvc

__stage 'Checking volume size...'

function expect_size() {
    kubectl create -f - <<EOF
    apiVersion: v1
    kind: Pod
    metadata:
      name: test-pod
    spec:
      restartPolicy: Never
      containers:
        - name: container
          image: subprovisioner/test:test
          command:
            - bash
            - -c
            - |
              set -o errexit -o pipefail -o nounset -o xtrace
              [[ "\$( blockdev --getsize64 /var/image )" = $1 ]]
          volumeMounts:
          volumeDevices:
            - name: image
              devicePath: /var/image
      volumes:
        - name: image
          persistentVolumeClaim:
            claimName: image-pvc
EOF

    __wait_for_pod_to_succeed 45 test-pod
    kubectl delete pod test-pod --timeout=45s
}

expect_size "$(( 128 * 1024 * 1024 ))"

__stage 'Expanding volume...'

kubectl patch pvc image-pvc --patch-file <( echo '
spec:
  resources:
    requests:
      storage: 256Mi
')

# shellcheck disable=SC2016
__poll 1 45 '[[
    "$( kubectl get pvc image-pvc -o=jsonpath="{.status.capacity.storage}" )" \
    = 256Mi
    ]]'

__stage 'Checking new volume size...'

expect_size "$(( 256 * 1024 * 1024 ))"

__stage "Deleting volume..."

kubectl delete pvc image-pvc --timeout=45s
