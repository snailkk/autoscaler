/*
Copyright 2023 KangWensheng
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
package inplaceupdate

import (
	"fmt"
	"strings"

	apiv1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
)

func (c *containersUpdateRestrictionImpl) updateCondition(pod *apiv1.Pod, condition apiv1.PodCondition) error {
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		clone, err := c.podAdapter.GetPod(pod.Namespace, pod.Name)
		if err != nil {
			return err
		}

		if hasEqualCondition(clone, &condition) {
			return nil
		}

		setPodCondition(clone, condition)
		// We only update the ready condition to False, and let Kubelet update it to True
		if condition.Status == apiv1.ConditionFalse {
			setPodReadyCondition(clone)
		}
		return c.podAdapter.UpdatePodStatus(clone)
	})
}

// InjectReadinessGate injects InPlaceUpdateReady into pod.spec.readinessGates
func (c *containersUpdateRestrictionImpl) injectReadinessGate(pod *apiv1.Pod) {
	for _, r := range pod.Spec.ReadinessGates {
		if r.ConditionType == inPlaceUpdateReady {
			return
		}
	}
	pod.Spec.ReadinessGates = append(pod.Spec.ReadinessGates, apiv1.PodReadinessGate{ConditionType: inPlaceUpdateReady})
	c.podAdapter.UpdatePod(pod)
}

func containsReadinessGate(pod *apiv1.Pod) bool {
	for _, r := range pod.Spec.ReadinessGates {
		if r.ConditionType == inPlaceUpdateReady {
			return true
		}
	}
	return false
}

// GetCondition returns the  condition in Pod.
func getCondition(pod *apiv1.Pod, cType apiv1.PodConditionType) *apiv1.PodCondition {
	for _, c := range pod.Status.Conditions {
		if c.Type == cType {
			return &c
		}
	}
	return nil
}

func hasEqualCondition(pod *apiv1.Pod, newCondition *apiv1.PodCondition) bool {
	oldCondition := getCondition(pod, newCondition.Type)
	isEqual := oldCondition != nil && oldCondition.Status == newCondition.Status &&
		oldCondition.Reason == newCondition.Reason && oldCondition.Message == newCondition.Message
	return isEqual
}

func setPodCondition(pod *apiv1.Pod, condition apiv1.PodCondition) {
	for i, c := range pod.Status.Conditions {
		if c.Type == condition.Type {
			if c.Status != condition.Status {
				pod.Status.Conditions[i] = condition
			}
			return
		}
	}
	pod.Status.Conditions = append(pod.Status.Conditions, condition)
}
func setPodReadyCondition(pod *apiv1.Pod) {
	podReady := getCondition(pod, apiv1.PodReady)
	if podReady == nil {
		return
	}

	containersReady := getCondition(pod, apiv1.ContainersReady)
	if containersReady == nil || containersReady.Status != apiv1.ConditionTrue {
		return
	}

	var unreadyMessages []string
	for _, rg := range pod.Spec.ReadinessGates {
		c := getCondition(pod, rg.ConditionType)
		if c == nil {
			unreadyMessages = append(unreadyMessages, fmt.Sprintf("corresponding condition of pod readiness gate %q does not exist.", string(rg.ConditionType)))
		} else if c.Status != apiv1.ConditionTrue {
			unreadyMessages = append(unreadyMessages, fmt.Sprintf("the status of pod readiness gate %q is not \"True\", but %v", string(rg.ConditionType), c.Status))
		}
	}

	newPodReady := apiv1.PodCondition{
		Type:               apiv1.PodReady,
		Status:             apiv1.ConditionTrue,
		LastTransitionTime: metav1.Now(),
	}
	// set "Ready" condition to "False" if any readiness gate is not ready.
	if len(unreadyMessages) != 0 {
		unreadyMessage := strings.Join(unreadyMessages, ", ")
		newPodReady = apiv1.PodCondition{
			Type:    apiv1.PodReady,
			Status:  apiv1.ConditionFalse,
			Reason:  "ReadinessGatesNotReady",
			Message: unreadyMessage,
		}
	}

	setPodCondition(pod, newPodReady)
}
