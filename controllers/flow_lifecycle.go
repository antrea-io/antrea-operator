// SPDX-License-Identifier: Apache-2.0

package controllers

import (
	"context"
	"fmt"
	"time"

	configv1 "github.com/openshift/api/config/v1"
	"github.com/openshift/cluster-network-operator/pkg/apply"
	"github.com/openshift/cluster-network-operator/pkg/names"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	operatorv1 "antrea.io/antrea-operator/api/v1"
	"antrea.io/antrea-operator/controllers/flow"
	"antrea.io/antrea-operator/controllers/install"
	"antrea.io/antrea-operator/controllers/statusmanager"
	"antrea.io/antrea-operator/controllers/types"
)

// A custom bundle supplies the agent workload and its ConfigMap. Check the
// actual rendered inputs before applying the optional permissions and objects.
func validateFlowAgent(agent *unstructured.Unstructured, objs []*unstructured.Unstructured) error {
	account, _, err := unstructured.NestedString(agent.Object, "spec", "template", "spec", "serviceAccountName")
	if err != nil || account != "antrea-agent" {
		return fmt.Errorf("managed Flow Aggregator requires the antrea-agent ServiceAccount in %s", agent.GetNamespace())
	}
	for _, obj := range objs {
		if obj.GroupVersionKind() != corev1.SchemeGroupVersion.WithKind("ConfigMap") || obj.GetNamespace() != agent.GetNamespace() || obj.GetName() != types.AntreaConfigMapName {
			continue
		}
		config, _, err := unstructured.NestedString(obj.Object, "data", types.AntreaAgentConfigOption)
		if err != nil {
			return err
		}
		var rendered struct {
			Gates map[string]bool `json:"featureGates"`
		}
		if err := yaml.Unmarshal([]byte(config), &rendered); err != nil {
			return err
		}
		if rendered.Gates["FlowExporter"] {
			return nil
		}
	}
	return fmt.Errorf("managed Flow Aggregator requires featureGates.FlowExporter in rendered %s/%s", agent.GetNamespace(), types.AntreaConfigMapName)
}

const flowAvailable = "FlowAggregatorAvailable"

func flowEnabled(spec *operatorv1.FlowAggregatorSpec) bool { return spec != nil && spec.Enabled }

// Flow resources have reserved identities. Never adopt a customer object by
// applying the ownership marker to it. Create provides atomic exclusion on a
// new identity, while resourceVersion guards the existing-object SSA.
func applyFlowObject(ctx context.Context, c client.Client, obj *unstructured.Unstructured, manager string, applyExisting func() error) error {
	current, err := inspectFlowObject(ctx, c, obj)
	if err != nil {
		return err
	}
	if current == nil {
		return c.Create(ctx, obj.DeepCopy(), client.FieldOwner(manager))
	}
	obj.SetResourceVersion(current.GetResourceVersion())
	defer obj.SetResourceVersion("")
	return applyExisting()
}

