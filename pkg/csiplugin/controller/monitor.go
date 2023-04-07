// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"log"
	"time"

	"gitlab.com/subprovisioner/subprovisioner/pkg/csiplugin/common"

	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/retry"
	"k8s.io/client-go/util/workqueue"
)

type ControllerMonitor struct {
	Clientset *common.Clientset
	Image     string
}

func (m *ControllerMonitor) Run() {
	optionsModifier := func(options *metav1.ListOptions) {
		options.LabelSelector = common.Domain + "/uid"
	}
	pvcListWatcher := cache.NewFilteredListWatchFromClient(
		m.Clientset.CoreV1().RESTClient(),
		"persistentvolumeclaims",
		corev1.NamespaceAll,
		optionsModifier,
	)

	queue := workqueue.NewRateLimitingQueue(workqueue.DefaultControllerRateLimiter())

	indexer, controller := cache.NewIndexerInformer(
		pvcListWatcher,
		&corev1.PersistentVolumeClaim{},
		0,
		cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				pvc := obj.(*corev1.PersistentVolumeClaim)
				if pvc.DeletionTimestamp != nil {
					key, err := cache.MetaNamespaceKeyFunc(pvc)
					if err == nil {
						queue.Add(key)
					}
				}
			},
			UpdateFunc: func(oldObj interface{}, newObj interface{}) {
				pvc := newObj.(*corev1.PersistentVolumeClaim)
				if pvc.DeletionTimestamp != nil {
					key, err := cache.MetaNamespaceKeyFunc(pvc)
					if err == nil {
						queue.Add(key)
					}
				}
			},
		},
		cache.Indexers{},
	)

	c := pvcDeletionController{
		clientset:  m.Clientset,
		image:      m.Image,
		indexer:    indexer,
		queue:      queue,
		controller: controller,
	}

	stopCh := make(chan struct{})
	defer close(stopCh)
	go c.run(stopCh)

	select {} // wait forever
}

type pvcDeletionController struct {
	clientset  *common.Clientset
	image      string
	indexer    cache.Indexer
	queue      workqueue.RateLimitingInterface
	controller cache.Controller
}

func (c *pvcDeletionController) run(stopCh chan struct{}) {
	defer runtime.HandleCrash()
	defer c.queue.ShutDown()

	go c.controller.Run(stopCh)

	if !cache.WaitForCacheSync(stopCh, c.controller.HasSynced) {
		runtime.HandleError(fmt.Errorf("timed out waiting for caches to sync"))
		return
	}

	workers := 4 // TODO: Choose number of workers.
	for i := 0; i < workers; i++ {
		go wait.Until(c.runWorker, 1*time.Second, stopCh)
	}

	<-stopCh
}

func (c *pvcDeletionController) runWorker() {
	for c.processNextItem() {
	}
}

func (c *pvcDeletionController) processNextItem() bool {
	ctx := context.Background() // TODO

	key, shutdown := c.queue.Get()
	if shutdown {
		return false
	}
	defer c.queue.Done(key)

	pvcNamespace, pvcName, err := cache.SplitMetaNamespaceKey(key.(string))
	if err != nil {
		runtime.HandleError(err)
		c.queue.AddRateLimited(key)
		return true
	}

	// We get the PVC ourselves to ensure we have the most recent version of it. This ensures we don't try to delete
	// a volume that we just successfully deleted.
	pvc, err := c.clientset.CoreV1().PersistentVolumeClaims(pvcNamespace).Get(ctx, pvcName, metav1.GetOptions{})
	if err != nil && !k8serrors.IsNotFound(err) {
		runtime.HandleError(err)
		c.queue.AddRateLimited(key)
		return true
	}

	if err == nil {
		pvcIsStaged := pvc.Annotations[common.Domain+"/staged-on-nodes"] != ""
		pvcHasFinalizer := func() bool {
			for _, finalizer := range pvc.GetFinalizers() {
				if finalizer == common.Domain+"/cleanup" {
					return true
				}
			}
			return false
		}

		if !pvcIsStaged && pvcHasFinalizer() {
			log.Printf("Deleting volume for PVC %s in namespace %s...", pvc.Name, pvc.Namespace)

			err = c.deleteVolume(ctx, pvc)
			if err != nil {
				log.Printf(
					"Failed to delete volume for PVC %s in namespace %s: %+v",
					pvc.Name, pvc.Namespace, err,
				)
				runtime.HandleError(err)
				c.queue.AddRateLimited(key)
				return true
			}

		}
	}

	c.queue.Forget(key)
	return true
}

func (c *pvcDeletionController) deleteVolume(ctx context.Context, pvc *corev1.PersistentVolumeClaim) error {
	backingPvcName := pvc.Annotations[common.Domain+"/backing-pvc-name"]
	backingPvcNamespace := pvc.Annotations[common.Domain+"/backing-pvc-namespace"]
	backingPvcBasePath := pvc.Annotations[common.Domain+"/backing-pvc-base-path"]

	// delete volume creation Job

	creationJobName := common.GenerateCreationJobName(pvc.UID)

	err := common.DeleteJobSynchronously(
		ctx, c.clientset,
		creationJobName, backingPvcNamespace,
	)
	if err != nil {
		return err
	}

	// create and await volume deletion Job

	volumeImagePath := common.GenerateVolumeImagePath(pvc.UID)
	deletionJobName := common.GenerateDeletionJobName(pvc.UID)

	// TODO: Also delete any qcow2 images in the backing chains that aren't referenced by any PVC or snapshot
	// anymore. To ensure idempotency, probably begin by creating graph of all qcow2 files connected to the
	// top-level file being deleted (regardless of edge direction), determine which will be left dangling and should
	// be deleted, and finally delete them all in one go.
	err = common.CreateJob(
		ctx, c.clientset,
		common.JobConfig{
			Name:      deletionJobName,
			Namespace: backingPvcNamespace,
			Labels: map[string]string{
				common.Domain + "/component": "volume-deletion",
				common.Domain + "/pvc-uid":   string(pvc.UID),
			},
			Image:              c.image,
			Command:            []string{"rm", "-f", volumeImagePath},
			BackingPvcName:     backingPvcName,
			BackingPvcBasePath: backingPvcBasePath,
		},
	)
	if err != nil {
		return err
	}

	err = common.WaitForJobToSucceed(
		ctx, c.clientset,
		deletionJobName, backingPvcNamespace,
	)
	if err != nil {
		return err
	}

	// delete volume deletion Job

	err = common.DeleteJobSynchronously(
		ctx, c.clientset,
		deletionJobName, backingPvcNamespace,
	)
	if err != nil {
		return err
	}

	// remove finalizer from PVC

	pvcs := c.clientset.CoreV1().PersistentVolumeClaims(pvc.Namespace)
	err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		pvc, err := pvcs.Get(ctx, pvc.Name, metav1.GetOptions{})
		if err != nil {
			return err
		}

		for i, finalizer := range pvc.GetFinalizers() {
			if finalizer == common.Domain+"/cleanup" {
				pvc.Finalizers = append(pvc.Finalizers[:i], pvc.Finalizers[i+1:]...)
				break
			}
		}

		_, err = pvcs.Update(ctx, pvc, metav1.UpdateOptions{})
		return err
	})
	if err != nil {
		return err
	}

	return nil
}
