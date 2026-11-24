/* Copyright © 2020 VMware, Inc. All Rights Reserved.
   SPDX-License-Identifier: Apache-2.0 */

package controllers

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"

	configv1 "github.com/openshift/api/config/v1"
	ocoperv1 "github.com/openshift/api/operator/v1"
	"github.com/openshift/cluster-network-operator/pkg/apply"
	cnoclient "github.com/openshift/cluster-network-operator/pkg/client"
	"github.com/openshift/cluster-network-operator/pkg/render"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	uns "k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	operatorv1 "antrea.io/antrea-operator/api/v1"
	configutil "antrea.io/antrea-operator/controllers/config"
	"antrea.io/antrea-operator/controllers/install"
	"antrea.io/antrea-operator/controllers/sharedinfo"
	"antrea.io/antrea-operator/controllers/statusmanager"
	operatortypes "antrea.io/antrea-operator/controllers/types"
	"antrea.io/antrea-operator/internal/version"
)

var log = ctrl.Log.WithName("controllers")

type Adaptor interface {
	SetupWithManager(r *AntreaInstallReconciler, mgr ctrl.Manager) error
	Reconcile(r *AntreaInstallReconciler, request ctrl.Request) (reconcile.Result, error)
	UpdateStatusManagerAndSharedInfo(r *AntreaInstallReconciler, objs []*uns.Unstructured, clusterConfig *configv1.Network) error
}

type AdaptorK8s struct {
	Config configutil.Config
}

type AdaptorOc struct {
	Config configutil.Config
}

func (k8s *AdaptorK8s) SetupWithManager(r *AntreaInstallReconciler, mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named("antrea-core-install").
		For(install.Object(r.installGVK)).
		Complete(r)
}

func (oc *AdaptorOc) SetupWithManager(r *AntreaInstallReconciler, mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named("antrea-core-install").
		For(install.Object(r.installGVK)).
		Watches(&configv1.Network{}, &handler.EnqueueRequestForObject{}).
		Complete(r)
}

func applyConfig(r *AntreaInstallReconciler, config configutil.Config, clusterConfig *configv1.Network, operConfig *operatorv1.AntreaInstall, operatorNetwork *ocoperv1.Network, raw *uns.Unstructured) (reconcile.Result, error) {
	// Fill default configurations.
	if err := config.FillConfigs(clusterConfig, operConfig); err != nil {
		log.Error(err, "failed to fill configurations")
		r.Status.SetDegraded(statusmanager.OperatorConfig, "FillConfigurationsError", fmt.Sprintf("Failed to fill configurations: %v", err))
		return reconcile.Result{Requeue: true}, err
	}

	// Validate configurations.
	if err := config.ValidateConfig(clusterConfig, operConfig); err != nil {
		log.Error(err, "failed to validate configurations")
		r.Status.SetDegraded(statusmanager.OperatorConfig, "InvalidOperatorConfig", fmt.Sprintf("The operator configuration is invalid: %v", err))
		return reconcile.Result{Requeue: true}, err
	}

	// Generate render data.
	renderData, err := config.GenerateRenderData(operatorNetwork, operConfig)
	if err != nil {
		log.Error(err, "failed to generate render data")
		r.Status.SetDegraded(statusmanager.OperatorConfig, "RenderConfigError", fmt.Sprintf("Failed to render operator configurations: %v", err))
		return reconcile.Result{Requeue: true}, err
	}

	renderData.Data[operatortypes.ReleaseVersion] = r.version
	if r.configure != nil {
		if err := r.configure(raw, renderData.Data); err != nil {
			r.Status.SetDegraded(statusmanager.OperatorConfig, "RenderConfigError", err.Error())
			return reconcile.Result{}, err
		}
	}

	appliedConfig := r.AppliedOperConfig
	if r.Status.CoreResources == nil && appliedConfig == nil {
		appliedConfig, err = r.getAppliedOperConfig()
		if err != nil {
			r.Status.SetDegraded(statusmanager.OperatorConfig, "InternalError", fmt.Sprintf("Failed to get current configurations: %v", err))
			return reconcile.Result{}, err
		}
	}
	agentNeedChange, controllerNeedChange, imageChange := configutil.NeedApplyChange(appliedConfig, operConfig)
	// A distribution's render may depend on fields outside the projection.
	// Standalone keeps its existing comparison against the installed config.
	if agentNeedChange || controllerNeedChange || r.Status.CoreResources != nil {
		objs, err := render.RenderDir(r.manifestDir, renderData)
		if err != nil {
			log.Error(err, "failed to render configuration")
			r.Status.SetDegraded(statusmanager.OperatorConfig, "RenderConfigError", fmt.Sprintf("Failed to render operator configurations: %v", err))
			return reconcile.Result{Requeue: true}, err
		}

		inventory, err := r.prepareInventory(objs, clusterConfig)
		if err != nil {
			r.Status.SetDegraded(statusmanager.OperatorConfig, "InvalidResourceBundle", err.Error())
			return reconcile.Result{}, err
		}

		r.SharedInfo.Lock()
		defer r.SharedInfo.Unlock()
		for _, obj := range objs {
			if err = apply.ApplyObject(context.TODO(), r.Client, obj, r.subcontroller); err != nil {
				r.Status.SetDegraded(statusmanager.OperatorConfig, "ApplyObjectsError", fmt.Sprintf("Failed to apply operator configurations: %v", err))
				return reconcile.Result{}, err
			}
		}
		if err := r.publishInventory(inventory); err != nil {
			r.Status.SetDegraded(statusmanager.OperatorConfig, "InventoryError", err.Error())
			return reconcile.Result{}, err
		}

		// Delete old antrea-agent and antrea-controller pods.
		if r.Status.CoreResources == nil && r.AppliedOperConfig != nil && agentNeedChange && !imageChange {
			if err = deleteExistingPods(r.Client.Default().CRClient(), operatortypes.AntreaAgentDaemonSetName); err != nil {
				msg := fmt.Sprintf("DaemonSet %s is not using the latest configuration updates because: %v", operatortypes.AntreaAgentDaemonSetName, err)
				r.Status.SetDegraded(statusmanager.OperatorConfig, "DeleteOldPodsError", msg)
				return reconcile.Result{Requeue: true}, err
			}
		}
		if r.Status.CoreResources == nil && r.AppliedOperConfig != nil && controllerNeedChange && !imageChange {
			if err = deleteExistingPods(r.Client.Default().CRClient(), operatortypes.AntreaControllerDeploymentName); err != nil {
				msg := fmt.Sprintf("Deployment %s is not using the latest configuration updates because: %v", operatortypes.AntreaControllerDeploymentName, err)
				r.Status.SetDegraded(statusmanager.OperatorConfig, "DeleteOldPodsError", msg)
				return reconcile.Result{Requeue: true}, err
			}
		}
	}
	return reconcile.Result{}, nil
}

