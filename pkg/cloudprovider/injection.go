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

	"github.com/aws/karpenter/pkg/utils/injection"
)

type injectionKey struct{}

func Inject(ctx context.Context, value CloudProvider) context.Context {
	return context.WithValue(ctx, injectionKey{}, value)
}

func Get(ctx context.Context) CloudProvider {
	return injection.Get(ctx, injectionKey{}).(CloudProvider)
}
