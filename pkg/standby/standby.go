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
	"time"

	"github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const (
	ConfigMapName       = "etcd-sync"
	DefaultRequeueAfter = 30 * time.Second
	GlobalClusterName   = "global"
)

var (
	ConfigMapNamespace = "kube-public"
	RequeueAfter       = DefaultRequeueAfter
)

type Detector interface {
	IsStandby(ctx context.Context) (bool, error)
}

type ConfigMapDetector struct {
	Client client.Reader
}

type ClusterNameGetter func(client.Object) string

type guardedReconciler struct {
	detector       Detector
	controllerName string
	inner          reconcile.Reconciler
}

type guardedClusterNamedReconciler struct {
	detector       Detector
	reader         client.Reader
	controllerName string
	newObject      func() client.Object
	getClusterName ClusterNameGetter
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

func ClusterNameFromLabel(obj client.Object) string {
	return obj.GetLabels()[clusterv1.ClusterNameLabel]
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

func WrapClusterNamedReconciler(detector Detector, reader client.Reader, controllerName string, newObject func() client.Object, getClusterName ClusterNameGetter, inner reconcile.Reconciler) reconcile.Reconciler {
	return &guardedClusterNamedReconciler{
		detector:       detector,
		reader:         reader,
		controllerName: controllerName,
		newObject:      newObject,
		getClusterName: getClusterName,
		inner:          inner,
	}
}

func WrapClusterNamedReconcilerWithConfigMapDetector(reader client.Reader, controllerName string, newObject func() client.Object, getClusterName ClusterNameGetter, inner reconcile.Reconciler) reconcile.Reconciler {
	return WrapClusterNamedReconciler(NewConfigMapDetector(reader), reader, controllerName, newObject, getClusterName, inner)
}

func (r *guardedReconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	isStandby, err := r.detector.IsStandby(ctx)
	if err != nil {
		// Fail closed: if standby state cannot be determined, do not mutate infrastructure.
		return reconcile.Result{}, err
	}
	if isStandby {
		logSkip(ctx, r.controllerName, "")
		return reconcile.Result{RequeueAfter: RequeueAfter}, nil
	}
	return r.inner.Reconcile(ctx, req)
}

func (r *guardedClusterNamedReconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	isStandby, err := r.detector.IsStandby(ctx)
	if err != nil {
		return reconcile.Result{}, err
	}
	if !isStandby {
		return r.inner.Reconcile(ctx, req)
	}

	obj := r.newObject()
	if err := r.reader.Get(ctx, req.NamespacedName, obj); err != nil {
		if apierrors.IsNotFound(err) {
			return r.inner.Reconcile(ctx, req)
		}
		return reconcile.Result{}, err
	}

	clusterName := r.getClusterName(obj)
	if clusterName == GlobalClusterName {
		return r.inner.Reconcile(ctx, req)
	}
	logSkip(ctx, r.controllerName, clusterName)
	return reconcile.Result{RequeueAfter: RequeueAfter}, nil
}

func logSkip(ctx context.Context, controllerName, clusterName string) {
	logValues := []any{
		"controller", controllerName,
		"configMap", klog.KRef(ConfigMapNamespace, ConfigMapName),
		"requeueAfter", RequeueAfter,
	}
	if clusterName != "" {
		logValues = append(logValues, "cluster", clusterName)
	}
	ctrl.LoggerFrom(ctx).V(2).Info("Skipping reconciliation because management cluster is DR standby", logValues...)
}
