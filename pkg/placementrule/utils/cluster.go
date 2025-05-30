// Copyright 2021 The Kubernetes Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package utils

import (
	"reflect"

	v1 "k8s.io/api/core/v1"
	"k8s.io/klog"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	spokeClusterV1 "open-cluster-management.io/api/cluster/v1"
	clusterv1beta1 "open-cluster-management.io/api/cluster/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

const (
	// #nosec G101
	ACMClusterSecretLabel = "apps.open-cluster-management.io/secret-type"
	// #nosec G101
	ArgocdClusterSecretLabel = "apps.open-cluster-management.io/acm-cluster"
	// #nosec G101
	ACMClusterNameLabel = "apps.open-cluster-management.io/cluster-name"
)

// ClusterPredicateFunc defines predicate function for cluster related watch, main purpose is to ignore heartbeat without change
var ClusterPredicateFunc = predicate.TypedFuncs[*spokeClusterV1.ManagedCluster]{
	CreateFunc: func(e event.TypedCreateEvent[*spokeClusterV1.ManagedCluster]) bool {
		newManagedCluster := e.Object

		klog.Infof("new managed cluster created, %v/%v", newManagedCluster.Namespace, newManagedCluster.Name)

		return true
	},

	UpdateFunc: func(e event.TypedUpdateEvent[*spokeClusterV1.ManagedCluster]) bool {
		oldcl := e.ObjectOld
		newcl := e.ObjectNew

		//if managed cluster is being deleted
		if !reflect.DeepEqual(oldcl.DeletionTimestamp, newcl.DeletionTimestamp) {
			return true
		}

		if !reflect.DeepEqual(oldcl.Labels, newcl.Labels) {
			return true
		}

		oldcondMap := make(map[string]metav1.ConditionStatus)
		for _, cond := range oldcl.Status.Conditions {
			oldcondMap[cond.Type] = cond.Status
		}
		for _, cond := range newcl.Status.Conditions {
			oldcondst, ok := oldcondMap[cond.Type]
			if !ok || oldcondst != cond.Status {
				return true
			}
			delete(oldcondMap, cond.Type)
		}

		if len(oldcondMap) > 0 {
			return true
		}

		klog.V(1).Info("Out Cluster Predicate Func ", oldcl.Name, " with false possitive")
		return false
	},
}

var PlacementDecisionPredicateFunc = predicate.TypedFuncs[*clusterv1beta1.PlacementDecision]{
	CreateFunc: func(e event.TypedCreateEvent[*clusterv1beta1.PlacementDecision]) bool {
		decision := e.Object

		klog.Infof("placement decision created, %v/%v", decision.Namespace, decision.Name)
		return true
	},
	DeleteFunc: func(e event.TypedDeleteEvent[*clusterv1beta1.PlacementDecision]) bool {
		decision := e.Object

		klog.Infof("placement decision deleted, %v/%v", decision.Namespace, decision.Name)
		return true
	},
	UpdateFunc: func(e event.TypedUpdateEvent[*clusterv1beta1.PlacementDecision]) bool {
		oldDecision := e.ObjectOld
		newDecision := e.ObjectNew

		klog.Infof("placement decision updated, %v/%v", newDecision.Namespace, newDecision.Name)

		return !reflect.DeepEqual(oldDecision.Status, newDecision.Status)
	},
}

// AcmClusterSecretPredicateFunc defines predicate function for ACM cluster secrets watch
var AcmClusterSecretPredicateFunc = predicate.TypedFuncs[*v1.Secret]{
	UpdateFunc: func(e event.TypedUpdateEvent[*v1.Secret]) bool {
		oldSecret := e.ObjectOld
		newSecret := e.ObjectNew

		oldSecretType, ok := e.ObjectOld.GetLabels()[ACMClusterSecretLabel]
		newSecretType, nok := e.ObjectNew.GetLabels()[ACMClusterSecretLabel]

		if ok && oldSecretType == "acm-cluster" {
			klog.Infof("Update a old ACM cluster secret, old: %v/%v, new: %v/%v", oldSecret.Namespace, oldSecret.Name, newSecret.Namespace, newSecret.Name)
			return true
		}

		if nok && newSecretType == "acm-cluster" {
			klog.Infof("Update a new ACM cluster secret, old: %v/%v, new: %v/%v", oldSecret.Namespace, oldSecret.Name, newSecret.Namespace, newSecret.Name)
			return true
		}

		klog.Infof("Not a ACM cluster secret update, old: %v/%v, new: %v/%v", oldSecret.Namespace, oldSecret.Name, newSecret.Namespace, newSecret.Name)
		return false
	},
	CreateFunc: func(e event.TypedCreateEvent[*v1.Secret]) bool {
		SecretType, ok := e.Object.GetLabels()[ACMClusterSecretLabel]

		if !ok {
			return false
		} else if SecretType != "acm-cluster" {
			return false
		}

		klog.Infof("Create a ACM cluster secret: %v/%v", e.Object.GetNamespace(), e.Object.GetName())
		return true
	},
	DeleteFunc: func(e event.TypedDeleteEvent[*v1.Secret]) bool {
		SecretType, ok := e.Object.GetLabels()[ACMClusterSecretLabel]

		if !ok {
			return false
		} else if SecretType != "acm-cluster" {
			return false
		}

		klog.Infof("Delete a ACM cluster secret: %v/%v", e.Object.GetNamespace(), e.Object.GetName())
		return true
	},
}

// ArgocdClusterSecretPredicateFunc defines predicate function for ArgoCD cluster secrets watch
var ArgocdClusterSecretPredicateFunc = predicate.TypedFuncs[*v1.Secret]{
	UpdateFunc: func(e event.TypedUpdateEvent[*v1.Secret]) bool {
		oldSecret := e.ObjectOld

		newSecret := e.ObjectNew

		oldSecretType, ok := e.ObjectOld.GetLabels()[ArgocdClusterSecretLabel]
		newSecretType, nok := e.ObjectNew.GetLabels()[ArgocdClusterSecretLabel]

		if ok && oldSecretType == "true" {
			klog.Infof("Update a old ArgoCD cluster secret, old: %v/%v, new: %v/%v", oldSecret.Namespace, oldSecret.Name, newSecret.Namespace, newSecret.Name)
			return true
		}

		if nok && newSecretType == "true" {
			klog.Infof("Update a new Argocd cluster secret, old: %v/%v, new: %v/%v", oldSecret.Namespace, oldSecret.Name, newSecret.Namespace, newSecret.Name)
			return true
		}

		klog.Infof("Not a ArgoCD cluster secret update, old: %v/%v, new: %v/%v", oldSecret.Namespace, oldSecret.Name, newSecret.Namespace, newSecret.Name)
		return false
	},
	CreateFunc: func(e event.TypedCreateEvent[*v1.Secret]) bool {
		SecretType, ok := e.Object.GetLabels()[ArgocdClusterSecretLabel]

		if !ok {
			return false
		} else if SecretType != "true" {
			return false
		}

		klog.Infof("Create a ArgoCD cluster secret: %v/%v", e.Object.GetNamespace(), e.Object.GetName())
		return true
	},
	DeleteFunc: func(e event.TypedDeleteEvent[*v1.Secret]) bool {
		SecretType, ok := e.Object.GetLabels()[ArgocdClusterSecretLabel]

		if !ok {
			return false
		} else if SecretType != "true" {
			return false
		}

		klog.Infof("Delete a ArgoCD cluster secret: %v/%v", e.Object.GetNamespace(), e.Object.GetName())
		return true
	},
}

// ManagedClusterSecretPredicateFunc defines predicate function for managed cluster secrets watch
var ManagedClusterSecretPredicateFunc = predicate.TypedFuncs[*v1.Secret]{
	UpdateFunc: func(e event.TypedUpdateEvent[*v1.Secret]) bool {
		_, isSecretInArgo := e.ObjectNew.GetLabels()[ArgocdClusterSecretLabel]

		if isSecretInArgo {
			klog.Infof("Managed cluster secret in ArgoCD namespace updated: %v/%v", e.ObjectNew.GetNamespace(), e.ObjectNew.GetName())

			// No reconcile if the secret is in argo server namespae
			return false
		}

		return true
	},
	CreateFunc: func(e event.TypedCreateEvent[*v1.Secret]) bool {
		_, isSecretInArgo := e.Object.GetLabels()[ArgocdClusterSecretLabel]

		if isSecretInArgo {
			klog.Infof("Managed cluster secret in ArgoCD namespace created: %v/%v", e.Object.GetNamespace(), e.Object.GetName())

			// No reconcile if the secret is in argo server namespae
			return false
		}

		return true
	},
	DeleteFunc: func(e event.TypedDeleteEvent[*v1.Secret]) bool {
		_, isSecretInArgo := e.Object.GetLabels()[ArgocdClusterSecretLabel]

		if isSecretInArgo {
			klog.Infof("Managed cluster secret in ArgoCD namespace deleted: %v/%v", e.Object.GetNamespace(), e.Object.GetName())

			return true
		}

		// No reconcile if the secret is deleted from managed cluster namespae. Let placement decision update
		// trigger reconcile
		return false
	},
}

// ArgocdServerPredicateFunc defines predicate function for cluster related watch
var ArgocdServerPredicateFunc = predicate.TypedFuncs[*v1.Service]{
	UpdateFunc: func(e event.TypedUpdateEvent[*v1.Service]) bool {
		oldService := e.ObjectOld
		newService := e.ObjectNew

		oldArgocdServerLabel := e.ObjectOld.GetLabels()
		newArgocdServerLabel := e.ObjectNew.GetLabels()

		if oldArgocdServerLabel != nil && oldArgocdServerLabel["app.kubernetes.io/part-of"] == "argocd" &&
			oldArgocdServerLabel["app.kubernetes.io/component"] == "server" {
			klog.Infof("Update a old ArgoCD Server Service, old: %v/%v, new: %v/%v", oldService.Namespace, oldService.Name, newService.Namespace, newService.Name)
			return true
		}

		if newArgocdServerLabel != nil && newArgocdServerLabel["app.kubernetes.io/part-of"] == "argocd" &&
			newArgocdServerLabel["app.kubernetes.io/component"] == "server" {
			klog.Infof("Update a new ArgoCD Server Service, old: %v/%v, new: %v/%v", oldService.Namespace, oldService.Name, newService.Namespace, newService.Name)
			return true
		}

		klog.Infof("Not a ArgoCD Server service, old: %v/%v, new: %v/%v", oldService.Namespace, oldService.Name, newService.Namespace, newService.Name)
		return false
	},
	CreateFunc: func(e event.TypedCreateEvent[*v1.Service]) bool {
		ArgocdServerLabel := e.Object.GetLabels()

		if ArgocdServerLabel == nil {
			return false
		} else if ArgocdServerLabel["app.kubernetes.io/part-of"] != "argocd" ||
			ArgocdServerLabel["app.kubernetes.io/component"] != "server" {
			return false
		}

		klog.Infof("Create a ArgoCD Server Service: %v/%v", e.Object.GetNamespace(), e.Object.GetName())
		return true
	},
	DeleteFunc: func(e event.TypedDeleteEvent[*v1.Service]) bool {
		ArgocdServerLabel := e.Object.GetLabels()

		if ArgocdServerLabel == nil {
			return false
		} else if ArgocdServerLabel["app.kubernetes.io/part-of"] != "argocd" ||
			ArgocdServerLabel["app.kubernetes.io/component"] != "server" {
			return false
		}

		klog.Infof("Delete a ArgoCD Server Service: %v/%v", e.Object.GetNamespace(), e.Object.GetName())
		return true
	},
}