func fetchAntreaInstall(r *AntreaInstallReconciler, request ctrl.Request) (*operatorv1.AntreaInstall, *uns.Unstructured, error) {
	raw, err := install.Read(context.TODO(), r.Client.Default().CRClient(), r.installGVK)
	if apierrors.IsNotFound(err) {
		r.Status.SetDegraded(statusmanager.ClusterConfig, "NoAntreaInstallCR", fmt.Sprintf("%s CR not found", operatortypes.OperatorConfigName))
		return nil, nil, nil
	}
	if err != nil {
		r.Status.SetDegraded(statusmanager.OperatorConfig, "InvalidAntreaInstallCR", err.Error())
		return nil, nil, err
	}
	// Status changes leave generation unchanged. A recreated install may
	// reuse a generation but has a different UID.
	applied := r.AppliedOperConfig
	if request.Name == raw.GetName() && applied != nil && raw.GetGeneration() == applied.Generation && raw.GetUID() == applied.UID {
		return nil, nil, nil
	}
	core, err := install.CoreConfig(raw)
	if err != nil {
		r.Status.SetDegraded(statusmanager.OperatorConfig, "InvalidAntreaInstallCR", err.Error())
	}
	return core, raw, err
}

func isOperatorRequest(r *AntreaInstallReconciler, request ctrl.Request) bool {
	reqLogger := r.Log.WithValues("Request.NamespacedName", request.NamespacedName)
	if request.Namespace == "" && request.Name == operatortypes.ClusterConfigName {
		reqLogger.Info("Reconciling antrea-operator Cluster Network CR change")
		return true
	}
	if request.Namespace == operatortypes.OperatorNameSpace && request.Name == operatortypes.OperatorConfigName {
		reqLogger.Info("Reconciling antrea-operator antrea-install CR change")
		return true
	}
	return false
}

