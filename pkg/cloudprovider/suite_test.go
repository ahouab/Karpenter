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
	"net"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/patrickmn/go-cache"
	v1 "k8s.io/api/core/v1"
	clock "k8s.io/utils/clock/testing"
	"knative.dev/pkg/ptr"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	. "knative.dev/pkg/logging/testing"

	"github.com/aws/karpenter-core/pkg/operator/controller"
	"github.com/aws/karpenter/pkg/apis"
	awssettings "github.com/aws/karpenter/pkg/apis/config/settings"
	"github.com/aws/karpenter/pkg/apis/v1alpha1"
	awscache "github.com/aws/karpenter/pkg/cache"
	"github.com/aws/karpenter/pkg/cloudprovider/amifamily"
	awscontext "github.com/aws/karpenter/pkg/context"
	"github.com/aws/karpenter/pkg/test"

	"github.com/aws/karpenter-core/pkg/apis/config/settings"
	corev1alpha5 "github.com/aws/karpenter-core/pkg/apis/v1alpha5"
	"github.com/aws/karpenter-core/pkg/controllers/provisioning"
	"github.com/aws/karpenter-core/pkg/controllers/state"
	"github.com/aws/karpenter-core/pkg/operator/injection"
	"github.com/aws/karpenter-core/pkg/operator/options"
	"github.com/aws/karpenter-core/pkg/operator/scheme"
	coretest "github.com/aws/karpenter-core/pkg/test"
	. "github.com/aws/karpenter-core/pkg/test/expectations"
	"github.com/aws/karpenter-core/pkg/utils/pretty"

	"github.com/aws/karpenter/pkg/fake"
)

var ctx context.Context
var stop context.CancelFunc
var opts options.Options
var env *coretest.Environment
var launchTemplateCache *cache.Cache
var securityGroupCache *cache.Cache
var subnetCache *cache.Cache
var ssmCache *cache.Cache
var ec2Cache *cache.Cache
var internalUnavailableOfferingsCache *cache.Cache
var unavailableOfferingsCache *awscache.UnavailableOfferings
var instanceTypeCache *cache.Cache
var instanceTypeProvider *InstanceTypeProvider
var amiProvider *amifamily.AMIProvider
var fakeEC2API *fake.EC2API
var fakePricingAPI *fake.PricingAPI
var prov *provisioning.Provisioner
var provisioningController controller.Controller
var cloudProvider *CloudProvider
var cluster *state.Cluster
var recorder *coretest.EventRecorder
var fakeClock *clock.FakeClock
var provisioner *corev1alpha5.Provisioner
var provider *v1alpha1.AWS
var pricingProvider *PricingProvider
var settingsStore coretest.SettingsStore

func TestAWS(t *testing.T) {
	ctx = TestContextWithLogger(t)
	RegisterFailHandler(Fail)
	RunSpecs(t, "CloudProvider/AWS")
}

