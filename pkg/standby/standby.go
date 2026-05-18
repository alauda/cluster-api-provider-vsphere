/*
Copyright 2026 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package standby

import (
	"context"

	"github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const (
	DefaultConfigMapNamespace = "cpaas-system"
	ConfigMapName             = "etcd-sync"
)

var ConfigMapNamespace = DefaultConfigMapNamespace

type Detector interface {
	IsStandby(ctx context.Context) (bool, error)
}

type ConfigMapDetector struct {
	Client client.Reader
}

type guardedReconciler struct {
	detector       Detector
	controllerName string
	inner          reconcile.Reconciler
}

func NewConfigMapDetector(reader client.Reader) *ConfigMapDetector {
	return &ConfigMapDetector{Client: reader}
}

func (d *ConfigMapDetector) IsStandby(ctx context.Context) (bool, error) {
	configMap := &corev1.ConfigMap{}
	configMapKey := types.NamespacedName{Namespace: ConfigMapNamespace, Name: ConfigMapName}
	if err := d.Client.Get(ctx, configMapKey, configMap); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, errors.Wrapf(err, "failed to check management cluster standby ConfigMap %s", configMapKey.String())
	}
	return true, nil
}

func WrapReconciler(detector Detector, controllerName string, inner reconcile.Reconciler) reconcile.Reconciler {
	return &guardedReconciler{
		detector:       detector,
		controllerName: controllerName,
		inner:          inner,
	}
}

func WrapWithConfigMapDetector(reader client.Reader, controllerName string, inner reconcile.Reconciler) reconcile.Reconciler {
	return WrapReconciler(NewConfigMapDetector(reader), controllerName, inner)
}

func (r *guardedReconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	isStandby, err := r.detector.IsStandby(ctx)
	if err != nil {
		// Fail closed: if standby state cannot be determined, do not mutate infrastructure.
		return reconcile.Result{}, err
	}
	if isStandby {
		ctrl.LoggerFrom(ctx).V(2).Info(
			"Skipping reconciliation because management cluster is DR standby",
			"controller", r.controllerName,
			"configMap", klog.KRef(ConfigMapNamespace, ConfigMapName),
		)
		return reconcile.Result{}, nil
	}
	return r.inner.Reconcile(ctx, req)
}
