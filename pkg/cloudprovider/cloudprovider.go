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

package cloudprovider

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/awslabs/operatorpkg/status"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"sigs.k8s.io/controller-runtime/pkg/log"
	corev1beta1 "sigs.k8s.io/karpenter/pkg/apis/v1beta1"
	"sigs.k8s.io/karpenter/pkg/events"
	"sigs.k8s.io/karpenter/pkg/scheduling"
	"sigs.k8s.io/karpenter/pkg/utils/functional"
	"sigs.k8s.io/karpenter/pkg/utils/resources"

	"github.com/aws/karpenter-provider-aws/pkg/apis/v1beta1"
	"github.com/aws/karpenter-provider-aws/pkg/utils"

	"github.com/samber/lo"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cloudproviderevents "github.com/aws/karpenter-provider-aws/pkg/cloudprovider/events"
	"github.com/aws/karpenter-provider-aws/pkg/providers/amifamily"
	"github.com/aws/karpenter-provider-aws/pkg/providers/instance"
	"github.com/aws/karpenter-provider-aws/pkg/providers/instancetype"
	"github.com/aws/karpenter-provider-aws/pkg/providers/securitygroup"

	"sigs.k8s.io/karpenter/pkg/cloudprovider"
)

var _ cloudprovider.CloudProvider = (*CloudProvider)(nil)

type CloudProvider struct {
	kubeClient client.Client
	recorder   events.Recorder

	instanceTypeProvider  instancetype.Provider
	instanceProvider      instance.Provider
	amiProvider           amifamily.Provider
	securityGroupProvider securitygroup.Provider
}

func New(instanceTypeProvider instancetype.Provider, instanceProvider instance.Provider, recorder events.Recorder,
	kubeClient client.Client, amiProvider amifamily.Provider, securityGroupProvider securitygroup.Provider) *CloudProvider {
	return &CloudProvider{
		instanceTypeProvider:  instanceTypeProvider,
		instanceProvider:      instanceProvider,
		kubeClient:            kubeClient,
		amiProvider:           amiProvider,
		securityGroupProvider: securityGroupProvider,
		recorder:              recorder,
	}
}

// Create a NodeClaim given the constraints.
func (c *CloudProvider) Create(ctx context.Context, nodeClaim *corev1beta1.NodeClaim) (*corev1beta1.NodeClaim, error) {
	log.FromContext(ctx).WithValues("nodeClaim", nodeClaim).V(0).Info("Create nodeClaim")
	nodeClass, err := c.resolveNodeClassFromNodeClaim(ctx, nodeClaim)
	if err != nil {
		if errors.IsNotFound(err) {
			c.recorder.Publish(cloudproviderevents.NodeClaimFailedToResolveNodeClass(nodeClaim))
		}
		// We treat a failure to resolve the NodeClass as an ICE since this means there is no capacity possibilities for this NodeClaim
		return nil, cloudprovider.NewInsufficientCapacityError(fmt.Errorf("resolving node class, %w", err))
	}
	nodeClassReady := nodeClass.StatusConditions().Get(status.ConditionReady)
	if !nodeClassReady.IsTrue() {
		return nil, fmt.Errorf("resolving ec2nodeclass, %s", nodeClassReady.Message)
	}
	instanceTypes, err := c.resolveInstanceTypes(ctx, nodeClaim, nodeClass)
	if err != nil {
		return nil, fmt.Errorf("resolving instance types, %w", err)
	}
	if len(instanceTypes) == 0 {
		return nil, cloudprovider.NewInsufficientCapacityError(fmt.Errorf("all requested instance types were unavailable during launch"))
	}
	instance, err := c.instanceProvider.Create(ctx, nodeClass, nodeClaim, instanceTypes)
	if err != nil {
		c.updateUnavailableCapacityReservation(ctx, nodeClass, err)
		return nil, fmt.Errorf("creating instance, %w", err)
	}
	instanceType, _ := lo.Find(instanceTypes, func(i *cloudprovider.InstanceType) bool {
		return i.Name == instance.Type
	})

	nc := c.instanceToNodeClaim(instance, instanceType, nodeClass)
	nc.Annotations = lo.Assign(nodeClass.Annotations, map[string]string{
		v1beta1.AnnotationEC2NodeClassHash:        nodeClass.Hash(),
		v1beta1.AnnotationEC2NodeClassHashVersion: v1beta1.EC2NodeClassHashVersion,
	})
	return nc, nil
}

