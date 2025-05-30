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

package git

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/ghodss/yaml"
	"helm.sh/helm/v3/pkg/repo"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/klog"

	corev1 "k8s.io/api/core/v1"

	chnv1 "open-cluster-management.io/multicloud-operators-channel/pkg/apis/apps/v1"
	appv1 "open-cluster-management.io/multicloud-operators-subscription/pkg/apis/apps/v1"
	"open-cluster-management.io/multicloud-operators-subscription/pkg/metrics"
	kubesynchronizer "open-cluster-management.io/multicloud-operators-subscription/pkg/synchronizer/kubernetes"
	"open-cluster-management.io/multicloud-operators-subscription/pkg/utils"
)

const (
	// UserID is key of GitHub user ID in secret
	UserID = "user"
	// AccessToken is key of GitHub user password or personal token in secret
	AccessToken = "accessToken"
	// Path is the key of GitHub package filter config map
	Path = "path"
)

var (
	helmGvk = schema.GroupVersionKind{
		Group:   appv1.SchemeGroupVersion.Group,
		Version: appv1.SchemeGroupVersion.Version,
		Kind:    "HelmRelease",
	}
)

// SubscriberItem - defines the unit of namespace subscription
type SubscriberItem struct {
	appv1.SubscriberItem
	crdsAndNamespaceFiles  []string
	rbacFiles              []string
	otherFiles             []string
	repoRoot               string
	commitID               string
	reconcileRate          string
	desiredCommit          string
	desiredTag             string
	syncTime               string
	stopch                 chan struct{}
	syncinterval           int
	count                  int
	synchronizer           SyncSource
	chartDirs              map[string]string
	kustomizeDirs          map[string]string
	resources              []kubesynchronizer.ResourceUnit
	indexFile              *repo.IndexFile
	webhookEnabled         bool
	successful             bool
	clusterAdmin           bool
	currentNamespaceScoped bool
	userID                 string
	userGroup              string
}

type kubeResource struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
}

// Start subscribes a subscriber item with github channel
func (ghsi *SubscriberItem) Start(restart bool) {
	// do nothing if already started
	if ghsi.stopch != nil {
		if restart {
			// restart this goroutine
			klog.Info("Stopping SubscriberItem: ", ghsi.Subscription.Name)
			ghsi.Stop()
		} else {
			klog.Info("SubscriberItem already started: ", ghsi.Subscription.Name)
			return
		}
	}

	ghsi.count = 0 // reset the counter

	ghsi.stopch = make(chan struct{})

	loopPeriod, retryInterval, retries := utils.GetReconcileInterval(ghsi.reconcileRate, chnv1.ChannelTypeGit)

	if strings.EqualFold(ghsi.reconcileRate, "off") {
		klog.Infof("auto-reconcile is OFF")

		ghsi.doSubscriptionWithRetries(retryInterval, retries)

		return
	}

	go wait.Until(func() {
		tw := ghsi.SubscriberItem.Subscription.Spec.TimeWindow
		if tw != nil {
			nextRun := utils.NextStartPoint(tw, time.Now())
			if nextRun > time.Duration(0) {
				klog.Infof("Subscription is currently blocked by the time window. It %v/%v will be deployed after %v",
					ghsi.SubscriberItem.Subscription.GetNamespace(),
					ghsi.SubscriberItem.Subscription.GetName(), nextRun)

				return
			}
		}

		// if the subscription pause lable is true, stop subscription here.
		if utils.GetPauseLabel(ghsi.SubscriberItem.Subscription) {
			klog.Infof("Git Subscription %v/%v is paused.", ghsi.SubscriberItem.Subscription.GetNamespace(), ghsi.SubscriberItem.Subscription.GetName())

			return
		}

		ghsi.doSubscriptionWithRetries(retryInterval, retries)
	}, loopPeriod, ghsi.stopch)
}

// Stop unsubscribes a subscriber item with namespace channel
func (ghsi *SubscriberItem) Stop() {
	klog.Info("Stopping SubscriberItem ", ghsi.Subscription.Name)
	close(ghsi.stopch)
}

