# SPDX-License-Identifier: Apache-2.0

__stage "Provisioning volume..."

kubectl create -f - <<EOF
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: image-pvc
  namespace: backing
spec:
  accessModes:
    - ReadWriteOnce
  resources:
    requests:
      storage: 128Mi
  volumeMode: Block
  storageClassName: storage-class
EOF

image_pvc_uid="$( kubectl get pvc -n=backing image-pvc -o jsonpath='{.metadata.uid}' )"
__wait_for_pvc_to_be_bound 45 -n=backing image-pvc

__stage "Deleting StorageClass early..."

backing_sub_path="$( kubectl get sc storage-class -o jsonpath='{.parameters.basePath}' )"
kubectl delete sc storage-class --timeout=45s

__stage 'Writing to the volume and checking if it matches its backing qcow2 file...'

# This pod mounts both the provisioned Block volume and the backing PVC (which
# normally shouldn't be done), writes some random data to the Block volume, and
# checks whether its contents match those of the corresponding image file in the
# backing volume.

kubectl create -f - <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: test-pod
  namespace: backing
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
          [[ "\$( blockdev --getsize64 /var/image )" = $(( 128*1024*1024 )) ]]
          dd if=/dev/urandom of=/var/image conv=fsync bs=1M count=128
          qemu-img compare -f raw -F qcow2 --force-share \
            /var/image /var/backing/pvc-${image_pvc_uid}.qcow2
      volumeMounts:
        - name: backing
          mountPath: /var/backing
          subPath: ${backing_sub_path}
      volumeDevices:
        - name: image
          devicePath: /var/image
  volumes:
    - name: backing
      persistentVolumeClaim:
        claimName: backing-pvc
    - name: image
      persistentVolumeClaim:
        claimName: image-pvc
EOF

__wait_for_pod_to_succeed 45 -n=backing test-pod
kubectl delete pod -n=backing test-pod --timeout=45s

__stage "Deleting volume..."

kubectl delete pvc -n=backing image-pvc --timeout=45s

# This serves to check that trying to delete the backing PVC immediately after
# deleting the image PVC doesn't cause issues.
__stage 'Deleting backing volume PVC...'

kubectl delete pvc -n=backing backing-pvc --timeout=45s
