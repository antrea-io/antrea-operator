/* Copyright © 2020 VMware, Inc. All Rights Reserved.
   SPDX-License-Identifier: Apache-2.0 */

package statusmanager

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"

	configv1 "github.com/openshift/api/config/v1"
	"github.com/openshift/library-go/pkg/config/clusteroperator/v1helpers"
	"gopkg.in/yaml.v2"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	uns "k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"k8s.io/utils/clock"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"antrea.io/antrea-operator/controllers/flow"
	"antrea.io/antrea-operator/controllers/install"
	"antrea.io/antrea-operator/controllers/sharedinfo"
	operatortypes "antrea.io/antrea-operator/controllers/types"
)

var log = logf.Log.WithName("status_manager")

type StatusLevel int

const (
	ClusterConfig StatusLevel = iota
	OperatorConfig
	PodDeployment
	RolloutHung
	ClusterNode
	maxStatusLevel
)

type Adaptor interface {
	getLastPodState(status *StatusManager) (map[types.NamespacedName]daemonsetState, map[types.NamespacedName]deploymentState)
	setLastPodState(status *StatusManager, dss map[types.NamespacedName]daemonsetState, deps map[types.NamespacedName]deploymentState) error
	set(status *StatusManager, reachedAvailableLevel bool, conditions ...configv1.ClusterOperatorStatusCondition)
}

// Status coordinates changes to AntreaInstall.status and ClusterOperator.Status.
type StatusManager struct {
	sync.Mutex

	client client.Client
	mapper meta.RESTMapper

	name    string
	version string

	failing [maxStatusLevel]*configv1.ClusterOperatorStatusCondition

	daemonSets  []types.NamespacedName
	deployments []types.NamespacedName
	InstallGVK  schema.GroupVersionKind
	// CoreResources is the immutable set of core object identities declared by
	// a distribution. A nil map selects the standalone manifest.
	CoreResources map[configv1.ObjectReference]bool
	// FlowResources also records inactive optional identities for standalone cleanup.
	FlowResources map[configv1.ObjectReference]bool

	OperatorNamespace string
	AdaptorName       string
	Adaptor
}

type StatusK8s struct{}

type StatusOc struct{}

func New(client client.Client, mapper meta.RESTMapper, name, operatorNamespace, version string, sharedInfo *sharedinfo.SharedInfo) (*StatusManager, error) {
	status := StatusManager{
		client:            client,
		mapper:            mapper,
		name:              name,
		version:           version,
		OperatorNamespace: operatorNamespace,
		AdaptorName:       sharedInfo.AntreaPlatform,
	}
	switch sharedInfo.AntreaPlatform {
	case "openshift":
		status.Adaptor = &StatusOc{}
	case "kubernetes":
		status.Adaptor = &StatusK8s{}
	default:
		return nil, errors.New("invalid platform: platform should be openshift or kubernetes")
	}
	return &status, nil
}

const (
	OperandLabel = "operator.antrea.io/operand"
	CoreOperand  = "core"
)

// syncRelatedObjects publishes the successfully applied inventory. Keep other
// controllers' references and unmarked legacy objects until explicit handover.
func (status *StatusManager) syncRelatedObjects(ctx context.Context, desired []configv1.ObjectReference) error {
	if status.AdaptorName != "openshift" || desired == nil {
		return nil
	}
	seen := make(map[configv1.ObjectReference]bool, len(desired))
	for _, ref := range desired {
		seen[ref] = true
	}
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		co := &configv1.ClusterOperator{ObjectMeta: metav1.ObjectMeta{Name: status.name}}
		err := status.client.Get(ctx, client.ObjectKeyFromObject(co), co)
		if k8serrors.IsNotFound(err) {
			if err := status.client.Create(ctx, co); err != nil {
				return err
			}
		} else if err != nil {
			return err
		}
		next := append([]configv1.ObjectReference(nil), desired...)
		for _, ref := range co.Status.RelatedObjects {
			if seen[ref] {
				continue
			}
			removed, err := status.prune(ctx, ref)
			if err != nil {
				return err
			}
			if !removed {
				next = append(next, ref)
			}
		}
		// A previous apply can be interrupted before inventory publication.
		// Retire marked objects from that attempt even if the install changed
		// while the manager was down. Only recorded references are reported.
		tracked := make(map[configv1.ObjectReference]bool, len(co.Status.RelatedObjects))
		for _, ref := range co.Status.RelatedObjects {
			tracked[ref] = true
		}
		inactive := status.CoreResources
		if inactive == nil {
			inactive = status.FlowResources
		}
		for ref := range inactive {
			if !seen[ref] && !tracked[ref] {
				if _, err := status.prune(ctx, ref); err != nil {
					return err
				}
			}
		}
		if reflect.DeepEqual(co.Status.RelatedObjects, next) {
			return nil
		}
		co.Status.RelatedObjects = next
		return status.client.Status().Update(ctx, co)
	})
}