func (ghsi *SubscriberItem) doSubscriptionWithRetries(retryInterval time.Duration, retries int) {
	// If the initial subscription fails, retry.
	for n := 0; n <= retries; n++ {
		klog.Infof("Try #%d/%d: subcribing to the Git repo", n, retries)

		err := ghsi.doSubscription()
		if err != nil {
			klog.Error(err, "Subscription error.")
			klog.Infof("mark appsub (%s/%s) as failed with reason: %v", ghsi.Subscription.Namespace, ghsi.Subscription.Name, err.Error())

			utils.UpdateSubscriptionStatus(ghsi.synchronizer.GetLocalClient(), ghsi.Subscription.Name,
				ghsi.Subscription.Namespace, appv1.SubscriptionFailed, err.Error())
		} else {
			klog.Infof("mark appsub (%s/%s) as subscribed", ghsi.Subscription.Namespace, ghsi.Subscription.Name)

			utils.UpdateSubscriptionStatus(ghsi.synchronizer.GetLocalClient(), ghsi.Subscription.Name,
				ghsi.Subscription.Namespace, appv1.SubscriptionSubscribed, "")
		}

		if !ghsi.successful && n+1 <= retries {
			klog.Info("failed to subscribed to Git rep, retry after sleep")
			time.Sleep(retryInterval)
		} else {
			break
		}
	}
}