func (k8s *AdaptorK8s) Reconcile(r *AntreaInstallReconciler, request ctrl.Request) (reconcile.Result, error) {
	if !isOperatorRequest(r, request) {
		return reconcile.Result{}, nil
	}

	// Fetch antrea-install CR.
	operConfig, raw, err := fetchAntreaInstall(r, request)
	if err != nil {
		return reconcile.Result{Requeue: true}, err
	}
	if operConfig == nil {
		return reconcile.Result{}, nil
	}

	// Apply configuration.
	if result, err := applyConfig(r, k8s.Config, nil, operConfig, nil, raw); err != nil {
		return result, err
	}

	r.Status.SetNotDegraded(statusmanager.ClusterConfig)
	r.Status.SetNotDegraded(statusmanager.OperatorConfig)

	r.AppliedOperConfig = operConfig

	return reconcile.Result{}, nil
}

func (oc *AdaptorOc) Reconcile(r *AntreaInstallReconciler, request ctrl.Request) (reconcile.Result, error) {
	if !isOperatorRequest(r, request) {
		return reconcile.Result{}, nil
	}

	// Fetch Cluster Network CR.
	clusterConfig := &configv1.Network{}
	err := r.Client.Default().CRClient().Get(context.TODO(), types.NamespacedName{Name: operatortypes.ClusterConfigName}, clusterConfig)
	if err != nil {
		if apierrors.IsNotFound(err) {
			msg := "Cluster Network CR not found"
			log.Info(msg)
			r.Status.SetDegraded(statusmanager.ClusterConfig, "NoClusterConfig", msg)
			return reconcile.Result{}, nil
		}
		r.Status.SetDegraded(statusmanager.ClusterConfig, "InvalidClusterConfig", fmt.Sprintf("Failed to get cluster network CRD: %v", err))
		log.Error(err, "failed to get Cluster Network CR")
		return reconcile.Result{Requeue: true}, err
	}
	if request.Name == clusterConfig.Name && r.AppliedClusterConfig != nil {
		if reflect.DeepEqual(clusterConfig.Spec, r.AppliedClusterConfig.Spec) {
			log.Info("no configuration change")
			return reconcile.Result{}, nil
		}
	}

	// Fetch the Network.operator.openshift.io instance
	operatorNetwork := &ocoperv1.Network{}
	err = r.Client.Default().CRClient().Get(context.TODO(), types.NamespacedName{Name: operatortypes.ClusterOperatorNetworkName}, operatorNetwork)
	if err != nil {
		if apierrors.IsNotFound(err) {
			r.Status.SetDegraded(statusmanager.OperatorConfig, "NoClusterNetworkOperatorConfig", fmt.Sprintf("Cluster network operator configuration not found"))
			return reconcile.Result{}, nil
		}
		// Error reading the object - requeue the request.
		log.Error(err, "Unable to retrieve Network.operator.openshift.io object")
		return reconcile.Result{Requeue: true}, err
	}

	// Fetch antrea-install CR.
	operConfig, raw, err := fetchAntreaInstall(r, request)
	if err != nil {
		return reconcile.Result{Requeue: true}, err
	}
	if operConfig == nil {
		return reconcile.Result{}, nil
	}

	// Apply configuration.
	if result, err := applyConfig(r, oc.Config, clusterConfig, operConfig, operatorNetwork, raw); err != nil {
		return result, err
	}

	// Update cluster network CR status.
	clusterNetworkConfigChanged := configutil.HasClusterNetworkConfigChange(r.AppliedClusterConfig, clusterConfig)
	defaultMTUChanged, curDefaultMTU, err := configutil.HasDefaultMTUChange(r.AppliedOperConfig, operConfig)
	if err != nil {
		r.Status.SetDegraded(statusmanager.OperatorConfig, "UpdateNetworkStatusError", fmt.Sprintf("failed to check default MTU configuration: %v", err))
		return reconcile.Result{Requeue: true}, err
	}
	if clusterNetworkConfigChanged || defaultMTUChanged {
		if err = updateNetworkStatus(r.Client, clusterConfig, curDefaultMTU); err != nil {
			r.Status.SetDegraded(statusmanager.ClusterConfig, "UpdateNetworkStatusError", fmt.Sprintf("Failed to update network status: %v", err))
			return reconcile.Result{Requeue: true}, err
		}
	}

	r.Status.SetNotDegraded(statusmanager.ClusterConfig)
	r.Status.SetNotDegraded(statusmanager.OperatorConfig)

	r.AppliedClusterConfig = clusterConfig
	r.AppliedOperConfig = operConfig

	return reconcile.Result{}, nil
}