func (c *CloudProvider) List(ctx context.Context) ([]*corev1beta1.NodeClaim, error) {
	instances, err := c.instanceProvider.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing instances, %w", err)
	}
	var nodeClaims []*corev1beta1.NodeClaim
	for _, instance := range instances {
		instanceType, err := c.resolveInstanceTypeFromInstance(ctx, instance)
		if err != nil {
			return nil, fmt.Errorf("resolving instance type, %w", err)
		}
		nc, err := c.resolveNodeClassFromInstance(ctx, instance)
		if client.IgnoreNotFound(err) != nil {
			return nil, fmt.Errorf("resolving nodeclass, %w", err)
		}
		nodeClaims = append(nodeClaims, c.instanceToNodeClaim(instance, instanceType, nc))
	}
	return nodeClaims, nil
}

func (c *CloudProvider) Get(ctx context.Context, providerID string) (*corev1beta1.NodeClaim, error) {
	id, err := utils.ParseInstanceID(providerID)
	if err != nil {
		return nil, fmt.Errorf("getting instance ID, %w", err)
	}
	ctx = log.IntoContext(ctx, log.FromContext(ctx).WithValues("id", id))
	instance, err := c.instanceProvider.Get(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("getting instance, %w", err)
	}
	instanceType, err := c.resolveInstanceTypeFromInstance(ctx, instance)
	if err != nil {
		return nil, fmt.Errorf("resolving instance type, %w", err)
	}
	nc, err := c.resolveNodeClassFromInstance(ctx, instance)
	if client.IgnoreNotFound(err) != nil {
		return nil, fmt.Errorf("resolving nodeclass, %w", err)
	}
	return c.instanceToNodeClaim(instance, instanceType, nc), nil
}

func (c *CloudProvider) LivenessProbe(req *http.Request) error {
	return c.instanceTypeProvider.LivenessProbe(req)
}

// GetInstanceTypes returns all available InstanceTypes
func (c *CloudProvider) GetInstanceTypes(ctx context.Context, nodePool *corev1beta1.NodePool) ([]*cloudprovider.InstanceType, error) {
	nodeClass, err := c.resolveNodeClassFromNodePool(ctx, nodePool)
	if err != nil {
		if errors.IsNotFound(err) {
			c.recorder.Publish(cloudproviderevents.NodePoolFailedToResolveNodeClass(nodePool))
		}
		// We must return an error here in the event of the node class not being found. Otherwise users just get
		// no instance types and a failure to schedule with no indicator pointing to a bad configuration
		// as the cause.
		return nil, fmt.Errorf("resolving node class, %w", err)
	}
	// TODO, break this coupling
	instanceTypes, err := c.instanceTypeProvider.List(ctx, nodePool.Spec.Template.Spec.Kubelet, nodeClass)
	if err != nil {
		return nil, err
	}
	return instanceTypes, nil
}

func (c *CloudProvider) Delete(ctx context.Context, nodeClaim *corev1beta1.NodeClaim) error {
	id, err := utils.ParseInstanceID(nodeClaim.Status.ProviderID)
	if err != nil {
		return fmt.Errorf("getting instance ID, %w", err)
	}
	ctx = log.IntoContext(ctx, log.FromContext(ctx).WithValues("id", id))
	return c.instanceProvider.Delete(ctx, id)
}

func (c *CloudProvider) IsDrifted(ctx context.Context, nodeClaim *corev1beta1.NodeClaim) (cloudprovider.DriftReason, error) {
	// Not needed when GetInstanceTypes removes nodepool dependency
	nodePoolName, ok := nodeClaim.Labels[corev1beta1.NodePoolLabelKey]
	if !ok {
		return "", nil
	}
	nodePool := &corev1beta1.NodePool{}
	if err := c.kubeClient.Get(ctx, types.NamespacedName{Name: nodePoolName}, nodePool); err != nil {
		return "", client.IgnoreNotFound(err)
	}
	if nodePool.Spec.Template.Spec.NodeClassRef == nil {
		return "", nil
	}
	nodeClass, err := c.resolveNodeClassFromNodePool(ctx, nodePool)
	if err != nil {
		if errors.IsNotFound(err) {
			c.recorder.Publish(cloudproviderevents.NodePoolFailedToResolveNodeClass(nodePool))
		}
		return "", client.IgnoreNotFound(fmt.Errorf("resolving node class, %w", err))
	}
	driftReason, err := c.isNodeClassDrifted(ctx, nodeClaim, nodePool, nodeClass)
	if err != nil {
		return "", err
	}
	return driftReason, nil
}

// Name returns the CloudProvider implementation name.
func (c *CloudProvider) Name() string {
	return "aws"
}

func (c *CloudProvider) GetSupportedNodeClasses() []schema.GroupVersionKind {
	return []schema.GroupVersionKind{
		{
			Group:   v1beta1.SchemeGroupVersion.Group,
			Version: v1beta1.SchemeGroupVersion.Version,
			Kind:    "EC2NodeClass",
		},
	}
}