func (ghsi *SubscriberItem) doSubscription() error {
	hostkey := types.NamespacedName{Name: ghsi.Subscription.Name, Namespace: ghsi.Subscription.Namespace}
	klog.Info("enter doSubscription: ", hostkey.String())

	defer klog.Info("exit doSubscription: ", hostkey.String())

	utils.UpdateLastUpdateTime(ghsi.synchronizer.GetLocalClient(), ghsi.Subscription)

	// If webhook is enabled, don't do anything until next reconcilitation.
	if ghsi.webhookEnabled {
		klog.Infof("Git Webhook is enabled on subscription %s.", ghsi.Subscription.Name)

		if ghsi.successful {
			klog.Infof("All resources are reconciled successfully. Waiting for the next Git Webhook event.")
			return nil
		}

		klog.Infof("Resources are not reconciled successfully yet. Continue reconciling.")
	}

	klog.Info("Subscribing ...", ghsi.Subscription.Name)

	//Update the secret and config map
	if ghsi.Channel != nil {
		sec, cm := utils.FetchChannelReferences(ghsi.synchronizer.GetRemoteNonCachedClient(), *ghsi.Channel)
		if sec != nil {
			if err := utils.ListAndDeployReferredObject(ghsi.synchronizer.GetLocalNonCachedClient(), ghsi.Subscription,
				schema.GroupVersionKind{Group: "", Kind: "Secret", Version: "v1"}, sec); err != nil {
				klog.Warningf("can't deploy reference secret %v for subscription %v", ghsi.ChannelSecret.GetName(), ghsi.Subscription.GetName())
			}
		}

		if cm != nil {
			if err := utils.ListAndDeployReferredObject(ghsi.synchronizer.GetLocalNonCachedClient(), ghsi.Subscription,
				schema.GroupVersionKind{Group: "", Kind: "ConfigMap", Version: "v1"}, cm); err != nil {
				klog.Warningf("can't deploy reference configmap %v for subscription %v", ghsi.ChannelConfigMap.GetName(), ghsi.Subscription.GetName())
			}
		}

		sec, cm = utils.FetchChannelReferences(ghsi.synchronizer.GetLocalNonCachedClient(), *ghsi.Channel)
		if sec != nil {
			klog.V(1).Info("updated in memory channel secret for ", ghsi.Subscription.Name)
			ghsi.ChannelSecret = sec
		}

		if cm != nil {
			klog.V(1).Info("updated in memory channel configmap for ", ghsi.Subscription.Name)
			ghsi.ChannelConfigMap = cm
		}
	}

	if ghsi.SecondaryChannel != nil {
		sec, cm := utils.FetchChannelReferences(ghsi.synchronizer.GetRemoteNonCachedClient(), *ghsi.SecondaryChannel)
		if sec != nil {
			if err := utils.ListAndDeployReferredObject(ghsi.synchronizer.GetLocalNonCachedClient(), ghsi.Subscription,
				schema.GroupVersionKind{Group: "", Kind: "Secret", Version: "v1"}, sec); err != nil {
				klog.Warningf("can't deploy reference secondary secret %v for subscription %v", ghsi.SecondaryChannelSecret.GetName(), ghsi.Subscription.GetName())
			}
		}

		if cm != nil {
			if err := utils.ListAndDeployReferredObject(ghsi.synchronizer.GetLocalNonCachedClient(), ghsi.Subscription,
				schema.GroupVersionKind{Group: "", Kind: "ConfigMap", Version: "v1"}, cm); err != nil {
				klog.Warningf("can't deploy reference secondary configmap %v for subscription %v", ghsi.SecondaryChannelConfigMap.GetName(), ghsi.Subscription.GetName())
			}
		}

		sec, cm = utils.FetchChannelReferences(ghsi.synchronizer.GetLocalNonCachedClient(), *ghsi.SecondaryChannel)
		if sec != nil {
			klog.Info("updated in memory secondary channel secret for ", ghsi.Subscription.Name)
			ghsi.SecondaryChannelSecret = sec
		}

		if cm != nil {
			klog.V(1).Info("updated in memory secondary channel configmap for ", ghsi.Subscription.Name)
			ghsi.SecondaryChannelConfigMap = cm
		}
	}

	//Clone the git repo
	startTime := time.Now().UnixMilli()
	commitID, err := ghsi.cloneGitRepo()
	endTime := time.Now().UnixMilli()

	if err != nil {
		klog.Error(err, "Unable to clone the git repo ", ghsi.Channel.Spec.Pathname)
		ghsi.successful = false

		metrics.GitFailedPullTime.
			WithLabelValues(ghsi.SubscriberItem.Subscription.Namespace, ghsi.SubscriberItem.Subscription.Name).
			Observe(float64(endTime - startTime))

		return err
	}

	metrics.GitSuccessfulPullTime.
		WithLabelValues(ghsi.SubscriberItem.Subscription.Namespace, ghsi.SubscriberItem.Subscription.Name).
		Observe(float64(endTime - startTime))

	klog.Info("Git commit: ", commitID)

	if strings.EqualFold(ghsi.reconcileRate, "medium") {
		// every 3 minutes, compare commit ID. If changed, reconcile resources.
		// every 15 minutes, reconcile resources without commit ID comparison.
		ghsi.count++

		if ghsi.commitID == "" {
			klog.Infof("No previous commit. DEPLOY")
		} else {
			if ghsi.count < 6 {
				if commitID == ghsi.commitID && ghsi.successful {
					klog.Infof("Appsub %s Git commit: %s hasn't changed. Skip reconcile.", hostkey.String(), commitID)

					return nil
				}
			} else {
				klog.Infof("Reconciling all resources")
				ghsi.count = 0
			}
		}
	}

	ghsi.resources = []kubesynchronizer.ResourceUnit{}

	err = ghsi.sortClonedGitRepo()
	if err != nil {
		klog.Error(err, " Unable to sort helm charts and kubernetes resources from the cloned git repo.")

		ghsi.successful = false
		metrics.LocalDeploymentFailedPullTime.
			WithLabelValues(ghsi.SubscriberItem.Subscription.Namespace, ghsi.SubscriberItem.Subscription.Name).
			Observe(0)

		return err
	}

	errMsg := ""

	klog.Info("Applying crd resources: ", ghsi.crdsAndNamespaceFiles)

	err = ghsi.subscribeResources(ghsi.crdsAndNamespaceFiles)

	if err != nil {
		klog.Error(err, " Unable to subscribe crd and ns resources")

		ghsi.successful = false

		errMsg += err.Error()
	}

	klog.Info("Applying rbac resources: ", ghsi.rbacFiles)

	err = ghsi.subscribeResources(ghsi.rbacFiles)

	if err != nil {
		klog.Error(err, " Unable to subscribe rbac resources")

		ghsi.successful = false

		if len(errMsg) > 0 {
			errMsg += ", "
		}

		errMsg += err.Error()
	}

	klog.Info("Applying other resources: ", ghsi.otherFiles)

	err = ghsi.subscribeResources(ghsi.otherFiles)

	if err != nil {
		klog.Error(err, " Unable to subscribe other resources")

		ghsi.successful = false

		if len(errMsg) > 0 {
			errMsg += ", "
		}

		errMsg += err.Error()
	}

	klog.Info("Applying kustomizations: ", ghsi.kustomizeDirs)

	err = ghsi.subscribeKustomizations()

	if err != nil {
		klog.Error(err, " Unable to subscribe kustomize resources")

		// Update subscription status with kustomization error
		ghsi.successful = false

		kusErr := fmt.Sprintf("failed to apply klustomization: %s", err.Error())
		if len(errMsg) > 0 {
			kusErr += ", "
		}

		errMsg = kusErr + errMsg

		if err = ghsi.synchronizer.UpdateAppsubOverallStatus(ghsi.Subscription, true, errMsg); err != nil {
			klog.Error(err, "Unable to update subscription overall status")
		}

		metrics.LocalDeploymentFailedPullTime.
			WithLabelValues(ghsi.SubscriberItem.Subscription.Namespace, ghsi.SubscriberItem.Subscription.Name).
			Observe(0)
	}

	klog.Info("Applying helm charts..")

	err = ghsi.subscribeHelmCharts(ghsi.indexFile)

	if err != nil {
		klog.Error(err, "Unable to subscribe helm charts")

		ghsi.successful = false

		if len(errMsg) > 0 {
			errMsg += ", "
		}

		errMsg += err.Error()
	}

	standaloneSubscription := false

	annotations := ghsi.Subscription.GetAnnotations()

	if annotations == nil || annotations[appv1.AnnotationHosting] == "" {
		standaloneSubscription = true
	}

	// If it failed to add applicable resources to the list, do not apply the empty list.
	// It will cause already deployed resourced to be removed.
	// Update the host subscription status accordingly and quit.
	if len(ghsi.resources) == 0 && !ghsi.successful {
		if (ghsi.synchronizer.GetRemoteClient() != nil) && !standaloneSubscription {
			klog.Error("failed to prepare resources to apply and there is no resource to apply. quit")
		}

		metrics.LocalDeploymentFailedPullTime.
			WithLabelValues(ghsi.SubscriberItem.Subscription.Namespace, ghsi.SubscriberItem.Subscription.Name).
			Observe(0)

		return fmt.Errorf("%.2000s", errMsg)
	}

	allowedGroupResources, deniedGroupResources := utils.GetAllowDenyLists(*ghsi.Subscription)

	if err := ghsi.synchronizer.ProcessSubResources(ghsi.Subscription, ghsi.resources,
		allowedGroupResources, deniedGroupResources, ghsi.clusterAdmin, true); err != nil {
		klog.Error(err)

		ghsi.successful = false

		return err
	}

	ghsi.commitID = commitID

	ghsi.resources = nil
	ghsi.chartDirs = nil
	ghsi.kustomizeDirs = nil
	ghsi.crdsAndNamespaceFiles = nil
	ghsi.rbacFiles = nil
	ghsi.otherFiles = nil
	ghsi.indexFile = nil
	ghsi.successful = true

	return nil
}

