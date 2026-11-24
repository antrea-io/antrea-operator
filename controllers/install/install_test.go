// SPDX-License-Identifier: Apache-2.0

package install_test

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"testing"

	configv1 "github.com/openshift/api/config/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"antrea.io/antrea-operator/controllers/install"
)

func TestSharedInstallStatusConflict(t *testing.T) {
	ctx := context.Background()
	// The historical commercial CRD uses the upstream GVK with extra fields.
	raw := install.Object(schema.GroupVersionKind{})
	raw.SetName("antrea-install")
	raw.SetNamespace("antrea-operator")
	spec := map[string]interface{}{"antreaPlatform": "openshift", "enableInterworking": true, "psk": "test-value"}
	raw.Object["spec"] = spec
	foreign := map[string]interface{}{"type": "RegistrationReady", "status": "True", "detail": map[string]interface{}{"observed": int64(2)}}
	raw.Object["status"] = map[string]interface{}{"futureField": "retained", "conditions": []interface{}{foreign}}
	gvk := raw.GroupVersionKind()
	store := fake.NewClientBuilder().WithScheme(runtime.NewScheme()).WithObjects(raw).WithStatusSubresource(raw).Build()
	attempts := 0
	// Commit another operand's report between the core read and its status
	// patch. The API must reject the stale patch and force a fresh merge.
	racing := interceptor.NewClient(store, interceptor.Funcs{
		SubResourcePatch: func(ctx context.Context, c client.Client, sub string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
			attempts++
			if attempts == 1 {
				err := install.ReportOperandCondition(ctx, c, gvk, configv1.ClusterOperatorStatusCondition{Type: "InterworkingAvailable", Status: configv1.ConditionTrue})
				if err != nil {
					return err
				}
			}
			// The fake's status merge-patch path discards metadata changes.
			// Enforce the API server's wire-level version precondition here.
			var body struct {
				Metadata struct {
					ResourceVersion string `json:"resourceVersion"`
				} `json:"metadata"`
			}
			data, err := patch.Data(obj)
			if err != nil {
				return err
			}
			if err := json.Unmarshal(data, &body); err != nil {
				return err
			}
			if body.Metadata.ResourceVersion == "" {
				t.Fatal("status patch has no resourceVersion")
			}
			live, err := install.Read(ctx, c, gvk)
			if err != nil {
				return err
			}
			if body.Metadata.ResourceVersion != live.GetResourceVersion() {
				return apierrors.NewConflict(gvk.GroupVersion().WithResource("antreainstalls").GroupResource(), obj.GetName(), fmt.Errorf("stale status patch"))
			}
			return c.SubResource(sub).Patch(ctx, obj, patch, opts...)
		},
	})
	ready := configv1.ClusterOperatorStatusCondition{Type: configv1.OperatorAvailable, Status: configv1.ConditionTrue}
	if err := install.ReportCoreConditions(ctx, racing, gvk, []configv1.ClusterOperatorStatusCondition{ready}); err != nil {
		t.Fatal(err)
	}
	if attempts != 2 {
		t.Fatalf("expected conflict and retry, got %d patches", attempts)
	}
	if err := install.ReportCoreConditions(ctx, racing, gvk, []configv1.ClusterOperatorStatusCondition{ready}); err != nil {
		t.Fatal(err)
	}
	if attempts != 2 {
		t.Fatal("unchanged health rewrote status")
	}
	if err := install.ReportOperandCondition(ctx, racing, gvk, ready); err == nil {
		t.Fatal("operand could write core Available")
	}
	after, err := install.Read(ctx, store, gvk)
	if err != nil {
		t.Fatal(err)
	}
	projected, err := install.CoreConfig(after)
	if err != nil || projected.Spec.AntreaPlatform != "openshift" {
		t.Fatalf("core projection: %v %v", projected, err)
	}
	conditions, _, err := unstructured.NestedSlice(after.Object, "status", "conditions")
	if err != nil || len(conditions) != 3 || !reflect.DeepEqual(conditions[0], foreign) {
		t.Fatalf("lost conditions: %v %v", conditions, err)
	}
	if conditions[1].(map[string]interface{})["type"] != "InterworkingAvailable" || conditions[2].(map[string]interface{})["type"] != "Available" {
		t.Fatalf("wrong condition ownership: %v", conditions)
	}
	value, _, err := unstructured.NestedString(after.Object, "status", "futureField")
	if err != nil || value != "retained" || !reflect.DeepEqual(after.Object["spec"], spec) {
		t.Fatal("status update changed commercial fields")
	}
}

func TestObservedConditionRejectsSupersededInstall(t *testing.T) {
	ctx := context.Background()
	raw := install.Object(schema.GroupVersionKind{})
	raw.SetName("antrea-install")
	raw.SetNamespace("antrea-operator")
	raw.SetUID("current")
	raw.SetGeneration(3)
	store := fake.NewClientBuilder().WithObjects(raw).WithStatusSubresource(raw).Build()
	report := configv1.ClusterOperatorStatusCondition{Type: "FlowAggregatorAvailable", Status: configv1.ConditionTrue}
	for _, key := range []string{"generation", "uid"} {
		old := raw.DeepCopy()
		if key == "generation" {
			old.SetGeneration(2)
		} else {
			old.SetUID("retired")
		}
		if err := install.ReportObservedOperandCondition(ctx, store, old, report); err != nil {
			t.Fatal(err)
		}
	}
	after, err := install.Read(ctx, store, raw.GroupVersionKind())
	if err != nil || after.Object["status"] != nil {
		t.Fatalf("stale report updated install: %v %v", after, err)
	}
	if err := install.ReportObservedOperandCondition(ctx, store, raw, report); err != nil {
		t.Fatal(err)
	}
	after, err = install.Read(ctx, store, raw.GroupVersionKind())
	if err != nil || after.Object["status"] == nil {
		t.Fatal("current condition missing", err)
	}
}
