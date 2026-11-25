// SPDX-License-Identifier: Apache-2.0

package flow

import (
	"reflect"
	"slices"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/yaml"

	operatorv1 "antrea.io/antrea-operator/api/v1"
	"antrea.io/antrea-operator/controllers/types"
)

func TestLifecycleRender(t *testing.T) {
	const ns = "antrea-core"
	const original = "trafficEncryptionMode: ipsec\nfeatureGates:\n  Egress: true\nflowExporter:\n  enable: false\n  flowPollInterval: 2s\n"
	spec := &operatorv1.FlowAggregatorSpec{Enabled: true, CollectorAddress: "receiver-a.example:4739"}
	render := func() (string, map[string]*unstructured.Unstructured) {
		t.Helper()
		values := map[string]interface{}{ImageKey: "registry.test/flow:dist", types.AntreaAgentConfigRenderKey: original}
		objs, err := Render(spec, ns, values)
		if err != nil {
			t.Fatal(err)
		}
		out := make(map[string]*unstructured.Unstructured)
		for _, obj := range objs {
			key := obj.GetKind() + "/" + obj.GetName()
			if out[key] != nil || obj.GetLabels()[ComponentLabel] != Name {
				t.Fatalf("bad identity: %s", key)
			}
			out[key] = obj
			if namespace := obj.GetNamespace(); namespace != ns && namespace != "" && key != "RoleBinding/flow-aggregator-authentication-reader" {
				t.Fatalf("%s namespace = %q", key, namespace)
			}
		}
		return values[types.AntreaAgentConfigRenderKey].(string), out
	}
	var previousHash string
	for _, mode := range []string{"Aggregate", "Proxy", "Aggregate"} {
		if previousHash != "" {
			spec.Mode = mode
			spec.PodLabels = !spec.PodLabels
			spec.CollectorAddress = "receiver-b.example:4739"
			spec.Image = "registry.test/flow:override"
		}
		agent, objs := render()
		var settings map[string]interface{}
		if err := yaml.Unmarshal([]byte(agent), &settings); err != nil {
			t.Fatal(err)
		}
		if got, _, _ := unstructured.NestedBool(settings, "featureGates", "FlowExporter"); !got || settings["trafficEncryptionMode"] != "ipsec" {
			t.Fatal(agent)
		}
		if got, _, _ := unstructured.NestedString(settings, "flowExporter", "flowPollInterval"); got != "2s" {
			t.Fatal(agent)
		}
		refs, err := OwnedResources(ns)
		if err != nil || len(objs) != len(refs) {
			t.Fatal("untracked optional resource")
		}
		dest := objs["FlowExporterDestination/operator-flow-aggregator"].Object
		for _, key := range []string{"caConfigMap", "clientSecret"} {
			if got, _, _ := unstructured.NestedString(dest, "spec", "tlsConfig", key, "namespace"); got != ns {
				t.Fatal(dest)
			}
		}
		if got, _, _ := unstructured.NestedString(dest, "spec", "address"); got != ns+"/flow-aggregator:14739" {
			t.Fatal(dest)
		}
		if _, ok, _ := unstructured.NestedMap(dest, "spec", "protocol", "grpc"); !ok {
			t.Fatal(dest)
		}
		cm := corev1.ConfigMap{}
		convert(t, objs["ConfigMap/flow-aggregator-config"], &cm)
		var cfg map[string]interface{}
		if err := yaml.UnmarshalStrict([]byte(cm.Data["flow-aggregator.conf"]), &cfg); err != nil {
			t.Fatal(err)
		}
		address, _, _ := unstructured.NestedString(cfg, "flowCollector", "address")
		labels, _, _ := unstructured.NestedBool(cfg, "recordContents", "podLabels")
		if cfg["mode"] != mode || cfg["aggregatorTransportProtocol"] != "none" || address != spec.CollectorAddress || labels != spec.PodLabels {
			t.Fatal(cfg)
		}
		dep := appsv1.Deployment{}
		convert(t, objs["Deployment/flow-aggregator"], &dep)
		tpl := dep.Spec.Template
		hash := tpl.Annotations["operator.antrea.io/flow-config-hash"]
		if hash == "" || hash == previousHash || *dep.Spec.Replicas != 1 || dep.Spec.Strategy.Type != appsv1.RecreateDeploymentStrategyType {
			t.Fatal(dep.Spec)
		}
		previousHash = hash
		container := tpl.Spec.Containers[0]
		if !slices.ContainsFunc(container.Env, func(env corev1.EnvVar) bool {
			return env.Name == "ANTREA_CLUSTER_IDENTITY_NAMESPACE" && env.Value == ns && env.ValueFrom == nil
		}) {
			t.Fatalf("missing cluster identity namespace %q in %v", ns, container.Env)
		}
		if container.Image != "registry.test/flow:dist" && container.Image != spec.Image {
			t.Fatal(container.Image)
		}
		if spec.Image != "" && container.Image != spec.Image {
			t.Fatal(container.Image)
		}
		if container.ReadinessProbe.TCPSocket.Port.StrVal != "grpc" || container.Ports[0].ContainerPort != 14739 || tpl.Spec.ServiceAccountName != Name {
			t.Fatal(container)
		}
		var svc corev1.Service
		convert(t, objs["Service/flow-aggregator"], &svc)
		if svc.Spec.Ports[0].Port != 14739 || svc.Spec.Selector[ComponentLabel] != tpl.Labels[ComponentLabel] {
			t.Fatal(svc.Spec)
		}
		for _, name := range []string{"flow-aggregator-ca-tls", "flow-aggregator-client-tls"} {
			if _, ok := objs["Secret/"+name].Object["data"]; ok {
				t.Fatal("operator owns provider's data")
			}
		}
		var role rbacv1.Role
		convert(t, objs["Role/flow-aggregator-certificates"], &role)
		for _, rule := range role.Rules {
			if len(rule.ResourceNames) == 0 || strings.Contains(strings.Join(rule.Verbs, ","), "create") {
				t.Fatal(rule)
			}
		}
	}
	spec.Enabled = false
	agent, objs := render()
	if agent != original || len(objs) != 0 {
		t.Fatal("disable did not restore requested raw config")
	}
}