func (ghsi *SubscriberItem) subscribeKustomizations() error {
	for _, kustomizeDir := range ghsi.kustomizeDirs {
		klog.Info("Applying kustomization ", kustomizeDir)

		//nolint:copyloopvar
		relativePath := kustomizeDir

		if len(strings.SplitAfter(kustomizeDir, ghsi.repoRoot+"/")) > 1 {
			relativePath = strings.SplitAfter(kustomizeDir, ghsi.repoRoot+"/")[1]
		}

		err := utils.VerifyAndOverrideKustomize(ghsi.Subscription.Spec.PackageOverrides, relativePath, kustomizeDir)
		if err != nil {
			klog.Error("Failed to override kustomization, clean up all resources that will deploy. error: ", err.Error())
			ghsi.resources = []kubesynchronizer.ResourceUnit{}

			return err
		}

		out, err := utils.RunKustomizeBuild(kustomizeDir)

		if err != nil {
			klog.Error("Failed to apply kustomization, clean up all resources that will deploy. error: ", err.Error())

			// If applying one kustomize folder fails after some other kustomize folder success, clean up the memory git resource list for stopping synchronizer.
			// Or only successfully kustomized resources are deployed,
			// that will trigger synchronizer to delete those resources that haven't been kustomized but deployed previously
			ghsi.resources = []kubesynchronizer.ResourceUnit{}

			return err
		}

		// Split the output of kustomize build output into individual kube resource YAML files
		resources := utils.ParseYAML(out)
		for _, resource := range resources {
			resourceFile := []byte(strings.Trim(resource, "\t \n"))

			t := kubeResource{}
			err := yaml.Unmarshal(resourceFile, &t)

			if err != nil {
				klog.Error(err, "Failed to unmarshal YAML file")
				continue
			}

			if t.APIVersion == "" || t.Kind == "" {
				klog.Info("Not a Kubernetes resource")
			} else {
				err := checkSubscriptionAnnotation(t)
				if err != nil {
					klog.Errorf("Failed to apply %s/%s resource. err: %s", t.APIVersion, t.Kind, err)
				}

				ghsi.subscribeResourceFile(resourceFile)
			}
		}
	}

	return nil
}

