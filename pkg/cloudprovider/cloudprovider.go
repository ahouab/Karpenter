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

// CloudProvider abstracts all instantiation logic.
type CloudProvider interface {
	// NewNodeGroup returns a new NodeGroup for the CloudProvider
	NewNodeGroup(name string) NodeGroup
	// NewQueue returns a Queue for the CloudProvider
}

// NodeGroup abstracts all provider specific behavior for NodeGroups.
type NodeGroup interface {
	// Name returns the name of the node group
	Name() string

	// SetReplicas sets the NodeGroups's replica count
	SetReplicas(value int) error
}

// Queue abstracts all provider specific behavior for Queues
type Queue interface {
	// Name returns the name of the queue
	Name() string
	// Length returns the length of the queue
	Length() (int64, error)
	// OldestMessageAge returns the age of the oldest message
	OldestMessageAge() (int64, error)
}
