/*
Copyright 2017 KangWensheng

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
package patch

import (
	"context"
	"encoding/json"

	apiv1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	patchtypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	vpa_types "k8s.io/autoscaler/vertical-pod-autoscaler/pkg/apis/autoscaling.k8s.io/v1"
	"k8s.io/autoscaler/vertical-pod-autoscaler/pkg/utils/annotations"
	vpa_api_util "k8s.io/autoscaler/vertical-pod-autoscaler/pkg/utils/vpa"
	kube_client "k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
)

// ContainersPatchRestriction controls containers patches. It ensures that we will not patch too
// many containers from one pod. For pod will allow to patch one pod or more
type ContainersPatchRestriction interface {
	// patch sends patch instruction to the api client.
	// Returns error if client returned error.
	Patch(pod *apiv1.Pod) error
	// CanEvict checks if a given container can be safely patched and returns
	// error if container cannot be patched
	CanPatch(container *apiv1.Container, recommendedRequest *vpa_types.RecommendedContainerResources) (bool, error)
}

type containersPatchRestrictionImpl struct {
	vpa                     *vpa_types.VerticalPodAutoscaler
	client                  kube_client.Interface
	recommendationProcessor vpa_api_util.RecommendationProcessor
}

// ContainersPatchRestrictionFactory creates ContainersPatchRestriction
type ContainersPatchRestrictionFactory interface {
	// NewContainersPatchRestriction creates ContainersPatchRestriction for a given pod,
	// controlled by a single VPA object.
	NewContainersPatchRestriction(vpa *vpa_types.VerticalPodAutoscaler) ContainersPatchRestriction
}

type containersPatchRestrictionFactoryImpl struct {
	client                  kube_client.Interface
	recommendationProcessor vpa_api_util.RecommendationProcessor
}

func (e *containersPatchRestrictionImpl) CanPatch(container *apiv1.Container,
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

func (e *containersPatchRestrictionImpl) Patch(pod *apiv1.Pod) error {
	processedRecommendation, _, err := e.recommendationProcessor.Apply(e.vpa.Status.Recommendation, e.vpa.Spec.ResourcePolicy, e.vpa.Status.Conditions, pod)
	if err != nil {
		klog.Errorf("cannot process recommendation for pod %s/%s: %v", pod.Namespace, pod.Name, err)
		return err
	}
	hasObservedContainers, vpaContainerSet := parseVpaObservedContainers(pod)

	// 找到需要修改的容器，并生成对应的 Patch 对象
	var patchBytes []byte
	var patchcontainers []map[string]interface{}
	// {
	// 	{
	// 		"name":      container.Name,
	// 		"resources": resources,
	// 	},
	// }
	havePatch := false
	for _, podContainer := range pod.Spec.Containers {
		if hasObservedContainers && !vpaContainerSet.Has(podContainer.Name) {
			klog.V(4).Infof("Patch:Not listed in %s:%s. Skipping container %s patch resources",
				annotations.VpaObservedContainersLabel, pod.GetAnnotations()[annotations.VpaObservedContainersLabel], podContainer.Name)
			continue
		}
		recommendedRequest := vpa_api_util.GetRecommendationForContainer(podContainer.Name, processedRecommendation)
		if recommendedRequest == nil {
			continue
		}

		if ret, _ := e.CanPatch(&podContainer, recommendedRequest.DeepCopy()); !ret {
			continue
		}

		havePatch = true

		resources := apiv1.ResourceRequirements{
			Requests: apiv1.ResourceList{
				apiv1.ResourceCPU:    resource.MustParse(recommendedRequest.Target.Cpu().String()),
				apiv1.ResourceMemory: resource.MustParse(recommendedRequest.Target.Memory().String()),
			},
		}
		obj := map[string]interface{}{
			"name":      podContainer.Name,
			"resources": resources,
		}
		patchcontainers = append(patchcontainers, obj)
	}
	patchObj := map[string]interface{}{
		"spec": map[string]interface{}{
			"containers": patchcontainers,
		},
	}

	patchBytes, err = json.Marshal(patchObj)
	if err != nil {
		klog.Errorf("cannot Marshal patchObj for pod %s/%s: %v", pod.Namespace, pod.Name, err)
		return err
	}

	// 更新 Pod 中指定容器的信息
	if havePatch {
		_, err = e.client.CoreV1().Pods(pod.Namespace).Patch(context.TODO(), pod.Name, patchtypes.StrategicMergePatchType, patchBytes, metav1.PatchOptions{})
		if err != nil {
			klog.Errorf("cannot patch for pod %s/%s: %v", pod.Namespace, pod.Name, err)
			return err
		}
		klog.V(2).Infof("Pod %s updated successfully\n", pod.Name)
	}
	return nil
}

// NewPodsEvictionRestrictionFactory creates PodsEvictionRestrictionFactory
func NewContainersPatchRestrictionFactory(client kube_client.Interface,
	recommendationProcessor vpa_api_util.RecommendationProcessor) (ContainersPatchRestrictionFactory, error) {
	return &containersPatchRestrictionFactoryImpl{
		client:                  client,
		recommendationProcessor: recommendationProcessor}, nil
}

// NewPodsEvictionRestriction creates PodsEvictionRestriction for a given set of pods,
// controlled by a single VPA object.
func (f *containersPatchRestrictionFactoryImpl) NewContainersPatchRestriction(vpa *vpa_types.VerticalPodAutoscaler) ContainersPatchRestriction {
	// We can evict pod only if it is a part of replica set
	// For each replica set we can evict only a fraction of pods.
	// Evictions may be later limited by pod disruption budget if configured.
	// processedRecommendation, _, err := calc.recommendationProcessor.Apply(calc.vpa.Status.Recommendation, calc.vpa.Spec.ResourcePolicy, calc.vpa.Status.Conditions, pod)
	// if err != nil {
	// 	klog.V(2).Infof("cannot process recommendation for pod %s/%s: %v", pod.Namespace, pod.Name, err)
	// 	return
	// }
	return &containersPatchRestrictionImpl{
		client:                  f.client,
		vpa:                     vpa,
		recommendationProcessor: f.recommendationProcessor}
}

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
