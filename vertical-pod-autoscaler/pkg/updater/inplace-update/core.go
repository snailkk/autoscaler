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
	"encoding/json"
	"errors"

	apiv1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	vpa_types "k8s.io/autoscaler/vertical-pod-autoscaler/pkg/apis/autoscaling.k8s.io/v1"
	"k8s.io/autoscaler/vertical-pod-autoscaler/pkg/utils/annotations"
	vpa_api_util "k8s.io/autoscaler/vertical-pod-autoscaler/pkg/utils/vpa"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"
)

func parseVpaObservedContainers(pod *apiv1.Pod) (bool, sets.String) {
	observedContainers, hasObservedContainers := pod.GetAnnotations()[annotations.VpaObservedContainersLabel]
	vpaContainerSet := sets.NewString()
	if hasObservedContainers {
		if containers, err := annotations.ParseVpaObservedContainersValue(observedContainers); err != nil {
			klog.Errorf("Vpa annotation %s failed to parse: %v", observedContainers, err)
			hasObservedContainers = false
		} else {
			vpaContainerSet.Insert(containers...)
		}
	}
	return hasObservedContainers, vpaContainerSet
}

// CanUpdate checks if a given container can be safely updated and returns
// error if container cannot be updated
func (e *containersUpdateRestrictionImpl) canUpdate(container *apiv1.Container,
	recommendedRequest *vpa_types.RecommendedContainerResources) (bool, error) {
	ret := false
	for resourceName, recommended := range recommendedRequest.Target {
		lowerBound, hasLowerBound := recommendedRequest.LowerBound[resourceName]
		upperBound, hasUpperBound := recommendedRequest.UpperBound[resourceName]
		if request, hasRequest := container.Resources.Requests[resourceName]; hasRequest {
			if recommended.MilliValue() > request.MilliValue() {
				ret = true
			}
			if (hasLowerBound && request.Cmp(lowerBound) < 0) ||
				(hasUpperBound && request.Cmp(upperBound) > 0) {
				ret = true
			}
		} else {
			// Note: if the request is not specified, the container will use the
			// namespace default request. Currently we ignore it and treat such
			// containers as if they had 0 request. A more correct approach would
			// be to always calculate the 'effective' request.
			ret = true
		}
	}
	return ret, nil
}
func (e *containersUpdateRestrictionImpl) getPatch(pod *apiv1.Pod, processedRecommendation *vpa_types.RecommendedPodResources) (patchContainerList, patchContainerNameIDMap, bool) {
	hasObservedContainers, vpaContainerSet := parseVpaObservedContainers(pod)
	var patchContainerList_ patchContainerList
	patchContainerNameIDMap_ := make(patchContainerNameIDMap)
	havePatch := false
	for i, podContainer := range pod.Spec.Containers {
		if hasObservedContainers && !vpaContainerSet.Has(podContainer.Name) {
			klog.V(4).Infof("Patch:Not listed in %s:%s. Skipping container %s patch resources",
				annotations.VpaObservedContainersLabel, pod.GetAnnotations()[annotations.VpaObservedContainersLabel], podContainer.Name)
			continue
		}
		recommendedRequest := vpa_api_util.GetRecommendationForContainer(podContainer.Name, processedRecommendation)
		if recommendedRequest == nil {
			continue
		}

		if ret, _ := e.canUpdate(&podContainer, recommendedRequest.DeepCopy()); !ret {
			continue
		}

		havePatch = true

		resources := apiv1.ResourceRequirements{
			Requests: apiv1.ResourceList{
				apiv1.ResourceCPU:    resource.MustParse(recommendedRequest.Target.Cpu().String()),
				apiv1.ResourceMemory: resource.MustParse(recommendedRequest.Target.Memory().String()),
			},
		}
		patchContainer := patchContainer{
			"name":      podContainer.Name,
			"resources": resources,
		}

		patchContainerList_ = append(patchContainerList_, patchContainer)

		if podContainer.Name == pod.Status.ContainerStatuses[i].Name {
			patchContainerNameIDMap_[podContainer.Name] = pod.Status.ContainerStatuses[i].ContainerID
		}
	}
	return patchContainerList_, patchContainerNameIDMap_, havePatch
}

