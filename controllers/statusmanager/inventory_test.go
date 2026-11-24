// SPDX-License-Identifier: Apache-2.0

package statusmanager

import (
	"context"
	"reflect"
	"testing"

	configv1 "github.com/openshift/api/config/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta/testrestmapper"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"antrea.io/antrea-operator/controllers/sharedinfo"
)

func TestPruneUsesExplicitOwnershipAndLivePreconditions(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	if err := configv1.Install(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	ref := func(name string) configv1.ObjectReference {
		return configv1.ObjectReference{Resource: "configmaps", Name: name, Namespace: "kube-system"}
	}
	desired, retired, interrupted := ref("current"), ref("retired"), ref("interrupted")
	unmarked, reassigned, outside := ref("legacy"), ref("transferred"), ref("foreign")
	ns := configv1.ObjectReference{Resource: "namespaces", Name: "kube-system"}
	co := &configv1.ClusterOperator{ObjectMeta: metav1.ObjectMeta{Name: "antrea"}}
	co.Status.RelatedObjects = []configv1.ObjectReference{unmarked, retired, reassigned, outside, ns}
	objects := []client.Object{co}
	for name, operand := range map[string]string{"retired": CoreOperand, "interrupted": CoreOperand, "legacy": "", "transferred": "downstream", "foreign": CoreOperand} {
		cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "kube-system", UID: types.UID(name)}}
		if operand != "" {
			cm.Labels = map[string]string{OperandLabel: operand}
		}
		objects = append(objects, cm)
	}
	deleted := map[string]bool{}
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(co).WithObjects(objects...).WithInterceptorFuncs(interceptor.Funcs{
		Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			options := (&client.DeleteOptions{}).ApplyOptions(opts)
			if options.Preconditions == nil || options.Preconditions.UID == nil || *options.Preconditions.UID != obj.GetUID() || options.Preconditions.ResourceVersion == nil || *options.Preconditions.ResourceVersion != obj.GetResourceVersion() {
				t.Fatalf("missing live preconditions for %s", obj.GetName())
			}
			deleted[obj.GetName()] = true
			return c.Delete(ctx, obj, opts...)
		},
	}).Build()
	status, err := New(c, testrestmapper.TestOnlyStaticRESTMapper(scheme), "antrea", "antrea-operator", "test", &sharedinfo.SharedInfo{AntreaPlatform: "openshift"})
	if err != nil {
		t.Fatal(err)
	}
	status.CoreResources = map[configv1.ObjectReference]bool{desired: true, retired: true, interrupted: true, unmarked: true, reassigned: true, ns: true}
	for i := 0; i < 2; i++ {
		if err := status.SetInventory(nil, nil, []configv1.ObjectReference{desired}); err != nil {
			t.Fatal(err)
		}
	}
	if !reflect.DeepEqual(deleted, map[string]bool{"retired": true, "interrupted": true}) {
		t.Fatalf("unexpected deletes: %v", deleted)
	}
	if err := c.Get(ctx, client.ObjectKey{Name: "antrea"}, co); err != nil {
		t.Fatal(err)
	}
	want := []configv1.ObjectReference{desired, unmarked, reassigned, outside, ns}
	if !reflect.DeepEqual(co.Status.RelatedObjects, want) {
		t.Fatalf("inventory %v, want %v", co.Status.RelatedObjects, want)
	}
	for _, r := range []configv1.ObjectReference{retired, interrupted} {
		if err := c.Get(ctx, client.ObjectKey{Namespace: r.Namespace, Name: r.Name}, &corev1.ConfigMap{}); !apierrors.IsNotFound(err) {
			t.Fatalf("resource %s remained: %v", r.Name, err)
		}
	}
}
