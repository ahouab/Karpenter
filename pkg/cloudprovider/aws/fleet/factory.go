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

package fleet

import (
	"time"

	"github.com/aws/aws-sdk-go/service/ec2/ec2iface"
	"github.com/aws/aws-sdk-go/service/iam/iamiface"
	"github.com/awslabs/karpenter/pkg/apis/provisioning/v1alpha1"
	"github.com/patrickmn/go-cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// CacheTTL restricts QPS to AWS APIs to this interval for verifying setup resources.
	CacheTTL              = 5 * time.Minute
	// CacheCleanupInterval triggers cache cleanup (lazy eviction) at this interval.
	CacheCleanupInterval  = 10 * time.Minute
	// ClusterTagKeyFormat is set on all Kubernetes owned resources.
	ClusterTagKeyFormat   = "kubernetes.io/cluster/%s"
	// KarpenterTagKeyFormat is set on all Karpenter owned resources.
	KarpenterTagKeyFormat = "karpenter.sh/cluster/%s"
)

func NewFactory(ec2 ec2iface.EC2API, iam iamiface.IAMAPI, kubeClient client.Client) *Factory {
	return &Factory{
		ec2: ec2,
		launchTemplateProvider: &LaunchTemplateProvider{
			launchTemplateCache: cache.New(CacheTTL, CacheCleanupInterval),
			ec2:                 ec2,
			instanceProfileProvider: &InstanceProfileProvider{
				iam:                  iam,
				kubeClient:           kubeClient,
				instanceProfileCache: cache.New(CacheTTL, CacheCleanupInterval),
			},
			securityGroupProvider: &SecurityGroupProvider{
				ec2:                ec2,
				securityGroupCache: cache.New(CacheTTL, CacheCleanupInterval),
			},
		},
		subnetProvider: &SubnetProvider{
			ec2:         ec2,
			subnetCache: cache.New(CacheTTL, CacheCleanupInterval),
		},
		nodeFactory: &NodeFactory{
			ec2: ec2,
		},
	}
}

type Factory struct {
	ec2                    ec2iface.EC2API
	launchTemplateProvider *LaunchTemplateProvider
	nodeFactory            *NodeFactory
	subnetProvider         *SubnetProvider
}

func (f *Factory) For(spec *v1alpha1.ProvisionerSpec) *Capacity {
	return &Capacity{
		spec:                   spec,
		ec2:                    f.ec2,
		launchTemplateProvider: f.launchTemplateProvider,
		nodeFactory:            f.nodeFactory,
		subnetProvider:         f.subnetProvider,
	}
}
