// SPDX-License-Identifier: Apache-2.0

package controllers

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	configv1 "github.com/openshift/api/config/v1"
	ocoperator "github.com/openshift/api/operator/v1"
	"github.com/openshift/cluster-network-operator/pkg/render"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/meta/testrestmapper"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	ktypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"antrea.io/antrea-operator/controllers/config"

	operatorv1 "antrea.io/antrea-operator/api/v1"
	"antrea.io/antrea-operator/controllers/flow"
	"antrea.io/antrea-operator/controllers/sharedinfo"
	"antrea.io/antrea-operator/controllers/statusmanager"
	"antrea.io/antrea-operator/controllers/types"
)

func TestFlowPartialApplyOwnershipAndCleanup(t *testing.T) {
	ctx := context.Background()
	const ns = "antrea-system"
	scheme := runtime.NewScheme()
	for _, register := range []func(*runtime.Scheme) error{configv1.Install, corev1.AddToScheme, appsv1.AddToScheme, rbacv1.AddToScheme} {
		if err := register(scheme); err != nil {
			t.Fatal(err)
		}
	}
	scheme.AddKnownTypeWithName(flow.DestinationGVK, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(flow.DestinationGVK.GroupVersion().WithKind("FlowExporterDestinationList"), &unstructured.UnstructuredList{})
	co := &configv1.ClusterOperator{ObjectMeta: metav1.ObjectMeta{Name: "antrea"}}
	network := &configv1.Network{ObjectMeta: metav1.ObjectMeta{Name: "cluster", UID: "network-uid"}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(co, network).WithStatusSubresource(co).Build()
	mapper := testrestmapper.TestOnlyStaticRESTMapper(scheme)
	status, err := statusmanager.New(c, mapper, "antrea", "antrea-operator", "test", &sharedinfo.SharedInfo{AntreaPlatform: "openshift"})
	if err != nil {
		t.Fatal(err)
	}
	status.FlowResources = make(map[configv1.ObjectReference]bool)
	refs, err := flow.OwnedResources(ns)
	if err != nil {
		t.Fatal(err)
	}
	for _, ref := range refs {
		status.FlowResources[ref] = true
	}
	spec := operatorv1.FlowAggregatorSpec{Enabled: true, Image: "fa:test", CollectorAddress: "192.0.2.1:4739"}
	objs, err := flow.Render(&spec, ns, map[string]interface{}{types.AntreaAgentConfigRenderKey: "{}"})
	if err != nil {
		t.Fatal(err)
	}
	owner := metav1.OwnerReference{APIVersion: "config.openshift.io/v1", Kind: "Network", Name: "cluster", UID: "network-uid", Controller: ptr.To(true)}
	for _, obj := range objs {
		obj.SetOwnerReferences([]metav1.OwnerReference{owner})
		obj.SetLabels(map[string]string{flow.ComponentLabel: flow.Name, statusmanager.OperandLabel: statusmanager.CoreOperand})
	}
	// A retained reference to flow credentials in a prior core namespace
	// belongs to the migration owner's explicit handover.
	old := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "previous-core", Name: "flow-aggregator-client-tls", Labels: map[string]string{flow.ComponentLabel: flow.Name, statusmanager.OperandLabel: statusmanager.CoreOperand}}}
	if err := c.Create(ctx, old); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, client.ObjectKey{Name: "antrea"}, co); err != nil {
		t.Fatal(err)
	}
	co.Status.RelatedObjects = []configv1.ObjectReference{{Resource: "secrets", Name: old.Name, Namespace: old.Namespace}}
	if err := c.Status().Update(ctx, co); err != nil {
		t.Fatal(err)
	}

	// Leave a subset of objects live without publishing an inventory.
	// The first collision is a customer-owned Secret at a reserved identity.
	key := client.ObjectKey{Namespace: ns, Name: "flow-aggregator-client-tls"}
	foreign := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: key.Name, UID: "customer"}, Data: map[string][]byte{"customer": []byte("value")}}
	if err := c.Create(ctx, foreign); err != nil {
		t.Fatal(err)
	}
	applied := 0
	for _, obj := range objs {
		err := applyFlowObject(ctx, c, obj, "cluster-network-operator/antrea-core", func() error { return errors.New("unexpected adoption") })
		if obj.GetName() == key.Name {
			if err == nil {
				t.Fatal("adopted customer Secret")
			}
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		applied++
	}
	if applied < 3 {
		t.Fatal("partial application was not exercised")
	}
	if err := status.SetInventory(nil, nil, []configv1.ObjectReference{}); err != nil {
		t.Fatal(err)
	}
	if pending, err := status.FlowCleanupPending(ctx); err != nil || pending {
		t.Fatalf("cleanup pending %t, err %v", pending, err)
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(old), old); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, key, foreign); err != nil || string(foreign.Data["customer"]) != "value" || foreign.UID != "customer" {
		t.Fatalf("lost customer Secret: %v", err)
	}
	for _, obj := range objs[:applied] {
		if err := c.Get(ctx, client.ObjectKeyFromObject(obj), obj.DeepCopy()); !apierrors.IsNotFound(err) {
			t.Fatalf("left %s %s: %v", obj.GetKind(), obj.GetName(), err)
		}
	}
	// After the simulated customer removes the collision, retries preserve
	// the provider's cert data and require the observed resourceVersion.
	if err := c.Delete(ctx, foreign); err != nil {
		t.Fatal(err)
	}
	for _, obj := range objs {
		if err := applyFlowObject(ctx, c, obj, "cluster-network-operator/antrea-core", func() error { return errors.New("unexpected update") }); err != nil {
			t.Fatalf("%s: %v", obj.GetKind(), err)
		}
	}
	generated := &corev1.Secret{}
	if err := c.Get(ctx, key, generated); err != nil {
		t.Fatal(err)
	}
	generated.Data = map[string][]byte{"tls.crt": []byte("provider bytes")}
	if err := c.Update(ctx, generated); err != nil {
		t.Fatal(err)
	}
	for _, obj := range objs {
		err := applyFlowObject(ctx, c, obj, "cluster-network-operator/antrea-core", func() error {
			if obj.GetResourceVersion() == "" {
				return errors.New("unguarded apply")
			}
			if obj.GetKind() == "Secret" && obj.Object["data"] != nil {
				return errors.New("operator overwrote certificate data")
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := status.SetInventory(nil, nil, refs); err != nil {
		t.Fatal(err)
	}
	if err := status.SetInventory(nil, nil, []configv1.ObjectReference{}); err != nil {
		t.Fatal(err)
	}
	if pending, err := status.FlowCleanupPending(ctx); err != nil || pending {
		t.Fatalf("pending=%t err=%v", pending, err)
	}
	for _, obj := range objs {
		if err := c.Get(ctx, client.ObjectKeyFromObject(obj), obj.DeepCopy()); !apierrors.IsNotFound(err) {
			t.Fatalf("left %s %s: %v", obj.GetKind(), obj.GetName(), err)
		}
	}
	// A replacement can retain labels from an earlier Network owner.
	replaced := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace, Labels: map[string]string{flow.ComponentLabel: flow.Name, statusmanager.OperandLabel: statusmanager.CoreOperand}}}
	previous := owner
	previous.UID = "retired-network-uid"
	replaced.OwnerReferences = []metav1.OwnerReference{previous}
	if err := c.Create(ctx, replaced); err != nil {
		t.Fatal(err)
	}
	if err := status.SetInventory(nil, nil, []configv1.ObjectReference{}); err != nil {
		t.Fatal(err)
	}
	if pending, err := status.FlowCleanupPending(ctx); err != nil || pending {
		t.Fatalf("foreign replacement blocks cleanup: %t %v", pending, err)
	}
	if err := c.Get(ctx, key, replaced); err != nil {
		t.Fatal(err)
	}
}

func TestFlowStatusWaitsForAgentsReplicaAndTermination(t *testing.T) {
	ctx := context.Background()
	const ns = "core"
	ds := &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Name: "antrea-agent", Namespace: ns, Generation: 3}, Status: appsv1.DaemonSetStatus{DesiredNumberScheduled: 2, UpdatedNumberScheduled: 2, NumberAvailable: 2, ObservedGeneration: 2}}
	dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: flow.Name, Namespace: ns, Generation: 4}, Status: appsv1.DeploymentStatus{ObservedGeneration: 3, Replicas: 1, UpdatedReplicas: 1, AvailableReplicas: 1}}
	c := fake.NewClientBuilder().WithObjects(ds, dep).Build()
	assert := func(enabled bool, reason string, generation int64) {
		t.Helper()
		got, err := flowCondition(ctx, c, ns, enabled, generation)
		if err != nil || got.Reason != reason || !strings.HasPrefix(got.Message, fmt.Sprintf("Install generation %d:", generation)) {
			t.Fatalf("%+v %v", got, err)
		}
	}
	assert(true, "Progressing", 1)
	ds.Status.ObservedGeneration = ds.Generation
	if err := c.Status().Update(ctx, ds); err != nil {
		t.Fatal(err)
	}
	assert(true, "Progressing", 2)
	dep.Status.ObservedGeneration = dep.Generation
	if err := c.Status().Update(ctx, dep); err != nil {
		t.Fatal(err)
	}
	assert(true, "Available", 2)
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "old", Namespace: ns, UID: ktypes.UID("old-pod"), Labels: map[string]string{flow.ComponentLabel: flow.Name, statusmanager.OperandLabel: statusmanager.CoreOperand}}}
	if err := c.Create(ctx, pod); err != nil {
		t.Fatal(err)
	}
	assert(false, "Progressing", 3)
	if err := c.Delete(ctx, pod); err != nil {
		t.Fatal(err)
	}
	assert(false, "Disabled", 3)
}