// FlowCleanupPending observes asynchronous deletes, including resources held
// by finalizers. A same-name replacement outside flow ownership is preserved.
func (status *StatusManager) FlowCleanupPending(ctx context.Context) (bool, error) {
	for ref := range status.FlowResources {
		gvk, err := status.mapper.KindFor(schema.GroupVersionResource{Group: ref.Group, Resource: ref.Resource})
		if meta.IsNoMatchError(err) {
			continue
		}
		if err != nil {
			return false, err
		}
		obj := new(uns.Unstructured)
		obj.SetGroupVersionKind(gvk)
		err = status.client.Get(ctx, client.ObjectKey{Namespace: ref.Namespace, Name: ref.Name}, obj)
		if k8serrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return false, err
		}
		owned, err := status.ownsLiveObject(ctx, ref, obj)
		if owned || err != nil {
			return owned, err
		}
	}
	return false, nil
}

func (status *StatusManager) prune(ctx context.Context, ref configv1.ObjectReference) (bool, error) {
	if (status.CoreResources != nil && !status.CoreResources[ref]) || (ref.Group == "" && ref.Resource == "namespaces") {
		return false, nil
	}
	gvk, err := status.mapper.KindFor(schema.GroupVersionResource{Group: ref.Group, Resource: ref.Resource})
	if err != nil {
		// An API may be absent or undiscoverable during an upgrade. Keep its
		// reference available for handover.
		return false, nil
	}
	obj := &uns.Unstructured{}
	obj.SetGroupVersionKind(gvk)
	err = status.client.Get(ctx, client.ObjectKey{Namespace: ref.Namespace, Name: ref.Name}, obj)
	if k8serrors.IsNotFound(err) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	owned, err := status.ownsLiveObject(ctx, ref, obj)
	if !owned || err != nil {
		return false, err
	}
	// A replacement, relabel, or concurrent handover invalidates the delete.
	uid, rv := obj.GetUID(), obj.GetResourceVersion()
	err = status.client.Delete(ctx, obj, client.PropagationPolicy("Background"), client.Preconditions{UID: &uid, ResourceVersion: &rv})
	return err == nil || k8serrors.IsNotFound(err), client.IgnoreNotFound(err)
}

func (status *StatusManager) ownsLiveObject(ctx context.Context, ref configv1.ObjectReference, obj *uns.Unstructured) (bool, error) {
	if obj.GetLabels()[OperandLabel] != CoreOperand {
		return false, nil
	}
	isFlow := obj.GetLabels()[flow.ComponentLabel] == flow.Name
	// Flow markers from a previous namespace or a retained inventory entry
	// do not grant cleanup authority in the current namespace.
	if status.FlowResources[ref] != isFlow {
		return false, nil
	}
	if !isFlow {
		return true, nil
	}
	owner := metav1.GetControllerOf(obj)
	if owner == nil || owner.Kind != "Network" || owner.APIVersion != configv1.GroupVersion.String() || owner.Name != operatortypes.ClusterConfigName {
		return false, nil
	}
	network := new(configv1.Network)
	if err := status.client.Get(ctx, client.ObjectKey{Name: owner.Name}, network); err != nil {
		return false, client.IgnoreNotFound(err)
	}
	return owner.UID == network.UID, nil
}

