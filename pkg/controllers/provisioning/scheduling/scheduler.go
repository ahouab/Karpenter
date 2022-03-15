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

package scheduling

import (
	"context"
	"fmt"

	"github.com/aws/karpenter/pkg/utils/resources"

	"github.com/prometheus/client_golang/prometheus"
	v1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	crmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	"github.com/aws/karpenter/pkg/apis/provisioning/v1alpha5"
	"github.com/aws/karpenter/pkg/cloudprovider"
	"github.com/aws/karpenter/pkg/metrics"
	"github.com/aws/karpenter/pkg/utils/injection"
)

var schedulingDuration = prometheus.NewHistogramVec(
	prometheus.HistogramOpts{
		Namespace: metrics.Namespace,
		Subsystem: "allocation_controller",
		Name:      "scheduling_duration_seconds",
		Help:      "Duration of scheduling process in seconds. Broken down by provisioner and error.",
		Buckets:   metrics.DurationBuckets(),
	},
	[]string{metrics.ProvisionerLabel},
)

func init() {
	crmetrics.Registry.MustRegister(schedulingDuration)
}

type Scheduler struct {
	kubeClient client.Client
	topology   *Topology
}

func NewScheduler(kubeClient client.Client) *Scheduler {
	return &Scheduler{
		kubeClient: kubeClient,
		topology:   &Topology{kubeClient: kubeClient},
	}
}

func (s *Scheduler) Solve(ctx context.Context, provisioner *v1alpha5.Provisioner, instanceTypes []cloudprovider.InstanceType, pods []*v1.Pod) ([]*Node, error) {
	defer metrics.Measure(schedulingDuration.WithLabelValues(injection.GetNamespacedName(ctx).Name))()
	constraints := provisioner.Spec.Constraints.DeepCopy()
	// Inject temporarily adds specific NodeSelectors to pods, which are then
	// used by scheduling logic. This isn't strictly necessary, but is a useful
	// trick to avoid passing topology decisions through the scheduling code. It
	// lets us treat TopologySpreadConstraints as just-in-time NodeSelectors.
	if err := s.topology.Inject(ctx, constraints, pods); err != nil {
		return nil, fmt.Errorf("injecting topology, %w", err)
	}
	return s.schedule(constraints, instanceTypes, pods), nil
}

// schedule separates pods into a list of TheoreticalNodes that contain the pods. All pods in each theoretical node
// contain isomorphic scheduling constraints and can be deployed together on the same node, or multiple similar nodes if
// the pods exceed one node's capacity.
func (s *Scheduler) schedule(constraints *v1alpha5.Constraints, instanceTypes []cloudprovider.InstanceType, pods []*v1.Pod) []*Node {
	var nodes []*Node

	genericInstanceTypes := s.filterGenericInstanceTypes(instanceTypes)

	podSets := s.splitByResourceRequestTypes(pods)
	for _, ps := range podSets {
		var newNodes []*Node

		for _, pod := range ps.pods {
			isScheduled := false

			for _, node := range nodes {
				if err := node.Compatible(pod); err == nil {
					node.Add(pod)
					isScheduled = true
					break
				}
			}

			if !isScheduled {
				var n *Node
				if !ps.isExotic {
					n = NewNode(constraints, genericInstanceTypes, pod)
				} else {
					n = NewNode(constraints, instanceTypes, pod)
				}
				nodes = append(nodes, n)
				newNodes = append(newNodes, n)
			}
		}

		// if the pod set contained pods that needed exotic resources, we currently want to prevent any non-exotic
		// pods from scheduling there as they may increase the instance size unnecessarily.  Once bin-packing and
		// scheduling are combined, we can just pin the instance type to the lowest one that supports the exotic
		// workload but still allow scheduling any non-exotic workloads
		// TODO(todd): when combining bin-packing with scheduling, just pin the instance type instead of preventing scheduling
		if ps.isExotic {
			for _, node := range newNodes {
				node.unschedulable = true
			}
		}
	}
	return nodes
}

type podSet struct {
	pods     []*v1.Pod
	isExotic bool
}

// splitByResourceRequestTypes splits a list of pods into podSets by their usage of exotic resource types.  This allows
// us to use any excess capacity on an instance type with exotic resources, but not scale that instance type up to the
// next larger size due to a non-exotic workload.
func (s *Scheduler) splitByResourceRequestTypes(pods []*v1.Pod) []podSet {
	var genericPods []*v1.Pod
	exoticPods := map[v1.ResourceName][]*v1.Pod{}

	for _, p := range pods {
		hadExoticResources := false
		for k, v := range resources.RequestsForPods(p) {
			if !v.IsZero() && cloudprovider.ResourceRegistration[k] == cloudprovider.ResourceFlagMinimizeUsage {
				exoticPods[k] = append(exoticPods[k], p)
				hadExoticResources = true
				break
			}
		}
		if !hadExoticResources {
			genericPods = append(genericPods, p)
		}
	}
	var podSets []podSet
	for _, v := range exoticPods {
		podSets = append(podSets, podSet{
			pods:     v,
			isExotic: true,
		})
	}
	podSets = append(podSets, podSet{
		pods:     genericPods,
		isExotic: false,
	})
	return podSets
}

// filterGenericInstanceTypes returns the list of instance types that only contain non-exotic hardware
func (s *Scheduler) filterGenericInstanceTypes(instanceTypes []cloudprovider.InstanceType) []cloudprovider.InstanceType {
	var generic []cloudprovider.InstanceType
	for _, it := range instanceTypes {
		exotic := false
		for name, quant := range it.Resources() {
			if !quant.IsZero() && cloudprovider.ResourceRegistration[name] == cloudprovider.ResourceFlagMinimizeUsage {
				exotic = true
				break
			}
		}
		if !exotic {
			generic = append(generic, it)
		}
	}
	return generic
}
