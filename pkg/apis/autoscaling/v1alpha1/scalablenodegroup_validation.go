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

package v1alpha1

import (
	"github.com/pkg/errors"
)

// +kubebuilder:object:generate=false
type ScalableNodeGroupValidator func(*ScalableNodeGroupSpec) error

var scalableNodeGroupValidators map[NodeGroupType]ScalableNodeGroupValidator = make(map[NodeGroupType]ScalableNodeGroupValidator)

func RegisterScalableNodeGroupValidator(nodeGroupType NodeGroupType, validator ScalableNodeGroupValidator) {
	scalableNodeGroupValidators[nodeGroupType] = validator
}

func (sng *ScalableNodeGroup) Validate() error {
	if validator, ok := scalableNodeGroupValidators[sng.Spec.Type]; ok {
		if err := validator(&sng.Spec); err != nil {
			return errors.Wrap(err, "invalid ScalableNodeGroup")
		}
	}
	return nil
}