func (status *StatusManager) setConditions(progressing []string, reachedAvailableLevel bool) {
	conditions := make([]configv1.ClusterOperatorStatusCondition, 0, 2)
	if len(progressing) > 0 {
		conditions = append(conditions,
			configv1.ClusterOperatorStatusCondition{
				Type:    configv1.OperatorProgressing,
				Status:  configv1.ConditionTrue,
				Reason:  "Deploying",
				Message: strings.Join(progressing, "\n"),
			},
		)
	} else {
		conditions = append(conditions,
			configv1.ClusterOperatorStatusCondition{
				Type:   configv1.OperatorProgressing,
				Status: configv1.ConditionFalse,
			},
		)
	}
	if reachedAvailableLevel {
		conditions = append(conditions,
			configv1.ClusterOperatorStatusCondition{
				Type:   configv1.OperatorAvailable,
				Status: configv1.ConditionTrue,
			},
		)
	}
	status.set(status, reachedAvailableLevel, conditions...)
}

func (status *StatusManager) setClusterOperatorConditions(co *configv1.ClusterOperator, reachedAvailableLevel bool, conditions *[]configv1.ClusterOperatorStatusCondition) {
	if reachedAvailableLevel {
		found := false
		for i := range co.Status.Versions {
			if co.Status.Versions[i].Name == "operator" {
				co.Status.Versions[i].Version = status.version
				found = true
				break
			}
		}
		if !found {
			co.Status.Versions = append(co.Status.Versions, configv1.OperandVersion{Name: "operator", Version: status.version})
		}
	}
	status.CombineConditions(&co.Status.Conditions, conditions)
	progressingCondition := v1helpers.FindStatusCondition(co.Status.Conditions, configv1.OperatorProgressing)
	availableCondition := v1helpers.FindStatusCondition(co.Status.Conditions, configv1.OperatorAvailable)
	if availableCondition == nil && progressingCondition != nil && progressingCondition.Status == configv1.ConditionTrue {
		v1helpers.SetStatusCondition(&co.Status.Conditions,
			configv1.ClusterOperatorStatusCondition{
				Type:    configv1.OperatorAvailable,
				Status:  configv1.ConditionFalse,
				Reason:  "Startup",
				Message: "The network is starting up",
			}, clock.RealClock{},
		)
	}
	v1helpers.SetStatusCondition(&co.Status.Conditions,
		configv1.ClusterOperatorStatusCondition{
			Type:   configv1.OperatorUpgradeable,
			Status: configv1.ConditionTrue,
		}, clock.RealClock{},
	)
}

// Set updates the AntreaInstall.Status with the provided conditions for platform kubernetes.
func (adaptor *StatusK8s) set(status *StatusManager, reachedAvailableLevel bool, conditions ...configv1.ClusterOperatorStatusCondition) {
	co := &configv1.ClusterOperator{}
	obj, err := install.Read(context.TODO(), status.client, status.InstallGVK)
	if err != nil {
		if !k8serrors.IsNotFound(err) {
			log.Error(err, "Failed to read AntreaInstall")
		}
		return
	}
	// Seed Available / Progressing so startup and transition logic sees the
	// preceding state, even when this call updates only Degraded.
	raw, _, err := uns.NestedSlice(obj.Object, "status", "conditions")
	if err != nil {
		log.Error(err, "Failed to read install conditions")
		return
	}
	for _, item := range raw {
		fields, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		var c configv1.ClusterOperatorStatusCondition
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(fields, &c); err == nil {
			co.Status.Conditions = append(co.Status.Conditions, c)
		}
	}
	status.setClusterOperatorConditions(co, reachedAvailableLevel, &conditions)
	if err := status.setAntreaInstallStatus(co.Status.Conditions); err != nil {
		log.Error(err, "Failed to set AntreaInstall")
	}
}

// Set updates the ClusterOperator.Status with the provided conditions for platform openshift.
func (adaptor *StatusOc) set(status *StatusManager, reachedAvailableLevel bool, conditions ...configv1.ClusterOperatorStatusCondition) {
	err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		co := &configv1.ClusterOperator{ObjectMeta: metav1.ObjectMeta{Name: status.name}}
		err := status.client.Get(context.TODO(), types.NamespacedName{Name: status.name}, co)
		if k8serrors.IsNotFound(err) {
			if err := status.client.Create(context.TODO(), co); err != nil {
				return err
			}
		} else if err != nil {
			return err
		}
		oldStatus := co.Status.DeepCopy()
		status.setClusterOperatorConditions(co, reachedAvailableLevel, &conditions)
		if reflect.DeepEqual(*oldStatus, co.Status) {
			return status.setAntreaInstallStatus(co.Status.Conditions)
		}
		buf, err := yaml.Marshal(co.Status.Conditions)
		if err != nil {
			buf = []byte(fmt.Sprintf("(failed to convert to YAML: %s)", err))
		}
		if err := status.client.Status().Update(context.TODO(), co); err != nil {
			return err
		}
		log.Info(fmt.Sprintf("Updated ClusterOperator with conditions:\n%s", string(buf)))
		return status.setAntreaInstallStatus(co.Status.Conditions)
	})
	if err != nil {
		log.Error(err, "Failed to set ClusterOperator")
	}
}

