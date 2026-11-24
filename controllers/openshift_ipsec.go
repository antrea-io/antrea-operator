// SPDX-License-Identifier: Apache-2.0

package controllers

import (
	"context"
	"fmt"
	"strings"

	configv1 "github.com/openshift/api/config/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/strategicpatch"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	operatorv1 "antrea.io/antrea-operator/api/v1"
	"antrea.io/antrea-operator/controllers/config"
	"antrea.io/antrea-operator/controllers/statusmanager"
	"antrea.io/antrea-operator/controllers/types"
)

const ipsecCredentialRevision = "operator.antrea.io/ipsec-credential-revision"

type openShiftOperands struct {
	agent, controller, configMap *unstructured.Unstructured
	managedPSK                   *unstructured.Unstructured
	encryption                   config.EncryptionProfile
}

func (r *AntreaInstallReconciler) prepareOpenShiftOperands(objects []*unstructured.Unstructured, cfg *config.ConfigOc, cluster *configv1.Network, operConfig *operatorv1.AntreaInstall) (openShiftOperands, error) {
	var operands openShiftOperands
	namespace := r.coreNamespace()
	for _, obj := range objects {
		if obj.GetNamespace() != namespace {
			continue
		}
		switch obj.GroupVersionKind() {
		case appsv1.SchemeGroupVersion.WithKind("DaemonSet"):
			if obj.GetName() == types.AntreaAgentDaemonSetName {
				operands.agent = obj
			}
		case appsv1.SchemeGroupVersion.WithKind("Deployment"):
			if obj.GetName() == types.AntreaControllerDeploymentName {
				operands.controller = obj
			}
		case corev1.SchemeGroupVersion.WithKind("ConfigMap"):
			if obj.GetName() == types.AntreaConfigMapName {
				operands.configMap = obj
			}
		}
	}
	if operands.agent == nil || operands.controller == nil || operands.configMap == nil {
		return operands, fmt.Errorf("OpenShift bundle requires antrea-agent, antrea-controller and antrea-config in %s", namespace)
	}
	expected, err := cfg.Encryption(operConfig)
	if err != nil {
		return operands, err
	}
	operands.encryption = expected
	if err := verifyOpenShiftConfigMap(cfg, cluster, operConfig, operands.configMap, expected); err != nil {
		return operands, err
	}
	if err := verifyCoreImage(operands.controller, "antrea-controller"); err != nil {
		return operands, err
	}
	if err := verifyCoreImage(operands.agent, "antrea-agent"); err != nil {
		return operands, err
	}
	for _, obj := range objects {
		if obj.GroupVersionKind() == corev1.SchemeGroupVersion.WithKind("Secret") && obj.GetNamespace() == namespace &&
			(obj.GetName() == expected.PSKSecretName || obj.GetName() == types.ManagedIPsecPSKName) {
			return operands, fmt.Errorf("OpenShift bundle cannot render IPsec credential Secret %s/%s", namespace, obj.GetName())
		}
	}
	if err := configureIPsecAgent(operands.agent, expected); err != nil {
		return operands, err
	}
	if expected.PSKSecretName == types.ManagedIPsecPSKName {
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: expected.PSKSecretName, Namespace: namespace},
			Type:       corev1.SecretTypeOpaque,
			Data:       map[string][]byte{types.IPsecPSKKey: []byte(operConfig.Spec.PSK)},
		}
		fields, err := runtime.DefaultUnstructuredConverter.ToUnstructured(secret)
		if err != nil {
			return operands, fmt.Errorf("could not render the managed IPsec credential Secret")
		}
		operands.managedPSK = &unstructured.Unstructured{Object: fields}
		operands.managedPSK.SetGroupVersionKind(corev1.SchemeGroupVersion.WithKind("Secret"))
	}
	return operands, nil
}

