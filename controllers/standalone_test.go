// SPDX-License-Identifier: Apache-2.0

package controllers

import (
	"path/filepath"
	"testing"

	configv1 "github.com/openshift/api/config/v1"
	ocoperv1 "github.com/openshift/api/operator/v1"
	"github.com/openshift/cluster-network-operator/pkg/render"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	operatorv1 "antrea.io/antrea-operator/api/v1"
	"antrea.io/antrea-operator/controllers/config"
	"antrea.io/antrea-operator/controllers/statusmanager"
	"antrea.io/antrea-operator/controllers/types"
)

func TestStandaloneBundleInventory(t *testing.T) {
	data, err := (&config.ConfigK8s{}).GenerateRenderData(nil, &operatorv1.AntreaInstall{Spec: operatorv1.AntreaInstallSpec{AntreaImage: "antrea:test"}})
	if err != nil {
		t.Fatal(err)
	}
	inventory, objects := renderStandaloneInventory(t, data, nil)
	if len(inventory.related) != len(objects) || inventory.agent.GetName() != types.AntreaAgentDaemonSetName || inventory.controller.GetName() != types.AntreaControllerDeploymentName {
		t.Fatalf("standalone inventory has %d references for %d rendered objects", len(inventory.related), len(objects))
	}
}

func TestOpenShiftCoreNamespace(t *testing.T) {
	const namespace = "antrea-system"
	cfg := &config.ConfigOc{CoreNamespace: namespace}
	install := &operatorv1.AntreaInstall{Spec: operatorv1.AntreaInstallSpec{
		CoreNamespace: namespace,
		AntreaImage:   "antrea:test",
	}}
	cluster := &configv1.Network{Spec: configv1.NetworkSpec{
		ServiceNetwork: []string{"10.96.0.0/12"},
		ClusterNetwork: []configv1.ClusterNetworkEntry{{CIDR: "10.128.0.0/14", HostPrefix: 23}},
	}}
	if err := cfg.FillConfigs(cluster, install); err != nil {
		t.Fatal(err)
	}
	if err := cfg.ValidateConfig(cluster, install); err != nil {
		t.Fatal(err)
	}
	data, err := cfg.GenerateRenderData(&ocoperv1.Network{}, install)
	if err != nil {
		t.Fatal(err)
	}
	inventory, objects := renderStandaloneInventory(t, data, &AdaptorOc{Config: cfg})
	if inventory.agent.GetNamespace() != namespace || inventory.controller.GetNamespace() != namespace {
		t.Fatalf("unexpected core workload namespaces: %q, %q", inventory.agent.GetNamespace(), inventory.controller.GetNamespace())
	}
	var serviceRefs, subjects int
	for _, obj := range objects {
		if obj.GetNamespace() != "" && obj.GetNamespace() != namespace {
			t.Errorf("%s %s renders in %s", obj.GetKind(), obj.GetName(), obj.GetNamespace())
		}
		switch obj.GetKind() {
		case "APIService":
			ref, _, _ := unstructured.NestedString(obj.Object, "spec", "service", "namespace")
			if ref != namespace {
				t.Errorf("%s points to service in %q", obj.GetName(), ref)
			}
			serviceRefs++
		case "ClusterRoleBinding":
			refs, _, err := unstructured.NestedSlice(obj.Object, "subjects")
			if err != nil {
				t.Fatal(err)
			}
			for _, raw := range refs {
				subject := raw.(map[string]interface{})
				if subject["kind"] == "ServiceAccount" {
					if subject["namespace"] != namespace {
						t.Errorf("%s grants permissions to a service account in %q", obj.GetName(), subject["namespace"])
					}
					subjects++
				}
			}
		}
	}
	if serviceRefs == 0 || subjects == 0 {
		t.Fatalf("missing service or RBAC references: %d, %d", serviceRefs, subjects)
	}
	install.Spec.CoreNamespace = "kube-system"
	if err := cfg.ValidateConfig(cluster, install); err == nil {
		t.Fatal("an explicit namespace mismatch was accepted")
	}
}

func renderStandaloneInventory(t *testing.T, data *render.RenderData, adaptor Adaptor) (*resourceInventory, []*unstructured.Unstructured) {
	t.Helper()
	objects, err := render.RenderDir(filepath.Join("..", types.DefaultManifestDir), data)
	if err != nil {
		t.Fatal(err)
	}
	mapper := meta.NewDefaultRESTMapper([]schema.GroupVersion{})
	for _, obj := range objects {
		scope := meta.RESTScopeRoot
		if obj.GetNamespace() != "" {
			scope = meta.RESTScopeNamespace
		}
		mapper.Add(obj.GroupVersionKind(), scope)
	}
	r := &AntreaInstallReconciler{Adaptor: adaptor, Mapper: mapper, Status: &statusmanager.StatusManager{}}
	inventory, err := r.prepareInventory(objects, nil)
	if err != nil {
		t.Fatal(err)
	}
	return inventory, objects
}
