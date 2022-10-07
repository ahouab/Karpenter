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

package aws

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/eventbridge"
	"github.com/aws/aws-sdk-go/service/eventbridge/eventbridgeiface"
	"github.com/samber/lo"
	"go.uber.org/multierr"
	"k8s.io/client-go/util/workqueue"

	awsv1alpha1 "github.com/aws/karpenter/pkg/cloudprovider/aws/apis/v1alpha1"
	"github.com/aws/karpenter/pkg/utils/injection"
)

type EventBridgeProvider struct {
	client           eventbridgeiface.EventBridgeAPI
	queueName        string
	metadataProvider *MetadataProvider
}

type EventRule struct {
	Name    string
	Pattern *EventPattern
	Target  *EventTarget
}

type EventTarget struct {
	ID  string
	ARN string
}

type EventPattern struct {
	Source     []string `json:"source,omitempty"`
	DetailType []string `json:"detail-type,omitempty"`
}

func (ep *EventPattern) Serialize() []byte {
	return lo.Must(json.Marshal(ep))
}

func NewEventBridgeProvider(eb eventbridgeiface.EventBridgeAPI, metadataProvider *MetadataProvider, queueName string) *EventBridgeProvider {
	return &EventBridgeProvider{
		client:           eb,
		metadataProvider: metadataProvider,
		queueName:        queueName,
	}
}

func (eb *EventBridgeProvider) CreateEC2NotificationRules(ctx context.Context) error {
	rules := eb.getEC2NotificationEventRules(ctx)
	errs := make([]error, len(rules))
	workqueue.ParallelizeUntil(ctx, len(rules), len(rules), func(i int) {
		_, err := eb.client.PutRuleWithContext(ctx, &eventbridge.PutRuleInput{
			Name:         aws.String(rules[i].Name),
			EventPattern: aws.String(string(rules[i].Pattern.Serialize())),
			Tags: []*eventbridge.Tag{
				{
					Key:   aws.String(awsv1alpha1.DiscoveryTagKey),
					Value: aws.String(injection.GetOptions(ctx).ClusterName),
				},
			},
		})
		if err != nil {
			errs[i] = multierr.Append(errs[i], err)
		}
		_, err = eb.client.PutTargetsWithContext(ctx, &eventbridge.PutTargetsInput{
			Rule: aws.String(rules[i].Name),
			Targets: []*eventbridge.Target{
				{
					Id:  aws.String(rules[i].Target.ID),
					Arn: aws.String(rules[i].Target.ARN),
				},
			},
		})
		if err != nil {
			errs[i] = multierr.Append(errs[i], err)
		}
	})
	return multierr.Combine(errs...)
}

func (eb *EventBridgeProvider) DeleteEC2NotificationRules(ctx context.Context) (err error) {
	wg := &sync.WaitGroup{}
	m := &sync.Mutex{}
	for _, rule := range eb.getEC2NotificationEventRules(ctx) {
		wg.Add(1)
		go func(r EventRule) {
			defer wg.Done()
			targetInput := &eventbridge.RemoveTargetsInput{
				Ids:  []*string{aws.String(r.Target.ID)},
				Rule: aws.String(r.Name),
			}
			_, e := eb.client.RemoveTargetsWithContext(ctx, targetInput)
			if e != nil && !IsNotFound(e) {
				m.Lock()
				err = multierr.Append(err, e)
				m.Unlock()
				return
			}
			ruleInput := &eventbridge.DeleteRuleInput{
				Name: aws.String(r.Name),
			}
			_, e = eb.client.DeleteRuleWithContext(ctx, ruleInput)
			if e != nil && !IsNotFound(e) {
				m.Lock()
				err = multierr.Append(err, e)
				m.Unlock()
				return
			}
		}(rule)
	}
	wg.Wait()
	return err
}

func (eb *EventBridgeProvider) getEC2NotificationEventRules(ctx context.Context) []EventRule {
	return []EventRule{
		{
			Name: fmt.Sprintf("Karpenter-%s-ScheduledChangeRule", injection.GetOptions(ctx).ClusterName),
			Pattern: &EventPattern{
				Source:     []string{"aws.health"},
				DetailType: []string{"AWS Health Event"},
			},
			Target: &EventTarget{
				ID:  "1",
				ARN: eb.getQueueARN(ctx),
			},
		},
		{
			Name: fmt.Sprintf("Karpenter-%s-SpotTerminationRule", injection.GetOptions(ctx).ClusterName),
			Pattern: &EventPattern{
				Source:     []string{"aws.ec2"},
				DetailType: []string{"EC2 Spot Instance Interruption Warning"},
			},
			Target: &EventTarget{
				ID:  "1",
				ARN: eb.getQueueARN(ctx),
			},
		},
		{
			Name: fmt.Sprintf("Karpenter-%s-RebalanceRule", injection.GetOptions(ctx).ClusterName),
			Pattern: &EventPattern{
				Source:     []string{"aws.ec2"},
				DetailType: []string{"EC2 Instance Rebalance Recommendation"},
			},
			Target: &EventTarget{
				ID:  "1",
				ARN: eb.getQueueARN(ctx),
			},
		},
		{
			Name: fmt.Sprintf("Karpenter-%s-InstanceStateChangeRule", injection.GetOptions(ctx).ClusterName),
			Pattern: &EventPattern{
				Source:     []string{"aws.ec2"},
				DetailType: []string{"EC2 Instance State-change Notification"},
			},
			Target: &EventTarget{
				ID:  "1",
				ARN: eb.getQueueARN(ctx),
			},
		},
	}
}

func (eb *EventBridgeProvider) getQueueARN(ctx context.Context) string {
	return fmt.Sprintf("arn:aws:sqs:%s:%s:%s", eb.metadataProvider.Region(ctx), eb.metadataProvider.AccountID(ctx), eb.queueName)
}
