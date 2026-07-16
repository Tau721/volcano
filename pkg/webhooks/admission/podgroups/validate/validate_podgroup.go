/*
Copyright 2021 The Volcano Authors.

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

package validate

import (
	"fmt"
	"strings"

	admissionv1 "k8s.io/api/admission/v1"
	whv1 "k8s.io/api/admissionregistration/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"

	schedulingv1beta1 "volcano.sh/apis/pkg/apis/scheduling/v1beta1"
	"volcano.sh/volcano/pkg/webhooks/router"
	"volcano.sh/volcano/pkg/webhooks/schema"
	"volcano.sh/volcano/pkg/webhooks/util"
)

func init() {
	router.RegisterAdmission(service)
}

var service = &router.AdmissionService{
	Path:   "/podgroups/validate",
	Func:   Validate,
	Config: config,

	ValidatingConfig: &whv1.ValidatingWebhookConfiguration{
		Webhooks: []whv1.ValidatingWebhook{{
			Name: "validatepodgroup.volcano.sh",
			Rules: []whv1.RuleWithOperations{
				{
					Operations: []whv1.OperationType{whv1.Create},
					Rule: whv1.Rule{
						APIGroups:   []string{schedulingv1beta1.SchemeGroupVersion.Group},
						APIVersions: []string{schedulingv1beta1.SchemeGroupVersion.Version},
						Resources:   []string{"podgroups"},
					},
				},
			},
		}},
	},
}

var config = &router.AdmissionServiceConfig{}

// Validate validates the PodGroup object when creating or updating it
func Validate(ar admissionv1.AdmissionReview) *admissionv1.AdmissionResponse {
	klog.V(3).Infof("Validating %s PodGroup %s", ar.Request.Operation, ar.Request.Name)

	podgroup, err := schema.DecodePodGroup(ar.Request.Object, ar.Request.Resource)
	if err != nil {
		return util.ToAdmissionResponse(err)
	}

	var errMsg string
	switch ar.Request.Operation {
	case admissionv1.Create:
		errMsg = validatePodGroup(podgroup)
	default:
		errMsg = fmt.Sprintf("unsupported operation %s", ar.Request.Operation)
	}

	if errMsg != "" {
		return &admissionv1.AdmissionResponse{
			Allowed: false,
			Result:  &metav1.Status{Message: errMsg},
		}
	}

	return &admissionv1.AdmissionResponse{
		Allowed: true,
	}
}

// validatePodGroup validates a PodGroup when it's being created
func validatePodGroup(pg *schedulingv1beta1.PodGroup) string {
	var errMsg string

	errMsg += checkQueueState(pg.Spec.Queue)
	errMsg += validateNetworkTopology(pg.Spec.NetworkTopology, pg.Spec.SubGroupPolicy)
	errMsg += validateTopologyAffinity(pg.Spec.TopologyAffinity, pg.Spec.SubGroupPolicy)

	return errMsg
}

// checkQueueState verifies if the queue exists and is in the open state
func checkQueueState(queueName string) string {
	if queueName == "" {
		return ""
	}

	queue, err := config.QueueLister.Get(queueName)
	if err != nil {
		return fmt.Sprintf("unable to find queue: %s", err.Error())
	}

	if queue.Status.State != schedulingv1beta1.QueueStateOpen {
		return fmt.Sprintf("can only submit PodGroup to queue with state `Open`, queue `%s` status is `%s`. ",
			queue.Name, queue.Status.State)
	}

	return ""
}

func validateNetworkTopology(networkTopology *schedulingv1beta1.NetworkTopologySpec, policies []schedulingv1beta1.SubGroupPolicySpec) string {
	var errs []string
	if networkTopology != nil && networkTopology.HighestTierAllowed != nil && networkTopology.HighestTierName != "" {
		errs = append(errs, "must not specify 'highestTierAllowed' and 'highestTierName' in networkTopology simultaneously.")
	}
	for _, policy := range policies {
		if policy.NetworkTopology != nil && policy.NetworkTopology.HighestTierAllowed != nil && policy.NetworkTopology.HighestTierName != "" {
			errs = append(errs, fmt.Sprintf("in subGroupPolicy '%s': must not specify 'highestTierAllowed' and 'highestTierName' in networkTopology simultaneously.", policy.Name))
			break
		}
	}
	return strings.Join(errs, " ")
}

func validateTopologyAffinity(topologyAffinity *schedulingv1beta1.TopologyAffinitySpec, policies []schedulingv1beta1.SubGroupPolicySpec) string {
	if topologyAffinity == nil {
		return ""
	}

	var errs []string

	if anti := topologyAffinity.PodGroupAntiAffinity; anti != nil {
		for index, term := range anti.Required {
			errs = append(errs, validatePodGroupAffinityTerm(
				fmt.Sprintf("topologyAffinity.podGroupAntiAffinity.required[%d]", index), term, true)...)
		}
		for index, term := range anti.Preferred {
			errs = append(errs, validatePodGroupAffinityTerm(
				fmt.Sprintf("topologyAffinity.podGroupAntiAffinity.preferred[%d]", index), term, false)...)
		}
	}
	policyMap := subGroupPolicyMap(policies)
	if affinity := topologyAffinity.SubGroupAffinity; affinity != nil {
		for index, term := range affinity.Required {
			errs = append(errs, validateSubGroupAffinityTerm(
				fmt.Sprintf("topologyAffinity.subGroupAffinity.required[%d]", index), term, true, true, policyMap)...)
		}
		for index, term := range affinity.Preferred {
			errs = append(errs, validateSubGroupAffinityTerm(
				fmt.Sprintf("topologyAffinity.subGroupAffinity.preferred[%d]", index), term, false, true, policyMap)...)
		}
	}
	if anti := topologyAffinity.SubGroupAntiAffinity; anti != nil {
		for index, term := range anti.Required {
			errs = append(errs, validateSubGroupAffinityTerm(
				fmt.Sprintf("topologyAffinity.subGroupAntiAffinity.required[%d]", index), term, true, false, policyMap)...)
		}
		for index, term := range anti.Preferred {
			errs = append(errs, validateSubGroupAffinityTerm(
				fmt.Sprintf("topologyAffinity.subGroupAntiAffinity.preferred[%d]", index), term, false, false, policyMap)...)
		}
	}
	errs = append(errs, validateHardSubGroupAffinityConflicts(topologyAffinity)...)

	return strings.Join(errs, " ")
}

func validatePodGroupAffinityTerm(path string, term schedulingv1beta1.PodGroupAffinityTerm, required bool) []string {
	var errs []string
	if required && term.Weight != 0 {
		errs = append(errs, fmt.Sprintf("%s: weight must not be set on required terms.", path))
	}
	if !required && (term.Weight < 1 || term.Weight > 100) {
		errs = append(errs, fmt.Sprintf("%s: weight must be an integer in the range 1-100 for preferred terms.", path))
	}
	if term.PodGroupSelector == nil {
		errs = append(errs, fmt.Sprintf("%s: podGroupSelector is required.", path))
	}
	if term.TopologyTier != nil && term.TopologyTierName != "" {
		errs = append(errs, fmt.Sprintf("%s: must not specify topologyTier and topologyTierName simultaneously.", path))
	}
	if term.TopologyTier == nil && term.TopologyTierName == "" {
		errs = append(errs, fmt.Sprintf("%s: must specify topologyTier or topologyTierName.", path))
	}
	return errs
}

func validateSubGroupAffinityTerm(path string, term schedulingv1beta1.SubGroupAffinityTerm, required, affinity bool, policies map[string]schedulingv1beta1.SubGroupPolicySpec) []string {
	var errs []string
	if required && term.Weight != 0 {
		errs = append(errs, fmt.Sprintf("%s: weight must not be set on required terms.", path))
	}
	if !required && (term.Weight < 1 || term.Weight > 100) {
		errs = append(errs, fmt.Sprintf("%s: weight must be an integer in the range 1-100 for preferred terms.", path))
	}
	if len(term.SubGroups) == 0 {
		errs = append(errs, fmt.Sprintf("%s: subGroups is required.", path))
	}
	if affinity && len(term.SubGroups) < 2 {
		errs = append(errs, fmt.Sprintf("%s: subGroupAffinity requires at least two subGroups.", path))
	}
	seen := map[string]struct{}{}
	for _, subGroup := range term.SubGroups {
		if _, ok := seen[subGroup]; ok {
			errs = append(errs, fmt.Sprintf("%s: subGroups must not contain duplicate name %q.", path, subGroup))
			continue
		}
		seen[subGroup] = struct{}{}
		if _, ok := policies[subGroup]; !ok {
			errs = append(errs, fmt.Sprintf("%s: subGroupPolicy %q is not defined.", path, subGroup))
		}
	}
	if !affinity && len(term.SubGroups) == 1 {
		if policy, ok := policies[term.SubGroups[0]]; ok && len(policy.MatchLabelKeys) == 0 && (policy.MinSubGroups == nil || *policy.MinSubGroups < 2) {
			errs = append(errs, fmt.Sprintf("%s: single-policy subGroupAntiAffinity requires MatchLabelKeys or minSubGroups >= 2 on subGroupPolicy %q.", path, policy.Name))
		}
	}
	if term.TopologyTier != nil && term.TopologyTierName != "" {
		errs = append(errs, fmt.Sprintf("%s: must not specify topologyTier and topologyTierName simultaneously.", path))
	}
	if term.TopologyTier == nil && term.TopologyTierName == "" {
		errs = append(errs, fmt.Sprintf("%s: must specify topologyTier or topologyTierName.", path))
	}
	return errs
}

func subGroupPolicyMap(policies []schedulingv1beta1.SubGroupPolicySpec) map[string]schedulingv1beta1.SubGroupPolicySpec {
	result := make(map[string]schedulingv1beta1.SubGroupPolicySpec, len(policies))
	for _, policy := range policies {
		result[policy.Name] = policy
	}
	return result
}

func validateHardSubGroupAffinityConflicts(topologyAffinity *schedulingv1beta1.TopologyAffinitySpec) []string {
	if topologyAffinity.SubGroupAffinity == nil || topologyAffinity.SubGroupAntiAffinity == nil {
		return nil
	}
	var errs []string
	for affinityIndex, affinityTerm := range topologyAffinity.SubGroupAffinity.Required {
		for antiIndex, antiTerm := range topologyAffinity.SubGroupAntiAffinity.Required {
			if !subGroupsOverlap(affinityTerm.SubGroups, antiTerm.SubGroups) {
				continue
			}
			if affinityTerm.TopologyTier != nil && antiTerm.TopologyTier != nil && *affinityTerm.TopologyTier < *antiTerm.TopologyTier {
				errs = append(errs, fmt.Sprintf("topologyAffinity: subGroupAffinity.required[%d] tier must be greater than or equal to subGroupAntiAffinity.required[%d] tier for overlapping subGroups.", affinityIndex, antiIndex))
			}
			if affinityTerm.TopologyTierName != "" && affinityTerm.TopologyTierName == antiTerm.TopologyTierName {
				continue
			}
		}
	}
	return errs
}

func subGroupsOverlap(left, right []string) bool {
	seen := map[string]struct{}{}
	for _, value := range left {
		seen[value] = struct{}{}
	}
	for _, value := range right {
		if _, ok := seen[value]; ok {
			return true
		}
	}
	return false
}