var _ = BeforeSuite(func() {
	env = coretest.NewEnvironment(scheme.Scheme, apis.CRDs...)
	settingsStore = coretest.SettingsStore{
		settings.ContextKey:    coretest.Settings(),
		awssettings.ContextKey: test.Settings(),
	}
	ctx = settingsStore.InjectSettings(ctx)
	ctx, stop = context.WithCancel(ctx)

	launchTemplateCache = cache.New(awscontext.CacheTTL, awscontext.CacheCleanupInterval)
	internalUnavailableOfferingsCache = cache.New(awscache.UnavailableOfferingsTTL, awscontext.CacheCleanupInterval)
	unavailableOfferingsCache = awscache.NewUnavailableOfferings(internalUnavailableOfferingsCache)
	securityGroupCache = cache.New(awscontext.CacheTTL, awscontext.CacheCleanupInterval)
	subnetCache = cache.New(awscontext.CacheTTL, awscontext.CacheCleanupInterval)
	ssmCache = cache.New(awscontext.CacheTTL, awscontext.CacheCleanupInterval)
	ec2Cache = cache.New(awscontext.CacheTTL, awscontext.CacheCleanupInterval)
	instanceTypeCache = cache.New(InstanceTypesAndZonesCacheTTL, awscontext.CacheCleanupInterval)
	fakeEC2API = &fake.EC2API{}
	fakePricingAPI = &fake.PricingAPI{}
	pricingProvider = NewPricingProvider(ctx, fakePricingAPI, fakeEC2API, "", false, make(chan struct{}))
	amiProvider = amifamily.NewAMIProvider(env.Client, fake.SSMAPI{}, fakeEC2API, ssmCache, ec2Cache, env.KubernetesInterface)
	subnetProvider := &SubnetProvider{
		ec2api: fakeEC2API,
		cache:  subnetCache,
		cm:     pretty.NewChangeMonitor(),
	}
	instanceTypeProvider = &InstanceTypeProvider{
		ec2api:               fakeEC2API,
		subnetProvider:       subnetProvider,
		cache:                instanceTypeCache,
		pricingProvider:      pricingProvider,
		unavailableOfferings: unavailableOfferingsCache,
		cm:                   pretty.NewChangeMonitor(),
	}
	securityGroupProvider := &SecurityGroupProvider{
		ec2api: fakeEC2API,
		cache:  securityGroupCache,
		cm:     pretty.NewChangeMonitor(),
	}
	cloudProvider = &CloudProvider{
		instanceTypeProvider: instanceTypeProvider,
		amiProvider:          amiProvider,
		instanceProvider: NewInstanceProvider(ctx, fakeEC2API, instanceTypeProvider, subnetProvider, &LaunchTemplateProvider{
			ec2api:                fakeEC2API,
			amiFamily:             amifamily.New(env.Client, amiProvider),
			securityGroupProvider: securityGroupProvider,
			cache:                 launchTemplateCache,
			caBundle:              ptr.String("ca-bundle"),
			cm:                    pretty.NewChangeMonitor(),
		}),
		kubeClient: env.Client,
	}
	fakeClock = clock.NewFakeClock(time.Now())
	cluster = state.NewCluster(ctx, fakeClock, env.Client, cloudProvider)
	recorder = coretest.NewEventRecorder()
	prov = provisioning.NewProvisioner(ctx, env.Client, env.KubernetesInterface.CoreV1(), recorder, cloudProvider, cluster)
	provisioningController = provisioning.NewController(env.Client, prov, recorder)

	env.CRDDirectoryPaths = append(env.CRDDirectoryPaths, RelativeToRoot("charts/karpenter/crds"))
	provisioning.WaitForClusterSync = false
})

var _ = AfterSuite(func() {
	stop()
	Expect(env.Stop()).To(Succeed(), "Failed to stop environment")
})

var _ = BeforeEach(func() {
	ctx = injection.WithOptions(ctx, opts)
	settingsStore = coretest.SettingsStore{
		settings.ContextKey:    coretest.Settings(),
		awssettings.ContextKey: test.Settings(),
	}
	ctx = settingsStore.InjectSettings(ctx)
	provider = &v1alpha1.AWS{
		AMIFamily:             aws.String(v1alpha1.AMIFamilyAL2),
		SubnetSelector:        map[string]string{"*": "*"},
		SecurityGroupSelector: map[string]string{"*": "*"},
	}

	provisioner = test.Provisioner(coretest.ProvisionerOptions{
		Provider: provider,
		Requirements: []v1.NodeSelectorRequirement{{
			Key:      v1alpha1.LabelInstanceCategory,
			Operator: v1.NodeSelectorOpExists,
		}},
	})

	recorder.Reset()
	fakeEC2API.Reset()
	fakePricingAPI.Reset()
	launchTemplateCache.Flush()
	securityGroupCache.Flush()
	subnetCache.Flush()
	internalUnavailableOfferingsCache.Flush()
	ssmCache.Flush()
	ec2Cache.Flush()
	instanceTypeCache.Flush()
	cloudProvider.instanceProvider.launchTemplateProvider.kubeDNSIP = net.ParseIP("10.0.100.10")
})

var _ = AfterEach(func() {
	ExpectCleanedUp(ctx, env.Client)
})

