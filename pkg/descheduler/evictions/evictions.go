/*
Copyright 2017 The Kubernetes Authors.

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

package evictions

import (
	"context"
	"fmt"
	"strings"

	v1 "k8s.io/api/core/v1"
	policy "k8s.io/api/policy/v1beta1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/errors"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	clientcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog"
	podutil "sigs.k8s.io/descheduler/pkg/descheduler/pod"
	"sigs.k8s.io/descheduler/pkg/utils"

	eutils "sigs.k8s.io/descheduler/pkg/descheduler/evictions/utils"
)

const (
	evictPodAnnotationKey = "descheduler.alpha.kubernetes.io/evict"
)

// nodePodEvictedCount keeps count of pods evicted on node
type nodePodEvictedCount map[*v1.Node]int

type PodEvictor struct {
	client                clientset.Interface
	policyGroupVersion    string
	dryRun                bool
	maxPodsToEvict        int
	nodepodCount          nodePodEvictedCount
	evictLocalStoragePods bool
}

func NewPodEvictor(
	client clientset.Interface,
	policyGroupVersion string,
	dryRun bool,
	maxPodsToEvict int,
	nodes []*v1.Node,
	evictLocalStoragePods bool,
) *PodEvictor {
	var nodePodCount = make(nodePodEvictedCount)
	for _, node := range nodes {
		// Initialize podsEvicted till now with 0.
		nodePodCount[node] = 0
	}

	return &PodEvictor{
		client:                client,
		policyGroupVersion:    policyGroupVersion,
		dryRun:                dryRun,
		maxPodsToEvict:        maxPodsToEvict,
		nodepodCount:          nodePodCount,
		evictLocalStoragePods: evictLocalStoragePods,
	}
}

// IsEvictable checks if a pod is evictable or not.
func (pe *PodEvictor) IsEvictable(pod *v1.Pod) bool {
	checkErrs := []error{}
	if IsCriticalPod(pod) {
		checkErrs = append(checkErrs, fmt.Errorf("pod is critical"))
	}

	ownerRefList := podutil.OwnerRef(pod)
	if IsDaemonsetPod(ownerRefList) {
		checkErrs = append(checkErrs, fmt.Errorf("pod is a DaemonSet pod"))
	}

	if len(ownerRefList) == 0 {
		checkErrs = append(checkErrs, fmt.Errorf("pod does not have any ownerrefs"))
	}

	if !pe.evictLocalStoragePods && IsPodWithLocalStorage(pod) {
		checkErrs = append(checkErrs, fmt.Errorf("pod has local storage and descheduler is not configured with --evict-local-storage-pods"))
	}

	if IsMirrorPod(pod) {
		checkErrs = append(checkErrs, fmt.Errorf("pod is a mirror pod"))
	}

	if len(checkErrs) > 0 && !HaveEvictAnnotation(pod) {
		klog.V(4).Infof("Pod %s in namespace %s is not evictable: Pod lacks an eviction annotation and fails the following checks: %v", pod.Name, pod.Namespace, errors.NewAggregate(checkErrs).Error())
		return false
	}
	return true
}

// NodeEvicted gives a number of pods evicted for node
func (pe *PodEvictor) NodeEvicted(node *v1.Node) int {
	return pe.nodepodCount[node]
}

// TotalEvicted gives a number of pods evicted through all nodes
func (pe *PodEvictor) TotalEvicted() int {
	var total int
	for _, count := range pe.nodepodCount {
		total += count
	}
	return total
}

// EvictPod returns non-nil error only when evicting a pod on a node is not
// possible (due to maxPodsToEvict constraint). Success is true when the pod
// is evicted on the server side.
func (pe *PodEvictor) EvictPod(ctx context.Context, pod *v1.Pod, node *v1.Node, reasons ...string) (bool, error) {
	var reason string
	if len(reasons) > 0 {
		reason = " (" + strings.Join(reasons, ", ") + ")"
	}
	if pe.maxPodsToEvict > 0 && pe.nodepodCount[node]+1 > pe.maxPodsToEvict {
		return false, fmt.Errorf("Maximum number %v of evicted pods per %q node reached", pe.maxPodsToEvict, node.Name)
	}

	err := evictPod(ctx, pe.client, pod, pe.policyGroupVersion, pe.dryRun)
	if err != nil {
		// err is used only for logging purposes
		klog.Errorf("Error evicting pod: %#v in namespace %#v%s: %#v", pod.Name, pod.Namespace, reason, err)
		return false, nil
	}

	pe.nodepodCount[node]++
	if pe.dryRun {
		klog.V(1).Infof("Evicted pod in dry run mode: %#v in namespace %#v%s", pod.Name, pod.Namespace, reason)
	} else {
		klog.V(1).Infof("Evicted pod: %#v in namespace %#v%s", pod.Name, pod.Namespace, reason)
		eventBroadcaster := record.NewBroadcaster()
		eventBroadcaster.StartLogging(klog.V(3).Infof)
		eventBroadcaster.StartRecordingToSink(&clientcorev1.EventSinkImpl{Interface: pe.client.CoreV1().Events(pod.Namespace)})
		r := eventBroadcaster.NewRecorder(scheme.Scheme, v1.EventSource{Component: "sigs.k8s.io.descheduler"})
		r.Event(pod, v1.EventTypeNormal, "Descheduled", fmt.Sprintf("pod evicted by sigs.k8s.io/descheduler%s", reason))
	}
	return true, nil
}

func evictPod(ctx context.Context, client clientset.Interface, pod *v1.Pod, policyGroupVersion string, dryRun bool) error {
	if dryRun {
		return nil
	}
	deleteOptions := &metav1.DeleteOptions{}
	// GracePeriodSeconds ?
	eviction := &policy.Eviction{
		TypeMeta: metav1.TypeMeta{
			APIVersion: policyGroupVersion,
			Kind:       eutils.EvictionKind,
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      pod.Name,
			Namespace: pod.Namespace,
		},
		DeleteOptions: deleteOptions,
	}
	err := client.PolicyV1beta1().Evictions(eviction.Namespace).Evict(ctx, eviction)

	if apierrors.IsTooManyRequests(err) {
		return fmt.Errorf("error when evicting pod (ignoring) %q: %v", pod.Name, err)
	}
	if apierrors.IsNotFound(err) {
		return fmt.Errorf("pod not found when evicting %q: %v", pod.Name, err)
	}
	return err
}

func IsCriticalPod(pod *v1.Pod) bool {
	return utils.IsCriticalPod(pod)
}

func IsDaemonsetPod(ownerRefList []metav1.OwnerReference) bool {
	for _, ownerRef := range ownerRefList {
		if ownerRef.Kind == "DaemonSet" {
			return true
		}
	}
	return false
}

// IsMirrorPod checks whether the pod is a mirror pod.
func IsMirrorPod(pod *v1.Pod) bool {
	return utils.IsMirrorPod(pod)
}

// HaveEvictAnnotation checks if the pod have evict annotation
func HaveEvictAnnotation(pod *v1.Pod) bool {
	_, found := pod.ObjectMeta.Annotations[evictPodAnnotationKey]
	return found
}

func IsPodWithLocalStorage(pod *v1.Pod) bool {
	for _, volume := range pod.Spec.Volumes {
		if volume.HostPath != nil || volume.EmptyDir != nil {
			return true
		}
	}

	return false
}
