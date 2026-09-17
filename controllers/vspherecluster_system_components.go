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
	"fmt"
	"path"
	"strings"
	"time"

	pkgerrors "github.com/pkg/errors"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	kerrors "k8s.io/apimachinery/pkg/util/errors"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	bootstrapv1 "sigs.k8s.io/cluster-api/bootstrap/kubeadm/api/v1beta1"
	controlplanev1 "sigs.k8s.io/cluster-api/controlplane/kubeadm/api/v1beta1"
	"sigs.k8s.io/cluster-api/util/conditions"
	containerutil "sigs.k8s.io/cluster-api/util/container"
	"sigs.k8s.io/cluster-api/util/patch"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	capvcontext "sigs.k8s.io/cluster-api-provider-vsphere/pkg/context"
)

const (
	systemComponentRequeueAfter = 10 * time.Second
	// registryAddressAnnotation holds the private registry the workload cluster
	// pulls system component images from.
	registryAddressAnnotation = "cpaas.io/registry-address"
	// kubeadmControlPlaneKind is the ControlPlaneRef kind this reconcile applies to.
	kubeadmControlPlaneKind = "KubeadmControlPlane"
	// cloudControllerManagerName is the DaemonSet, in kube-system, that runs the
	// vSphere CPI on the workload cluster.
	cloudControllerManagerName = "vsphere-cloud-controller-manager"
)

// reconcileKubeadmControlPlaneSystemComponents configures CoreDNS before the
// workload cluster exists and prevents KCP from overwriting the runtime
// objects reconciled by this provider during upgrades.
func (r *clusterReconciler) reconcileKubeadmControlPlaneSystemComponents(ctx context.Context, clusterCtx *capvcontext.ClusterContext) error {
	cluster := clusterCtx.Cluster
	if cluster == nil || cluster.Spec.ControlPlaneRef == nil || cluster.Spec.ControlPlaneRef.Kind != kubeadmControlPlaneKind {
		return nil
	}

	registry := cluster.Annotations[registryAddressAnnotation]
	if registry == "" {
		ctrl.LoggerFrom(ctx).V(4).Info("Skipping KubeadmControlPlane system component reconcile because registry annotation is missing")
		return nil
	}

	kcp, err := r.kubeadmControlPlaneForCluster(ctx, cluster)
	if err != nil {
		if apierrors.IsNotFound(pkgerrors.Cause(err)) {
			ctrl.LoggerFrom(ctx).V(4).Info("Skipping KubeadmControlPlane system component reconcile because KubeadmControlPlane is not available")
			return nil
		}
		return err
	}
	if kcp == nil {
		return nil
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
	if cluster == nil || cluster.Spec.ControlPlaneRef == nil || cluster.Spec.ControlPlaneRef.Kind != kubeadmControlPlaneKind {
		return reconcile.Result{}, nil
	}
	if cluster.Annotations[registryAddressAnnotation] == "" {
		return reconcile.Result{}, nil
	}
	// The kubeconfig secret exists as soon as KubeadmControlPlane generates it,
	// well before the workload API server answers. Without this gate every
	// reconcile of a cluster being created builds a client and blocks on three
	// dial timeouts before any infrastructure step runs, and logs the failure
	// at Error level each pass. The control plane only has to be initialized,
	// not rolled out: a stalled rollout is what this reconcile has to repair.
	if !conditions.IsTrue(cluster, clusterv1.ControlPlaneInitializedCondition) {
		return reconcile.Result{}, nil
	}

	kcp, err := r.kubeadmControlPlaneForCluster(ctx, cluster)
	if err != nil {
		return reconcile.Result{}, err
	}
	if kcp == nil || !kcp.DeletionTimestamp.IsZero() {
		return reconcile.Result{}, nil
	}

	restConfig, err := r.newRemoteRestConfig(ctx, cluster)
	if err != nil {
		// This runs before the vCenter and CNI steps, so a workload cluster that
		// is not reachable yet must not fail the whole Cluster reconcile.
		ctrl.LoggerFrom(ctx).V(4).Info("Skipping workload system component reconcile because workload cluster client is unavailable", "err", err)
		return reconcile.Result{}, nil
	}
	remoteClient, err := client.New(restConfig, client.Options{Scheme: r.Client.Scheme()})
	if err != nil {
		return reconcile.Result{}, err
	}

	sentryPullSecret, err := firstSentryImagePullSecret(ctx, remoteClient)
	if err != nil && !apierrors.IsNotFound(err) {
		return reconcile.Result{}, err
	}

	// The cloud controller manager comes first because CAPI Machines depend on
	// it: nodes keep the uninitialized taint and an empty providerID until it
	// runs. Every component is attempted on every pass so a failing one cannot
	// hide the others.
	var errs []error
	if err := r.reconcileCloudControllerManagerRegistry(ctx, cluster, remoteClient, sentryPullSecret); err != nil {
		errs = append(errs, err)
	}
	if err := r.reconcileKubeProxyRepository(ctx, cluster, kcp, remoteClient, sentryPullSecret); err != nil {
		errs = append(errs, err)
	}
	if err := r.reconcileCoreDNSRepository(ctx, cluster, kcp, remoteClient, sentryPullSecret); err != nil {
		errs = append(errs, err)
	}
	return reconcile.Result{}, kerrors.NewAggregate(errs)
}

func (r *clusterReconciler) reconcileKubeProxyRepository(ctx context.Context, cluster *clusterv1.Cluster, kcp *controlplanev1.KubeadmControlPlane, remoteClient client.Client, imagePullSecret *corev1.LocalObjectReference) error {
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
	imageRepository := path.Join(cluster.Annotations[registryAddressAnnotation], repository)

	daemonSet := &appsv1.DaemonSet{}
	key := client.ObjectKey{Namespace: "kube-system", Name: "kube-proxy"}
	if err := remoteClient.Get(ctx, key, daemonSet); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}
	return patchSystemComponentImage(ctx, remoteClient, daemonSet, &daemonSet.Spec.Template.Spec, "kube-proxy", imageRepository, kcp.Spec.Version, imagePullSecret)
}