// Check every reserved identity before enabling the agents or granting the
// aggregator access to its credentials. Recheck on each write to close races.
func (r *AntreaInstallReconciler) checkFlowOwnership(ctx context.Context, objs []*unstructured.Unstructured) error {
	for _, obj := range objs {
		_, err := inspectFlowObject(ctx, r.Client.Default().CRClient(), obj)
		if meta.IsNoMatchError(err) && obj.GroupVersionKind() == flow.DestinationGVK {
			// A newly installed CRD will be discoverable on a subsequent retry.
			continue
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func inspectFlowObject(ctx context.Context, c client.Reader, obj *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	current := new(unstructured.Unstructured)
	current.SetGroupVersionKind(obj.GroupVersionKind())
	err := c.Get(ctx, client.ObjectKeyFromObject(obj), current)
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if current.GetLabels()[statusmanager.OperandLabel] != statusmanager.CoreOperand ||
		current.GetLabels()[flow.ComponentLabel] != flow.Name || !sameController(current, obj) {
		return nil, fmt.Errorf("Flow Aggregator identity %s %s is occupied by another owner", obj.GetKind(), client.ObjectKeyFromObject(obj))
	}
	if current.GetDeletionTimestamp() != nil {
		return nil, fmt.Errorf("waiting for deletion of %s %s", obj.GetKind(), client.ObjectKeyFromObject(obj))
	}
	return current, nil
}

func sameController(current, desired *unstructured.Unstructured) bool {
	existing, requested := metav1.GetControllerOf(current), metav1.GetControllerOf(desired)
	return existing != nil && requested != nil && existing.UID == requested.UID && existing.Kind == requested.Kind && existing.APIVersion == requested.APIVersion
}

func (r *AntreaInstallReconciler) applyOperand(ctx context.Context, obj *unstructured.Unstructured) error {
	applyExisting := func() error { return apply.ApplyObject(ctx, r.Client, obj, r.subcontroller) }
	manager := "cluster-network-operator"
	if r.subcontroller != "" {
		manager += "/" + r.subcontroller
	}
	if obj.GetLabels()[flow.ComponentLabel] == flow.Name {
		return applyFlowObject(ctx, r.Client.Default().CRClient(), obj, manager, applyExisting)
	}
	if obj.GetKind() == "CustomResourceDefinition" && obj.GetName() == flow.DestinationCRD && obj.GetAnnotations()[names.CreateOnlyAnnotation] == "true" {
		// The fallback serves older standalone bundles. A newer installed
		// shared schema may serve other users. Preserve it, including on a
		// concurrent create between the read and create.
		c := r.Client.Default().CRClient()
		current := new(unstructured.Unstructured)
		current.SetGroupVersionKind(obj.GroupVersionKind())
		err := c.Get(ctx, client.ObjectKeyFromObject(obj), current)
		if !apierrors.IsNotFound(err) {
			return err
		}
		err = c.Create(ctx, obj.DeepCopy(), client.FieldOwner(manager))
		return client.IgnoreAlreadyExists(err)
	}
	return applyExisting()
}

func hadFlowCondition(raw *unstructured.Unstructured) bool {
	conditions, _, _ := unstructured.NestedSlice(raw.Object, "status", "conditions")
	for _, item := range conditions {
		if c, ok := item.(map[string]interface{}); ok && c["type"] == flowAvailable {
			return true
		}
	}
	return false
}

func flowCondition(ctx context.Context, c client.Reader, ns string, enabled bool, generation int64) (configv1.ClusterOperatorStatusCondition, error) {
	condition := configv1.ClusterOperatorStatusCondition{Type: flowAvailable, Status: configv1.ConditionFalse, Reason: "Progressing"}
	message := func(s string) { condition.Message = fmt.Sprintf("Install generation %d: %s", generation, s) }
	ds := &appsv1.DaemonSet{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: types.AntreaAgentDaemonSetName}, ds); err != nil {
		if !apierrors.IsNotFound(err) {
			return condition, err
		}
	}
	if ds.Status.ObservedGeneration < ds.Generation || ds.Status.DesiredNumberScheduled == 0 || ds.Status.UpdatedNumberScheduled != ds.Status.DesiredNumberScheduled || ds.Status.NumberAvailable != ds.Status.DesiredNumberScheduled || ds.Status.NumberMisscheduled != 0 {
		message("waiting for the agent rollout")
		return condition, nil
	}
	if enabled {
		dep := &appsv1.Deployment{}
		err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: flow.Name}, dep)
		if err != nil && !apierrors.IsNotFound(err) {
			return condition, err
		}
		if dep.GetDeletionTimestamp() != nil || dep.Status.ObservedGeneration < dep.Generation || dep.Status.UpdatedReplicas != 1 || dep.Status.Replicas != 1 || dep.Status.AvailableReplicas != 1 {
			message("waiting for the ready Flow Aggregator replica")
		} else {
			condition.Status, condition.Reason = configv1.ConditionTrue, "Available"
			message("the agents and aggregator are available")
		}
		return condition, nil
	}
	// Background deletion may leave terminating Pods after the Deployment
	// disappears. Retire the flow condition only after the listener stops.
	pods := &corev1.PodList{}
	if err := c.List(ctx, pods, client.InNamespace(ns), client.MatchingLabels{flow.ComponentLabel: flow.Name, statusmanager.OperandLabel: statusmanager.CoreOperand}); err != nil {
		return condition, err
	}
	if len(pods.Items) != 0 {
		message("waiting for Flow Aggregator Pods to terminate")
	} else {
		condition.Reason = "Disabled"
		message("the managed destination has been removed and agents have converged")
	}
	return condition, nil
}

func (r *AntreaInstallReconciler) reportFlow(ctx context.Context, spec *operatorv1.AntreaInstall, raw *unstructured.Unstructured) (time.Duration, error) {
	if spec.Spec.FlowAggregator == nil && !hadFlowCondition(raw) {
		return 0, nil
	}
	c := r.Client.Default().CRClient()
	enabled := flowEnabled(spec.Spec.FlowAggregator)
	condition, err := flowCondition(ctx, c, r.coreNamespace(), enabled, spec.Generation)
	if err != nil {
		return 0, err
	}
	if condition.Reason == "Disabled" {
		pending, err := r.Status.FlowCleanupPending(ctx)
		if err != nil {
			return 0, err
		}
		if pending {
			condition.Reason = "Progressing"
			condition.Message = fmt.Sprintf("Install generation %d: waiting for retired Flow Aggregator resources to terminate", spec.Generation)
		}
	}
	if err := install.ReportOperandCondition(ctx, c, r.installGVK, condition); err != nil {
		return 0, err
	}
	if condition.Reason == "Progressing" {
		return 5 * time.Second, nil
	}
	if enabled {
		return ResyncPeriod, nil
	}
	return 0, nil
}
