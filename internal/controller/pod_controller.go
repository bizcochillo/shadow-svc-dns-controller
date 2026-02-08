/*
Copyright 2024.

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

package controller

import (
	"context"
	"encoding/json"
	"reflect"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups="",resources=services/finalizers,verbs=update
// +kubebuilder:rbac:groups=discovery.k8s.io,resources=endpointslices,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=discovery.k8s.io,resources=endpointslices/finalizers,verbs=update

const (
	// The annotation on the Shadow Service pointing to the Original Service
	SyncServiceAnnotation = "bizcochillo.io/shadow-of"
	// The field index name for fast lookups
	refFieldIndex = "metadata.annotations.shadow-of"
)

// NetworkStatus matches the Multus/UDN annotation structure
type NetworkStatus struct {
	Name      string   `json:"name"`
	Interface string   `json:"interface"`
	Ips       []string `json:"ips"`
	Default   bool     `json:"default"`
}

type PodReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// SetupWithManager sets up the controller with the Manager.
func (r *PodReconciler) SetupWithManager(mgr ctrl.Manager) error {
	ctx := context.Background()

	// 1. Index the Shadow Services by the annotation value (Target Service Name)
	// This allows us to quickly answer: "Which Shadow Service cares about Target Service X?"
	if err := mgr.GetFieldIndexer().IndexField(ctx, &corev1.Service{}, refFieldIndex, func(rawObj client.Object) []string {
		svc := rawObj.(*corev1.Service)
		if targetName, ok := svc.Annotations[SyncServiceAnnotation]; ok {
			return []string{targetName}
		}
		return nil
	}); err != nil {
		return err
	}

	return ctrl.NewControllerManagedBy(mgr).
		// A. We care about the Shadow Service (Primary Resource)
		For(&corev1.Service{}).
		// B. We own the EndpointSlices we create
		Owns(&discoveryv1.EndpointSlice{}).
		// C. Watch Pods -> Trigger Shadow Service Reconcile via Map Function
		Watches(
			&corev1.Pod{},
			handler.EnqueueRequestsFromMapFunc(r.mapPodToShadow),
		).
		// D. Watch Original Services -> Trigger Shadow Service Reconcile via Index Lookup
		Watches(
			&corev1.Service{},
			handler.EnqueueRequestsFromMapFunc(r.mapOriginalServiceToShadow),
		).
		Complete(r)
}

// Reconcile is the main logic loop
func (r *PodReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// 1. Fetch the Shadow Service (The one with the annotation)
	shadowSvc := &corev1.Service{}
	if err := r.Get(ctx, req.NamespacedName, shadowSvc); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// 2. Filter: Is this actually a Shadow Service?
	targetSvcName := shadowSvc.Annotations[SyncServiceAnnotation]
	if targetSvcName == "" {
		return ctrl.Result{}, nil
	}

	// Initialize variables for the sync step
	targetSvc := &corev1.Service{}
	podList := &corev1.PodList{}

	// 3. Fetch the Original (Target) Service
	// We treat "Not Found" as a valid state (meaning: 0 pods, 0 ports)
	err := r.Get(ctx, types.NamespacedName{Name: targetSvcName, Namespace: shadowSvc.Namespace}, targetSvc)

	if err != nil {
		if errors.IsNotFound(err) {
			// [SAFETY FIX]: Target is gone. Do NOT error out.
			// We log a simple info message and proceed with empty podList.
			logger.Info("Target service deleted, cleaning up Shadow Service endpoints", "target", targetSvcName)

			// We leave targetSvc as an empty object.
			// We leave podList as an empty list.
		} else {
			// Real API error (e.g. timeout), let's retry
			return ctrl.Result{}, err
		}
	} else {
		// 4. Target Found: Find all Pods matching its selector
		selector := labels.SelectorFromSet(targetSvc.Spec.Selector)

		// Only list pods if the selector is valid/non-empty
		if !selector.Empty() {
			if err := r.List(ctx, podList, client.InNamespace(shadowSvc.Namespace), client.MatchingLabelsSelector{Selector: selector}); err != nil {
				return ctrl.Result{}, err
			}
		}
	}

	// 5. Sync the EndpointSlice for the Shadow Service
	// If target was missing, podList is empty -> Reconcile will remove all UDN IPs.
	return ctrl.Result{}, r.reconcileEndpointSlice(ctx, shadowSvc, targetSvc, podList)
}

// reconcileEndpointSlice ensures the EndpointSlice contains the correct UDN IPs
func (r *PodReconciler) reconcileEndpointSlice(ctx context.Context, shadowSvc, targetSvc *corev1.Service, pods *corev1.PodList) error {
	logger := log.FromContext(ctx)

	// Expected Slice Name (one slice per shadow service for simplicity)
	sliceName := shadowSvc.Name + "-udn"

	// 1. Prepare the desired list of Endpoints
	var desiredEndpoints []discoveryv1.Endpoint
	for _, pod := range pods.Items {
		// Only consider running pods that are not terminating
		if pod.Status.Phase != corev1.PodRunning || !pod.DeletionTimestamp.IsZero() {
			continue
		}

		udnIP := r.getPrimaryUDNIP(&pod)
		if udnIP == "" {
			continue // Skip if no UDN IP found yet
		}

		ready := true // Simplification: Assume ready if running. Ideally check readiness gates.
		ep := discoveryv1.Endpoint{
			Addresses: []string{udnIP},
			Hostname:  &pod.Name, // Critical for StatefulSet DNS (e.g., pod-0.service...)
			TargetRef: &corev1.ObjectReference{
				Kind:      "Pod",
				Name:      pod.Name,
				Namespace: pod.Namespace,
				UID:       pod.UID,
			},
			Conditions: discoveryv1.EndpointConditions{
				Ready: &ready,
			},
		}
		desiredEndpoints = append(desiredEndpoints, ep)
	}

	// 2. Inherit Ports from the Original Target Service
	var desiredPorts []discoveryv1.EndpointPort
	for _, svcPort := range targetSvc.Spec.Ports {
		p := svcPort // copy to avoid loop variable pointer issues
		desiredPorts = append(desiredPorts, discoveryv1.EndpointPort{
			Name:     &p.Name,
			Protocol: &p.Protocol,
			Port:     &p.Port,
		})
	}

	// 3. Fetch existing Slice
	existingSlice := &discoveryv1.EndpointSlice{}
	err := r.Get(ctx, types.NamespacedName{Name: sliceName, Namespace: shadowSvc.Namespace}, existingSlice)

	if err != nil && errors.IsNotFound(err) {
		// CREATE NEW
		logger.Info("Creating new EndpointSlice", "name", sliceName)
		newSlice := &discoveryv1.EndpointSlice{
			ObjectMeta: metav1.ObjectMeta{
				Name:      sliceName,
				Namespace: shadowSvc.Namespace,
				Labels: map[string]string{
					"kubernetes.io/service-name":             shadowSvc.Name, // Links to Shadow Service DNS
					"endpointslice.kubernetes.io/managed-by": "bizcochillo.io",
				},
			},
			AddressType: discoveryv1.AddressTypeIPv4,
			Endpoints:   desiredEndpoints,
			Ports:       desiredPorts,
		}
		// Set Shadow Service as Owner so if Service is deleted, Slice is deleted
		if err := controllerutil.SetControllerReference(shadowSvc, newSlice, r.Scheme); err != nil {
			return err
		}
		return r.Create(ctx, newSlice)
	} else if err != nil {
		return err
	}

	// 4. Update if necessary
	// We compare Endpoints and Ports. If different, we update.
	if !reflect.DeepEqual(existingSlice.Endpoints, desiredEndpoints) || !reflect.DeepEqual(existingSlice.Ports, desiredPorts) {
		logger.Info("Updating EndpointSlice", "name", sliceName)
		existingSlice.Endpoints = desiredEndpoints
		existingSlice.Ports = desiredPorts
		return r.Update(ctx, existingSlice)
	}

	return nil
}

// getPrimaryUDNIP extracts the IP from the Multus/CNI annotation
func (r *PodReconciler) getPrimaryUDNIP(pod *corev1.Pod) string {
	anno, ok := pod.Annotations["k8s.v1.cni.cncf.io/network-status"]
	if !ok {
		return ""
	}

	var statuses []NetworkStatus
	if err := json.Unmarshal([]byte(anno), &statuses); err != nil {
		return ""
	}

	// Strategy: Find the interface that is marked 'default: true'
	// In UDN Primary Network mode, the UDN interface is usually the default.
	for _, s := range statuses {
		if s.Default && len(s.Ips) > 0 {
			return s.Ips[0]
		}
	}
	return ""
}

// mapPodToShadow triggers reconciliation for Shadow Services when a referenced Pod changes
func (r *PodReconciler) mapPodToShadow(ctx context.Context, obj client.Object) []reconcile.Request {
	pod := obj.(*corev1.Pod)
	var requests []reconcile.Request

	// 1. List all Shadow Services in this namespace
	// We list all services and filter by annotation because we don't know
	// which shadow service cares about this pod until we check the target.
	shadowSvcs := &corev1.ServiceList{}
	if err := r.List(ctx, shadowSvcs, client.InNamespace(pod.Namespace)); err != nil {
		return nil
	}

	for _, shadow := range shadowSvcs.Items {
		targetName := shadow.Annotations[SyncServiceAnnotation]
		if targetName == "" {
			continue
		}

		// 2. Find the Original Service this Shadow points to
		targetSvc := &corev1.Service{}
		if err := r.Get(ctx, types.NamespacedName{Name: targetName, Namespace: pod.Namespace}, targetSvc); err != nil {
			continue
		}

		// 3. Does this Pod match the Original Service's selector?
		selector := labels.SelectorFromSet(targetSvc.Spec.Selector)
		if !selector.Empty() && selector.Matches(labels.Set(pod.Labels)) {
			// Bingo! This pod belongs to the target service.
			// Therefore, we must update the Shadow Service.
			requests = append(requests, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: shadow.Name, Namespace: shadow.Namespace},
			})
		}
	}
	return requests
}

// mapOriginalServiceToShadow triggers reconciliation when the Target Service itself changes
func (r *PodReconciler) mapOriginalServiceToShadow(ctx context.Context, obj client.Object) []reconcile.Request {
	originalSvc := obj.(*corev1.Service)
	var requests []reconcile.Request

	// Use our Indexer to instantly find which Shadow Services point to this Original Service
	shadowList := &corev1.ServiceList{}
	if err := r.List(ctx, shadowList, client.InNamespace(originalSvc.Namespace),
		client.MatchingFields{refFieldIndex: originalSvc.Name}); err != nil {
		return nil
	}

	for _, shadow := range shadowList.Items {
		requests = append(requests, reconcile.Request{
			NamespacedName: types.NamespacedName{Name: shadow.Name, Namespace: shadow.Namespace},
		})
	}

	return requests
}
