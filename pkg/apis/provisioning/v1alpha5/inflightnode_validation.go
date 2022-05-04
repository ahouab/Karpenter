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

package v1alpha5

import (
	"context"

	"knative.dev/pkg/apis"
)

func (in *InFlightNode) Validate(ctx context.Context) (errs *apis.FieldError) {
	return errs.Also(
		apis.ValidateObjectMetadata(in).ViaField("metadata"),
		in.Spec.validate(ctx).ViaField("spec"),
	)
}
func (in *InFlightNodeSpec) validate(ctx context.Context) (errs *apis.FieldError) {
	return nil
}
