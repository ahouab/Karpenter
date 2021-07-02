/*
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

package expiration

import (
	"context"
	"fmt"
	"time"

	"github.com/awslabs/karpenter/pkg/apis/provisioning/v1alpha2"
	"go.uber.org/zap"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"knative.dev/pkg/ptr"

	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"
)

// Controller for the resource
type Controller struct {
	kubeClient client.Client
}

// NewController constructs a controller instance
func NewController(kubeClient client.Client) *Controller {
	return &Controller{
		kubeClient: kubeClient,
	}
}

// Reconcile executes an expiration control loop for a node
func (c *Controller) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	// 1. Get node
	node := &v1.Node{}
	if err := c.kubeClient.Get(ctx, req.NamespacedName, node); err != nil {
		if errors.IsNotFound(err) {
			return reconcile.Result{}, nil
		}
		return reconcile.Result{}, err
	}
	// 2. Ignore if node is already deleting
	if !node.DeletionTimestamp.IsZero() {
		return reconcile.Result{}, nil
	}
	// 3. Ignore if provisioner doesn't exist
	name, ok := node.Labels[v1alpha2.ProvisionerNameLabelKey]
	if !ok {
		return reconcile.Result{}, nil
	}
	namespace, ok := node.Labels[v1alpha2.ProvisionerNamespaceLabelKey]
	if !ok {
		return reconcile.Result{}, nil
	}
	provisioner := &v1alpha2.Provisioner{}
	if err := c.kubeClient.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, provisioner); err != nil {
		if errors.IsNotFound(err) {
			return reconcile.Result{}, nil
		}
		return reconcile.Result{}, err
	}
	// 4. Ignore if TTLSecondsUntilExpired isn't defined
	if provisioner.Spec.TTLSecondsUntilExpired == nil {
		return reconcile.Result{}, nil
	}
	// 5. Trigger termination workflow if expired
	expirationTTL := time.Duration(ptr.Int64Value(provisioner.Spec.TTLSecondsUntilExpired)) * time.Second
	expirationTime := node.CreationTimestamp.Add(expirationTTL)
	if time.Now().After(expirationTime) {
		zap.S().Infof("Triggering termination after %s (+%s) for expired node %s", expirationTTL, time.Since(expirationTime), node.Name)
		if err := c.kubeClient.Delete(ctx, node); err != nil {
			return reconcile.Result{}, fmt.Errorf("expiring node %s, %w", node.Name, err)
		}
		return reconcile.Result{}, nil
	}

	// 6. Backoff until expired
	return reconcile.Result{RequeueAfter: time.Until(expirationTime)}, nil
}

func (c *Controller) provisionerToNodes(o client.Object) (requests []reconcile.Request) {
	nodes := &v1.NodeList{}
	if err := c.kubeClient.List(context.Background(), nodes, client.MatchingLabels(map[string]string{
		v1alpha2.ProvisionerNameLabelKey:      o.GetName(),
		v1alpha2.ProvisionerNamespaceLabelKey: o.GetNamespace(),
	})); err != nil {
		zap.S().Errorf("Failed to list nodes when mapping expiration watch events, %s", err.Error())
	}
	for _, node := range nodes.Items {
		requests = append(requests, reconcile.Request{NamespacedName: types.NamespacedName{Name: node.Name}})
	}
	return requests
}

func (c *Controller) Register(m manager.Manager) error {
	return controllerruntime.
		NewControllerManagedBy(m).
		Named("Expiration").
		For(&v1.Node{}).
		Watches(
			// Reconcile all nodes related to a provisioner when it changes.
			&source.Kind{Type: &v1alpha2.Provisioner{}},
			handler.EnqueueRequestsFromMapFunc(c.provisionerToNodes),
		).
		Complete(c)
}
