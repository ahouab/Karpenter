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

	"github.com/awslabs/karpenter/pkg/apis/provisioning/v1alpha1"
	"github.com/awslabs/karpenter/pkg/cloudprovider"
	"github.com/awslabs/karpenter/pkg/cloudprovider/aws/packing"
	"go.uber.org/zap"
	v1 "k8s.io/api/core/v1"
)

// Capacity cloud provider implementation using AWS Fleet.
type Capacity struct {
	spec                   *v1alpha1.ProvisionerSpec
	nodeFactory            *NodeFactory
	packer                 packing.Packer
	instanceProvider       *InstanceProvider
	vpcProvider            *VPCProvider
	launchTemplateProvider *LaunchTemplateProvider
	instanceTypeProvider   *InstanceTypeProvider
}

// Create a set of nodes given the constraints.
func (c *Capacity) Create(ctx context.Context, constraints *cloudprovider.Constraints) ([]cloudprovider.Packing, error) {
	// 1. Filter for instance types that fit constraints
	zoneToInstanceTypes, err := c.instanceTypeProvider.Get(ctx, c.spec.Cluster.Name, constraints)
	if err != nil {
		return nil, fmt.Errorf("filtering instance types by constraints, %w", err)
	}

	// 2. Compute Packing given the pods and instance types
	instancePackings := c.packer.Pack(ctx, constraints.Pods, c.instanceTypeProvider.UniqueInstanceTypesFrom(zoneToInstanceTypes))

	zap.S().Debugf("Computed packings for %d pod(s) onto %d node(s)", len(constraints.Pods), len(instancePackings))
	launchTemplate, err := c.launchTemplateProvider.Get(ctx, c.spec.Cluster, constraints)
	if err != nil {
		return nil, fmt.Errorf("getting launch template, %w", err)
	}

	zonalSubnetOptions, err := c.vpcProvider.GetZonalSubnets(ctx, constraints, c.spec.Cluster.Name)
	if err != nil {
		return nil, fmt.Errorf("getting zonal subnets, %w", err)
	}

	// 2. Create Instances
	var instanceIds []*string
	podsForInstance := make(map[string][]*v1.Pod)
	for _, packing := range instancePackings {
		instanceID, err := c.instanceProvider.Create(ctx, launchTemplate,
			c.instanceTypeProvider.InstanceTypesPerZoneFrom(packing.InstanceTypes, zoneToInstanceTypes), zonalSubnetOptions)
		if err != nil {
			// TODO Aggregate errors and continue
			return nil, fmt.Errorf("creating capacity %w", err)
		}
		podsForInstance[*instanceID] = packing.Pods
		instanceIds = append(instanceIds, instanceID)
	}

	// 3. Convert to Nodes
	nodes, err := c.nodeFactory.For(ctx, instanceIds)
	if err != nil {
		return nil, fmt.Errorf("determining nodes, %w", err)
	}
	nodePackings := []cloudprovider.Packing{}
	for instanceID, node := range nodes {
		node.Labels = constraints.Labels
		node.Spec.Taints = constraints.Taints
		nodePackings = append(nodePackings, cloudprovider.Packing{
			Node: node,
			Pods: podsForInstance[instanceID],
		})
	}
	return nodePackings, nil
}

func (c *Capacity) Delete(ctx context.Context, nodes []*v1.Node) error {
	return c.instanceProvider.Terminate(ctx, nodes)
}

func (c *Capacity) GetInstanceTypes(ctx context.Context) ([]string, error) {
	return nil, nil // TODO @bwagner5
}

func (c *Capacity) GetZones(ctx context.Context) ([]string, error) {
	return c.vpcProvider.GetZones(ctx, c.spec.Cluster.Name)
}

func (c *Capacity) GetArchitectures(ctx context.Context) ([]string, error) {
	return []string{
		v1alpha1.ArchitectureAmd64,
		v1alpha1.ArchitectureArm64,
	}, nil
}

func (c *Capacity) GetOperatingSystems(ctx context.Context) ([]string, error) {
	return []string{
		v1alpha1.OperatingSystemLinux,
	}, nil
}
