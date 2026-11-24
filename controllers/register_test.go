// SPDX-License-Identifier: Apache-2.0

package controllers_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"

	configv1 "github.com/openshift/api/config/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	crconfig "sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"antrea.io/antrea-operator/controllers"
	"antrea.io/antrea-operator/controllers/install"
)

type consumerManager struct {
	manager.Manager
	store       client.Client
	controllers []reconcile.Reconciler
}

func (m *consumerManager) GetAPIReader() client.Reader { return m.store }
func (m *consumerManager) GetClient() client.Client    { return m.store }
func (m *consumerManager) Add(r manager.Runnable) error {
	controller, ok := r.(reconcile.Reconciler)
	if !ok {
		return fmt.Errorf("unexpected runnable %T", r)
	}
	m.controllers = append(m.controllers, controller)
	return nil
}

// Exercise a distribution entry point with its own GVK, per-agent image, and
// controller in the same manager. Discovery and SSA use real HTTP clients.
func TestDistributionRegistersAndReconcilesCore(t *testing.T) {
	ctx := context.Background()
	gvk := schema.GroupVersionKind{Group: "distribution.example.org", Version: "v1", Kind: "AntreaInstall"}
	raw := install.Object(gvk)
	raw.SetNamespace("antrea-operator")
	raw.SetName("antrea-install")
	raw.SetUID("shared-install")
	raw.SetGeneration(1)
	raw.Object["spec"] = map[string]interface{}{
		"antreaPlatform": "kubernetes", "antreaAgentConfig": "serviceCIDR: 10.96.0.0/12",
		"antreaAgentImage": "agent:one", "enableInterworking": true, "mode": "a",
	}
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{appsv1.AddToScheme, corev1.AddToScheme, configv1.Install} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	store := fake.NewClientBuilder().WithScheme(scheme).WithObjects(raw).WithStatusSubresource(raw).Build()
	gv := gvk.GroupVersion()
	mapper := meta.NewDefaultRESTMapper([]schema.GroupVersion{gv, appsv1.SchemeGroupVersion})
	mapper.Add(gvk, meta.RESTScopeNamespace)
	mapper.Add(corev1.SchemeGroupVersion.WithKind("ConfigMap"), meta.RESTScopeNamespace)
	for _, kind := range []string{"DaemonSet", "Deployment"} {
		mapper.Add(appsv1.SchemeGroupVersion.WithKind(kind), meta.RESTScopeNamespace)
	}
	var mu sync.Mutex
	var applied []unstructured.Unstructured
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var result interface{}
		switch r.URL.Path {
		case "/api":
			result = metav1.APIVersions{Versions: []string{"v1"}}
		case "/apis":
			groups := metav1.APIGroupList{}
			for _, group := range []schema.GroupVersion{gv, appsv1.SchemeGroupVersion} {
				v := metav1.GroupVersionForDiscovery{GroupVersion: group.String(), Version: group.Version}
				groups.Groups = append(groups.Groups, metav1.APIGroup{Name: group.Group, PreferredVersion: v, Versions: []metav1.GroupVersionForDiscovery{v}})
			}
			result = groups
		case "/api/v1":
			result = metav1.APIResourceList{GroupVersion: "v1", APIResources: []metav1.APIResource{{Name: "configmaps", Kind: "ConfigMap", Namespaced: true}}}
		case "/apis/" + gv.String():
			result = metav1.APIResourceList{GroupVersion: gv.String(), APIResources: []metav1.APIResource{{Name: "antreainstalls", Kind: "AntreaInstall", Namespaced: true}}}
		case "/apis/apps/v1":
			result = metav1.APIResourceList{GroupVersion: "apps/v1", APIResources: []metav1.APIResource{{Name: "daemonsets", Kind: "DaemonSet", Namespaced: true}, {Name: "deployments", Kind: "Deployment", Namespaced: true}}}
		case "/apis/" + gv.String() + "/namespaces/antrea-operator/antreainstalls/antrea-install":
			obj, err := install.Read(ctx, store, gvk)
			if err != nil {
				t.Error(err)
				w.WriteHeader(500)
				return
			}
			result = obj
		case "/api/v1/namespaces/kube-system/configmaps/antrea-config", "/apis/apps/v1/namespaces/kube-system/daemonsets/antrea-agent", "/apis/apps/v1/namespaces/kube-system/deployments/antrea-controller":
			if r.Method == http.MethodGet && r.URL.Path == "/apis/apps/v1/namespaces/kube-system/daemonsets/antrea-agent" {
				w.WriteHeader(http.StatusNotFound)
				result = metav1.Status{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Status"}, Status: "Failure", Reason: metav1.StatusReasonNotFound, Code: 404}
				break
			}
			if r.Method != http.MethodPatch {
				t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
				w.WriteHeader(405)
				return
			}
			if got := r.URL.Query().Get("fieldManager"); got != "cluster-network-operator/antrea-core" {
				t.Errorf("unexpected SSA manager %q", got)
			}
			var obj unstructured.Unstructured
			if err := json.NewDecoder(r.Body).Decode(&obj); err != nil {
				t.Error(err)
				w.WriteHeader(400)
				return
			}
			mu.Lock()
			applied = append(applied, obj)
			mu.Unlock()
			result = &obj
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(404)
			return
		}
		if err := json.NewEncoder(w).Encode(result); err != nil {
			t.Error(err)
		}
	}))
	defer server.Close()
	base, err := ctrl.NewManager(&rest.Config{Host: server.URL, QPS: 100, Burst: 100}, ctrl.Options{
		Controller: crconfig.Controller{SkipNameValidation: ptr.To(true)},
		Scheme:     scheme, Metrics: metricsserver.Options{BindAddress: "0"},
		MapperProvider: func(*rest.Config, *http.Client) (meta.RESTMapper, error) { return mapper, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	mgr := &consumerManager{Manager: base, store: store}
	dir := t.TempDir()
	owned := []configv1.ObjectReference{{Resource: "configmaps", Namespace: "kube-system", Name: "antrea-config"}}
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(`apiVersion: v1
kind: ConfigMap
metadata:
  namespace: kube-system
  name: antrea-config
  annotations:
    release: "{{.ReleaseVersion}}"
data:
  mode: "{{.Mode}}"
`), 0600); err != nil {
		t.Fatal(err)
	}
	for _, workload := range []struct{ kind, resource, name, image string }{
		{"DaemonSet", "daemonsets", "antrea-agent", "{{.AntreaAgentImage}}"}, {"Deployment", "deployments", "antrea-controller", "{{.AntreaImage}}"},
	} {
		key := configv1.ObjectReference{Group: "apps", Resource: workload.resource, Namespace: "kube-system", Name: workload.name}
		owned = append(owned, key)
		manifest := fmt.Sprintf(`apiVersion: apps/v1
kind: %s
metadata:
  name: %s
  namespace: kube-system
  annotations:
    release: "{{.ReleaseVersion}}"
spec:
  selector:
    matchLabels:
      component: %[2]s
  template:
    metadata:
      labels:
        component: %[2]s
    spec:
      containers:
      - name: %[2]s
        image: "%[3]s"
`, workload.kind, workload.name, workload.image)
		if err := os.WriteFile(filepath.Join(dir, workload.name+".yaml"), []byte(manifest), 0600); err != nil {
			t.Fatal(err)
		}
	}
	opts := controllers.CoreOptions{InstallGVK: gvk, Version: "distribution-1", Bundle: &controllers.ResourceBundle{
		ManifestDir: dir, OwnedResources: owned,
		Configure: func(obj *unstructured.Unstructured, values map[string]interface{}) error {
			image, found, err := unstructured.NestedString(obj.Object, "spec", "antreaAgentImage")
			if err != nil || !found {
				return fmt.Errorf("expected spec.antreaAgentImage")
			}
			values["AntreaAgentImage"] = image
			mode, _, err := unstructured.NestedString(obj.Object, "spec", "mode")
			if err != nil {
				return err
			}
			values["Mode"] = mode
			return nil
		},
	}}
	if err := controllers.RegisterCore(ctx, mgr, opts); err != nil {
		t.Fatal(err)
	}
	if err := ctrl.NewControllerManagedBy(mgr).Named("distribution-interworking").For(install.Object(gvk)).Complete(reconcile.Func(func(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
		return reconcile.Result{}, install.ReportOperandCondition(ctx, mgr.GetClient(), gvk, configv1.ClusterOperatorStatusCondition{Type: "InterworkingAvailable", Status: configv1.ConditionTrue})
	})); err != nil {
		t.Fatal(err)
	}
	if len(mgr.controllers) != 3 {
		t.Fatalf("registered %d controllers", len(mgr.controllers))
	}
	request := reconcile.Request{NamespacedName: client.ObjectKeyFromObject(raw)}
	run := func() {
		t.Helper()
		if _, err := mgr.controllers[0].Reconcile(ctx, request); err != nil {
			t.Fatal(err)
		}
	}
	run()
	if _, err := mgr.controllers[2].Reconcile(ctx, request); err != nil {
		t.Fatal(err)
	}
	run() // A status update leaves generation unchanged.
	updated, err := install.Read(ctx, store, gvk)
	if err != nil {
		t.Fatal(err)
	}
	if err := unstructured.SetNestedField(updated.Object, "agent:two", "spec", "antreaAgentImage"); err != nil {
		t.Fatal(err)
	}
	updated.SetGeneration(2)
	if err := store.Update(ctx, updated); err != nil {
		t.Fatal(err)
	}
	run()
	updated, err = install.Read(ctx, store, gvk)
	if err != nil {
		t.Fatal(err)
	}
	if err := unstructured.SetNestedField(updated.Object, "b", "spec", "mode"); err != nil {
		t.Fatal(err)
	}
	updated.SetGeneration(3)
	if err := store.Update(ctx, updated); err != nil {
		t.Fatal(err)
	}
	run()
	// Even if a generator emits an extra object, reject the entire bundle
	// before its first apply and preserve the prior successful inventory.
	if err := os.WriteFile(filepath.Join(dir, "unexpected.yaml"), []byte("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  namespace: kube-system\n  name: unrelated\n"), 0600); err != nil {
		t.Fatal(err)
	}
	updated, err = install.Read(ctx, store, gvk)
	if err != nil {
		t.Fatal(err)
	}
	updated.SetGeneration(4)
	if err := store.Update(ctx, updated); err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.controllers[0].Reconcile(ctx, request); err == nil {
		t.Fatal("accepted unowned bundle resource")
	}
	// Workload recovery still uses the last complete bundle and its SSA owner.
	podRequest := reconcile.Request{NamespacedName: client.ObjectKey{Namespace: "kube-system", Name: "antrea-agent"}}
	if _, err := mgr.controllers[1].Reconcile(ctx, podRequest); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(applied) != 10 {
		t.Fatalf("got %d applies for three install generations", len(applied))
	}
	revisions := map[string][]string{}
	if !reflect.DeepEqual(applied[6].Object, applied[9].Object) {
		t.Fatal("recreation did not use the last successfully applied agent spec")
	}
	for i, obj := range applied[:9] {
		if obj.GetKind() != "ConfigMap" {
			revision, found, err := unstructured.NestedString(obj.Object, "spec", "template", "metadata", "annotations", "operator.antrea.io/core-config-hash")
			if err != nil || !found {
				t.Fatalf("missing revision: %v", err)
			}
			revisions[obj.GetName()] = append(revisions[obj.GetName()], revision)
		}
		if obj.GetLabels()["operator.antrea.io/operand"] != "core" || obj.GetAnnotations()["release"] != "distribution-1" {
			t.Fatalf("incorrect metadata: %v", obj.Object["metadata"])
		}
		if obj.GetName() == "antrea-agent" {
			containers, _, err := unstructured.NestedSlice(obj.Object, "spec", "template", "spec", "containers")
			if err != nil || len(containers) != 1 {
				t.Fatalf("unexpected containers: %v, %v", containers, err)
			}
			want := []string{"agent:one", "agent:two", "agent:two"}[i/3]
			if got := containers[0].(map[string]interface{})["image"]; got != want {
				t.Fatalf("agent image %v, want %s", got, want)
			}
		}
	}
	for name, revisions := range revisions {
		if len(revisions) != 3 || revisions[0] != revisions[1] || revisions[1] == revisions[2] {
			t.Fatalf("%s rollout revisions %v", name, revisions)
		}
	}
}