// Exercise the generated core manifests and registration inventory together
// with the optional manifests, including a first install before discovery
// lists the freshly installed destination CRD.
func TestFlowRevisionsAndDeclaredCoreInventory(t *testing.T) {
	const ns = "antrea-system"
	cfg := &config.ConfigOc{CoreNamespace: ns}
	raw := &operatorv1.AntreaInstall{Spec: operatorv1.AntreaInstallSpec{AntreaAgentConfig: "featureGates:\n  Egress: true\n", AntreaImage: "agent:test"}}
	cluster := &configv1.Network{ObjectMeta: metav1.ObjectMeta{Name: "cluster", UID: "network-uid"}, Spec: configv1.NetworkSpec{ServiceNetwork: []string{"172.30.0.0/16"}, ClusterNetwork: []configv1.ClusterNetworkEntry{{CIDR: "10.128.0.0/14", HostPrefix: 23}}, NetworkType: "Antrea"}}
	scheme := runtime.NewScheme()
	if err := configv1.Install(scheme); err != nil {
		t.Fatal(err)
	}
	mapper := meta.NewDefaultRESTMapper(nil)
	r := &AntreaInstallReconciler{Scheme: scheme, Mapper: mapper, Adaptor: &AdaptorOc{Config: cfg}, Status: &statusmanager.StatusManager{CoreResources: make(map[configv1.ObjectReference]bool)}}
	flowRefs, err := flow.OwnedResources(ns)
	if err != nil {
		t.Fatal(err)
	}
	for _, ref := range flowRefs {
		r.Status.CoreResources[ref] = true
	}
	spec := operatorv1.FlowAggregatorSpec{Enabled: true, CollectorAddress: "192.0.2.1:4739", Image: "fa:test"}
	observe := func(enabled bool) (string, string, error) {
		t.Helper()
		spec.Enabled = enabled
		network := &ocoperator.Network{}
		projected := raw.DeepCopy()
		if err := cfg.FillConfigs(cluster, projected); err != nil {
			t.Fatal(err)
		}
		values, err := cfg.GenerateRenderData(network, projected)
		if err != nil {
			t.Fatal(err)
		}
		f, err := flow.Render(&spec, ns, values.Data)
		if err != nil {
			t.Fatal(err)
		}
		base, err := render.RenderDir(filepath.Join("..", types.DefaultManifestDir), values)
		if err != nil {
			t.Fatal(err)
		}
		base, err = flow.EnsureDestinationCRD(base)
		if err != nil {
			t.Fatal(err)
		}
		for _, obj := range append(base, f...) {
			gvk := obj.GroupVersionKind()
			if gvk != flow.DestinationGVK {
				scope := meta.RESTScopeRoot
				if obj.GetNamespace() != "" {
					scope = meta.RESTScopeNamespace
				}
				mapper.Add(gvk, scope)
			}
			if obj.GetLabels()[flow.ComponentLabel] != flow.Name {
				res, _ := meta.UnsafeGuessKindToResource(gvk)
				r.Status.CoreResources[configv1.ObjectReference{Group: res.Group, Resource: res.Resource, Name: obj.GetName(), Namespace: obj.GetNamespace()}] = true
			}
		}
		inventory, err := r.prepareInventory(append(base, f...), cluster)
		if err != nil {
			return "", "", err
		}
		if enabled {
			if err := validateFlowAgent(inventory.agent, base); err != nil {
				t.Fatal(err)
			}
		}
		rev, _, _ := unstructured.NestedString(inventory.agent.Object, "spec", "template", "metadata", "annotations", "operator.antrea.io/core-config-hash")
		var flowRev string
		for _, obj := range f {
			if obj.GetKind() == "Deployment" {
				if meta := metav1.GetControllerOf(obj); meta == nil || meta.UID != cluster.UID {
					t.Fatalf("no Network owner on deployment")
				}
				flowRev, _, _ = unstructured.NestedString(obj.Object, "spec", "template", "metadata", "annotations", "operator.antrea.io/flow-config-hash")
			}
		}
		return rev, flowRev, nil
	}
	off, _, err := observe(false)
	if err != nil {
		t.Fatal(err)
	}
	on, first, err := observe(true)
	if err != nil || on == "" || off == on || first == "" {
		t.Fatalf("enable: %s %s %s %v", off, on, first, err)
	}
	spec.Mode = "Proxy"
	spec.PodLabels = true
	spec.CollectorAddress = "192.0.2.2:4739"
	during, second, err := observe(true)
	if err != nil || during != on || second == first {
		t.Fatalf("reconfigure: %s %s %v", during, second, err)
	}
	restored, _, err := observe(false)
	if err != nil || restored != off {
		t.Fatalf("disable: %s %v", restored, err)
	}
	delete(r.Status.CoreResources, flowRefs[0])
	if _, _, err := observe(true); err == nil {
		t.Fatal("accepted undeclared flow object")
	}
}
