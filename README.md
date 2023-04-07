<!-- ----------------------------------------------------------------------- -->

# Subprovisioner

A CSI plugin for Kubernetes that allows provisioning `Block` volumes from an
existing `Filesystem` volume.

<!-- ----------------------------------------------------------------------- -->

## How to install

The Subprovisioner images aren't currently published anywhere, so you must build
them yourself:

```console
$ make
```

This will use Docker to build an image tagged
`subprovisioner/subprovisioner:0.0.0`. You must then make sure your Kubernetes
cluster can pull it, and finally run:

```console
$ kubectl apply -f deployment.yaml
```

If you need to prefix a registry to the image tag, adjust `deployment.yaml`
accordingly before running the command above.

And to uninstall:

```console
$ kubectl delete --ignore-not-found -f deployment.yaml
```

<!-- ----------------------------------------------------------------------- -->

## How it's used

Assume you have a `PersistentVolumeClaim` (PVC) of `Filesystem` type with
support for the `ReadWriteMany` access mode, and that it is named `backing-pvc`
and belongs to namespace `default`.

First, create a `StorageClass`:

```yaml
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: my-storage-class
provisioner: subprovisioner.gitlab.io
parameters:
  backingClaimName: backing-pvc
  backingClaimNamespace: default
  basePath: volumes  # path in backing-pvc under which to store volumes; default is "", i.e., at the root
allowVolumeExpansion: true  # optional
```

Then provision `Block` volumes from that `StorageClass` as usual:

```yaml
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: my-dynamically-provisioned-block-pvc
spec:
  accessModes:
    - ReadWriteOnce
  resources:
    requests:
      storage: 1Ti
  volumeMode: Block
  storageClassName: my-storage-class
```

That's it!

Note that the volumes will initially be filled with zeroes and are thinly
provisioned, so you can specify volumes sizes bigger than the capacity of the
backing volume.

### Expanding volumes

It is possible to increase the capacity of an existing volume. To do so simply
edit the `spec.resources.requests.storage` field of the PVC. Once the volume is
expanded, `status.capacity.storage` will be updated to reflect its new size.

The volume will only be expanded once it isn't mounted by any pod.

### Cloning volumes

`Block` volumes may be provisioned by cloning other existing `Block` volumes.
This is achieved by referencing an existing PVC in the `spec.dataSource` field
of the new PVC:

```yaml
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: my-cloned-pvc
spec:
  accessModes:
    - ReadWriteOnce
  dataSource:
    kind: PersistentVolumeClaim
    name: my-dynamically-provisioned-block-pvc
  resources:
    requests:
      storage: 2Ti
  volumeMode: Block
  storageClassName: my-storage-class
```

Note that you may give the cloned volume a bigger size than the original volume.
The excess size will be filled with zeroes.

The clone will be completed only once the original PVC isn't mounted by any pod.

### Snapshotting volumes

> Your Kubernetes distribution might not support volume snapshotting out of the
> box. Follow [this documentation] to manually install the volume snapshot CRDs
> and controller.

[this documentation]: https://github.com/kubernetes-csi/external-snapshotter#csi-snapshotter

You can create `VolumeSnapshot`s from an existing `Block` volume, to later
provision new volumes from it. In this case you will need to create a
[`VolumeSnapshotClass`] beforehand. No parameters need to be specified on it,
and a single `VolumeSnapshotClass` is enough for all Subprovisioner volumes,
even if they have different backing volumes or `StorageClass`es. This will do:

```yaml
apiVersion: snapshot.storage.k8s.io/v1
kind: VolumeSnapshotClass
metadata:
  name: subprovisioner
driver: subprovisioner.gitlab.io
deletionPolicy: Delete
```

Then create a `VolumeSnapshot` from an existing PVC like this:

```yaml
apiVersion: snapshot.storage.k8s.io/v1
kind: VolumeSnapshot
metadata:
  name: my-snapshot
spec:
  volumeSnapshotClassName: subprovisioner
  source:
    persistentVolumeClaimName: my-dynamically-provisioned-block-pvc
```

The snapshot will be completed only once the PVC isn't mounted by any pod.

You can then provision `Block` volumes from that `VolumeSnapshot`:

```yaml
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: my-cloned-pvc
spec:
  accessModes:
    - ReadWriteOnce
  dataSource:
    apiGroup: snapshot.storage.k8s.io
    kind: VolumeSnapshot
    name: my-snapshot
  resources:
    requests:
      storage: 2Ti
  volumeMode: Block
  storageClassName: my-storage-class
```

Just like with volume cloning, you may give the volume a bigger size than that
of the snapshot, and the excess size will be filled with zeroes.

[`VolumeSnapshotClass`]: https://kubernetes.io/docs/concepts/storage/volume-snapshot-classes/

<!-- ----------------------------------------------------------------------- -->

## How it works

Provisioned `Block` volumes are stored in the backing `Filesystem` volume as
qcow2 image files. [qemu-storage-daemon] is used to expose those images as block
devices. Volume cloning and snapshotting is implemented by creating overlay
qcow2 files, making it very efficient.

[qemu-storage-daemon]: https://qemu.readthedocs.io/en/latest/tools/qemu-storage-daemon.html

<!-- ----------------------------------------------------------------------- -->

## Features

- Dynamic `Block` volume provisioning.
- `ReadWriteOnce`, `ReadWriteOncePod`, and `ReadOnlyMany` access modes.
- Efficient (constant-time) offline volume expansion.
- Efficient (constant-time) offline volume cloning.
- Efficient (constant-time) offline volume snapshotting.

<!-- ----------------------------------------------------------------------- -->

## Limitations

- The backing volume must be `ReadWriteMany`.

- You have to be careful not to delete the backing volume PVC prior to deleting
  PVCs backed by it, or else deletion of the latter will hang.

- The plugin assumes that all nodes in the Kubernetes cluster have the the
  kernel NBD client loaded, such that NBD block nodes are available at
  `/dev/nbd0`, `/dev/nbd1`, etc.

- The number of Subprovisioner-provisioned volumes that can be simultaneously
  mounted on a given node is limited by the amount of kernel NBD block devices
  that are available. Assuming the NBD kernel client was built as a module, use
  the `nbds_max` option to increase the maximum number of NBD block devices if
  needed, _e.g._, `modprobe nbd nbds_max=64`.

- The plugin assumes that it created all PVs that have `spec.csi.driver` set to
  `subprovisioner.gitlab.io`, so don't create such a PV manually.

- On some systems, you may need to configure Docker [mount-propagation] to allow
  for bidirectional volume mounts, which Subprovisioner will eventually on.

[mount-propagation]: https://kubernetes.io/docs/concepts/storage/volumes/#configuration

<!-- ----------------------------------------------------------------------- -->

## Some TODO

- Support online volume cloning.
- Support online volume snapshotting.
- Allow provisioning `Filesystem` volumes.
- Propagate volume accessibility constraints from the backing volume to the
  provisioned volumes.
- Support multiple backing volumes, as long as the set of nodes they're
  accessible from is disjoint.
- Opt-in support for making provisioned volumes accessible from any node even
  if the corresponding backing volume is not, by using networked NBD.
- Run at most one qemu-storage-daemon instance per node, per backing volume,
  per `Block` volume namespace.

<!-- ----------------------------------------------------------------------- -->

## License

This project is released under the Apache 2.0 license. See [LICENSE](LICENSE).

<!-- ----------------------------------------------------------------------- -->
