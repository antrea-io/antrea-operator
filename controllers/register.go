// SPDX-License-Identifier: Apache-2.0

package controllers

import (
	"context"
	"fmt"

	configv1 "github.com/openshift/api/config/v1"
	cnoclient "github.com/openshift/cluster-network-operator/pkg/client"
	"github.com/openshift/cluster-network-operator/pkg/names"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation"
	ctrl "sigs.k8s.io/controller-runtime"

	"antrea.io/antrea-operator/controllers/install"
	"antrea.io/antrea-operator/controllers/sharedinfo"
	"antrea.io/antrea-operator/controllers/statusmanager"
	"antrea.io/antrea-operator/controllers/types"
	"antrea.io/antrea-operator/internal/version"
)

// ResourceBundle is a generated directory of core manifests. OwnedResources
// names every object the distribution assigns to the core, including retired
// identities still eligible for cleanup. A new registration copies the list.
type ResourceBundle struct {
	ManifestDir    string
	OwnedResources []configv1.ObjectReference
	// Configure supplies distribution template values after core defaults and
	// validation. It receives the full install object, including extra fields.
	Configure func(install *unstructured.Unstructured, values map[string]interface{}) error
}

// CoreOptions selects the distribution inputs. Zero values preserve the
// standalone install GVK, manifest directory and build version.
type CoreOptions struct {
	InstallGVK schema.GroupVersionKind
	Bundle     *ResourceBundle
	Version    string
	// OpenShiftNamespace selects the already provisioned namespace for core
	// operands. Leave it empty to use kube-system. A namespace migration
	// requires a separate ownership handover.
	OpenShiftNamespace string
	// DisableOpenShiftWireGuard blocks WireGuard configuration when the
	// distribution's OpenShift qualification excludes it.
	DisableOpenShiftWireGuard bool
}

func (opts CoreOptions) validate() error {
	if gvk := opts.InstallGVK; !gvk.Empty() && (gvk.Group == "" || gvk.Version == "" || gvk.Kind != "AntreaInstall") {
		return fmt.Errorf("expected a versioned AntreaInstall GVK, got %v", gvk)
	}
	if ns := opts.OpenShiftNamespace; ns != "" {
		if errs := validation.IsDNS1123Label(ns); len(errs) != 0 {
			return fmt.Errorf("invalid OpenShift core namespace %q: %v", ns, errs)
		}
		if ns == types.OperatorNameSpace {
			return fmt.Errorf("the OpenShift core namespace must differ from the operator namespace %q", ns)
		}
	}
	if opts.Bundle != nil {
		if opts.Bundle.ManifestDir == "" || len(opts.Bundle.OwnedResources) == 0 {
			return fmt.Errorf("a custom bundle requires a manifest directory and owned resources")
		}
		for _, ref := range opts.Bundle.OwnedResources {
			if ref.Resource == "" || ref.Name == "" {
				return fmt.Errorf("incomplete owned resource identity: %+v", ref)
			}
		}
	}
	return nil
}

// RegisterCore registers the install and workload controllers on the caller's
// manager. Call once before starting the manager. The caller supplies Kubernetes
// and OpenShift schemes and owns manager startup and leader election.
func RegisterCore(ctx context.Context, mgr ctrl.Manager, opts CoreOptions) error {
	if err := opts.validate(); err != nil {
		return err
	}
	info, err := sharedinfo.NewForInstall(ctx, mgr, opts.InstallGVK)
	if err != nil {
		return fmt.Errorf("reading install platform: %w", err)
	}
	if opts.OpenShiftNamespace != "" && info.AntreaPlatform != "openshift" {
		return fmt.Errorf("OpenShiftNamespace requires the OpenShift platform")
	}
	if opts.Version == "" {
		opts.Version = version.GetVersion()
	}
	status, err := statusmanager.New(mgr.GetClient(), mgr.GetRESTMapper(), types.AntreaClusterOperatorName, types.OperatorNameSpace, opts.Version, info)
	if err != nil {
		return err
	}
	status.InstallGVK = opts.InstallGVK
	cfg := mgr.GetConfig()
	cli, err := cnoclient.NewClient(cfg, cfg, names.DefaultClusterName, nil)
	if err != nil {
		return err
	}
	controller, err := New(mgr, status, info, cli)
	if err != nil {
		return err
	}
	controller.installGVK = install.Object(opts.InstallGVK).GroupVersionKind()
	controller.version = opts.Version
	if oc, ok := controller.Adaptor.(*AdaptorOc); ok {
		oc.Config.CoreNamespace = opts.OpenShiftNamespace
		oc.Config.DisableWireGuard = opts.DisableOpenShiftWireGuard
	}
	if opts.Bundle != nil {
		controller.manifestDir = opts.Bundle.ManifestDir
		controller.configure = opts.Bundle.Configure
		owned := make(map[configv1.ObjectReference]bool, len(opts.Bundle.OwnedResources))
		for _, ref := range opts.Bundle.OwnedResources {
			owned[ref] = true
		}
		status.CoreResources = owned
		controller.subcontroller = "antrea-core"
	} else if info.AntreaPlatform == "openshift" {
		status.RecoverableResources = []configv1.ObjectReference{{
			Resource: "secrets", Namespace: controller.coreNamespace(), Name: types.ManagedIPsecPSKName,
		}}
	}
	if err := controller.SetupWithManager(mgr); err != nil {
		return err
	}
	pods := &PodReconciler{
		Client: cli, Log: ctrl.Log.WithName("controllers").WithName("Pod"), Scheme: mgr.GetScheme(),
		Status: status, SharedInfo: info, subcontroller: controller.subcontroller,
	}
	return pods.SetupWithManager(mgr)
}