// AntreaInstallReconciler reconciles a AntreaInstall object
type AntreaInstallReconciler struct {
	Client cnoclient.Client
	Log    logr.Logger
	Scheme *runtime.Scheme
	Status *statusmanager.StatusManager
	Mapper meta.RESTMapper

	Adaptor
	installGVK    schema.GroupVersionKind
	manifestDir   string
	version       string
	configure     func(*uns.Unstructured, map[string]interface{}) error
	subcontroller string

	SharedInfo           *sharedinfo.SharedInfo
	AppliedClusterConfig *configv1.Network
	AppliedOperConfig    *operatorv1.AntreaInstall
}

func New(mgr ctrl.Manager, statusManager *statusmanager.StatusManager, info *sharedinfo.SharedInfo, cli cnoclient.Client) (*AntreaInstallReconciler, error) {
	r := AntreaInstallReconciler{
		Client:      cli,
		manifestDir: operatortypes.DefaultManifestDir,
		version:     version.GetVersion(),
		Log:         ctrl.Log.WithName("controllers").WithName("AntreaInstall"),
		Scheme:      mgr.GetScheme(),
		Status:      statusManager,
		Mapper:      mgr.GetRESTMapper(),
		SharedInfo:  info,
	}
	switch info.AntreaPlatform {
	case "openshift":
		r.Adaptor = &AdaptorOc{
			Config: &configutil.ConfigOc{},
		}
	case "kubernetes":
		r.Adaptor = &AdaptorK8s{
			Config: &configutil.ConfigK8s{},
		}
	default:
		return nil, errors.New("invalid platform: platform should be openshift or kubernetes")
	}
	return &r, nil
}

func (r *AntreaInstallReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return r.Adaptor.SetupWithManager(r, mgr)
}

// +kubebuilder:rbac:groups=operator.antrea.vmware.com,resources=antreainstalls,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=operator.antrea.vmware.com,resources=antreainstalls/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=config.openshift.io,resources=clusteroperators,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=config.openshift.io,resources=clusteroperators/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=config.openshift.io,resources=networks;networks/finalizers,verbs=get;list;watch;patch;update
// +kubebuilder:rbac:groups=operator.openshift.io,resources=networks,verbs=get;list;watch;patch;update
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;watch;list
// +kubebuilder:rbac:groups="",resources=namespaces;pods;configmaps;services;serviceaccounts,verbs=create;delete;get;list;patch;update;watch;deletecollection
// +kubebuilder:rbac:groups=apps,resources=deployments;daemonsets,verbs=create;delete;get;list;patch;update;watch
// +kubebuilder:rbac:groups=apiextensions.k8s.io,resources=customresourcedefinitions,verbs=create;delete;get;list;patch;update;watch
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=clusterroles;clusterrolebindings;roles;rolebindings,verbs=create;delete;get;list;patch;update;watch
// +kubebuilder:rbac:groups=apiregistration.k8s.io,resources=apiservices,verbs=get;create;update;delete
// +kubebuilder:rbac:groups=authorization.k8s.io,resources=subjectaccessreviews,verbs=create
// +kubebuilder:rbac:groups=authentication.k8s.io,resources=tokenreviews,verbs=create
// +kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies,verbs=get;watch;list
// +kubebuilder:rbac:groups=admissionregistration.k8s.io,resources=mutatingwebhookconfigurations;validatingwebhookconfigurations,verbs=create
// +kubebuilder:rbac:groups=crd.antrea.io,resources=traceflows;traceflows/status,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=crd.antrea.io,resources=antreaagentinfos;antreacontrollerinfos,verbs=get;list;create;update;delete
// +kubebuilder:rbac:groups=controlplane.antrea.io,resources=networkpolicies;appliedtogroups;addressgroups,verbs=get;watch;list;delete
// +kubebuilder:rbac:groups=crd.antrea.io,resources=clusternetworkpolicies,verbs=get;watch;list;delete
// +kubebuilder:rbac:groups=system.antrea.io,resources=agentinfos;supportbundles;supportbundles/download,verbs=get;watch;list;post;delete
// +kubebuilder:rbac:urls=/agentinfo;/addressgroups;/appliedtogroups;/networkpolicies;/ovsflows;/ovstracing;/podinterfaces,verbs=get
// +kubebuilder:rbac:groups=security.openshift.io,resources=securitycontextconstraints,resourceNames=hostnetwork,verbs=use

func (r *AntreaInstallReconciler) Reconcile(cxt context.Context, request ctrl.Request) (reconcile.Result, error) {
	return r.Adaptor.Reconcile(r, request)
}

