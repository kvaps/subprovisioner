# SPDX-License-Identifier: Apache-2.0

# This test attempts to delete a Block PVC while it is mounted by a pod, and
# ensures that it is only actually deleted until no longer under use.

__stage 'Provisioning volume...'

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

__stage 'Starting pod that writes continuously to volume...'

kubectl create -f - <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: test-pod
spec:
  restartPolicy: Never
  terminationGracePeriodSeconds: 0
  containers:
    - name: container
      image: subprovisioner/test:test
      command:
        - bash
        - -c
        - |
          set -o errexit -o pipefail -o nounset -o xtrace
          while true; do
            dd if=/dev/urandom of=/var/image conv=fsync bs=1M count=1
            sleep 0.1
          done
      volumeDevices:
        - name: image
          devicePath: /var/image
  volumes:
    - name: image
      persistentVolumeClaim:
        claimName: image-pvc
EOF

__wait_for_pod_to_start_running 45 test-pod

__stage 'Triggering volume deletion...'

kubectl delete pvc image-pvc --wait=false

__stage 'Letting pod write to the volume for a bit...'

sleep 15

__stage 'Checking if pod is still running...'

__pod_is_running test-pod

__stage 'Deleting pod...'

kubectl delete pod test-pod --timeout=45s

__stage 'Waiting for volume to be deleted...'

kubectl delete pvc image-pvc --timeout=45s
