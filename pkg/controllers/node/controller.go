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

package node

import (
	"context"
	"fmt"
	awsv1alpha1 "github.com/aws/karpenter/pkg/apis/awsnodetemplate/v1alpha1"
	"github.com/samber/lo"

	"go.uber.org/multierr"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/clock"
	"knative.dev/pkg/logging"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	"github.com/aws/karpenter/pkg/apis/provisioning/v1alpha5"
	"github.com/aws/karpenter/pkg/cloudprovider"
	"github.com/aws/karpenter/pkg/controllers/state"
	"github.com/aws/karpenter/pkg/utils/result"
)

const controllerName = "node"

// NewController constructs a controller instance
func NewController(clk clock.Clock, kubeClient client.Client, cloudProvider cloudprovider.CloudProvider, cluster *state.Cluster) *Controller {
	return &Controller{
		kubeClient:     kubeClient,
		initialization: &Initialization{kubeClient: kubeClient, cloudProvider: cloudProvider},
		emptiness:      &Emptiness{kubeClient: kubeClient, clock: clk, cluster: cluster},
		expiration:     &Expiration{kubeClient: kubeClient, clock: clk},
		tags:           &Tags{cloudProvider: cloudProvider},
	}
}

// Controller manages a set of properties on karpenter provisioned nodes, such as
// taints, labels, finalizers.
type Controller struct {
	kubeClient     client.Client
	initialization *Initialization
	emptiness      *Emptiness
	expiration     *Expiration
	finalizer      *Finalizer
	tags           *Tags
}

// Reconcile executes a reallocation control loop for the resource
func (c *Controller) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	ctx = logging.WithLogger(ctx, logging.FromContext(ctx).Named(controllerName).With("node", req.Name))
	// 1. Retrieve Node, ignore if not provisioned or terminating
	stored := &v1.Node{}
	if err := c.kubeClient.Get(ctx, req.NamespacedName, stored); err != nil {
		if errors.IsNotFound(err) {
			return reconcile.Result{}, nil
		}
		return reconcile.Result{}, err
	}
	if _, ok := stored.Labels[v1alpha5.ProvisionerNameLabelKey]; !ok {
		return reconcile.Result{}, nil
	}
	if !stored.DeletionTimestamp.IsZero() {
		return reconcile.Result{}, nil
	}

	// 2. Retrieve Provisioner
	provisioner := &v1alpha5.Provisioner{}
	if err := c.kubeClient.Get(ctx, types.NamespacedName{Name: stored.Labels[v1alpha5.ProvisionerNameLabelKey]}, provisioner); err != nil {
		if errors.IsNotFound(err) {
			return reconcile.Result{}, nil
		}
		return reconcile.Result{}, err
	}

	// 3. Execute reconcilers
	node := stored.DeepCopy()
	var results []reconcile.Result
	var errs error
	for _, reconciler := range []interface {
		Reconcile(context.Context, *v1alpha5.Provisioner, *v1.Node) (reconcile.Result, error)
	}{
		c.initialization,
		c.expiration,
		c.emptiness,
		c.finalizer,
		c.tags,
	} {
		res, err := reconciler.Reconcile(ctx, provisioner, node)
		errs = multierr.Append(errs, err)
		results = append(results, res)
	}

	// 4. Patch any changes, regardless of errors
	if !equality.Semantic.DeepEqual(node, stored) {
		if err := c.kubeClient.Patch(ctx, node, client.MergeFrom(stored)); err != nil {
			return reconcile.Result{}, fmt.Errorf("patching node, %w", err)
		}
	}
	// 5. Requeue if error or if retryAfter is set
	if errs != nil {
		return reconcile.Result{}, errs
	}
	return result.Min(results...), nil
}

func (c *Controller) Register(ctx context.Context, m manager.Manager) error {
	return controllerruntime.
		NewControllerManagedBy(m).
		Named(controllerName).
		For(&v1.Node{}).
		Watches(
			// Reconcile all nodes related to a provisioner when it changes.
			&source.Kind{Type: &v1alpha5.Provisioner{}},
			handler.EnqueueRequestsFromMapFunc(func(o client.Object) (requests []reconcile.Request) {
				nodes := &v1.NodeList{}
				if err := c.kubeClient.List(ctx, nodes, client.MatchingLabels(map[string]string{v1alpha5.ProvisionerNameLabelKey: o.GetName()})); err != nil {
					logging.FromContext(ctx).Errorf("Failed to list nodes when mapping expiration watch events, %s", err)
					return requests
				}
				for _, node := range nodes.Items {
					requests = append(requests, reconcile.Request{NamespacedName: types.NamespacedName{Name: node.Name}})
				}
				return requests
			}),
		).
		Watches(
			// Reconcile node when a pod assigned to it changes.
			&source.Kind{Type: &v1.Pod{}},
			handler.EnqueueRequestsFromMapFunc(func(o client.Object) (requests []reconcile.Request) {
				if name := o.(*v1.Pod).Spec.NodeName; name != "" {
					requests = append(requests, reconcile.Request{NamespacedName: types.NamespacedName{Name: name}})
				}
				return requests
			}),
		).
		Watches(
			// Reconcile all nodes related to a nodetemplate when it changes.
			&source.Kind{Type: &awsv1alpha1.AWSNodeTemplate{}},
			handler.EnqueueRequestsFromMapFunc(func(o client.Object) (requests []reconcile.Request) {
				var provisionerList v1alpha5.ProvisionerList
				if err := c.kubeClient.List(ctx, &provisionerList); err != nil {
					logging.FromContext(ctx).Errorf("Failed to list provisioners when mapping node template watch events, %s", err)
					return requests
				}

				provisioners := lo.Filter(provisionerList.Items, func(p v1alpha5.Provisioner, _ int) bool {
					return o.GetName() == p.Spec.ProviderRef.Name
				})

				for _, provisioner := range provisioners {
					nodes := &v1.NodeList{}
					if err := c.kubeClient.List(ctx, nodes, client.MatchingLabels(map[string]string{v1alpha5.ProvisionerNameLabelKey: provisioner.Name})); err != nil {
						logging.FromContext(ctx).Errorf("Failed to list nodes when mapping node template watch events, %s", err)
						return requests
					}
					for _, node := range nodes.Items {
						requests = append(requests, reconcile.Request{NamespacedName: types.NamespacedName{Name: node.Name}})
					}
				}

				return requests
			}),
		).
		WithOptions(controller.Options{MaxConcurrentReconciles: 10}).
		Complete(c)
}