func checkSubscriptionAnnotation(resource kubeResource) error {
	if strings.EqualFold(resource.APIVersion, appv1.SchemeGroupVersion.String()) && strings.EqualFold(resource.Kind, "Subscription") {
		annotations := resource.GetAnnotations()
		if strings.EqualFold(annotations[appv1.AnnotationClusterAdmin], "true") {
			klog.Errorf("%s %s contains annotation %s set to true.", resource.APIVersion, resource.Name, appv1.AnnotationClusterAdmin)
			return errors.New("contains " + appv1.AnnotationClusterAdmin + " = true annotation.")
		}
	}

	return nil
}

func (ghsi *SubscriberItem) subscribeResources(rscFiles []string) error {
	// sync kube resource manifests
	for _, rscFile := range rscFiles {
		file, err := os.ReadFile(rscFile) // #nosec G304 rscFile is not user input

		if err != nil {
			klog.Error(err, "Failed to read YAML file "+rscFile)

			return err
		}

		resources := utils.ParseKubeResoures(file)

		if len(resources) > 0 {
			for _, resource := range resources {
				t := kubeResource{}
				err := yaml.Unmarshal(resource, &t)

				if err != nil {
					// Ignore if it does not have apiVersion or kind fields in the YAML
					klog.Infof("Invalid kube resources. err: %v ", err)

					continue
				}

				klog.V(1).Info("Applying Kubernetes resource of kind ", t.Kind)

				if t.Kind == "Subscription" {
					klog.V(1).Infof("Injecting userID(%s), Group(%s) to subscription", ghsi.userID, ghsi.userGroup)

					o := &unstructured.Unstructured{}
					if err := yaml.Unmarshal(resource, o); err != nil {
						klog.Error("Failed to unmarshal resource YAML.")

						return err
					}

					annotations := o.GetAnnotations()
					if len(annotations) == 0 {
						annotations = map[string]string{}
					}

					annotations[appv1.AnnotationUserIdentity] = ghsi.userID
					annotations[appv1.AnnotationUserGroup] = ghsi.userGroup
					o.SetAnnotations(annotations)

					resource, err = yaml.Marshal(o)
					if err != nil {
						klog.Error(err)

						continue
					}
				}

				ghsi.subscribeResourceFile(resource)
			}
		}
	}

	return nil
}

func (ghsi *SubscriberItem) subscribeResourceFile(file []byte) {
	resourceToSync, validgvk, err := ghsi.subscribeResource(file)
	if err != nil {
		klog.Error(err)
	}

	if resourceToSync == nil || validgvk == nil {
		klog.Info("Skipping resource")

		return
	}

	ghsi.resources = append(ghsi.resources, kubesynchronizer.ResourceUnit{Resource: resourceToSync, Gvk: *validgvk})
}

