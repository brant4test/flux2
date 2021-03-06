/*
Copyright 2020 The Flux authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"context"
	"fmt"
	"time"

	"github.com/fluxcd/pkg/apis/meta"
	"k8s.io/client-go/util/retry"

	"github.com/fluxcd/flux2/internal/utils"

	"github.com/spf13/cobra"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"

	sourcev1 "github.com/fluxcd/source-controller/api/v1beta1"
)

var reconcileSourceBucketCmd = &cobra.Command{
	Use:   "bucket [name]",
	Short: "Reconcile a Bucket source",
	Long:  `The reconcile source command triggers a reconciliation of a Bucket resource and waits for it to finish.`,
	Example: `  # Trigger a reconciliation for an existing source
  flux reconcile source bucket podinfo
`,
	RunE: reconcileSourceBucketCmdRun,
}

func init() {
	reconcileSourceCmd.AddCommand(reconcileSourceBucketCmd)
}

func reconcileSourceBucketCmdRun(cmd *cobra.Command, args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("source name is required")
	}
	name := args[0]

	ctx, cancel := context.WithTimeout(context.Background(), rootArgs.timeout)
	defer cancel()

	kubeClient, err := utils.KubeClient(rootArgs.kubeconfig, rootArgs.kubecontext)
	if err != nil {
		return err
	}

	namespacedName := types.NamespacedName{
		Namespace: rootArgs.namespace,
		Name:      name,
	}
	var bucket sourcev1.Bucket
	err = kubeClient.Get(ctx, namespacedName, &bucket)
	if err != nil {
		return err
	}

	if bucket.Spec.Suspend {
		return fmt.Errorf("resource is suspended")
	}

	lastHandledReconcileAt := bucket.Status.LastHandledReconcileAt
	logger.Actionf("annotating Bucket source %s in %s namespace", name, rootArgs.namespace)
	if err := requestBucketReconciliation(ctx, kubeClient, namespacedName, &bucket); err != nil {
		return err
	}
	logger.Successf("Bucket source annotated")

	logger.Waitingf("waiting for Bucket source reconciliation")
	if err := wait.PollImmediate(
		rootArgs.pollInterval, rootArgs.timeout,
		bucketReconciliationHandled(ctx, kubeClient, namespacedName, &bucket, lastHandledReconcileAt),
	); err != nil {
		return err
	}
	logger.Successf("Bucket source reconciliation completed")

	if apimeta.IsStatusConditionFalse(bucket.Status.Conditions, meta.ReadyCondition) {
		return fmt.Errorf("Bucket source reconciliation failed")
	}
	logger.Successf("fetched revision %s", bucket.Status.Artifact.Revision)
	return nil
}

func isBucketReady(ctx context.Context, kubeClient client.Client,
	namespacedName types.NamespacedName, bucket *sourcev1.Bucket) wait.ConditionFunc {
	return func() (bool, error) {
		err := kubeClient.Get(ctx, namespacedName, bucket)
		if err != nil {
			return false, err
		}

		// Confirm the state we are observing is for the current generation
		if bucket.Generation != bucket.Status.ObservedGeneration {
			return false, nil
		}

		if c := apimeta.FindStatusCondition(bucket.Status.Conditions, meta.ReadyCondition); c != nil {
			switch c.Status {
			case metav1.ConditionTrue:
				return true, nil
			case metav1.ConditionFalse:
				return false, fmt.Errorf(c.Message)
			}
		}
		return false, nil
	}
}

func bucketReconciliationHandled(ctx context.Context, kubeClient client.Client,
	namespacedName types.NamespacedName, bucket *sourcev1.Bucket, lastHandledReconcileAt string) wait.ConditionFunc {
	return func() (bool, error) {
		err := kubeClient.Get(ctx, namespacedName, bucket)
		if err != nil {
			return false, err
		}
		return bucket.Status.LastHandledReconcileAt != lastHandledReconcileAt, nil
	}
}

func requestBucketReconciliation(ctx context.Context, kubeClient client.Client,
	namespacedName types.NamespacedName, bucket *sourcev1.Bucket) error {
	return retry.RetryOnConflict(retry.DefaultBackoff, func() (err error) {
		if err := kubeClient.Get(ctx, namespacedName, bucket); err != nil {
			return err
		}
		if bucket.Annotations == nil {
			bucket.Annotations = map[string]string{
				meta.ReconcileRequestAnnotation: time.Now().Format(time.RFC3339Nano),
			}
		} else {
			bucket.Annotations[meta.ReconcileRequestAnnotation] = time.Now().Format(time.RFC3339Nano)
		}
		return kubeClient.Update(ctx, bucket)
	})
}