func convert(t *testing.T, obj *unstructured.Unstructured, out interface{}) {
	t.Helper()
	if obj == nil {
		t.Fatalf("missing manifest for %T", out)
	}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(obj.Object, out); err != nil {
		t.Fatal(err)
	}
}

func TestInvalidSettingsAndStaticExporter(t *testing.T) {
	for _, address := range []string{"", "receiver", "10.0.0.1:0", "[::1]:4739", "collector:65536", "10.0.0.901:4739", "a:99:udp"} {
		if _, err := Render(&operatorv1.FlowAggregatorSpec{Enabled: true, Image: "fa:test", CollectorAddress: address}, "core", nil); err == nil {
			t.Errorf("accepted address %q", address)
		}
	}
	for _, s := range []operatorv1.FlowAggregatorSpec{{Enabled: true, CollectorAddress: "a:4"}, {Enabled: true, Image: "fa:test", CollectorAddress: "a:4", Mode: "invalid"}} {
		if _, err := Render(&s, "core", map[string]interface{}{types.AntreaAgentConfigRenderKey: "{}"}); err == nil {
			t.Errorf("accepted %+v", s)
		}
	}
	enabled := operatorv1.FlowAggregatorSpec{Enabled: true, Image: "fa:test", CollectorAddress: "receiver.example:4739"}
	values := map[string]interface{}{types.AntreaAgentConfigRenderKey: "featureGates:\nflowExporter:\n  enable: true\n  flowCollectorAddr: existing:4739:tcp"}
	if _, err := Render(&enabled, "on", values); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(values[types.AntreaAgentConfigRenderKey].(string), "existing:4739:tcp") || !strings.Contains(values[types.AntreaAgentConfigRenderKey].(string), "enable: true") {
		t.Fatal(values)
	}
	crds, err := EnsureDestinationCRD(nil)
	if err != nil || len(crds) != 1 || crds[0].GetName() != DestinationCRD {
		t.Fatalf("%v %v", crds, err)
	}
	again, err := EnsureDestinationCRD(crds)
	if err != nil || !reflect.DeepEqual(crds, again) {
		t.Fatal("core CRD duplicated")
	}
}