func verifyOpenShiftConfigMap(cfg *config.ConfigOc, cluster *configv1.Network, operConfig *operatorv1.AntreaInstall, cm *unstructured.Unstructured, expected config.EncryptionProfile) error {
	data, _, err := unstructured.NestedStringMap(cm.Object, "data")
	if err != nil {
		return fmt.Errorf("invalid rendered antrea-config: %w", err)
	}
	rendered := operConfig.DeepCopy()
	rendered.Spec.AntreaAgentConfig = data[types.AntreaAgentConfigOption]
	rendered.Spec.AntreaControllerConfig = data[types.AntreaControllerConfigOption]
	rendered.Spec.AntreaCNIConfig = data[types.AntreaCNIConfigOption]
	actual, err := cfg.Encryption(rendered)
	if err != nil {
		return fmt.Errorf("invalid rendered Antrea encryption config: %w", err)
	}
	if actual != expected {
		return fmt.Errorf("rendered antrea-config does not preserve the requested encryption mode and authentication")
	}
	if err := cfg.ValidateConfig(cluster, rendered); err != nil {
		return fmt.Errorf("invalid rendered antrea-config: %w", err)
	}
	return nil
}

func verifyCoreImage(obj *unstructured.Unstructured, name string) error {
	containers, _, err := unstructured.NestedSlice(obj.Object, "spec", "template", "spec", "containers")
	if err != nil {
		return fmt.Errorf("invalid %s container list: %w", name, err)
	}
	for _, raw := range containers {
		container, ok := raw.(map[string]interface{})
		if !ok || container["name"] != name {
			continue
		}
		image, _ := container["image"].(string)
		if image != "" && !strings.ContainsAny(image, " \t\r\n") {
			return nil
		}
	}
	return fmt.Errorf("%s requires an explicit usable image", name)
}

func configureIPsecAgent(obj *unstructured.Unstructured, profile config.EncryptionProfile) error {
	podSpec, _, err := unstructured.NestedMap(obj.Object, "spec", "template", "spec")
	if err != nil {
		return err
	}
	var spec corev1.PodSpec
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(podSpec, &spec); err != nil {
		return fmt.Errorf("invalid Antrea agent Pod template: %w", err)
	}
	var agent, ovs *corev1.Container
	for i := range spec.Containers {
		container := &spec.Containers[i]
		switch container.Name {
		case "antrea-agent":
			agent = container
		case "antrea-ovs":
			ovs = container
		case "antrea-ipsec":
			return fmt.Errorf("the OpenShift core bundle delegates the antrea-ipsec container to the operator")
		}
	}
	if profile.Mode != config.EncryptionIPsec {
		return nil
	}
	if agent == nil || ovs == nil || ovs.Image != agent.Image || !spec.HostNetwork {
		return fmt.Errorf("OpenShift IPsec requires host networking and the same IPsec agent image for antrea-agent and antrea-ovs")
	}
	var runMount, logMount corev1.VolumeMount
	for _, mount := range ovs.VolumeMounts {
		switch mount.MountPath {
		case "/var/run/openvswitch":
			runMount = mount
		case "/var/log/openvswitch":
			logMount = mount
		}
	}
	if runMount.Name == "" || logMount.Name == "" {
		return fmt.Errorf("OpenShift IPsec requires shared OVS run and log volumes")
	}
	strongSwanLog := logMount
	strongSwanLog.MountPath = "/var/log/strongswan"
	strongSwanLog.SubPath = "strongswan"
	probe := corev1.Probe{
		ProbeHandler:        corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: []string{"/bin/sh", "-c", "timeout 5 container_liveness_probe ovs-ipsec"}}},
		InitialDelaySeconds: 5,
		PeriodSeconds:       5,
		TimeoutSeconds:      5,
	}
	sidecar := corev1.Container{
		Name:            "antrea-ipsec",
		Image:           agent.Image,
		ImagePullPolicy: agent.ImagePullPolicy,
		Command:         []string{"start_ovs_ipsec"},
		SecurityContext: &corev1.SecurityContext{Capabilities: &corev1.Capabilities{Add: []corev1.Capability{"NET_ADMIN"}}},
		LivenessProbe:   probe.DeepCopy(),
		ReadinessProbe:  probe.DeepCopy(),
		VolumeMounts:    []corev1.VolumeMount{runMount, logMount, strongSwanLog},
	}
	switch profile.IPsecAuth {
	case config.IPsecPSK:
		for _, env := range agent.Env {
			if env.Name == "ANTREA_IPSEC_PSK" {
				return fmt.Errorf("the OpenShift core bundle delegates ANTREA_IPSEC_PSK to the operator")
			}
		}
		agent.Env = append(agent.Env, corev1.EnvVar{
			Name: "ANTREA_IPSEC_PSK",
			ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: profile.PSKSecretName}, Key: types.IPsecPSKKey,
			}},
		})
	case config.IPsecCert:
		caMount := corev1.VolumeMount{Name: types.IPsecCAName, MountPath: "/var/run/openvswitch/ca", ReadOnly: true}
		agent.VolumeMounts = append(agent.VolumeMounts, caMount)
		caMount.MountPath = "/etc/ipsec.d/cacerts"
		sidecar.VolumeMounts = append(sidecar.VolumeMounts, caMount)
		spec.Volumes = append(spec.Volumes, corev1.Volume{
			Name: types.IPsecCAName,
			VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: types.IPsecCAName}, Optional: ptr.To(true),
			}},
		})
	}
	// Keep any distribution-specific fields in the rendered Pod spec when
	// merging the known Antrea container and volume changes.
	spec.Containers = append(spec.Containers, sidecar)
	fields, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&spec)
	if err != nil {
		return err
	}
	patch := map[string]interface{}{"containers": fields["containers"]}
	if profile.IPsecAuth == config.IPsecCert {
		patch["volumes"] = fields["volumes"]
	}
	merged, err := strategicpatch.StrategicMergeMapPatch(podSpec, patch, corev1.PodSpec{})
	if err != nil {
		return err
	}
	return unstructured.SetNestedMap(obj.Object, merged, "spec", "template", "spec")
}