func (r *AntreaInstallReconciler) getAppliedOperConfig() (*operatorv1.AntreaInstall, error) {
	if r.AppliedOperConfig != nil {
		return r.AppliedOperConfig, nil
	}
	operConfig := &operatorv1.AntreaInstall{}
	var antreaConfig *corev1.ConfigMap
	configList := &corev1.ConfigMapList{}
	label := map[string]string{"app": "antrea"}
	crcClient := r.Client.Default().CRClient()
	if err := crcClient.List(context.TODO(), configList, client.InNamespace(operatortypes.AntreaNamespace), client.MatchingLabels(label)); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		} else {
			return nil, err
		}
	}
	for i := range configList.Items {
		if strings.HasPrefix(configList.Items[i].Name, operatortypes.AntreaConfigMapName) {
			antreaConfig = &configList.Items[i]
			break
		}
	}
	if antreaConfig == nil {
		log.Info("no antrea-config found")
		return nil, nil
	}
	antreaControllerDeployment := appsv1.Deployment{}
	if err := crcClient.Get(context.TODO(), types.NamespacedName{Namespace: operatortypes.AntreaNamespace, Name: operatortypes.AntreaControllerDeploymentName}, &antreaControllerDeployment); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		} else {
			return nil, err
		}
	}
	image := antreaControllerDeployment.Spec.Template.Spec.Containers[0].Image
	operConfigSpec := operatorv1.AntreaInstallSpec{
		AntreaAgentConfig:      antreaConfig.Data[operatortypes.AntreaAgentConfigOption],
		AntreaCNIConfig:        antreaConfig.Data[operatortypes.AntreaCNIConfigOption],
		AntreaControllerConfig: antreaConfig.Data[operatortypes.AntreaControllerConfigOption],
		AntreaImage:            image,
	}
	operConfig.Spec = operConfigSpec
	return operConfig, nil
}

func deleteExistingPods(c client.Client, component string) error {
	var period int64 = 0
	policy := metav1.DeletePropagationBackground
	label := map[string]string{"component": component}
	err := c.DeleteAllOf(context.TODO(), &corev1.Pod{}, client.InNamespace(operatortypes.AntreaNamespace), client.MatchingLabels(label), client.PropagationPolicy(policy), client.GracePeriodSeconds(period))
	if err != nil {
		log.Error(err, fmt.Sprintf("failed to delete pods for component: %s", component))
	}
	return err
}

func updateNetworkStatus(c cnoclient.Client, clusterConfig *configv1.Network, defaultMTU int) error {
	status := configutil.BuildNetworkStatus(clusterConfig, defaultMTU)
	clusterConfig.Status = *status
	if err := apply.ApplyObject(context.TODO(), c, clusterConfig, ""); err != nil {
		log.Error(err, fmt.Sprintf("Could not apply (%s) %s/%s", clusterConfig.GroupVersionKind(),
			clusterConfig.GetNamespace(), clusterConfig.GetName()))
		return err
	}
	log.Info("Successfully updated Network Status")
	return nil
}

type resourceInventory struct {
	daemonSets, deployments []types.NamespacedName
	related                 []configv1.ObjectReference
	agent, controller       *uns.Unstructured
}