func (ghsi *SubscriberItem) subscribeResource(file []byte) (*unstructured.Unstructured, *schema.GroupVersionKind, error) {
	rsc := &unstructured.Unstructured{}
	err := yaml.Unmarshal(file, &rsc)

	if err != nil {
		klog.Errorf("Failed to unmarshal Kubernetes resource to Unstructured, err:%v ", err)
		return nil, nil, err
	}

	// The labels patched by kustomization could not be decoded to rsc correctly if not all label values are defined as string
	// The label type is defined as map[string]string. If a label value is defined as a boolean data type such as true/false, the whole label patch will be ignored
	// As a workaround, users can manually update the label value to "true" as string type in the kustomization.yaml
	// To resolve it, get all labels from original resource file and explicitly set them to the new resource to be deployed.
	t := kubeResource{}
	err = yaml.Unmarshal(file, &t)

	if err != nil {
		klog.Errorf("Failed to unmarshal Kubernetes resource to kubeResource, err:%v ", err)
		return nil, nil, err
	}

	if resourceLabels := t.GetLabels(); resourceLabels != nil {
		rsc.SetLabels(resourceLabels)
	}

	if resourceAnnos := t.GetAnnotations(); resourceAnnos != nil {
		rsc.SetAnnotations(resourceAnnos)
	}

	validgvk := rsc.GetObjectKind().GroupVersionKind()

	if ghsi.synchronizer.IsResourceNamespaced(rsc) {
		if ghsi.clusterAdmin {
			klog.Info("cluster-admin is true.")

			if rsc.GetNamespace() != "" {
				if ghsi.currentNamespaceScoped {
					// If current-namespace-scoped annotation is true, deploy resources into subscription's namespace
					klog.Info("Setting it to subscription namespace " + ghsi.Subscription.Namespace)
					rsc.SetNamespace(ghsi.Subscription.Namespace)
				} else {
					klog.Info("Using resource's original namespace. Resource namespace is " + rsc.GetNamespace())
				}
			} else {
				klog.Info("Setting it to subscription namespace " + ghsi.Subscription.Namespace)
				rsc.SetNamespace(ghsi.Subscription.Namespace)
			}

			rscAnnotations := rsc.GetAnnotations()

			if rscAnnotations == nil {
				rscAnnotations = make(map[string]string)
			}

			if strings.EqualFold(rsc.GroupVersionKind().Group, "apps.open-cluster-management.io") &&
				strings.EqualFold(rsc.GroupVersionKind().Kind, "Subscription") {
				// Adding cluster-admin=true annotation to child subscription
				rscAnnotations[appv1.AnnotationClusterAdmin] = "true"
				rsc.SetAnnotations(rscAnnotations)
			}
		} else {
			klog.Info("No cluster-admin. Setting it to subscription namespace " + ghsi.Subscription.Namespace)
			rsc.SetNamespace(ghsi.Subscription.Namespace)
		}
	}

	if ghsi.Subscription.Spec.PackageFilter != nil {
		errMsg := ghsi.checkFilters(rsc)
		if errMsg != "" {
			klog.Infof("failed to check package filter, err: %v", errMsg)

			return nil, nil, nil
		}
	}

	if ghsi.Subscription.Spec.PackageOverrides != nil {
		rsc, err = utils.OverrideResourceBySubscription(rsc, rsc.GetName(), ghsi.Subscription)
		if err != nil {
			errmsg := "Failed override package " + rsc.GetName() + " with error: " + err.Error()
			err = utils.SetInClusterPackageStatus(&(ghsi.Subscription.Status), rsc.GetName(), err, nil)

			if err != nil {
				errmsg += " and failed to set in cluster package status with error: " + err.Error()
			}

			klog.V(2).Info(errmsg)

			return nil, nil, errors.New(errmsg)
		}
	}

	subAnnotations := ghsi.Subscription.GetAnnotations()
	if subAnnotations != nil {
		rscAnnotations := rsc.GetAnnotations()
		if rscAnnotations == nil {
			rscAnnotations = make(map[string]string)
		}

		if strings.EqualFold(subAnnotations[appv1.AnnotationClusterAdmin], "true") {
			rscAnnotations[appv1.AnnotationClusterAdmin] = "true"
		}

		// If the reconcile-option is set in the resource, honor that. Otherwise, take the subscription's reconcile-option
		if rscAnnotations[appv1.AnnotationResourceReconcileOption] == "" {
			if subAnnotations[appv1.AnnotationResourceReconcileOption] != "" {
				rscAnnotations[appv1.AnnotationResourceReconcileOption] = subAnnotations[appv1.AnnotationResourceReconcileOption]
			} else {
				// By default, merge reconcile
				rscAnnotations[appv1.AnnotationResourceReconcileOption] = appv1.MergeReconcile
			}
		}

		rsc.SetAnnotations(rscAnnotations)
	}

	// Set app label
	utils.SetPartOfLabel(ghsi.SubscriberItem.Subscription, rsc)

	klog.Infof("new resource for deployment: %#v", rsc)

	return rsc, &validgvk, nil
}