func (r *AntreaInstallReconciler) readIPsecCredential(name string) (*corev1.Secret, error) {
	secret := &corev1.Secret{}
	key := client.ObjectKey{Namespace: r.coreNamespace(), Name: name}
	if err := r.Client.Default().CRClient().Get(context.TODO(), key, secret); err != nil {
		return nil, err
	}
	return secret, nil
}

func (r *AntreaInstallReconciler) validateIPsecCredential(operands openShiftOperands) error {
	name := operands.encryption.PSKSecretName
	if name == "" {
		return nil
	}
	secret, err := r.readIPsecCredential(name)
	if apierrors.IsNotFound(err) && operands.managedPSK != nil {
		return nil
	}
	if err != nil {
		return fmt.Errorf("could not read IPsec credential Secret %s/%s: %w", r.coreNamespace(), name, err)
	}
	if operands.managedPSK != nil && secret.Labels[statusmanager.OperandLabel] != statusmanager.CoreOperand {
		return fmt.Errorf("IPsec credential Secret %s/%s exists without core ownership", r.coreNamespace(), name)
	}
	if operands.managedPSK == nil {
		if secret.Labels[statusmanager.OperandLabel] == statusmanager.CoreOperand {
			return fmt.Errorf("external IPsec credential Secret %s/%s is marked for core ownership", r.coreNamespace(), name)
		}
		if !config.ValidIPsecPSK(string(secret.Data[types.IPsecPSKKey])) {
			return fmt.Errorf("IPsec credential Secret %s/%s needs a nonblank psk key without control characters", r.coreNamespace(), name)
		}
	}
	return nil
}

func (r *AntreaInstallReconciler) stampIPsecCredential(operands openShiftOperands) error {
	if name := operands.encryption.PSKSecretName; name != "" {
		secret, err := r.readIPsecCredential(name)
		if err != nil {
			return fmt.Errorf("could not read IPsec credential revision for %s/%s: %w", r.coreNamespace(), name, err)
		}
		if !config.ValidIPsecPSK(string(secret.Data[types.IPsecPSKKey])) {
			return fmt.Errorf("invalid IPsec credential in Secret %s/%s", r.coreNamespace(), name)
		}
		revision := string(secret.UID) + "/" + secret.ResourceVersion
		return unstructured.SetNestedField(operands.agent.Object, revision, "spec", "template", "metadata", "annotations", ipsecCredentialRevision)
	}
	return nil
}
