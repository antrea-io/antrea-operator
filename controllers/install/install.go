// SPDX-License-Identifier: Apache-2.0

// Package install reads the shared AntreaInstall and merges operand conditions
// through its status subresource. The distribution owns the CRD and its schema.
package install

import (
	"context"
	"fmt"
	"reflect"

	configv1 "github.com/openshift/api/config/v1"
	"github.com/openshift/library-go/pkg/config/clusteroperator/v1helpers"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/util/retry"
	"k8s.io/utils/clock"
	"sigs.k8s.io/controller-runtime/pkg/client"

	operatorv1 "antrea.io/antrea-operator/api/v1"
	"antrea.io/antrea-operator/controllers/types"
)

// Object returns the watch / read type for the distribution's install API.
// A zero GVK selects the upstream AntreaInstall.
func Object(gvk schema.GroupVersionKind) *unstructured.Unstructured {
	if gvk.Empty() {
		gvk = operatorv1.GroupVersion.WithKind("AntreaInstall")
	}
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(gvk)
	return obj
}

func Read(ctx context.Context, reader client.Reader, gvk schema.GroupVersionKind) (*unstructured.Unstructured, error) {
	obj := Object(gvk)
	key := client.ObjectKey{Namespace: types.OperatorNameSpace, Name: types.OperatorConfigName}
	if err := reader.Get(ctx, key, obj); err != nil {
		return nil, err
	}
	return obj, nil
}

// CoreConfig projects just the shared spec fields into a detached object.
// Never write this projection back to the API.
func CoreConfig(obj *unstructured.Unstructured) (*operatorv1.AntreaInstall, error) {
	spec, found, err := unstructured.NestedMap(obj.Object, "spec")
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, fmt.Errorf("%s has no spec", obj.GetName())
	}
	core := &operatorv1.AntreaInstall{}
	core.SetName(obj.GetName())
	core.SetNamespace(obj.GetNamespace())
	core.SetGeneration(obj.GetGeneration())
	core.SetUID(obj.GetUID())
	err = runtime.DefaultUnstructuredConverter.FromUnstructured(spec, &core.Spec)
	return core, err
}

func coreCondition(t configv1.ClusterStatusConditionType) bool {
	switch t {
	case configv1.OperatorAvailable, configv1.OperatorDegraded, configv1.OperatorProgressing, configv1.OperatorUpgradeable:
		return true
	default:
		return false
	}
}

// ReportOperandCondition publishes a distribution-owned condition, such as
// InterworkingAvailable. The four OpenShift core condition types are reserved.
func ReportOperandCondition(ctx context.Context, c client.Client, gvk schema.GroupVersionKind, condition configv1.ClusterOperatorStatusCondition) error {
	if condition.Type == "" || coreCondition(condition.Type) {
		return fmt.Errorf("expected an operand-specific condition type, got %q", condition.Type)
	}
	switch condition.Status {
	case configv1.ConditionTrue, configv1.ConditionFalse, configv1.ConditionUnknown:
	default:
		return fmt.Errorf("invalid status %q for %s", condition.Status, condition.Type)
	}
	return mergeConditions(ctx, c, gvk, []configv1.ClusterOperatorStatusCondition{condition})
}

// ReportCoreConditions publishes only the four standard conditions. A
// ClusterOperator may also contain other controllers' conditions.
func ReportCoreConditions(ctx context.Context, c client.Client, gvk schema.GroupVersionKind, conditions []configv1.ClusterOperatorStatusCondition) error {
	var core []configv1.ClusterOperatorStatusCondition
	for _, condition := range conditions {
		if coreCondition(condition.Type) {
			core = append(core, condition)
		}
	}
	if len(core) == 0 {
		return nil
	}
	return mergeConditions(ctx, c, gvk, core)
}

func mergeConditions(ctx context.Context, c client.Client, gvk schema.GroupVersionKind, updates []configv1.ClusterOperatorStatusCondition) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		obj, err := Read(ctx, c, gvk)
		if err != nil {
			return err
		}
		before := obj.DeepCopy()
		conditions, _, err := unstructured.NestedSlice(obj.Object, "status", "conditions")
		if err != nil {
			return err
		}
		for _, update := range updates {
			index := -1
			var old []configv1.ClusterOperatorStatusCondition
			for i, raw := range conditions {
				fields, ok := raw.(map[string]interface{})
				if !ok {
					return fmt.Errorf("invalid install condition at index %d", i)
				}
				if fields["type"] == string(update.Type) {
					var existing configv1.ClusterOperatorStatusCondition
					if err := runtime.DefaultUnstructuredConverter.FromUnstructured(fields, &existing); err != nil {
						return err
					}
					old = append(old, existing)
					index = i
					break
				}
			}
			v1helpers.SetStatusCondition(&old, update, clock.RealClock{})
			fields, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&old[0])
			if err != nil {
				return err
			}
			if index == -1 {
				conditions = append(conditions, fields)
			} else {
				existing := conditions[index].(map[string]interface{})
				// Clear optional fields the reporter removed and leave additional
				// distribution fields on this condition intact.
				delete(existing, "reason")
				delete(existing, "message")
				for k, v := range fields {
					existing[k] = v
				}
			}
		}
		if err := unstructured.SetNestedSlice(obj.Object, conditions, "status", "conditions"); err != nil {
			return err
		}
		if reflect.DeepEqual(obj.Object, before.Object) {
			return nil
		}
		patch := client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})
		return c.Status().Patch(ctx, obj, patch)
	})
}
