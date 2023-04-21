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
	apiv1 "k8s.io/api/core/v1"
	vpa_types "k8s.io/autoscaler/vertical-pod-autoscaler/pkg/apis/autoscaling.k8s.io/v1"
	vpa_api_util "k8s.io/autoscaler/vertical-pod-autoscaler/pkg/utils/vpa"
	kube_client "k8s.io/client-go/kubernetes"
	"k8s.io/utils/clock"
)

const (
	inPlaceUpdateReady apiv1.PodConditionType = "InPlaceUpdateReady"
)

var Clock clock.Clock = clock.RealClock{}

// type PatchBytes []byte
type patchContainer map[string]interface{}
type patchContainerList []map[string]interface{}
type patchObj map[string]interface{}
type patchContainerNameIDMap map[string]string

// ContainersUpdateRestriction controls containers updates. It ensures that we will not update too
// many containers from one pod. For pod will allow to update one pod or more
type ContainersUpdateRestriction interface {
	// Update sends update instruction to the api client.
	// Returns error if client returned error.
	Update(pod *apiv1.Pod) error
}

type containersUpdateRestrictionImpl struct {
	vpa                     *vpa_types.VerticalPodAutoscaler
	client                  kube_client.Interface
	recommendationProcessor vpa_api_util.RecommendationProcessor
	podAdapter              Adapter
}

// ContainersUpdateRestrictionFactory creates ContainersUpdateRestriction
type ContainersUpdateRestrictionFactory interface {
	// NewContainersUpdateRestriction creates ContainersUpdateRestriction for a given pod,
	// controlled by a single VPA object.
	NewContainersUpdateRestriction(vpa *vpa_types.VerticalPodAutoscaler) ContainersUpdateRestriction
}

type containersUpdateRestrictionFactoryImpl struct {
	client                  kube_client.Interface
	recommendationProcessor vpa_api_util.RecommendationProcessor
}

// NewContainersUpdateRestrictionFactory creates ContainersUpdateRestrictionFactory
func NewContainersUpdateRestrictionFactory(client kube_client.Interface,
	recommendationProcessor vpa_api_util.RecommendationProcessor) (ContainersUpdateRestrictionFactory, error) {
	return &containersUpdateRestrictionFactoryImpl{
		client:                  client,
		recommendationProcessor: recommendationProcessor}, nil
}

// NewContainersUpdateRestriction creates ContainersUpdateRestriction for a given pods,
// controlled by a single VPA object.
func (f *containersUpdateRestrictionFactoryImpl) NewContainersUpdateRestriction(vpa *vpa_types.VerticalPodAutoscaler) ContainersUpdateRestriction {
	return &containersUpdateRestrictionImpl{
		client:                  f.client,
		vpa:                     vpa,
		recommendationProcessor: f.recommendationProcessor,
		podAdapter: &AdapterTypedClient{
			Client: f.client,
		}}
}