func (ghsi *SubscriberItem) checkFilters(rsc *unstructured.Unstructured) (errMsg string) {
	if ghsi.Subscription.Spec.Package != "" && ghsi.Subscription.Spec.Package != rsc.GetName() {
		errMsg = "Name does not match, skiping:" + ghsi.Subscription.Spec.Package + "|" + rsc.GetName()

		return errMsg
	}

	if ghsi.Subscription.Spec.Package == rsc.GetName() {
		klog.V(4).Info("Name does matches: " + ghsi.Subscription.Spec.Package + "|" + rsc.GetName())
	}

	if ghsi.Subscription.Spec.PackageFilter != nil {
		if utils.LabelChecker(ghsi.Subscription.Spec.PackageFilter.LabelSelector, rsc.GetLabels()) {
			klog.V(4).Info("Passed label check on resource " + rsc.GetName())
		} else {
			errMsg = "Failed to pass label check on resource " + rsc.GetName()

			return errMsg
		}

		annotations := ghsi.Subscription.Spec.PackageFilter.Annotations
		if annotations != nil {
			klog.V(4).Info("checking annotations filter:", annotations)

			rscanno := rsc.GetAnnotations()
			if rscanno == nil {
				rscanno = make(map[string]string)
			}

			matched := true

			for k, v := range annotations {
				if rscanno[k] != v {
					klog.Info("Annotation filter does not match:", k, "|", v, "|", rscanno[k])

					matched = false

					break
				}
			}

			if !matched {
				errMsg = "Failed to pass annotation check to manifest " + rsc.GetName()

				return errMsg
			}
		}
	}

	return ""
}

func (ghsi *SubscriberItem) subscribeHelmCharts(indexFile *repo.IndexFile) (err error) {
	for packageName, chartVersions := range indexFile.Entries {
		klog.V(1).Infof("chart: %s\n%v", packageName, chartVersions)

		helmReleaseCR, err := utils.CreateHelmCRManifest(
			"", packageName, chartVersions, ghsi.synchronizer.GetLocalClient(), ghsi.Channel, ghsi.SecondaryChannel, ghsi.Subscription, ghsi.clusterAdmin)

		if err != nil {
			klog.Error("Failed to create a helmrelease CR manifest, err: ", err)

			return err
		}

		ghsi.resources = append(ghsi.resources, kubesynchronizer.ResourceUnit{Resource: helmReleaseCR, Gvk: helmGvk})
	}

	return err
}

func (ghsi *SubscriberItem) cloneGitRepo() (commitID string, err error) {
	annotations := ghsi.Subscription.GetAnnotations()

	cloneDepth := 1

	if annotations[appv1.AnnotationGitCloneDepth] != "" {
		cloneDepth, err = strconv.Atoi(annotations[appv1.AnnotationGitCloneDepth])

		if err != nil {
			cloneDepth = 1

			klog.Error(err, " failed to convert git-clone-depth annotation to integer")
		}
	}

	ghsi.repoRoot = utils.GetLocalGitFolder(ghsi.Subscription)

	cloneOptions := &utils.GitCloneOption{
		CommitHash:  ghsi.desiredCommit,
		RevisionTag: ghsi.desiredTag,
		CloneDepth:  cloneDepth,
		Branch:      utils.GetSubscriptionBranch(ghsi.Subscription),
		DestDir:     ghsi.repoRoot,
	}

	// Get the primary channel connection options
	primaryChannelConnectionConfig, err := getChannelConnectionConfig(ghsi.ChannelSecret, ghsi.ChannelConfigMap)

	if err != nil {
		return "", err
	}

	primaryChannelConnectionConfig.RepoURL = ghsi.Channel.Spec.Pathname
	primaryChannelConnectionConfig.InsecureSkipVerify = ghsi.Channel.Spec.InsecureSkipVerify
	cloneOptions.PrimaryConnectionOption = primaryChannelConnectionConfig

	// Get the secondary channel connection options
	if ghsi.SecondaryChannel != nil {
		// Get the secondary channel connection options
		secondaryChannelConnectionConfig, err := getChannelConnectionConfig(ghsi.SecondaryChannelSecret, ghsi.SecondaryChannelConfigMap)

		if err != nil {
			return "", err
		}

		secondaryChannelConnectionConfig.RepoURL = ghsi.SecondaryChannel.Spec.Pathname
		secondaryChannelConnectionConfig.InsecureSkipVerify = ghsi.SecondaryChannel.Spec.InsecureSkipVerify
		cloneOptions.SecondaryConnectionOption = secondaryChannelConnectionConfig
	}

	return utils.CloneGitRepo(cloneOptions)
}