func (status *StatusManager) setAntreaInstallStatus(conditions []configv1.ClusterOperatorStatusCondition) error {
	return client.IgnoreNotFound(install.ReportCoreConditions(context.TODO(), status.client, status.InstallGVK, conditions))
}

func (status *StatusManager) CombineConditions(conditions *[]configv1.ClusterOperatorStatusCondition,
	newConditions *[]configv1.ClusterOperatorStatusCondition) (bool, string) {
	messages := ""
	changed := false
	for _, newCondition := range *newConditions {
		existingCondition := v1helpers.FindStatusCondition(*conditions, newCondition.Type)
		if existingCondition == nil {
			v1helpers.SetStatusCondition(conditions, newCondition, clock.RealClock{})
			messages += fmt.Sprintf("%v. ", newCondition)
			changed = true
		} else if existingCondition.Status != newCondition.Status ||
			existingCondition.Reason != newCondition.Reason ||
			existingCondition.Message != newCondition.Message {
			v1helpers.SetStatusCondition(conditions, newCondition, clock.RealClock{})
			messages += fmt.Sprintf("%v. ", newCondition)
			changed = true
		}
	}
	return changed, messages
}

func (status *StatusManager) syncDegraded() {
	for _, c := range status.failing {
		if c != nil {
			status.set(status, false, *c)
			return
		}
	}
	status.set(
		status,
		false,
		configv1.ClusterOperatorStatusCondition{
			Type:   configv1.OperatorDegraded,
			Status: configv1.ConditionFalse,
		},
	)
}

func (status *StatusManager) setDegraded(statusLevel StatusLevel, reason, message string) {
	status.failing[statusLevel] = &configv1.ClusterOperatorStatusCondition{
		Type:    configv1.OperatorDegraded,
		Status:  configv1.ConditionTrue,
		Reason:  reason,
		Message: message,
	}
	status.syncDegraded()
}

func (status *StatusManager) setNotDegraded(statusLevel StatusLevel) {
	if status.failing[statusLevel] != nil {
		status.failing[statusLevel] = nil
	}
	status.syncDegraded()
}

func (status *StatusManager) SetDegraded(statusLevel StatusLevel, reason, message string) {
	status.Lock()
	defer status.Unlock()
	status.setDegraded(statusLevel, reason, message)
}

func (status *StatusManager) SetNotDegraded(statusLevel StatusLevel) {
	status.Lock()
	defer status.Unlock()
	status.setNotDegraded(statusLevel)
}

func (status *StatusManager) SetDaemonSets(daemonSets []types.NamespacedName) {
	status.Lock()
	defer status.Unlock()
	status.daemonSets = daemonSets
}

func (status *StatusManager) SetDeployments(deployments []types.NamespacedName) {
	status.Lock()
	defer status.Unlock()
	status.deployments = deployments
}

func (status *StatusManager) SetRelatedObjects(relatedObjects []configv1.ObjectReference) {
	status.Lock()
	defer status.Unlock()
	if err := status.syncRelatedObjects(context.TODO(), relatedObjects); err != nil {
		log.Error(err, "Failed to sync core resource inventory")
	}
}

// SetInventory publishes all three views of a successfully applied bundle.
func (status *StatusManager) SetInventory(ds, deps []types.NamespacedName, related []configv1.ObjectReference) error {
	status.Lock()
	defer status.Unlock()
	if err := status.syncRelatedObjects(context.TODO(), related); err != nil {
		return err
	}
	status.daemonSets = ds
	status.deployments = deps
	return nil
}
