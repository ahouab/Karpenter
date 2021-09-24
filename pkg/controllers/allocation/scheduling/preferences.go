package scheduling

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/awslabs/karpenter/pkg/utils/pretty"
	"github.com/patrickmn/go-cache"
	v1 "k8s.io/api/core/v1"
	"knative.dev/pkg/logging"
	"knative.dev/pkg/ptr"
)

const (
	ExpirationTTL   = 5 * time.Minute
	CleanupInterval = 1 * time.Minute
)

type Preferences struct {
	cache *cache.Cache
}

func NewPreferences() *Preferences {
	return &Preferences{
		cache: cache.New(ExpirationTTL, CleanupInterval),
	}
}

// Relax removes soft preferences from pods to enable scheduling if the cloud
// provider's capacity is constrained. For example, this can be leveraged to
// prefer a specific zone, but relax the preferences if the pod cannot be
// scheduled to that zone. Preferences are removed iteratively until only hard
// constraints remain. Pods relaxation is reset (forgotten) after 5 minutes.
func (p *Preferences) Relax(ctx context.Context, pods []*v1.Pod) {
	for _, pod := range pods {
		affinity, ok := p.cache.Get(string(pod.UID))
		// Add to cache if we've never seen it before
		if !ok {
			p.cache.Set(string(pod.UID), pod.Spec.Affinity, ExpirationTTL)
			continue
		}
		// Attempt to relax the pod and update the cache
		pod.Spec.Affinity = affinity.(*v1.Affinity)
		if relaxed := p.relax(ctx, pod); relaxed {
			p.cache.Set(string(pod.UID), pod.Spec.Affinity, ExpirationTTL)
		}
	}
}

func (p *Preferences) relax(ctx context.Context, pod *v1.Pod) bool {
	for _, relaxFunc := range []func(*v1.Pod) *string{
		func(pod *v1.Pod) *string { return p.removePreferredNodeAffinityTerm(ctx, pod) },
		func(pod *v1.Pod) *string { return p.removeRequiredNodeAffinityTerm(ctx, pod) },
	} {
		if reason := relaxFunc(pod); reason != nil {
			logging.FromContext(ctx).Debugf("Pod %s/%s previously failed to schedule, removing soft constraint: %s", pod.Namespace, pod.Name, ptr.StringValue(reason))
			return true
		}
	}
	logging.FromContext(ctx).Debugf("Pod %s/%s previously failed to schedule, but no soft constraints remain", pod.Namespace, pod.Name)
	return false
}

func (p *Preferences) removePreferredNodeAffinityTerm(ctx context.Context, pod *v1.Pod) *string {
	if pod.Spec.Affinity == nil || pod.Spec.Affinity.NodeAffinity == nil || len(pod.Spec.Affinity.NodeAffinity.PreferredDuringSchedulingIgnoredDuringExecution) == 0 {
		return nil
	}
	terms := pod.Spec.Affinity.NodeAffinity.PreferredDuringSchedulingIgnoredDuringExecution
	// Remove the terms if there are any (terms are an OR semantic)
	if len(terms) > 0 {
		// Sort ascending by weight to remove lightest preferences first
		sort.SliceStable(terms, func(i, j int) bool { return terms[i].Weight < terms[j].Weight })
		pod.Spec.Affinity.NodeAffinity.PreferredDuringSchedulingIgnoredDuringExecution = terms[1:]
		return ptr.String(fmt.Sprintf("spec.affinity.nodeAffinity.preferredDuringSchedulingIgnoredDuringExecution[0]=%s", pretty.Concise(terms[0])))
	}
	return nil
}

func (p *Preferences) removeRequiredNodeAffinityTerm(ctx context.Context, pod *v1.Pod) *string {
	if pod.Spec.Affinity == nil || pod.Spec.Affinity.NodeAffinity == nil || len(pod.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms) == 0 {
		return nil
	}
	terms := pod.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms
	// Remove the first term if there's more than one (terms are an OR semantic), Unlike preferred affinity, we cannot remove all terms
	if len(terms) > 1 {
		pod.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms = terms[1:]
		return ptr.String(fmt.Sprintf("spec.affinity.nodeAffinity.requiredDuringSchedulingIgnoredDuringExecution[0]=%s", pretty.Concise(terms[0])))
	}
	return nil
}