func getChannelConnectionConfig(secret *corev1.Secret, configmap *corev1.ConfigMap) (connCfg *utils.ChannelConnectionCfg, err error) {
	connCfg = &utils.ChannelConnectionCfg{}

	if secret != nil {
		user, token, sshKey, passphrase, clientkey, clientcert, err := utils.ParseChannelSecret(secret)

		if err != nil {
			return nil, err
		}

		connCfg.User = user
		connCfg.Password = token
		connCfg.SSHKey = sshKey
		connCfg.Passphrase = passphrase
		connCfg.ClientKey = clientkey
		connCfg.ClientCert = clientcert
	}

	if configmap != nil {
		caCert := configmap.Data[appv1.ChannelCertificateData]

		connCfg.CaCerts = caCert
	}

	return connCfg, nil
}

func (ghsi *SubscriberItem) sortClonedGitRepo() error {
	if ghsi.Subscription.Spec.PackageFilter != nil && ghsi.Subscription.Spec.PackageFilter.FilterRef != nil {
		ghsi.SubscriberItem.SubscriptionConfigMap = &corev1.ConfigMap{}
		subcfgkey := types.NamespacedName{
			Name:      ghsi.Subscription.Spec.PackageFilter.FilterRef.Name,
			Namespace: ghsi.Subscription.Namespace,
		}

		err := ghsi.synchronizer.GetLocalClient().Get(context.TODO(), subcfgkey, ghsi.SubscriberItem.SubscriptionConfigMap)
		if err != nil {
			klog.Error("Failed to get filterRef configmap, error: ", err)
		}
	}

	resourcePath := ghsi.repoRoot

	annotations := ghsi.Subscription.GetAnnotations()

	if annotations[appv1.AnnotationGithubPath] != "" {
		resourcePath = filepath.Join(ghsi.repoRoot, annotations[appv1.AnnotationGithubPath])
	} else if annotations[appv1.AnnotationGitPath] != "" {
		resourcePath = filepath.Join(ghsi.repoRoot, annotations[appv1.AnnotationGitPath])
	} else if ghsi.SubscriberItem.SubscriptionConfigMap != nil {
		resourcePath = filepath.Join(ghsi.repoRoot, ghsi.SubscriberItem.SubscriptionConfigMap.Data["path"])
	}

	// chartDirs contains helm chart directories
	// crdsAndNamespaceFiles contains CustomResourceDefinition and Namespace Kubernetes resources file paths
	// rbacFiles contains ServiceAccount, ClusterRole and Role Kubernetes resource file paths
	// otherFiles contains all other Kubernetes resource file paths
	chartDirs, kustomizeDirs, crdsAndNamespaceFiles, rbacFiles, otherFiles, err := utils.SortResources(ghsi.repoRoot, resourcePath, utils.SkipHooksOnManaged)
	if err != nil {
		klog.Error(err, "Failed to sort kubernetes resources and helm charts.")

		return err
	}

	ghsi.chartDirs = chartDirs
	ghsi.kustomizeDirs = kustomizeDirs
	ghsi.crdsAndNamespaceFiles = crdsAndNamespaceFiles
	ghsi.rbacFiles = rbacFiles
	ghsi.otherFiles = otherFiles

	// Build a helm repo index file
	indexFile, err := utils.GenerateHelmIndexFile(ghsi.Subscription, ghsi.repoRoot, chartDirs)

	if err != nil {
		// If package name is not specified in the subscription, filterCharts throws an error. In this case, just return the original index file.
		klog.Error(err, "Failed to generate helm index file.")

		return err
	}

	ghsi.indexFile = indexFile

	b, _ := yaml.Marshal(ghsi.indexFile)
	klog.V(4).Info("New index file ", string(b))

	return nil
}
