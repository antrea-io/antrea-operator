// SPDX-License-Identifier: Apache-2.0

package controllers

import (
	"context"
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	configv1 "github.com/openshift/api/config/v1"
	ocoperv1 "github.com/openshift/api/operator/v1"
	cnofake "github.com/openshift/cluster-network-operator/pkg/client/fake"
	"github.com/openshift/cluster-network-operator/pkg/network"
	"github.com/openshift/cluster-network-operator/pkg/render"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	operatorv1 "antrea.io/antrea-operator/api/v1"
	"antrea.io/antrea-operator/controllers/config"
	"antrea.io/antrea-operator/controllers/types"
)

func TestOpenShiftIPsecBundle(t *testing.T) {
	const fixtureKey = "generate-this-only-in-the-test"
	cfg := &config.ConfigOc{CoreNamespace: "antrea-system"}
	cluster := &configv1.Network{
		Spec: configv1.NetworkSpec{
			NetworkType: "antrea", ServiceNetwork: []string{"172.30.0.0/16"},
			ClusterNetwork: []configv1.ClusterNetworkEntry{{CIDR: "10.128.0.0/14", HostPrefix: 23}},
		},
	}
	ocNetwork := &ocoperv1.Network{}
	ocNetwork.Spec.DisableMultiNetwork = ptr.To(true)
	install := &operatorv1.AntreaInstall{Spec: operatorv1.AntreaInstallSpec{
		AntreaCNIConfig:       `{"name":"antrea","cniVersion":"0.3.1","plugins":[{"type":"antrea"}]}`,
		AntreaAgentImage:      "example.org/antrea/agent-ubi-ipsec@sha256:fixture",
		AntreaControllerImage: "example.org/antrea/controller-ubi-ipsec@sha256:fixture",
		TrafficEncryptionMode: "ipsec",
		PSK:                   fixtureKey,
	}}
	if err := cfg.FillConfigs(cluster, install); err != nil {
		t.Fatal(err)
	}
	data, err := cfg.GenerateRenderData(ocNetwork, install)
	if err != nil {
		t.Fatal(err)
	}
	objects, err := render.RenderDir(filepath.Join("..", types.DefaultManifestDir), data)
	if err != nil {
		t.Fatal(err)
	}
	r := &AntreaInstallReconciler{Adaptor: &AdaptorOc{Config: cfg}}
	operands, err := r.prepareOpenShiftOperands(objects, cfg, cluster, install)
	if err != nil {
		t.Fatal(err)
	}
	agent := &appsv1.DaemonSet{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(operands.agent.Object, agent); err != nil {
		t.Fatal(err)
	}
	controller := &appsv1.Deployment{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(operands.controller.Object, controller); err != nil {
		t.Fatal(err)
	}
	agentContainers := agent.Spec.Template.Spec.Containers
	if len(agentContainers) != 3 || agentContainers[0].Image != install.Spec.AntreaAgentImage || agentContainers[1].Image != install.Spec.AntreaAgentImage {
		t.Fatalf("unexpected agent containers: %v", agentContainers)
	}
	ipsec := agentContainers[2]
	if ipsec.Name != "antrea-ipsec" || ipsec.Command[0] != "start_ovs_ipsec" || ipsec.Image != install.Spec.AntreaAgentImage || ipsec.ReadinessProbe == nil {
		t.Fatalf("unexpected IPsec sidecar: %v", ipsec)
	}
	pskEnv := agentContainers[0].Env[len(agentContainers[0].Env)-1]
	if pskEnv.Name != "ANTREA_IPSEC_PSK" || pskEnv.ValueFrom.SecretKeyRef.Name != types.ManagedIPsecPSKName || pskEnv.ValueFrom.SecretKeyRef.Key != types.IPsecPSKKey {
		t.Fatalf("unexpected PSK reference: %v", pskEnv)
	}
	if image := controller.Spec.Template.Spec.Containers[0].Image; image != install.Spec.AntreaControllerImage {
		t.Fatalf("controller image = %s", image)
	}
	for _, volume := range agent.Spec.Template.Spec.Volumes {
		if volume.Name == "host-cni-conf" && volume.HostPath.Path != network.SystemCNIConfDir {
			t.Errorf("agent uses the wrong CNI configuration directory: %s", volume.HostPath.Path)
		}
	}
	for _, obj := range objects {
		bits, err := json.Marshal(obj.Object)
		if err != nil || strings.Contains(string(bits), fixtureKey) {
			t.Fatalf("fixture credential appeared outside the Secret in %s", obj.GetName())
		}
	}
	secret := &corev1.Secret{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(operands.managedPSK.Object, secret); err != nil {
		t.Fatal(err)
	}
	if string(secret.Data[types.IPsecPSKKey]) != fixtureKey || secret.Namespace != cfg.Namespace() {
		t.Fatal("expected managed PSK Secret in the selected namespace")
	}

	// Exercise an external credential and its rollout revision without
	// owning the Secret or putting its key in the DaemonSet.
	install.Spec.PSK = ""
	install.Spec.IPsecPSKSecretName = "fixture-secret"
	objects, err = render.RenderDir(filepath.Join("..", types.DefaultManifestDir), data)
	if err != nil {
		t.Fatal(err)
	}
	operands, err = r.prepareOpenShiftOperands(objects, cfg, cluster, install)
	if err != nil || operands.managedPSK != nil {
		t.Fatalf("external credential produced a managed Secret: %v", err)
	}
	r.Client = cnofake.NewFakeClient()
	if err := r.validateIPsecCredential(operands); err == nil {
		t.Fatal("missing credential was accepted")
	}
	secret.Name = "fixture-secret"
	secret.UID = "fixture-uid"
	secret.ResourceVersion = "30"
	r.Client = cnofake.NewFakeClient(secret)
	if err := r.validateIPsecCredential(operands); err != nil {
		t.Fatal(err)
	}
	if err := r.stampIPsecCredential(operands); err != nil {
		t.Fatal(err)
	}
	revision, _, _ := unstructured.NestedString(operands.agent.Object, "spec", "template", "metadata", "annotations", ipsecCredentialRevision)
	if revision != "fixture-uid/30" {
		t.Errorf("unexpected credential revision %q", revision)
	}

	// A distribution callback can extend agent options. A change that
	// removes explicit protection must fail before any object is applied.
	install.Spec.IPsecPSKSecretName = ""
	data.Data[types.AntreaAgentConfigRenderKey] = "trafficEncryptionMode: none\ndefaultMTU: 1450\n"
	objects, err = render.RenderDir(filepath.Join("..", types.DefaultManifestDir), data)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.prepareOpenShiftOperands(objects, cfg, cluster, install); err == nil || !strings.Contains(err.Error(), "does not preserve") {
		t.Fatalf("plaintext bundled config was accepted for an explicitly encrypted request: %v", err)
	}
}

func TestOpenShiftNetworkStatusPreservesForeignFields(t *testing.T) {
	cluster := &configv1.Network{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster"},
		Spec: configv1.NetworkSpec{
			NetworkType: "antrea", ServiceNetwork: []string{"172.30.0.0/16"},
			ClusterNetwork: []configv1.ClusterNetworkEntry{{CIDR: "10.128.0.0/14", HostPrefix: 23}},
		},
		Status: configv1.NetworkStatus{
			Migration:  &configv1.NetworkMigration{NetworkType: "Other"},
			Conditions: []metav1.Condition{{Type: "NetworkDiagnosticsAvailable", Status: metav1.ConditionTrue}},
		},
	}
	scheme := runtime.NewScheme()
	if err := configv1.Install(scheme); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	store := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cluster).Build()
	input := &configv1.Network{}
	if err := store.Get(ctx, client.ObjectKeyFromObject(cluster), input); err != nil {
		t.Fatal(err)
	}
	if err := updateNetworkStatus(store, input, 1400); err != nil {
		t.Fatal(err)
	}
	if err := store.Get(ctx, client.ObjectKeyFromObject(cluster), input); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(input.Spec, cluster.Spec) ||
		!reflect.DeepEqual(input.Status.Conditions, cluster.Status.Conditions) ||
		!reflect.DeepEqual(input.Status.Migration, cluster.Status.Migration) ||
		input.Status.ClusterNetworkMTU != 1400 || input.Status.NetworkType != "antrea" {
		t.Fatalf("unexpected network object after reconcile: %+v", input)
	}
}
