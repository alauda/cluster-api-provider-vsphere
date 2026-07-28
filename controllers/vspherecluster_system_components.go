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

package controllers

import (
	"context"
	"path"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	bootstrapv1 "sigs.k8s.io/cluster-api/bootstrap/kubeadm/api/v1beta1"
	controlplanev1 "sigs.k8s.io/cluster-api/controlplane/kubeadm/api/v1beta1"
	containerutil "sigs.k8s.io/cluster-api/util/container"
	"sigs.k8s.io/cluster-api/util/patch"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	capvcontext "sigs.k8s.io/cluster-api-provider-vsphere/pkg/context"
)

const systemComponentRequeueAfter = 10 * time.Second

// reconcileKubeadmControlPlaneSystemComponents configures CoreDNS before the
// workload cluster exists and prevents KCP from overwriting the runtime
// objects reconciled by this provider during upgrades.
func (r *clusterReconciler) reconcileKubeadmControlPlaneSystemComponents(ctx context.Context, clusterCtx *capvcontext.ClusterContext) error {
	cluster := clusterCtx.Cluster
	if cluster == nil || cluster.Spec.ControlPlaneRef == nil || cluster.Spec.ControlPlaneRef.Kind != "KubeadmControlPlane" {
		return nil
	}

	registry := cluster.Annotations["cpaas.io/registry-address"]
	if registry == "" {
		ctrl.LoggerFrom(ctx).V(4).Info("Skipping KubeadmControlPlane system component reconcile because registry annotation is missing")
		return nil
	}

	ref := cluster.Spec.ControlPlaneRef
	namespace := ref.Namespace
	if namespace == "" {
		namespace = cluster.Namespace
	}
	kcp := &controlplanev1.KubeadmControlPlane{}
	if err := r.Client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: ref.Name}, kcp); err != nil {
		if apierrors.IsNotFound(err) {
			ctrl.LoggerFrom(ctx).V(4).Info("Skipping KubeadmControlPlane system component reconcile because KubeadmControlPlane is not available")
			return nil
		}
		return err
	}

	dnsImageRepository := path.Join(registry, coreDNSImageRepositoryForTag(coreDNSImageTag(kcp)))
	originalKCP := kcp.DeepCopy()
	changed := false
	if kcp.Spec.KubeadmConfigSpec.ClusterConfiguration == nil {
		kcp.Spec.KubeadmConfigSpec.ClusterConfiguration = &bootstrapv1.ClusterConfiguration{}
	}
	if kcp.Spec.KubeadmConfigSpec.ClusterConfiguration.DNS.ImageRepository != dnsImageRepository {
		kcp.Spec.KubeadmConfigSpec.ClusterConfiguration.DNS.ImageRepository = dnsImageRepository
		changed = true
	}

	if kcp.Annotations == nil {
		kcp.Annotations = map[string]string{}
	}
	for _, annotation := range []string{controlplanev1.SkipCoreDNSAnnotation, controlplanev1.SkipKubeProxyAnnotation} {
		if _, ok := kcp.Annotations[annotation]; !ok {
			kcp.Annotations[annotation] = "true"
			changed = true
		}
	}
	if !changed {
		return nil
	}

	patchHelper, err := patch.NewHelper(originalKCP, r.Client)
	if err != nil {
		return err
	}
	if err := patchHelper.Patch(ctx, kcp); err != nil {
		return err
	}
	ctrl.LoggerFrom(ctx).Info("Reconciled KubeadmControlPlane system component settings",
		"coreDNSImageRepository", dnsImageRepository,
		"coreDNSImageTag", coreDNSImageTag(kcp))
	return nil
}

func (r *clusterReconciler) reconcileWorkloadSystemComponentRepositories(ctx context.Context, clusterCtx *capvcontext.ClusterContext) (reconcile.Result, error) {
	cluster := clusterCtx.Cluster
	if cluster == nil || cluster.Spec.ControlPlaneRef == nil || cluster.Spec.ControlPlaneRef.Kind != "KubeadmControlPlane" {
		return reconcile.Result{}, nil
	}
	if cluster.Annotations["cpaas.io/registry-address"] == "" {
		return reconcile.Result{}, nil
	}

	kcp, err := r.kubeadmControlPlaneForCluster(ctx, cluster)
	if err != nil {
		return reconcile.Result{}, err
	}
	if kcp == nil || !kcp.DeletionTimestamp.IsZero() {
		return reconcile.Result{}, nil
	}

	_, restConfig, err := r.newRemoteClients(ctx, cluster)
	if err != nil {
		ctrl.LoggerFrom(ctx).Error(err, "Skipping workload system component reconcile because workload cluster client is unavailable")
		return reconcile.Result{RequeueAfter: systemComponentRequeueAfter}, nil
	}
	remoteClient, err := client.New(restConfig, client.Options{Scheme: r.Client.Scheme()})
	if err != nil {
		return reconcile.Result{}, err
	}

	if err := r.reconcileKubeProxyRepository(ctx, cluster, kcp, remoteClient); err != nil {
		return reconcile.Result{}, err
	}
	if err := r.reconcileCoreDNSRepository(ctx, cluster, kcp, remoteClient); err != nil {
		return reconcile.Result{}, err
	}
	return reconcile.Result{}, nil
}

