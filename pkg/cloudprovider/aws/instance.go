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

package aws

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/avast/retry-go"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/ec2/ec2iface"
	"github.com/awslabs/karpenter/pkg/apis/provisioning/v1alpha4"
	"github.com/awslabs/karpenter/pkg/cloudprovider"
	"github.com/awslabs/karpenter/pkg/cloudprovider/aws/apis/v1alpha1"
	"knative.dev/pkg/logging"

	"go.uber.org/multierr"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
)

const (
	EC2InstanceIDNotFoundErrCode = "InvalidInstanceID.NotFound"
)

type InstanceProvider struct {
	ec2api                 ec2iface.EC2API
	instanceTypeProvider   *InstanceTypeProvider
	launchTemplateProvider *LaunchTemplateProvider
	subnetProvider         *SubnetProvider
}

// Create an instance given the constraints.
// instanceTypes should be sorted by priority for spot capacity type.
// If spot is not used, the instanceTypes are not required to be sorted
// because we are using ec2 fleet's lowest-price OD allocation strategy
func (p *InstanceProvider) Create(ctx context.Context, constraints *v1alpha1.Constraints, instanceTypes []cloudprovider.InstanceType, quantity int) ([]*v1.Node, error) {
	// Launch Instance
	ids, err := p.launchInstances(ctx, constraints, instanceTypes, quantity)
	if err != nil {
		return nil, err
	}
	// Get Instance with backoff retry since EC2 is eventually consistent
	instances := []*ec2.Instance{}
	if err := retry.Do(
		func() (err error) { instances, err = p.getInstances(ctx, ids); return err },
		retry.Delay(1*time.Second),
		retry.Attempts(3),
	); err != nil && len(instances) == 0 {
		return nil, err
	} else if err != nil {
		logging.FromContext(ctx).Errorf("retrieving node name for %d instances out of %d", quantity-len(instances), quantity)
	}

	nodes := []*v1.Node{}
	for _, instance := range instances {
		logging.FromContext(ctx).Infof("Launched instance: %s, hostname: %s, type: %s, zone: %s, capacityType: %s",
			aws.StringValue(instance.InstanceId),
			aws.StringValue(instance.PrivateDnsName),
			aws.StringValue(instance.InstanceType),
			aws.StringValue(instance.Placement.AvailabilityZone),
			getCapacityType(instance),
		)

		// Convert Instance to Node
		node, err := p.instanceToNode(ctx, instance, instanceTypes)
		if err != nil {
			logging.FromContext(ctx).Errorf("creating Node from an EC2 Instance: %s", err.Error())
			continue
		}
		nodes = append(nodes, node)
	}
	if len(nodes) == 0 {
		return nil, fmt.Errorf("zero nodes were created")
	}
	return nodes, nil
}

func (p *InstanceProvider) Terminate(ctx context.Context, node *v1.Node) error {
	id, err := getInstanceID(node)
	if err != nil {
		return fmt.Errorf("getting instance ID for node %s, %w", node.Name, err)
	}
	if _, err = p.ec2api.TerminateInstancesWithContext(ctx, &ec2.TerminateInstancesInput{
		InstanceIds: []*string{id},
	}); err != nil {
		if aerr, ok := err.(awserr.Error); ok && aerr.Code() == EC2InstanceIDNotFoundErrCode {
			return nil
		}
		return fmt.Errorf("terminating instance %s, %w", node.Name, err)
	}
	return nil
}