func (c *CloudProvider) resolveNodeClassFromNodeClaim(ctx context.Context, nodeClaim *corev1beta1.NodeClaim) (*v1beta1.EC2NodeClass, error) {
	nodeClass := &v1beta1.EC2NodeClass{}
	if err := c.kubeClient.Get(ctx, types.NamespacedName{Name: nodeClaim.Spec.NodeClassRef.Name}, nodeClass); err != nil {
		return nil, err
	}
	// For the purposes of NodeClass CloudProvider resolution, we treat deleting NodeClasses as NotFound
	if !nodeClass.DeletionTimestamp.IsZero() {
		// For the purposes of NodeClass CloudProvider resolution, we treat deleting NodeClasses as NotFound,
		// but we return a different error message to be clearer to users
		return nil, newTerminatingNodeClassError(nodeClass.Name)
	}
	return nodeClass, nil
}

func (c *CloudProvider) resolveNodeClassFromNodePool(ctx context.Context, nodePool *corev1beta1.NodePool) (*v1beta1.EC2NodeClass, error) {
	nodeClass := &v1beta1.EC2NodeClass{}
	if err := c.kubeClient.Get(ctx, types.NamespacedName{Name: nodePool.Spec.Template.Spec.NodeClassRef.Name}, nodeClass); err != nil {
		return nil, err
	}
	if !nodeClass.DeletionTimestamp.IsZero() {
		// For the purposes of NodeClass CloudProvider resolution, we treat deleting NodeClasses as NotFound,
		// but we return a different error message to be clearer to users
		return nil, newTerminatingNodeClassError(nodeClass.Name)
	}
	return nodeClass, nil
}

func (c *CloudProvider) resolveInstanceTypes(ctx context.Context, nodeClaim *corev1beta1.NodeClaim, nodeClass *v1beta1.EC2NodeClass) ([]*cloudprovider.InstanceType, error) {
	instanceTypes, err := c.instanceTypeProvider.List(ctx, nodeClaim.Spec.Kubelet, nodeClass)
	if err != nil {
		return nil, fmt.Errorf("getting instance types, %w", err)
	}
	reqs := scheduling.NewNodeSelectorRequirementsWithMinValues(nodeClaim.Spec.Requirements...)
	return lo.Filter(instanceTypes, func(i *cloudprovider.InstanceType, _ int) bool {
		return reqs.Compatible(i.Requirements, scheduling.AllowUndefinedWellKnownLabels) == nil &&
			len(i.Offerings.Compatible(reqs).Available()) > 0 &&
			resources.Fits(nodeClaim.Spec.Resources.Requests, i.Allocatable())
	}), nil
}

func (c *CloudProvider) resolveInstanceTypeFromInstance(ctx context.Context, instance *instance.Instance) (*cloudprovider.InstanceType, error) {
	nodePool, err := c.resolveNodePoolFromInstance(ctx, instance)
	if err != nil {
		// If we can't resolve the NodePool, we fall back to not getting instance type info
		return nil, client.IgnoreNotFound(fmt.Errorf("resolving nodepool, %w", err))
	}
	instanceTypes, err := c.GetInstanceTypes(ctx, nodePool)
	if err != nil {
		// If we can't resolve the NodePool, we fall back to not getting instance type info
		return nil, client.IgnoreNotFound(fmt.Errorf("resolving nodeclass, %w", err))
	}
	instanceType, _ := lo.Find(instanceTypes, func(i *cloudprovider.InstanceType) bool {
		return i.Name == instance.Type
	})
	return instanceType, nil
}

func (c *CloudProvider) resolveNodeClassFromInstance(ctx context.Context, instance *instance.Instance) (*v1beta1.EC2NodeClass, error) {
	np, err := c.resolveNodePoolFromInstance(ctx, instance)
	if err != nil {
		return nil, fmt.Errorf("resolving nodepool, %w", err)
	}
	return c.resolveNodeClassFromNodePool(ctx, np)
}

func (c *CloudProvider) resolveNodePoolFromInstance(ctx context.Context, instance *instance.Instance) (*corev1beta1.NodePool, error) {
	if nodePoolName, ok := instance.Tags[corev1beta1.NodePoolLabelKey]; ok {
		nodePool := &corev1beta1.NodePool{}
		if err := c.kubeClient.Get(ctx, types.NamespacedName{Name: nodePoolName}, nodePool); err != nil {
			return nil, err
		}
		return nodePool, nil
	}
	return nil, errors.NewNotFound(schema.GroupResource{Group: corev1beta1.Group, Resource: "nodepools"}, "")
}