func (r *clusterReconciler) reconcileKubeProxyRepository(ctx context.Context, cluster *clusterv1.Cluster, kcp *controlplanev1.KubeadmControlPlane, remoteClient client.Client) error {
	if kcp.Status.Replicas != kcp.Status.UpdatedReplicas {
		ctrl.LoggerFrom(ctx).V(4).Info("Skipping kube-proxy repository reconcile until KubeadmControlPlane is fully updated",
			"replicas", kcp.Status.Replicas,
			"updatedReplicas", kcp.Status.UpdatedReplicas)
		return nil
	}

	repository, err := kubeProxyRepositoryForKubernetesVersion(kcp.Spec.Version)
	if err != nil {
		return err
	}
	imageRepository := path.Join(cluster.Annotations["cpaas.io/registry-address"], repository)

	daemonSet := &appsv1.DaemonSet{}
	key := client.ObjectKey{Namespace: "kube-system", Name: "kube-proxy"}
	if err := remoteClient.Get(ctx, key, daemonSet); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}
	originalDaemonSet := daemonSet.DeepCopy()
	changed := false
	for i := range daemonSet.Spec.Template.Spec.Containers {
		container := &daemonSet.Spec.Template.Spec.Containers[i]
		if container.Name != "kube-proxy" {
			continue
		}
		image, err := containerutil.ModifyImageRepository(container.Image, imageRepository)
		if err != nil {
			return err
		}
		image, err = containerutil.ModifyImageTag(image, kcp.Spec.Version)
		if err != nil {
			return err
		}
		if container.Image != image {
			container.Image = image
			changed = true
		}
		break
	}
	if !changed {
		return nil
	}

	patchHelper, err := patch.NewHelper(originalDaemonSet, remoteClient)
	if err != nil {
		return err
	}
	return patchHelper.Patch(ctx, daemonSet)
}

func (r *clusterReconciler) reconcileCoreDNSRepository(ctx context.Context, cluster *clusterv1.Cluster, kcp *controlplanev1.KubeadmControlPlane, remoteClient client.Client) error {
	imageTag := coreDNSImageTag(kcp)
	imageRepository := path.Join(cluster.Annotations["cpaas.io/registry-address"], coreDNSImageRepositoryForTag(imageTag))

	deployment := &appsv1.Deployment{}
	key := client.ObjectKey{Namespace: "kube-system", Name: "coredns"}
	if err := remoteClient.Get(ctx, key, deployment); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}
	originalDeployment := deployment.DeepCopy()
	changed := false
	for i := range deployment.Spec.Template.Spec.Containers {
		container := &deployment.Spec.Template.Spec.Containers[i]
		if container.Name != "coredns" {
			continue
		}
		image, err := containerutil.ModifyImageRepository(container.Image, imageRepository)
		if err != nil {
			return err
		}
		if imageTag != "" {
			image, err = containerutil.ModifyImageTag(image, imageTag)
			if err != nil {
				return err
			}
		}
		if container.Image != image {
			container.Image = image
			changed = true
		}
		break
	}
	if !changed {
		return nil
	}

	patchHelper, err := patch.NewHelper(originalDeployment, remoteClient)
	if err != nil {
		return err
	}
	return patchHelper.Patch(ctx, deployment)
}

func coreDNSImageTag(kcp *controlplanev1.KubeadmControlPlane) string {
	if kcp == nil || kcp.Spec.KubeadmConfigSpec.ClusterConfiguration == nil {
		return ""
	}
	return kcp.Spec.KubeadmConfigSpec.ClusterConfiguration.DNS.ImageTag
}

func shouldEnqueueVSphereClusterForKubeadmControlPlaneUpdate(oldKCP, newKCP *controlplanev1.KubeadmControlPlane) bool {
	if oldKCP == nil || newKCP == nil {
		return true
	}
	return oldKCP.Spec.Version != newKCP.Spec.Version ||
		coreDNSImageTag(oldKCP) != coreDNSImageTag(newKCP) ||
		oldKCP.Status.Replicas != newKCP.Status.Replicas ||
		oldKCP.Status.UpdatedReplicas != newKCP.Status.UpdatedReplicas
}
