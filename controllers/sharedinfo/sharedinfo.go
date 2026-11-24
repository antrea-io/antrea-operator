/* Copyright © 2020 VMware, Inc. All Rights Reserved.
   SPDX-License-Identifier: Apache-2.0 */

package sharedinfo

import (
	"context"
	"errors"
	"sync"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	"antrea.io/antrea-operator/controllers/install"
	operatortypes "antrea.io/antrea-operator/controllers/types"
)

var log = logf.Log.WithName("shared_info")

type SharedInfo struct {
	sync.Mutex

	AntreaPlatform                 string
	AntreaAgentDaemonSetSpec       *unstructured.Unstructured
	AntreaControllerDeploymentSpec *unstructured.Unstructured
}

func New(mgr manager.Manager) (*SharedInfo, error) {
	return NewForInstall(context.Background(), mgr, schema.GroupVersionKind{})
}

func NewForInstall(ctx context.Context, mgr manager.Manager, gvk schema.GroupVersionKind) (*SharedInfo, error) {
	obj, err := install.Read(ctx, mgr.GetAPIReader(), gvk)
	if err != nil {
		return nil, err
	}
	antreaInstall, err := install.CoreConfig(obj)
	if err != nil {
		log.Error(err, "failed to get antrea-install", "namespace", operatortypes.OperatorNameSpace, "name", operatortypes.OperatorConfigName)
		return nil, err
	}
	switch antreaInstall.Spec.AntreaPlatform {
	case "openshift", "kubernetes":
		return &SharedInfo{AntreaPlatform: antreaInstall.Spec.AntreaPlatform}, nil
	default:
		return nil, errors.New("invalid platform: platform should be openshift or kubernetes")
	}
}
