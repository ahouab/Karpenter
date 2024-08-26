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

package termination_test

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"

	"sigs.k8s.io/karpenter/pkg/test"
)

var _ = Describe("Termination", func() {
	It("should terminate the node and the instance on deletion", func() {
		pod := test.Pod()
		env.ExpectCreated(nodeClass, nodePool, pod)
		env.EventuallyExpectHealthy(pod)
		env.ExpectCreatedNodeCount("==", 1)

		nodes := env.Monitor.CreatedNodes()
		instanceID := env.ExpectParsedProviderID(nodes[0].Spec.ProviderID)
		env.GetInstance(nodes[0].Name)

		// Pod is deleted so that we don't re-provision after node deletion
		// NOTE: We have to do this right now to deal with a race condition in nodepool ownership
		// This can be removed once this race is resolved with the NodePool
		env.ExpectDeleted(pod)

		// Node is deleted and now should be not found
		env.ExpectDeleted(nodes[0])
		env.EventuallyExpectNotFound(nodes[0])
		Eventually(func(g Gomega) {
			g.Expect(lo.FromPtr(env.GetInstanceByID(instanceID).State.Name)).To(BeElementOf("terminated", "shutting-down"))
		}, time.Second*10).Should(Succeed())
	})
	// Pods from Karpenter nodes are expected to drain in the following order:
	//   1. Non-Critical Non-Daemonset pods
	//   2. Non-Critical Daemonset pods
	//   3. Critical Non-Daemonset pods
	//   4. Critical Daemonset pods
	// Pods in one group are expected to be fully removed before the next group is executed
	It("should drain pods on a node in order", func() {
		nonCriticalDaemonset := test.DaemonSet(test.DaemonSetOptions{
			Selector: map[string]string{"app": "non-critical-daemonset"},
			PodOptions: test.PodOptions{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"drain-test": "true",
						"app":        "non-critical-daemonset",
					},
				},
				TerminationGracePeriodSeconds: lo.ToPtr(int64(60)),
				Image:                         "alpine:3.20.2",
				Command:                       []string{"/bin/sh", "-c", "sleep 1000"},
				PreStopSleep:                  lo.ToPtr(int64(60)),
				ResourceRequirements:          corev1.ResourceRequirements{Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi")}},
			},
		})
		criticalDaemonSet := test.DaemonSet(test.DaemonSetOptions{
			Selector: map[string]string{"app": "critical-daemonset"},
			PodOptions: test.PodOptions{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"drain-test": "true",
						"app":        "critical-daemonset",
					},
				},
				TerminationGracePeriodSeconds: lo.ToPtr(int64(10)), // shorter terminationGracePeriod since it's the last pod
				Image:                         "alpine:3.20.2",
				Command:                       []string{"/bin/sh", "-c", "sleep 1000"},
				PreStopSleep:                  lo.ToPtr(int64(10)), // shorter preStopSleep since it's the last pod
				PriorityClassName:             "system-node-critical",
				ResourceRequirements:          corev1.ResourceRequirements{Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi")}},
			},
		})
		nonCriticalDeployment := test.Deployment(test.DeploymentOptions{
			Replicas: int32(1),
			PodOptions: test.PodOptions{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"drain-test": "true",
						"app":        "non-critical-deployment",
					},
				},
				TerminationGracePeriodSeconds: lo.ToPtr(int64(60)),
				Image:                         "alpine:3.20.2",
				Command:                       []string{"/bin/sh", "-c", "sleep 1000"},
				PreStopSleep:                  lo.ToPtr(int64(60)),
				ResourceRequirements:          corev1.ResourceRequirements{Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi")}},
			},
		})
		criticalDeployment := test.Deployment(test.DeploymentOptions{
			Replicas: int32(1),
			PodOptions: test.PodOptions{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"drain-test": "true",
						"app":        "critical-deployment",
					},
				},
				TerminationGracePeriodSeconds: lo.ToPtr(int64(60)),
				Image:                         "alpine:3.20.2",
				Command:                       []string{"/bin/sh", "-c", "sleep 1000"},
				PreStopSleep:                  lo.ToPtr(int64(60)),
				PriorityClassName:             "system-node-critical",
				ResourceRequirements:          corev1.ResourceRequirements{Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi")}},
			},
		})
		env.ExpectCreated(nodeClass, nodePool, nonCriticalDaemonset, criticalDaemonSet, nonCriticalDeployment, criticalDeployment)

		nodeClaim := env.EventuallyExpectCreatedNodeClaimCount("==", 1)[0]
		_ = env.EventuallyExpectCreatedNodeCount("==", 1)[0]
		env.EventuallyExpectHealthyPodCount(labels.SelectorFromSet(map[string]string{"drain-test": "true"}), 4)

		nonCriticalDaemonsetPod := env.ExpectPodsMatchingSelector(labels.SelectorFromSet(map[string]string{"app": "non-critical-daemonset"}))[0]
		criticalDaemonsetPod := env.ExpectPodsMatchingSelector(labels.SelectorFromSet(map[string]string{"app": "critical-daemonset"}))[0]
		nonCriticalDeploymentPod := env.ExpectPodsMatchingSelector(labels.SelectorFromSet(map[string]string{"app": "non-critical-deployment"}))[0]
		criticalDeploymentPod := env.ExpectPodsMatchingSelector(labels.SelectorFromSet(map[string]string{"app": "critical-deployment"}))[0]

		env.ExpectDeleted(nodeClaim)

		// Wait for non-critical deployment pod to drain and delete
		env.EventuallyExpectTerminating(nonCriticalDeploymentPod)
		// We check that other pods are live for 30s since pre-stop sleep and terminationGracePeriod are 60s
		env.ConsistentlyExpectLivePods(time.Second*30, nonCriticalDaemonsetPod, criticalDeploymentPod, criticalDaemonsetPod)
		env.EventuallyExpectNotFound(nonCriticalDeploymentPod)

		// Wait for non-critical daemonset pod to drain and delete
		env.EventuallyExpectTerminating(nonCriticalDaemonsetPod)
		// We check that other pods are live for 30s since pre-stop sleep and terminationGracePeriod are 60s
		env.ConsistentlyExpectLivePods(time.Second*30, criticalDeploymentPod, criticalDaemonsetPod)
		env.EventuallyExpectNotFound(nonCriticalDaemonsetPod)

		// Wait for critical deployment pod to drain and delete
		env.EventuallyExpectTerminating(criticalDeploymentPod)
		// We check that other pods are live for 30s since pre-stop sleep and terminationGracePeriod are 60s
		env.ConsistentlyExpectLivePods(time.Second*30, criticalDaemonsetPod)
		env.EventuallyExpectNotFound(criticalDeploymentPod)

		// Wait for critical daemonset pod to drain and delete
		env.EventuallyExpectTerminating(criticalDaemonsetPod)
		env.EventuallyExpectNotFound(criticalDeploymentPod)
	})
})
