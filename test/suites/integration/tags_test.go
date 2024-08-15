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

package integration_test

import (
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/awslabs/operatorpkg/object"

	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	coretest "sigs.k8s.io/karpenter/pkg/test"

	v1 "github.com/aws/karpenter-provider-aws/pkg/apis/v1"
	"github.com/aws/karpenter-provider-aws/pkg/providers/instance"
	"github.com/aws/karpenter-provider-aws/pkg/test"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

const createdAtTag = "node.k8s.amazonaws.com/createdAt"

var _ = Describe("Tags", func() {
	Context("Static Tags", func() {
		It("should tag all associated resources", func() {
			nodeClass.Spec.Tags["TestTag"] = "TestVal"
			pod := coretest.Pod()

			env.ExpectCreated(pod, nodeClass, nodePool)
			env.EventuallyExpectHealthy(pod)
			env.ExpectCreatedNodeCount("==", 1)
			instance := env.GetInstance(pod.Spec.NodeName)
			volumes := env.GetVolumes(lo.Map(instance.BlockDeviceMappings, func(bdm *ec2.InstanceBlockDeviceMapping, _ int) *string {
				return bdm.Ebs.VolumeId
			})...)
			networkInterfaces := env.GetNetworkInterfaces(lo.Map(instance.NetworkInterfaces, func(ni *ec2.InstanceNetworkInterface, _ int) *string {
				return ni.NetworkInterfaceId
			})...)

			Expect(instance.Tags).To(ContainElement(&ec2.Tag{Key: lo.ToPtr("TestTag"), Value: lo.ToPtr("TestVal")}))
			for _, volume := range volumes {
				Expect(volume.Tags).To(ContainElement(&ec2.Tag{Key: lo.ToPtr("TestTag"), Value: lo.ToPtr("TestVal")}))
			}
			for _, networkInterface := range networkInterfaces {
				// Any ENI that contains this createdAt tag was created by the VPC CNI DaemonSet
				if !lo.ContainsBy(networkInterface.TagSet, func(t *ec2.Tag) bool { return lo.FromPtr(t.Key) == createdAtTag }) {
					Expect(networkInterface.TagSet).To(ContainElement(&ec2.Tag{Key: lo.ToPtr("TestTag"), Value: lo.ToPtr("TestVal")}))
				}
			}
		})
		It("should tag spot instance requests when creating resources", func() {
			coretest.ReplaceRequirements(nodePool, karpv1.NodeSelectorRequirementWithMinValues{
				NodeSelectorRequirement: corev1.NodeSelectorRequirement{
					Key:      karpv1.CapacityTypeLabelKey,
					Operator: corev1.NodeSelectorOpIn,
					Values:   []string{karpv1.CapacityTypeSpot},
				}})
			nodeClass.Spec.Tags = map[string]string{"TestTag": "TestVal"}
			pod := coretest.Pod()

			env.ExpectCreated(pod, nodeClass, nodePool)
			env.EventuallyExpectHealthy(pod)
			env.ExpectCreatedNodeCount("==", 1)
			instance := env.GetInstance(pod.Spec.NodeName)
			Expect(instance.SpotInstanceRequestId).ToNot(BeNil())
			spotInstanceRequest := env.GetSpotInstanceRequest(instance.SpotInstanceRequestId)
			Expect(spotInstanceRequest.Tags).To(ContainElement(&ec2.Tag{Key: lo.ToPtr("TestTag"), Value: lo.ToPtr("TestVal")}))
		})
		It("should tag managed instance profiles", func() {
			nodeClass.Spec.Tags["TestTag"] = "TestVal"
			env.ExpectCreated(nodeClass)

			profile := env.EventuallyExpectInstanceProfileExists(env.GetInstanceProfileName(nodeClass))
			Expect(profile.Tags).To(ContainElements(
				&iam.Tag{Key: lo.ToPtr(fmt.Sprintf("kubernetes.io/cluster/%s", env.ClusterName)), Value: lo.ToPtr("owned")},
				&iam.Tag{Key: lo.ToPtr(v1.LabelNodeClass), Value: lo.ToPtr(nodeClass.Name)},
				&iam.Tag{Key: lo.ToPtr(v1.EKSClusterNameTagKey), Value: lo.ToPtr(env.ClusterName)},
			))
		})
		It("should tag managed instance profiles with the eks:eks-cluster-name tag key after restart", func() {
			env.ExpectCreated(nodeClass)
			env.EventuallyExpectInstanceProfileExists(env.GetInstanceProfileName(nodeClass))

			_, err := env.IAMAPI.UntagInstanceProfile(&iam.UntagInstanceProfileInput{
				InstanceProfileName: lo.ToPtr(env.GetInstanceProfileName(nodeClass)),
				TagKeys: []*string{
					lo.ToPtr(v1.EKSClusterNameTagKey),
				},
			})
			Expect(err).ToNot(HaveOccurred())

			// Restart Karpenter to flush the instance profile cache and to trigger re-tagging of the instance profile
			env.EventuallyExpectKarpenterRestarted()

			Eventually(func(g Gomega) {
				out, err := env.IAMAPI.GetInstanceProfile(&iam.GetInstanceProfileInput{
					InstanceProfileName: lo.ToPtr(env.GetInstanceProfileName(nodeClass)),
				})
				g.Expect(err).ToNot(HaveOccurred())
				g.Expect(out.InstanceProfile.Tags).To(ContainElement(&iam.Tag{Key: lo.ToPtr(v1.EKSClusterNameTagKey), Value: lo.ToPtr(env.ClusterName)}))
			}).WithTimeout(time.Second * 20).Should(Succeed())
		})
	})

	Context("Tagging Controller", func() {
		It("should tag with karpenter.sh/nodeclaim and Name tag", func() {
			pod := coretest.Pod()

			env.ExpectCreated(nodePool, nodeClass, pod)
			env.EventuallyExpectCreatedNodeCount("==", 1)
			node := env.EventuallyExpectInitializedNodeCount("==", 1)[0]
			nodeClaim := env.ExpectNodeClaimCount("==", 1)[0]

			Eventually(func(g Gomega) {
				g.Expect(env.Client.Get(env.Context, client.ObjectKeyFromObject(nodeClaim), nodeClaim)).To(Succeed())
				g.Expect(nodeClaim.Annotations).To(HaveKeyWithValue(v1.AnnotationInstanceTagged, "true"))
				g.Expect(nodeClaim.Annotations).To(HaveKeyWithValue(v1.AnnotationClusterNameTaggedCompatability, "true"))
			}, time.Minute).Should(Succeed())

			nodeInstance := instance.NewInstance(lo.ToPtr(env.GetInstance(node.Name)))
			Expect(nodeInstance.Tags).To(HaveKeyWithValue("Name", node.Name))
			Expect(nodeInstance.Tags).To(HaveKeyWithValue("karpenter.sh/nodeclaim", nodeClaim.Name))
			Expect(nodeInstance.Tags).To(HaveKeyWithValue("eks:eks-cluster-name", env.ClusterName))
		})
		It("shouldn't overwrite custom Name tags", func() {
			nodeClass = test.EC2NodeClass(*nodeClass, v1.EC2NodeClass{Spec: v1.EC2NodeClassSpec{
				Tags: map[string]string{"Name": "custom-name", "testing/cluster": env.ClusterName},
			}})
			if env.PrivateCluster {
				nodeClass.Spec.Role = ""
				nodeClass.Spec.InstanceProfile = lo.ToPtr(fmt.Sprintf("KarpenterNodeInstanceProfile-%s", env.ClusterName))
			}
			nodePool = coretest.NodePool(*nodePool, karpv1.NodePool{
				Spec: karpv1.NodePoolSpec{
					Template: karpv1.NodeClaimTemplate{
						Spec: karpv1.NodeClaimTemplateSpec{
							NodeClassRef: &karpv1.NodeClassReference{
								Group: object.GVK(nodeClass).Group,
								Kind:  object.GVK(nodeClass).Kind,
								Name:  nodeClass.Name,
							},
						},
					},
				},
			})
			pod := coretest.Pod()

			env.ExpectCreated(nodePool, nodeClass, pod)
			env.EventuallyExpectCreatedNodeCount("==", 1)
			node := env.EventuallyExpectInitializedNodeCount("==", 1)[0]
			nodeClaim := env.ExpectNodeClaimCount("==", 1)[0]

			Eventually(func(g Gomega) {
				g.Expect(env.Client.Get(env.Context, client.ObjectKeyFromObject(nodeClaim), nodeClaim)).To(Succeed())
				g.Expect(nodeClaim.Annotations).To(HaveKeyWithValue(v1.AnnotationInstanceTagged, "true"))
				g.Expect(nodeClaim.Annotations).To(HaveKeyWithValue(v1.AnnotationClusterNameTaggedCompatability, "true"))
			}, time.Minute).Should(Succeed())

			nodeInstance := instance.NewInstance(lo.ToPtr(env.GetInstance(node.Name)))
			Expect(nodeInstance.Tags).To(HaveKeyWithValue("Name", "custom-name"))
			Expect(nodeInstance.Tags).To(HaveKeyWithValue("karpenter.sh/nodeclaim", nodeClaim.Name))
			Expect(nodeInstance.Tags).To(HaveKeyWithValue("eks:eks-cluster-name", env.ClusterName))
		})
		It("should tag instance with eks:eks-cluster-name tag when the tag doesn't exist", func() {
			pod := coretest.Pod()

			env.ExpectCreated(nodePool, nodeClass, pod)
			env.EventuallyExpectCreatedNodeCount("==", 1)
			node := env.EventuallyExpectInitializedNodeCount("==", 1)[0]
			nodeClaim := env.ExpectNodeClaimCount("==", 1)[0]

			Eventually(func(g Gomega) {
				g.Expect(env.Client.Get(env.Context, client.ObjectKeyFromObject(nodeClaim), nodeClaim)).To(Succeed())
				g.Expect(nodeClaim.Annotations).To(HaveKeyWithValue(v1.AnnotationInstanceTagged, "true"))
				g.Expect(nodeClaim.Annotations).To(HaveKeyWithValue(v1.AnnotationClusterNameTaggedCompatability, "true"))
			}, time.Minute).Should(Succeed())

			_, err := env.EC2API.DeleteTags(&ec2.DeleteTagsInput{
				Resources: []*string{lo.ToPtr(env.ExpectParsedProviderID(node.Spec.ProviderID))},
				Tags:      []*ec2.Tag{{Key: lo.ToPtr(v1.EKSClusterNameTagKey)}},
			})
			Expect(err).ToNot(HaveOccurred())

			By(fmt.Sprintf("Removing the %s annotation to re-trigger tagging", v1.AnnotationClusterNameTaggedCompatability))
			Expect(env.Client.Get(env.Context, client.ObjectKeyFromObject(nodeClaim), nodeClaim)).To(Succeed())
			delete(nodeClaim.Annotations, v1.AnnotationClusterNameTaggedCompatability)
			env.ExpectUpdated(nodeClaim)

			By(fmt.Sprintf("Polling for the %s tag update", v1.EKSClusterNameTagKey))
			Eventually(func(g Gomega) {
				out, err := env.EC2API.DescribeInstances(&ec2.DescribeInstancesInput{
					InstanceIds: []*string{lo.ToPtr(env.ExpectParsedProviderID(node.Spec.ProviderID))},
				})
				g.Expect(err).ToNot(HaveOccurred())
				g.Expect(out.Reservations).To(HaveLen(1))
				g.Expect(out.Reservations[0].Instances).To(HaveLen(1))
				g.Expect(out.Reservations[0].Instances[0].Tags).To(ContainElement(&ec2.Tag{Key: lo.ToPtr(v1.EKSClusterNameTagKey), Value: lo.ToPtr(env.ClusterName)}))
			}).Should(Succeed())
		})
	})
})
