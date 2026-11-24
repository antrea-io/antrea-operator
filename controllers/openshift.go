// SPDX-License-Identifier: Apache-2.0

package controllers

import (
	"context"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"antrea.io/antrea-operator/controllers/types"
)

// A second agent DaemonSet could serve the same nodes just as soon as the
// new controller applied it. Require handover before changing namespaces.
func (r *AntreaInstallReconciler) checkOpenShiftNamespace() error {
	ctx := context.TODO()
	reader := r.Client.Default().CRClient()
	namespace := r.coreNamespace()
	if err := reader.Get(ctx, client.ObjectKey{Name: namespace}, &corev1.Namespace{}); err != nil {
		return fmt.Errorf("checking for the selected core namespace %q: %w", namespace, err)
	}
	agents := &appsv1.DaemonSetList{}
	if err := reader.List(ctx, agents, client.MatchingFields{"metadata.name": types.AntreaAgentDaemonSetName}); err != nil {
		return fmt.Errorf("checking for existing Antrea agents: %w", err)
	}
	for _, ds := range agents.Items {
		if ds.Namespace != namespace {
			return fmt.Errorf("Antrea agent DaemonSet exists in %q; complete the namespace handover before deploying core in %q", ds.Namespace, namespace)
		}
	}
	controllers := &appsv1.DeploymentList{}
	if err := reader.List(ctx, controllers, client.MatchingFields{"metadata.name": types.AntreaControllerDeploymentName}); err != nil {
		return fmt.Errorf("checking for existing Antrea controllers: %w", err)
	}
	for _, deployment := range controllers.Items {
		if deployment.Namespace != namespace {
			return fmt.Errorf("Antrea controller Deployment exists in %q; complete the namespace handover before deploying core in %q", deployment.Namespace, namespace)
		}
	}
	for _, component := range []string{types.AntreaAgentDaemonSetName, types.AntreaControllerDeploymentName} {
		pods := &corev1.PodList{}
		if err := reader.List(ctx, pods, client.MatchingLabels{"app": "antrea", "component": component}); err != nil {
			return fmt.Errorf("checking for existing %s Pods: %w", component, err)
		}
		for _, pod := range pods.Items {
			if pod.Namespace != namespace {
				return fmt.Errorf("Antrea Pod %s/%s remains from the previous namespace; complete the handover before deploying core in %q", pod.Namespace, pod.Name, namespace)
			}
		}
	}
	return nil
}