func (p *InstanceProvider) launchInstances(ctx context.Context, constraints *v1alpha1.Constraints, instanceTypes []cloudprovider.InstanceType, quantity int) ([]*string, error) {
	// Default to on-demand unless constrained otherwise. This code assumes two
	// options: {spot, on-demand}, which is enforced by constraints.Constrain().
	// Spot may be selected by constraining the provisioner, or using
	// nodeSelectors, required node affinity, or preferred node affinity.
	capacityType := v1alpha1.CapacityTypeOnDemand
	if len(constraints.CapacityTypes) == 0 {
		return nil, fmt.Errorf("invariant violated, must contain at least one capacity type")
	} else if len(constraints.CapacityTypes) == 1 {
		capacityType = constraints.CapacityTypes[0]
	}
	// Get Launch Template Configs, which may differ due to GPU or Architecture requirements
	launchTemplateConfigs, err := p.getLaunchTemplateConfigs(ctx, constraints, instanceTypes, capacityType)
	if err != nil {
		return nil, fmt.Errorf("getting launch template configs, %w", err)
	}
	// Create fleet
	createFleetOutput, err := p.ec2api.CreateFleetWithContext(ctx, &ec2.CreateFleetInput{
		Type:                  aws.String(ec2.FleetTypeInstant),
		LaunchTemplateConfigs: launchTemplateConfigs,
		TargetCapacitySpecification: &ec2.TargetCapacitySpecificationRequest{
			DefaultTargetCapacityType: aws.String(capacityType),
			TotalTargetCapacity:       aws.Int64(int64(quantity)),
		},
		// OnDemandOptions are allowed to be specified even when requesting spot
		OnDemandOptions: &ec2.OnDemandOptionsRequest{AllocationStrategy: aws.String(ec2.FleetOnDemandAllocationStrategyLowestPrice)},
		// SpotOptions are allowed to be specified even when requesting on-demand
		SpotOptions: &ec2.SpotOptionsRequest{AllocationStrategy: aws.String(ec2.SpotAllocationStrategyCapacityOptimizedPrioritized)},
	})
	if err != nil {
		return nil, fmt.Errorf("creating fleet %w", err)
	}
	instanceIds := combineFleetInstances(*createFleetOutput)
	if len(instanceIds) == 0 {
		return nil, combineFleetErrors(createFleetOutput.Errors)
	} else if len(instanceIds) != quantity {
		logging.FromContext(ctx).Errorf("Failed to launch %d EC2 instances out of the %d EC2 instances requested: %s",
			quantity-len(instanceIds), quantity, combineFleetErrors(createFleetOutput.Errors).Error())
	}
	return instanceIds, nil
}

func (p *InstanceProvider) getLaunchTemplateConfigs(ctx context.Context, constraints *v1alpha1.Constraints, instanceTypes []cloudprovider.InstanceType, capacityType string) ([]*ec2.FleetLaunchTemplateConfigRequest, error) {
	// Get subnets given the constraints
	subnets, err := p.subnetProvider.Get(ctx, constraints)
	if err != nil {
		return nil, fmt.Errorf("getting subnets, %w", err)
	}

	additionalLabels := map[string]string{v1alpha1.CapacityTypeLabel: capacityType}
	var launchTemplateConfigs []*ec2.FleetLaunchTemplateConfigRequest
	launchTemplates, err := p.launchTemplateProvider.Get(ctx, constraints, instanceTypes, additionalLabels)
	if err != nil {
		return nil, fmt.Errorf("getting launch templates, %w", err)
	}
	for launchTemplateName, instanceTypes := range launchTemplates {
		launchTemplateConfigs = append(launchTemplateConfigs, &ec2.FleetLaunchTemplateConfigRequest{
			Overrides: p.getOverrides(instanceTypes, subnets, capacityType),
			LaunchTemplateSpecification: &ec2.FleetLaunchTemplateSpecificationRequest{
				LaunchTemplateName: aws.String(launchTemplateName),
				Version:            aws.String("$Default"),
			},
		})
	}
	return launchTemplateConfigs, nil
}

func (p *InstanceProvider) getOverrides(instanceTypeOptions []cloudprovider.InstanceType, subnets []*ec2.Subnet, capacityType string) []*ec2.FleetLaunchTemplateOverridesRequest {
	var overrides []*ec2.FleetLaunchTemplateOverridesRequest
	for i, instanceType := range instanceTypeOptions {
		for _, zone := range instanceType.Zones() {
			for _, subnet := range subnets {
				if aws.StringValue(subnet.AvailabilityZone) == zone {
					override := &ec2.FleetLaunchTemplateOverridesRequest{
						InstanceType: aws.String(instanceType.Name()),
						SubnetId:     subnet.SubnetId,
					}
					// Add a priority for spot requests since we are using the capacity-optimized-prioritized spot allocation strategy
					// to reduce the likelihood of getting an excessively large instance type.
					// instanceTypeOptions are sorted by vcpus and memory so this prioritizes smaller instance types.
					if capacityType == v1alpha1.CapacityTypeSpot {
						override.Priority = aws.Float64(float64(i))
					}
					overrides = append(overrides, override)
					// FleetAPI cannot span subnets from the same AZ, so break after the first one.
					break
				}
			}
		}
	}
	return overrides
}

