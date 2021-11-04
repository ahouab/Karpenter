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

package fake

import (
	"context"
	"fmt"
	"strings"

	"github.com/Pallinder/go-randomdata"
	"github.com/awslabs/karpenter/pkg/apis/provisioning/v1alpha5"
	"github.com/awslabs/karpenter/pkg/cloudprovider"
	"github.com/awslabs/karpenter/pkg/cloudprovider/aws/apis/v1alpha1"
	"knative.dev/pkg/apis"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
)

type CloudProvider struct{}

func (c *CloudProvider) Create(_ context.Context, constraints *v1alpha5.Constraints, instanceTypes []cloudprovider.InstanceType, quantity int, bind func(*v1.Node) error) chan error {
	err := make(chan error)
	for i := 0; i < quantity; i++ {
		name := strings.ToLower(randomdata.SillyName())
		instance := instanceTypes[0]
		zoneSet := sets.String{}
		offeringMap := make(map[string]sets.String)
		constrainedCapacityTypes := constraints.Requirements.CapacityTypes()
		if constrainedCapacityTypes.Len() == 0 {
			constrainedCapacityTypes = sets.String{}.Insert(v1alpha1.CapacityTypeOnDemand)
		}
		for _, o := range instance.Offerings() {
			if !constrainedCapacityTypes.Has(o.CapacityType) {
				continue
			}
			zoneSet.Insert(o.Zone)
			_, exists := offeringMap[o.Zone]
			if !exists {
				offeringMap[o.Zone] = sets.String{}
			}
			offeringMap[o.Zone].Insert(o.CapacityType)
		}
		zone := zoneSet.Intersection(constraints.Requirements.Zones()).UnsortedList()[0]
		capacityType := offeringMap[zone].UnsortedList()[0]

		go func() {
			err <- bind(&v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: name,
					Labels: map[string]string{
						v1.LabelTopologyZone:       zone,
						v1.LabelInstanceTypeStable: instance.Name(),
						v1alpha5.LabelCapacityType: capacityType,
					},
				},
				Spec: v1.NodeSpec{
					ProviderID: fmt.Sprintf("fake:///%s/%s", name, zone),
				},
				Status: v1.NodeStatus{
					NodeInfo: v1.NodeSystemInfo{
						Architecture:    instance.Architecture(),
						OperatingSystem: v1alpha5.OperatingSystemLinux,
					},
					Allocatable: v1.ResourceList{
						v1.ResourcePods:   *instance.Pods(),
						v1.ResourceCPU:    *instance.CPU(),
						v1.ResourceMemory: *instance.Memory(),
					},
				},
			})
		}()
	}
	return err
}

func (c *CloudProvider) GetInstanceTypes(_ context.Context, _ *v1alpha5.Constraints) ([]cloudprovider.InstanceType, error) {
	return []cloudprovider.InstanceType{
		NewInstanceType(InstanceTypeOptions{
			name: "default-instance-type",
		}),
		NewInstanceType(InstanceTypeOptions{
			name:       "nvidia-gpu-instance-type",
			nvidiaGPUs: resource.MustParse("2"),
		}),
		NewInstanceType(InstanceTypeOptions{
			name:    "amd-gpu-instance-type",
			amdGPUs: resource.MustParse("2"),
		}),
		NewInstanceType(InstanceTypeOptions{
			name:       "aws-neuron-instance-type",
			awsNeurons: resource.MustParse("2"),
		}),
		NewInstanceType(InstanceTypeOptions{
			name:         "arm-instance-type",
			architecture: "arm64",
		}),
	}, nil
}

func (c *CloudProvider) Delete(context.Context, *v1.Node) error {
	return nil
}

func (c *CloudProvider) Default(context.Context, *v1alpha5.Constraints) {
}

func (c *CloudProvider) Validate(context.Context, *v1alpha5.Constraints) *apis.FieldError {
	return nil
}
