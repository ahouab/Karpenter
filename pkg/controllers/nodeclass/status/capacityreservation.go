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

package status

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/samber/lo"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/aws/karpenter-provider-aws/pkg/apis/v1beta1"
	"github.com/aws/karpenter-provider-aws/pkg/providers/capacityreservation"
)

type CapacityReservation struct {
	capacityReservationProvider capacityreservation.Provider
}

func (sg *CapacityReservation) Reconcile(ctx context.Context, nodeClass *v1beta1.EC2NodeClass) (reconcile.Result, error) {
	capacityReservations, err := sg.capacityReservationProvider.List(ctx, nodeClass)
	if err != nil {
		return reconcile.Result{}, err
	}
	if len(capacityReservations) == 0 && len(nodeClass.Spec.CapacityReservationSelectorTerms) > 0 {
		nodeClass.Status.CapacityReservations = nil
		return reconcile.Result{}, fmt.Errorf("no capacity reservations exist given constraints")
	}
	sort.Slice(capacityReservations, func(i, j int) bool {
		return *capacityReservations[i].CapacityReservationId < *capacityReservations[j].CapacityReservationId
	})
	nodeClass.Status.CapacityReservations = lo.Map(capacityReservations, func(capacityReservation *ec2.CapacityReservation, _ int) v1beta1.CapacityReservation {
		var endDate *string
		if capacityReservation.EndDate != nil {
			endDate = aws.String(capacityReservation.EndDate.Format(time.RFC3339))
		}
		return v1beta1.CapacityReservation{
			ID:                     *capacityReservation.CapacityReservationId,
			AvailabilityZone:       *capacityReservation.AvailabilityZone,
			AvailableInstanceCount: int(*capacityReservation.AvailableInstanceCount),
			EndDate:                endDate,
			EndDateType:            *capacityReservation.EndDateType,
			InstanceMatchCriteria:  *capacityReservation.InstanceMatchCriteria,
			InstancePlatform:       *capacityReservation.InstancePlatform,
			InstanceType:           *capacityReservation.InstanceType,
			OwnerID:                *capacityReservation.OwnerId,
			StartDate:              capacityReservation.StartDate.Format(time.RFC3339),
			TotalInstanceCount:     int(*capacityReservation.TotalInstanceCount),
		}
	})
	return reconcile.Result{RequeueAfter: 5 * time.Minute}, nil
}