//nolint:gocyclo
func (c *CloudProvider) instanceToNodeClaim(i *instance.Instance, instanceType *cloudprovider.InstanceType, nodeClass *v1beta1.EC2NodeClass) *corev1beta1.NodeClaim {
	nodeClaim := &corev1beta1.NodeClaim{}
	labels := map[string]string{}
	annotations := map[string]string{}

	if instanceType != nil {
		for key, req := range instanceType.Requirements {
			if req.Len() == 1 {
				labels[key] = req.Values()[0]
			}
		}
		resourceFilter := func(n v1.ResourceName, v resource.Quantity) bool {
			if resources.IsZero(v) {
				return false
			}
			// The nodeclaim should only advertise an EFA resource if it was requested. EFA network interfaces are only
			// added to the launch template if they're requested, otherwise the instance is launched with a normal ENI.
			if n == v1beta1.ResourceEFA {
				return i.EFAEnabled
			}
			return true
		}
		nodeClaim.Status.Capacity = functional.FilterMap(instanceType.Capacity, resourceFilter)
		nodeClaim.Status.Allocatable = functional.FilterMap(instanceType.Allocatable(), resourceFilter)
	}
	labels[v1.LabelTopologyZone] = i.Zone
	// Attempt to resolve the zoneID from the instance's EC2NodeClass' status condition.
	// If the EC2NodeClass is nil, we know we're in the List or Get paths, where we don't care about the zone-id value.
	// If we're in the Create path, we've already validated the EC2NodeClass exists. In this case, we resolve the zone-id from the status condition
	// both when creating offerings and when adding the label.
	if nodeClass != nil {
		if subnet, ok := lo.Find(nodeClass.Status.Subnets, func(s v1beta1.Subnet) bool {
			return s.Zone == i.Zone
		}); ok && subnet.ZoneID != "" {
			labels[v1beta1.LabelTopologyZoneID] = subnet.ZoneID
		}
	}
	labels[corev1beta1.CapacityTypeLabelKey] = i.CapacityType
	if v, ok := i.Tags[corev1beta1.NodePoolLabelKey]; ok {
		labels[corev1beta1.NodePoolLabelKey] = v
	}
	if v, ok := i.Tags[corev1beta1.ManagedByAnnotationKey]; ok {
		annotations[corev1beta1.ManagedByAnnotationKey] = v
	}

	if i.CapacityReservationID != nil {
		labels[v1beta1.LabelCapactiyReservationID] = *i.CapacityReservationID
	}

	nodeClaim.Labels = labels
	nodeClaim.Annotations = annotations
	nodeClaim.CreationTimestamp = metav1.Time{Time: i.LaunchTime}
	// Set the deletionTimestamp to be the current time if the instance is currently terminating
	if i.State == ec2.InstanceStateNameShuttingDown || i.State == ec2.InstanceStateNameTerminated {
		nodeClaim.DeletionTimestamp = &metav1.Time{Time: time.Now()}
	}
	nodeClaim.Status.ProviderID = fmt.Sprintf("aws:///%s/%s", i.Zone, i.ID)
	nodeClaim.Status.ImageID = i.ImageID

	return nodeClaim
}

// newTerminatingNodeClassError returns a NotFound error for handling by
func newTerminatingNodeClassError(name string) *errors.StatusError {
	qualifiedResource := schema.GroupResource{Group: corev1beta1.Group, Resource: "ec2nodeclasses"}
	err := errors.NewNotFound(qualifiedResource, name)
	err.ErrStatus.Message = fmt.Sprintf("%s %q is terminating, treating as not found", qualifiedResource.String(), name)
	return err
}

func (c *CloudProvider) updateUnavailableCapacityReservation(ctx context.Context, nodeClass *v1beta1.EC2NodeClass, err error) {
	// 1. GetInstanceTypes() relies on the NodeClass status capacity reservation information
	// 2. A failure to launch a capacity reservation causes us to update the NodeClass status avaialble count for that CR ID to 0
	//        CreateFleet returns that it can't fulfill the request
	// 3. Launching into a capacity reservation pool causes us to decrement the available count on the NodeClass status for that CR ID
	// 4. When there is an update on the NodeClass capacity reservation status, that should invalidate the current GetInstanceTypes() cache for that NodePool/NodeClass
	// 5. The Capacity reservation provider reconciliation polls to update the capacity reservation status

	if _, ok := err.(*cloudprovider.InsufficientCapacityError); !ok {
		return
	}

	stored := nodeClass.DeepCopy()

	for i, capacityReservation := range nodeClass.Status.CapacityReservations {
		capacityReservation.AvailableInstanceCount = 0
		nodeClass.Status.CapacityReservations[i] = capacityReservation
	}

	if !equality.Semantic.DeepEqual(stored, nodeClass) {
		if err := c.kubeClient.Patch(ctx, nodeClass, client.MergeFrom(stored)); err != nil {
			log.FromContext(ctx).Error(err, fmt.Sprintf("failed to update unavailable capacity reservations for node class %s", nodeClass.Name))
		}
	}
}
