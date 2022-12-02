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

package amifamily

import (
	"context"

	"k8s.io/apimachinery/pkg/types"
	"knative.dev/pkg/logging"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/aws/karpenter-core/pkg/apis/v1alpha5"
	"github.com/aws/karpenter/pkg/apis/v1alpha1"
)

type UserDataProvider struct {
	kubeClient client.Client
}

// NewUserDataProvider constructs a new UserData provider
func NewUserDataProvider(kubeClient client.Client) *UserDataProvider {
	return &UserDataProvider{
		kubeClient: kubeClient,
	}
}

// Get returns the UserData from the ConfigMap specified in the provider
func (u *UserDataProvider) Get(ctx context.Context, providerRef *v1alpha5.ProviderRef) (string, error) {
	if providerRef == nil {
		return "", nil
	}
	var awsnodetemplate v1alpha1.AWSNodeTemplate
	if err := u.kubeClient.Get(ctx, types.NamespacedName{Name: providerRef.Name}, &awsnodetemplate); err != nil {
		logging.FromContext(ctx).Errorf("retrieving provider reference, %s", err)
		return "", err
	}
	if awsnodetemplate.Spec.UserData == nil {
		return "", nil
	}
	return *awsnodetemplate.Spec.UserData, nil
}