func (r *AntreaInstallReconciler) prepareInventory(objs []*uns.Unstructured, clusterConfig *configv1.Network) (*resourceInventory, error) {
	inventory := &resourceInventory{}
	seen := make(map[configv1.ObjectReference]bool)
	configMaps := make(map[string]interface{})
	installKind := install.Object(r.installGVK).GroupVersionKind().GroupKind()
	for _, obj := range objs {
		gvk := obj.GroupVersionKind()
		mapping, err := r.Mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
		if err != nil {
			return nil, fmt.Errorf("mapping %s: %w", gvk, err)
		}
		ref := configv1.ObjectReference{Group: gvk.Group, Resource: mapping.Resource.Resource, Namespace: obj.GetNamespace(), Name: obj.GetName()}
		if ref.Name == "" || seen[ref] {
			return nil, fmt.Errorf("missing or duplicate bundle identity: %+v", ref)
		}
		seen[ref] = true
		if r.Status.CoreResources != nil && !r.Status.CoreResources[ref] {
			return nil, fmt.Errorf("bundle contains an object outside core ownership: %+v", ref)
		}
		if ref.Group == "apiextensions.k8s.io" && ref.Resource == "customresourcedefinitions" {
			kind, _, err := uns.NestedString(obj.Object, "spec", "names", "kind")
			if err != nil {
				return nil, err
			}
			if kind == installKind.Kind || ref.Name == "antreainstalls."+installKind.Group {
				return nil, fmt.Errorf("the distribution manages the install CRD: %+v", ref)
			}
		}
		if gvk.GroupKind() == installKind {
			return nil, fmt.Errorf("the install input cannot be a core operand: %+v", ref)
		}
		inventory.related = append(inventory.related, ref)
		labels := obj.GetLabels()
		if labels == nil {
			labels = make(map[string]string)
		}
		if owner := labels[statusmanager.OperandLabel]; owner != "" && owner != statusmanager.CoreOperand {
			return nil, fmt.Errorf("bundle resource %+v declares operand %q", ref, owner)
		}
		labels[statusmanager.OperandLabel] = statusmanager.CoreOperand
		obj.SetLabels(labels)
		if clusterConfig != nil {
			if err := controllerutil.SetControllerReference(clusterConfig, obj, r.Scheme); err != nil {
				return nil, err
			}
		}
		key := client.ObjectKeyFromObject(obj)
		if r.Status.CoreResources != nil && gvk == corev1.SchemeGroupVersion.WithKind("ConfigMap") {
			configMaps[key.String()] = []interface{}{obj.Object["data"], obj.Object["binaryData"]}
		}
		if gvk == appsv1.SchemeGroupVersion.WithKind("DaemonSet") {
			inventory.daemonSets = append(inventory.daemonSets, key)
			if key == (client.ObjectKey{Namespace: operatortypes.AntreaNamespace, Name: operatortypes.AntreaAgentDaemonSetName}) {
				inventory.agent = obj
			}
		}
		if gvk == appsv1.SchemeGroupVersion.WithKind("Deployment") {
			inventory.deployments = append(inventory.deployments, key)
			if key == (client.ObjectKey{Namespace: operatortypes.AntreaNamespace, Name: operatortypes.AntreaControllerDeploymentName}) {
				inventory.controller = obj
			}
		}
	}
	if inventory.agent == nil || inventory.controller == nil {
		return nil, fmt.Errorf("bundle requires %s DaemonSet and %s Deployment in %s", operatortypes.AntreaAgentDaemonSetName, operatortypes.AntreaControllerDeploymentName, operatortypes.AntreaNamespace)
	}
	// A composed bundle rolls its own Pods through their workload template.
	// This avoids deleting Pods by a component label shared with other owners.
	if r.Status.CoreResources != nil {
		// Hash the rendered data, including distribution values. encoding/json
		// orders map keys, so manifest order and metadata do not trigger rolls.
		data, err := json.Marshal(configMaps)
		if err != nil {
			return nil, err
		}
		revision := fmt.Sprintf("%x", sha256.Sum256(data))
		for _, workload := range []*uns.Unstructured{inventory.agent, inventory.controller} {
			if err := uns.SetNestedField(workload.Object, revision, "spec", "template", "metadata", "annotations", "operator.antrea.io/core-config-hash"); err != nil {
				return nil, err
			}
		}
	}
	return inventory, nil
}

func (r *AntreaInstallReconciler) publishInventory(inventory *resourceInventory) error {
	if err := r.Status.SetInventory(inventory.daemonSets, inventory.deployments, inventory.related); err != nil {
		return err
	}
	r.SharedInfo.AntreaAgentDaemonSetSpec = inventory.agent.DeepCopy()
	r.SharedInfo.AntreaControllerDeploymentSpec = inventory.controller.DeepCopy()
	return nil
}

func updateStatusManagerAndSharedInfo(r *AntreaInstallReconciler, objs []*uns.Unstructured, clusterConfig *configv1.Network) error {
	inventory, err := r.prepareInventory(objs, clusterConfig)
	if err != nil {
		return err
	}
	return r.publishInventory(inventory)
}

func (a *AdaptorK8s) UpdateStatusManagerAndSharedInfo(r *AntreaInstallReconciler, objs []*uns.Unstructured, clusterConfig *configv1.Network) error {
	return updateStatusManagerAndSharedInfo(r, objs, clusterConfig)
}

func (a *AdaptorOc) UpdateStatusManagerAndSharedInfo(r *AntreaInstallReconciler, objs []*uns.Unstructured, clusterConfig *configv1.Network) error {
	return updateStatusManagerAndSharedInfo(r, objs, clusterConfig)
}
