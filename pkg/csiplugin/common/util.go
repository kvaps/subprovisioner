// SPDX-License-Identifier: Apache-2.0

package common

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"time"

	"github.com/kubernetes-csi/external-snapshotter/client/v6/clientset/versioned"
	"k8s.io/client-go/kubernetes"
)

type SnapshotClientSet = versioned.Clientset
type Clientset struct {
	*kubernetes.Clientset
	*SnapshotClientSet
}

func WaitUntilFileIsBlockDevice(ctx context.Context, name string) error {
	for {
		if stat, err := os.Stat(name); err == nil {
			// file exists
			if stat.Mode()&(fs.ModeDevice|fs.ModeCharDevice) == fs.ModeDevice {
				// file is block device
				return nil
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return err // could not determine whether file exists and is a block device
		} else if ctx.Err() != nil {
			return ctx.Err() // file doesn't exist or isn't a block device but context is done
		}

		time.Sleep(1 * time.Second)
	}
}