func (p *InstanceProvider) getInstances(ctx context.Context, ids []*string) ([]*ec2.Instance, error) {
	describeInstancesOutput, err := p.ec2api.DescribeInstancesWithContext(ctx, &ec2.DescribeInstancesInput{InstanceIds: ids})
	if aerr, ok := err.(awserr.Error); ok && aerr.Code() == EC2InstanceIDNotFoundErrCode {
		return nil, aerr
	}
	if err != nil {
		return nil, fmt.Errorf("failed to describe ec2 instances, %w", err)
	}
	describedInstances := combineReservations(describeInstancesOutput.Reservations)
	if len(describedInstances) != len(ids) {
		return nil, fmt.Errorf("expected %d instance(s), but got %d", len(ids), len(describedInstances))
	}
	instances := []*ec2.Instance{}
	for _, instance := range describedInstances {
		if len(aws.StringValue(instance.PrivateDnsName)) == 0 {
			err = multierr.Append(err, fmt.Errorf("got instance %s but PrivateDnsName was not set", aws.StringValue(instance.InstanceId)))
			continue
		}
		instances = append(instances, instance)
	}
	return instances, err
}

func (p *InstanceProvider) instanceToNode(ctx context.Context, instance *ec2.Instance, instanceTypes []cloudprovider.InstanceType) (*v1.Node, error) {
	for _, instanceType := range instanceTypes {
		if instanceType.Name() == aws.StringValue(instance.InstanceType) {
			return &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: aws.StringValue(instance.PrivateDnsName),
					Labels: map[string]string{
						v1alpha1.CapacityTypeLabel: getCapacityType(instance),
					},
				},
				Spec: v1.NodeSpec{
					ProviderID: fmt.Sprintf("aws:///%s/%s", aws.StringValue(instance.Placement.AvailabilityZone), aws.StringValue(instance.InstanceId)),
				},
				Status: v1.NodeStatus{
					Allocatable: v1.ResourceList{
						v1.ResourcePods:   *instanceType.Pods(),
						v1.ResourceCPU:    *instanceType.CPU(),
						v1.ResourceMemory: *instanceType.Memory(),
					},
					NodeInfo: v1.NodeSystemInfo{
						Architecture:    aws.StringValue(instance.Architecture),
						OSImage:         aws.StringValue(instance.ImageId),
						OperatingSystem: v1alpha4.OperatingSystemLinux,
					},
				},
			}, nil
		}
	}
	return nil, fmt.Errorf("unrecognized instance type %s", aws.StringValue(instance.InstanceType))
}

func getInstanceID(node *v1.Node) (*string, error) {
	id := strings.Split(node.Spec.ProviderID, "/")
	if len(id) < 5 {
		return nil, fmt.Errorf("parsing instance id %s", node.Spec.ProviderID)
	}
	return aws.String(id[4]), nil
}

func combineFleetErrors(errors []*ec2.CreateFleetError) (errs error) {
	unique := sets.NewString()
	for _, err := range errors {
		unique.Insert(fmt.Sprintf("%s: %s", aws.StringValue(err.ErrorCode), aws.StringValue(err.ErrorMessage)))
	}
	for _, errorCode := range unique.List() {
		errs = multierr.Append(errs, fmt.Errorf(errorCode))
	}
	return fmt.Errorf("with fleet error(s), %w", errs)
}

func getCapacityType(instance *ec2.Instance) string {
	capacityType := v1alpha1.CapacityTypeOnDemand
	if instance.SpotInstanceRequestId != nil {
		capacityType = v1alpha1.CapacityTypeSpot
	}
	return capacityType
}

func combineFleetInstances(createFleetOutput ec2.CreateFleetOutput) []*string {
	instanceIds := []*string{}
	for _, reservation := range createFleetOutput.Instances {
		instanceIds = append(instanceIds, reservation.InstanceIds...)
	}
	return instanceIds
}

func combineReservations(reservations []*ec2.Reservation) []*ec2.Instance {
	instances := []*ec2.Instance{}
	for _, reservation := range reservations {
		instances = append(instances, reservation.Instances...)
	}
	return instances
}