func (r *clusterReconciler) reconcileCoreDNSRepository(ctx context.Context, cluster *clusterv1.Cluster, kcp *controlplanev1.KubeadmControlPlane, remoteClient client.Client, imagePullSecret *corev1.LocalObjectReference) error {
	if kcp.Status.Replicas != kcp.Status.UpdatedReplicas {
		ctrl.LoggerFrom(ctx).V(4).Info("Skipping CoreDNS repository reconcile until KubeadmControlPlane is fully updated",
			"replicas", kcp.Status.Replicas,
			"updatedReplicas", kcp.Status.UpdatedReplicas)
		return nil
	}

	imageTag := coreDNSImageTag(kcp)
	imageRepository := path.Join(cluster.Annotations[registryAddressAnnotation], coreDNSImageRepositoryForTag(imageTag))

	deployment := &appsv1.Deployment{}
	key := client.ObjectKey{Namespace: "kube-system", Name: "coredns"}
	if err := remoteClient.Get(ctx, key, deployment); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}
	return patchSystemComponentImage(ctx, remoteClient, deployment, &deployment.Spec.Template.Spec, "coredns", imageRepository, imageTag, imagePullSecret)
}

// reconcileCloudControllerManagerRegistry repoints the vSphere cloud controller
// manager DaemonSet at the registry named by the Cluster annotation. The
// DaemonSet is rendered once, when the cluster is installed, and nothing updates
// it afterwards: a cluster whose registry moved keeps pulling the CPI from the
// old address and only finds out when a node without a local image cache joins.
//
// Unlike kube-proxy and CoreDNS this does not wait for the KubeadmControlPlane
// rollout to settle. Only the registry host changes here, and the rollout is
// exactly what cannot finish while the CPI is unpullable.
//
// This provider does not own the CPI, so this is a stopgap. Once the CPI is
// rendered as an AppRelease the way kube-ovn is, its registry follows the
// annotation by construction and this can go.
func (r *clusterReconciler) reconcileCloudControllerManagerRegistry(ctx context.Context, cluster *clusterv1.Cluster, remoteClient client.Client, imagePullSecret *corev1.LocalObjectReference) error {
	registry := cluster.Annotations[registryAddressAnnotation]

	daemonSet := &appsv1.DaemonSet{}
	key := client.ObjectKey{Namespace: "kube-system", Name: cloudControllerManagerName}
	if err := remoteClient.Get(ctx, key, daemonSet); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}

	original := daemonSet.DeepCopy()
	podSpec := &daemonSet.Spec.Template.Spec
	changed := false
	// Every container, init containers included, is repointed instead of one
	// looked up by name: the container is called vsphere-cpi in the current
	// upstream manifests and vsphere-cloud-controller-manager in older ones,
	// and replacing only the registry host is safe for whatever else the
	// DaemonSet carries.
	for _, containers := range [][]corev1.Container{podSpec.InitContainers, podSpec.Containers} {
		for i := range containers {
			container := &containers[i]
			image, err := repointImageRegistry(container.Image, registry)
			if err != nil {
				// One unparsable image must not stop the remaining containers.
				ctrl.LoggerFrom(ctx).Error(err, "Skipping cloud controller manager container image",
					"container", container.Name, "image", container.Image)
				continue
			}
			if container.Image != image {
				container.Image = image
				changed = true
			}
		}
	}
	if ensureImagePullSecret(podSpec, imagePullSecret) {
		changed = true
	}
	if !changed {
		return nil
	}

	patchHelper, err := patch.NewHelper(original, remoteClient)
	if err != nil {
		return err
	}
	if err := patchHelper.Patch(ctx, daemonSet); err != nil {
		return err
	}
	ctrl.LoggerFrom(ctx).Info("Repointed cloud controller manager at the cluster registry", "registry", registry)
	return nil
}