func (e *containersUpdateRestrictionImpl) inplaceUpdate(pod *apiv1.Pod, patchContainerList_ patchContainerList, patchContainerNameIDMap_ patchContainerNameIDMap) error {
	patchObj_ := patchObj{
		"spec": map[string]interface{}{
			"containers": patchContainerList_,
		},
	}
	patchBytes, err := json.Marshal(patchObj_)
	if err != nil {
		klog.Errorf("cannot Marshal patchObj for pod %s/%s: %v", pod.Namespace, pod.Name, err)
		return err
	}
	var clone *apiv1.Pod
	err = retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		clone, err = e.podAdapter.PatchPod(pod, patchBytes)
		if err != nil {
			return err
		}
		return nil
	})

	//通过比较容器ID来得知是否已经升级成功
	for _, cloneContainer := range clone.Status.ContainerStatuses {
		oldId, ok := patchContainerNameIDMap_[cloneContainer.Name]
		if ok {
			if cloneContainer.ContainerID == oldId {
				return errors.New("inplaceUpdate: ContainerID didn't change")
			}
		}
	}
	return nil
}
func (e *containersUpdateRestrictionImpl) Update(pod *apiv1.Pod) error {
	processedRecommendation, _, err := e.recommendationProcessor.Apply(e.vpa.Status.Recommendation, e.vpa.Spec.ResourcePolicy, e.vpa.Status.Conditions, pod)
	if err != nil {
		klog.Errorf("cannot process recommendation for pod %s/%s: %v", pod.Namespace, pod.Name, err)
		return err
	}
	patchContainerList, patchContainerNameIDMap, havePatch := e.getPatch(pod, processedRecommendation)
	if havePatch {
		// Set the InPlaceUpdateReady condition type in pod.spec.readinessGates.
		// When performing an in-place update:
		// First set the InPlaceUpdateReady condition in pod.status.conditions to "False",
		// which will trigger kubelet to report the Pod as NotReady, causing traffic components (such as endpoint controller) to remove the Pod from the service endpoint;
		// Then execute Update(pod) to perform the update.
		// After the in-place update is complete, set the InPlaceUpdateReady condition to "True" to bring the Pod back to Ready state.
		// ${INSERT_HERE} = append(pod.Spec.ReadinessGates, corev1.PodReadinessGate{
		// 	Name: "InPlaceUpdateReady",
		// })

		//update condition for pod with readiness-gate
		e.injectReadinessGate(pod)
		if containsReadinessGate(pod) {
			newCondition := apiv1.PodCondition{
				Type:               inPlaceUpdateReady,
				LastTransitionTime: metav1.NewTime(Clock.Now()),
				Status:             apiv1.ConditionFalse,
				Reason:             "StartInPlaceUpdate",
			}
			if err := e.updateCondition(pod, newCondition); err != nil {
				return err
			}
		}
		err = e.inplaceUpdate(pod, patchContainerList, patchContainerNameIDMap)
		if err != nil {
			klog.Errorf("cannot patch for pod %s/%s: %v", pod.Namespace, pod.Name, err)
			return err
		}
		if containsReadinessGate(pod) {
			newCondition := apiv1.PodCondition{
				Type:               inPlaceUpdateReady,
				LastTransitionTime: metav1.NewTime(Clock.Now()),
				Status:             apiv1.ConditionTrue,
				Reason:             "FinishInPlaceUpdate",
			}
			if err := e.updateCondition(pod, newCondition); err != nil {
				return err
			}
		}
		klog.V(2).Infof("Pod %s updated successfully\n", pod.Name)
	}
	return nil
}