var _ = Describe("Allocation", func() {
	Context("Defaulting", func() {
		// Intent here is that if updates occur on the provisioningController, the Provisioner doesn't need to be recreated
		It("should not set the InstanceProfile with the default if none provided in Provisioner", func() {
			provisioner.SetDefaults(ctx)
			constraints, err := v1alpha1.Deserialize(provisioner.Spec.Provider)
			Expect(err).ToNot(HaveOccurred())
			Expect(constraints.InstanceProfile).To(BeNil())
		})
		It("should default requirements", func() {
			provisioner.SetDefaults(ctx)
			Expect(provisioner.Spec.Requirements).To(ContainElement(v1.NodeSelectorRequirement{
				Key:      corev1alpha5.LabelCapacityType,
				Operator: v1.NodeSelectorOpIn,
				Values:   []string{corev1alpha5.CapacityTypeOnDemand},
			}))
			Expect(provisioner.Spec.Requirements).To(ContainElement(v1.NodeSelectorRequirement{
				Key:      v1.LabelArchStable,
				Operator: v1.NodeSelectorOpIn,
				Values:   []string{corev1alpha5.ArchitectureAmd64},
			}))
		})
	})
	Context("EC2 Context", func() {
		It("should set context on the CreateFleet request if specified on the Provisioner", func() {
			provider, err := v1alpha1.Deserialize(provisioner.Spec.Provider)
			Expect(err).ToNot(HaveOccurred())
			provider.Context = aws.String("context-1234")
			provisioner = coretest.Provisioner(coretest.ProvisionerOptions{Provider: provider})
			provisioner.SetDefaults(ctx)
			ExpectApplied(ctx, env.Client, provisioner)
			pod := ExpectProvisioned(ctx, env.Client, recorder, provisioningController, prov, coretest.UnschedulablePod())[0]
			ExpectScheduled(ctx, env.Client, pod)
			Expect(fakeEC2API.CalledWithCreateFleetInput.Len()).To(Equal(1))
			createFleetInput := fakeEC2API.CalledWithCreateFleetInput.Pop()
			Expect(aws.StringValue(createFleetInput.Context)).To(Equal("context-1234"))
		})
		It("should default to no EC2 Context", func() {
			provisioner.SetDefaults(ctx)
			ExpectApplied(ctx, env.Client, provisioner)
			pod := ExpectProvisioned(ctx, env.Client, recorder, provisioningController, prov, coretest.UnschedulablePod())[0]
			ExpectScheduled(ctx, env.Client, pod)
			Expect(fakeEC2API.CalledWithCreateFleetInput.Len()).To(Equal(1))
			createFleetInput := fakeEC2API.CalledWithCreateFleetInput.Pop()
			Expect(createFleetInput.Context).To(BeNil())
		})
	})
	Context("Node Drift", func() {
		It("should detect drift if ami gets changed", func() {
			instanceTypes, _ := cloudProvider.GetInstanceTypes(ctx, provisioner)
			node := coretest.Node(coretest.NodeOptions{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						v1alpha1.LabelInstanceAMIID: "ami-changed",
						v1.LabelInstanceTypeStable:  instanceTypes[0].Name,
					},
				},
			})
			isDrifted, err := cloudProvider.IsNodeDrifted(ctx, provisioner, node)
			Expect(err).ToNot(HaveOccurred())
			Expect(isDrifted).To(BeTrue())
		})
		It("should not detect drift if ami isn't changed", func() {
			aws, _ := cloudProvider.getProvider(ctx, provisioner.Spec.Provider, provisioner.Spec.ProviderRef)
			instanceTypes, _ := cloudProvider.GetInstanceTypes(ctx, provisioner)
			validAmis, err := amiProvider.GetAMIsForProvider(ctx, provisioner.Spec.ProviderRef, instanceTypes, aws.AMIFamily)
			Expect(err).ToNot(HaveOccurred())
			node := coretest.Node(coretest.NodeOptions{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						v1alpha1.LabelInstanceAMIID: validAmis[0],
						v1.LabelInstanceTypeStable:  instanceTypes[0].Name,
					},
				},
			})
			isDrifted, err := cloudProvider.IsNodeDrifted(ctx, provisioner, node)
			Expect(err).ToNot(HaveOccurred())
			Expect(isDrifted).To(BeFalse())
		})
		It("should error drift if node doesn't have instance-type label", func() {
			aws, _ := cloudProvider.getProvider(ctx, provisioner.Spec.Provider, provisioner.Spec.ProviderRef)
			instanceTypes, _ := cloudProvider.GetInstanceTypes(ctx, provisioner)
			validAmis, err := amiProvider.GetAMIsForProvider(ctx, provisioner.Spec.ProviderRef, instanceTypes, aws.AMIFamily)
			Expect(err).ToNot(HaveOccurred())
			node := coretest.Node(coretest.NodeOptions{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						v1alpha1.LabelInstanceAMIID: validAmis[0],
					},
				},
			})
			isDrifted, err := cloudProvider.IsNodeDrifted(ctx, provisioner, node)
			Expect(err).To(HaveOccurred())
			Expect(isDrifted).To(BeFalse())
		})
	})
})

func RelativeToRoot(path string) string {
	_, file, _, _ := runtime.Caller(0)
	manifestsRoot := filepath.Join(filepath.Dir(file), "..", "..")
	return filepath.Join(manifestsRoot, path)
}