// repointImageRegistry replaces the registry host in image with registry and
// leaves the repository path, tag and digest alone. The path belongs to whoever
// rendered the CPI and the tag pins a CPI version that has to keep matching the
// cluster, so neither is this provider's to rewrite.
func repointImageRegistry(image, registry string) (string, error) {
	parsed, err := containerutil.ImageFromString(image)
	if err != nil {
		return "", err
	}
	// parsed.Repository holds the host and the path, e.g. "192.168.0.1:11443/ait".
	if _, repositoryPath, found := strings.Cut(parsed.Repository, "/"); found {
		parsed.Repository = path.Join(registry, repositoryPath)
	} else {
		parsed.Repository = registry
	}
	repointed := parsed.String()
	// The registry comes from an annotation, so the result is re-parsed instead
	// of trusted. An image nothing can pull would otherwise be written to the
	// live DaemonSet, and the pass after it would be a no-op because the image
	// already equals the computed value.
	if _, err := containerutil.ImageFromString(repointed); err != nil {
		return "", pkgerrors.Wrapf(err, "registry %q turns image %q into an invalid reference %q", registry, image, repointed)
	}
	return repointed, nil
}

func firstSentryImagePullSecret(ctx context.Context, remoteClient client.Client) (*corev1.LocalObjectReference, error) {
	sa := &corev1.ServiceAccount{}
	if err := remoteClient.Get(ctx, client.ObjectKey{Namespace: "cpaas-system", Name: "sentry"}, sa); err != nil {
		return nil, err
	}
	if len(sa.ImagePullSecrets) == 0 {
		return nil, nil
	}
	return &sa.ImagePullSecrets[0], nil
}

// patchSystemComponentImage repoints the named container's image in obj to
// imageRepository (and imageTag when non-empty), adds imagePullSecret when
// provided, and patches obj when the pod template changes. podSpec must be
// obj's own pod spec so mutations are reflected in the patch.
func patchSystemComponentImage(ctx context.Context, remoteClient client.Client, obj client.Object, podSpec *corev1.PodSpec, containerName, imageRepository, imageTag string, imagePullSecret *corev1.LocalObjectReference) error {
	original, ok := obj.DeepCopyObject().(client.Object)
	if !ok {
		return fmt.Errorf("object %T does not implement client.Object", obj)
	}
	changed := false
	for i := range podSpec.Containers {
		container := &podSpec.Containers[i]
		if container.Name != containerName {
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
	if ensureImagePullSecret(podSpec, imagePullSecret) {
		changed = true
	}
	if !changed {
		return nil
	}

	patchHelper, err := patch.NewHelper(original, remoteClient)
	if err != nil {
		return err
	}
	return patchHelper.Patch(ctx, obj)
}

func ensureImagePullSecret(podSpec *corev1.PodSpec, imagePullSecret *corev1.LocalObjectReference) bool {
	if imagePullSecret == nil || imagePullSecret.Name == "" {
		return false
	}
	for _, existing := range podSpec.ImagePullSecrets {
		if existing.Name == imagePullSecret.Name {
			return false
		}
	}
	podSpec.ImagePullSecrets = append(podSpec.ImagePullSecrets, *imagePullSecret)
	return true
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
