// SPDX-License-Identifier: Apache-2.0

package controllers

import (
	"path/filepath"
	"testing"

	"github.com/openshift/cluster-network-operator/pkg/render"
	"k8s.io/apimachinery/pkg/api/meta"
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
	r := &AntreaInstallReconciler{Mapper: mapper, Status: &statusmanager.StatusManager{}}
	inventory, err := r.prepareInventory(objects, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(inventory.related) != len(objects) || inventory.agent.GetName() != types.AntreaAgentDaemonSetName || inventory.controller.GetName() != types.AntreaControllerDeploymentName {
		t.Fatalf("standalone inventory has %d references for %d rendered objects", len(inventory.related), len(objects))
	}
}
