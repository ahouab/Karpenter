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

package capacityreservation

import (
	"context"
	"fmt"
	"sync"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/ec2/ec2iface"
	"github.com/mitchellh/hashstructure/v2"
	"github.com/patrickmn/go-cache"
	"github.com/samber/lo"
	"knative.dev/pkg/logging"

	"sigs.k8s.io/karpenter/pkg/utils/pretty"

	"github.com/aws/karpenter-provider-aws/pkg/apis/v1beta1"
)

type Provider interface {
	List(context.Context, *v1beta1.EC2NodeClass) ([]*ec2.CapacityReservation, error)
}

type DefaultProvider struct {
	sync.Mutex
	ec2api ec2iface.EC2API
	cache  *cache.Cache
	cm     *pretty.ChangeMonitor
}

func NewDefaultProvider(ec2api ec2iface.EC2API, cache *cache.Cache) *DefaultProvider {
	return &DefaultProvider{
		ec2api: ec2api,
		cm:     pretty.NewChangeMonitor(),
		cache:  cache,
	}
}

func (p *DefaultProvider) List(ctx context.Context, nodeClass *v1beta1.EC2NodeClass) ([]*ec2.CapacityReservation, error) {
	p.Lock()
	defer p.Unlock()

	capacityReservations, err := p.getCapacityReservations(ctx, nodeClass.Spec.CapacityReservationSelectorTerms)
	if err != nil {
		return nil, fmt.Errorf("get capacity reservations, %w", err)
	}
	if p.cm.HasChanged(fmt.Sprintf("capacity-reservations/%s", nodeClass.Name), capacityReservations) {
		logging.FromContext(ctx).
			With("capacity-reservations", lo.Map(capacityReservations, func(s *ec2.CapacityReservation, _ int) string {
				return aws.StringValue(s.CapacityReservationId)
			})).
			Debugf("discovered capacity reservations")
	}
	return capacityReservations, nil
}

func (p *DefaultProvider) getCapacityReservations(ctx context.Context, terms []v1beta1.CapacityReservationSelectorTerm) ([]*ec2.CapacityReservation, error) {
	hash, err := hashstructure.Hash(terms, hashstructure.FormatV2, &hashstructure.HashOptions{SlicesAsSets: true})
	if err != nil {
		return nil, err
	}

	if cr, ok := p.cache.Get(fmt.Sprint(hash)); ok {
		return cr.([]*ec2.CapacityReservation), nil
	}

	capacityReservationsUnfiltered, err := p.describeCapacityReservations(ctx)
	if err != nil {
		return nil, err
	}

	capacityReservationsMap := map[string]*ec2.CapacityReservation{}
	for _, term := range terms {
		capacityReservations := getCapacityReservations(capacityReservationsUnfiltered, term)
		for _, capacityReservation := range capacityReservations {
			capacityReservationsMap[lo.FromPtr(capacityReservation.CapacityReservationId)] = capacityReservation
		}
	}
	p.cache.SetDefault(fmt.Sprint(hash), lo.Values(capacityReservationsMap))
	return lo.Values(capacityReservationsMap), nil
}

func (p *DefaultProvider) describeCapacityReservations(ctx context.Context) ([]*ec2.CapacityReservation, error) {
	describeCapacityReservations := []*ec2.CapacityReservation{}

	err := p.ec2api.DescribeCapacityReservationsPagesWithContext(
		ctx,
		&ec2.DescribeCapacityReservationsInput{
			Filters: []*ec2.Filter{
				{
					Name:   aws.String("state"),
					Values: aws.StringSlice([]string{ec2.CapacityReservationFleetStateActive}),
				},
			},
		},
		func(describeCapacityReservationsOutput *ec2.DescribeCapacityReservationsOutput, lastPage bool) bool {
			describeCapacityReservations = append(
				describeCapacityReservations,
				describeCapacityReservationsOutput.CapacityReservations...,
			)
			return !lastPage
		},
	)
	if err != nil {
		return nil, err
	}

	return describeCapacityReservations, nil
}

func getCapacityReservations(
	capacityReservationsUnfiltered []*ec2.CapacityReservation,
	term v1beta1.CapacityReservationSelectorTerm,
) []*ec2.CapacityReservation {
	capacityReservations := []*ec2.CapacityReservation{}

	for _, capacityReservation := range capacityReservationsUnfiltered {
		if matches(capacityReservation, term) {
			capacityReservations = append(
				capacityReservations,
				capacityReservation,
			)
		}
	}

	return capacityReservations
}

func matches(
	capacityReservation *ec2.CapacityReservation,
	term v1beta1.CapacityReservationSelectorTerm,
) bool {
	if term.ID != "" {
		return term.ID == aws.StringValue(capacityReservation.CapacityReservationId)
	}

	if term.Type != "" {
		if term.Type != aws.StringValue(capacityReservation.InstanceMatchCriteria) {
			return false
		}
	}

	if term.AvailabilityZone != "" {
		if term.AvailabilityZone != aws.StringValue(capacityReservation.AvailabilityZone) {
			return false
		}
	}

	if term.InstanceType != "" {
		if term.InstanceType != aws.StringValue(capacityReservation.InstanceType) {
			return false
		}
	}

	if term.OwnerID != "" {
		if term.OwnerID != aws.StringValue(capacityReservation.OwnerId) {
			return false
		}
	}

	tags := getCapacityReservationTags(capacityReservation)
	for key, value := range term.Tags {
		if _, ok := tags[key]; !ok {
			return false
		}

		if value == "*" {
			continue
		}

		if tags[key] != value {
			return false
		}

		continue
	}

	return true
}

func getCapacityReservationTags(capacityReservation *ec2.CapacityReservation) map[string]string {
	tags := map[string]string{}

	for _, tag := range capacityReservation.Tags {
		tags[aws.StringValue(tag.Key)] = aws.StringValue(tag.Value)
	}

	return tags
}
