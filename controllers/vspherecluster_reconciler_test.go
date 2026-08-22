/*
Copyright 2021 The Kubernetes Authors.

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
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/vmware/govmomi/simulator"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	kubernetesfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	apirecord "k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	controlplanev1 "sigs.k8s.io/cluster-api/controlplane/kubeadm/api/v1beta1"
	"sigs.k8s.io/cluster-api/util/conditions"
	v1beta2conditions "sigs.k8s.io/cluster-api/util/conditions/v1beta2"
	"sigs.k8s.io/cluster-api/util/kubeconfig"
	"sigs.k8s.io/cluster-api/util/patch"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlutil "sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	infrav1 "sigs.k8s.io/cluster-api-provider-vsphere/apis/v1beta1"
	"sigs.k8s.io/cluster-api-provider-vsphere/internal/test/helpers/vcsim"
	capvcontext "sigs.k8s.io/cluster-api-provider-vsphere/pkg/context"
	"sigs.k8s.io/cluster-api-provider-vsphere/pkg/context/fake"
	"sigs.k8s.io/cluster-api-provider-vsphere/pkg/identity"
)

const (
	timeout = time.Second * 30
)

var _ = Describe("VIM based VSphere ClusterReconciler", func() {
	BeforeEach(func() {})
	AfterEach(func() {})

	Context("Reconcile an VSphereCluster", func() {
		It("should create a cluster", func() {
			fakeVCenter := startVcenter()
			vcURL := fakeVCenter.ServerURL()
			defer fakeVCenter.Destroy()

			// Create the secret containing the credentials
			password, _ := vcURL.User.Password()
			secret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					GenerateName: "secret-",
					Namespace:    "default",
					OwnerReferences: []metav1.OwnerReference{
						{
							APIVersion: "bitnami.com/v1alpha1",
							Kind:       "SealedSecret",
							Name:       "some-name",
							UID:        "some-uid",
						},
					},
				},
				Data: map[string][]byte{
					identity.UsernameKey: []byte(vcURL.User.Username()),
					identity.PasswordKey: []byte(password),
				},
			}
			Expect(testEnv.Create(ctx, secret)).To(Succeed())

			// Create the VSphereCluster object
			instance := &infrav1.VSphereCluster{
				ObjectMeta: metav1.ObjectMeta{
					GenerateName: "vsphere-test1",
					Namespace:    "default",
				},
				Spec: infrav1.VSphereClusterSpec{
					IdentityRef: &infrav1.VSphereIdentityReference{
						Kind: infrav1.SecretKind,
						Name: secret.Name,
					},
					Server: fmt.Sprintf("%s://%s", vcURL.Scheme, vcURL.Host),
				},
			}
			Expect(testEnv.Create(ctx, instance)).To(Succeed())
			key := client.ObjectKey{Namespace: instance.Namespace, Name: instance.Name}
			defer func() {
				Expect(testEnv.Delete(ctx, instance)).To(Succeed())
			}()

			capiCluster := &clusterv1.Cluster{
				ObjectMeta: metav1.ObjectMeta{
					GenerateName: "test1-",
					Namespace:    "default",
				},
				Spec: clusterv1.ClusterSpec{
					InfrastructureRef: &corev1.ObjectReference{
						APIVersion: infrav1.GroupVersion.String(),
						Kind:       "VsphereCluster",
						Name:       instance.Name,
					},
				},
			}
			// Create the CAPI cluster (owner) object
			Expect(testEnv.Create(ctx, capiCluster)).To(Succeed())
			defer func() {
				Expect(testEnv.Cleanup(ctx, capiCluster)).To(Succeed())
			}()

			// Make sure the VSphereCluster exists.
			Eventually(func() error {
				return testEnv.Get(ctx, key, instance)
			}, timeout).Should(BeNil())

			By("setting the OwnerRef on the VSphereCluster")
			Eventually(func() error {
				ph, err := patch.NewHelper(instance, testEnv)
				Expect(err).ShouldNot(HaveOccurred())
				instance.OwnerReferences = append(instance.OwnerReferences, metav1.OwnerReference{
					Kind:       "Cluster",
					APIVersion: clusterv1.GroupVersion.String(),
					Name:       capiCluster.Name,
					UID:        "blah",
				})
				return ph.Patch(ctx, instance, patch.WithStatusObservedGeneration{})
			}, timeout).Should(BeNil())

			Eventually(func() bool {
				if err := testEnv.Get(ctx, key, instance); err != nil {
					return false
				}
				return ctrlutil.ContainsFinalizer(instance, infrav1.ClusterFinalizer)
			}, timeout).Should(BeTrue())

			// checking cluster is setting the ownerRef on the secret
			secretKey := client.ObjectKey{Namespace: secret.Namespace, Name: secret.Name}
			Eventually(func() bool {
				if err := testEnv.Get(ctx, secretKey, secret); err != nil {
					return false
				}
				for _, ref := range secret.OwnerReferences {
					if ref.Name == instance.Name {
						return true
					}
				}
				return false
			}, timeout).Should(BeTrue())

			By("setting the VSphereCluster's VCenterAvailableCondition to true")
			Eventually(func() bool {
				if err := testEnv.Get(ctx, key, instance); err != nil {
					return false
				}
				return conditions.IsTrue(instance, infrav1.VCenterAvailableCondition)
			}, timeout).Should(BeTrue())
		})

		It("should error if secret is already owned by a different cluster", func() {
			ctx := context.Background()
			capiCluster := &clusterv1.Cluster{
				ObjectMeta: metav1.ObjectMeta{
					GenerateName: "test1-",
					Namespace:    "default",
				},
				Spec: clusterv1.ClusterSpec{
					InfrastructureRef: &corev1.ObjectReference{
						APIVersion: infrav1.GroupVersion.String(),
						Kind:       "VsphereCluster",
						Name:       "vsphere-test1",
					},
				},
			}
			// Create the CAPI cluster (owner) object
			Expect(testEnv.Create(ctx, capiCluster)).To(Succeed())

			// Create the secret containing the credentials
			secret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					GenerateName: "secret-",
					Namespace:    "default",
					OwnerReferences: []metav1.OwnerReference{
						{
							APIVersion: infrav1.GroupVersion.String(),
							Kind:       "VSphereClusterIdentity",
							Name:       "another-cluster",
							UID:        "some-uid",
						},
					},
				},
			}
			Expect(testEnv.Create(ctx, secret)).To(Succeed())

			// Create the VSphereCluster object
			instance := &infrav1.VSphereCluster{
				ObjectMeta: metav1.ObjectMeta{
					GenerateName: "vsphere-cluster",
					Namespace:    "default",
				},
				Spec: infrav1.VSphereClusterSpec{
					IdentityRef: &infrav1.VSphereIdentityReference{
						Kind: infrav1.SecretKind,
						Name: secret.Name,
					},
				},
			}

			Expect(testEnv.Create(ctx, instance)).To(Succeed())
			key := client.ObjectKey{Namespace: instance.Namespace, Name: instance.Name}
			defer func() {
				err := testEnv.Delete(ctx, instance)
				Expect(err).NotTo(HaveOccurred())
			}()
			By("setting the OwnerRef on the VSphereCluster")
			Eventually(func() bool {
				ph, err := patch.NewHelper(instance, testEnv)
				Expect(err).ShouldNot(HaveOccurred())
				instance.OwnerReferences = append(instance.OwnerReferences, metav1.OwnerReference{Kind: "Cluster", APIVersion: clusterv1.GroupVersion.String(), Name: capiCluster.Name, UID: "blah"})
				Expect(ph.Patch(ctx, instance, patch.WithStatusObservedGeneration{})).ShouldNot(HaveOccurred())
				return true
			}, timeout).Should(BeTrue())

			Eventually(func() bool {
				if err := testEnv.Get(ctx, key, instance); err != nil {
					return false
				}

				actual := conditions.Get(instance, infrav1.VCenterAvailableCondition)
				if actual == nil {
					return false
				}
				actual.Message = ""
				return Expect(actual).Should(conditions.HaveSameStateOf(&clusterv1.Condition{
					Type:     infrav1.VCenterAvailableCondition,
					Status:   corev1.ConditionFalse,
					Severity: clusterv1.ConditionSeverityError,
					Reason:   infrav1.VCenterUnreachableReason,
				}))
			}, timeout).Should(BeTrue())
		})
	})

	It("should remove vspherecluster finalizer if the secret does not exist", func() {
		ctx := context.Background()
		capiCluster := &clusterv1.Cluster{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "test1-",
				Namespace:    "default",
			},
			Spec: clusterv1.ClusterSpec{
				InfrastructureRef: &corev1.ObjectReference{
					APIVersion: infrav1.GroupVersion.String(),
					Kind:       "VsphereCluster",
					Name:       "vsphere-test1",
				},
			},
		}
		// Create the CAPI cluster (owner) object
		Expect(testEnv.Create(ctx, capiCluster)).To(Succeed())

		// Create the VSphereCluster object
		instance := &infrav1.VSphereCluster{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "vsphere-test1",
				Namespace: "default",
				OwnerReferences: []metav1.OwnerReference{{
					APIVersion: clusterv1.GroupVersion.String(),
					Kind:       "Cluster",
					Name:       capiCluster.Name,
					UID:        capiCluster.UID,
					Controller: ptr.To(true),
				}},
			},
			Spec: infrav1.VSphereClusterSpec{
				IdentityRef: &infrav1.VSphereIdentityReference{
					Kind: infrav1.SecretKind,
					Name: "foo",
				},
			},
		}

		Expect(testEnv.Create(ctx, instance)).To(Succeed())
		key := client.ObjectKey{Namespace: instance.Namespace, Name: instance.Name}

		// Make sure the VSphereCluster exists and has its finalizer set.
		Eventually(func() bool {
			if err := testEnv.Get(ctx, key, instance); err != nil {
				return false
			}
			return ctrlutil.ContainsFinalizer(instance, infrav1.ClusterFinalizer)
		}, timeout).Should(BeTrue())

		By("deleting the VSphereCluster while the secret is gone")
		Eventually(func() bool {
			err := testEnv.Delete(ctx, instance)
			return err == nil
		}, timeout).Should(BeTrue())

		Eventually(func() bool {
			err := testEnv.Get(ctx, key, instance)
			return apierrors.IsNotFound(err)
		}, timeout).Should(BeTrue())
	})

	It("should be able to delete VSphereCluster if the Cluster does not exist", func() {
		ctx := context.Background()

		// Create the VSphereCluster object
		instance := &infrav1.VSphereCluster{
			ObjectMeta: metav1.ObjectMeta{
				Finalizers: []string{infrav1.ClusterFinalizer},
				Name:       "vsphere-test1",
				Namespace:  "default",
			},
		}

		Expect(testEnv.Create(ctx, instance)).To(Succeed())
		key := client.ObjectKey{Namespace: instance.Namespace, Name: instance.Name}

		// Make sure the VSphereCluster exists and has its finalizer set.
		Eventually(func() bool {
			if err := testEnv.Get(ctx, key, instance); err != nil {
				return false
			}
			return ctrlutil.ContainsFinalizer(instance, infrav1.ClusterFinalizer)
		}, timeout).Should(BeTrue())

		By("deleting the VSphereCluster when no Cluster exists")
		Eventually(func() bool {
			err := testEnv.Delete(ctx, instance)
			return err == nil
		}, timeout).Should(BeTrue())

		Eventually(func() bool {
			err := testEnv.Get(ctx, key, instance)
			return apierrors.IsNotFound(err)
		}, timeout).Should(BeTrue())
	})

	It("should be able to delete a paused VSphereCluster", func() {
		ctx := context.Background()

		instance := &infrav1.VSphereCluster{
			ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{clusterv1.PausedAnnotation: "true"},
				Finalizers:  []string{infrav1.ClusterFinalizer},
				Name:        "paused-vsphere-test1",
				Namespace:   "default",
			},
		}

		Expect(testEnv.Create(ctx, instance)).To(Succeed())
		key := client.ObjectKey{Namespace: instance.Namespace, Name: instance.Name}

		By("deleting the paused VSphereCluster")
		Eventually(func() bool {
			err := testEnv.Delete(ctx, instance)
			return err == nil
		}, timeout).Should(BeTrue())

		Eventually(func() bool {
			err := testEnv.Get(ctx, key, instance)
			return apierrors.IsNotFound(err)
		}, timeout).Should(BeTrue())
	})

	Context("With Deployment Zones", func() {
		var (
			namespace   *corev1.Namespace
			capiCluster *clusterv1.Cluster
			instance    *infrav1.VSphereCluster
			zoneOne     *infrav1.VSphereDeploymentZone
		)

		BeforeEach(func() {
			var err error
			namespace, err = testEnv.CreateNamespace(ctx, "dz-test")
			Expect(err).NotTo(HaveOccurred())

			capiCluster = &clusterv1.Cluster{
				ObjectMeta: metav1.ObjectMeta{
					GenerateName: "test1-",
					Namespace:    namespace.Name,
				},
				Spec: clusterv1.ClusterSpec{
					InfrastructureRef: &corev1.ObjectReference{
						APIVersion: infrav1.GroupVersion.String(),
						Kind:       "VSphereCluster",
						Name:       "vsphere-test2",
					},
				},
			}
			// Create the CAPI cluster (owner) object
			Expect(testEnv.Create(ctx, capiCluster)).To(Succeed())
			Expect(testEnv.CreateKubeconfigSecret(ctx, capiCluster)).To(Succeed())

			By("Create the VSphere Deployment Zone")
			zoneOne = &infrav1.VSphereDeploymentZone{
				ObjectMeta: metav1.ObjectMeta{Name: "zone-one"},
				Spec: infrav1.VSphereDeploymentZoneSpec{
					Server:        testEnv.Simulator.ServerURL().Host,
					FailureDomain: "fd-one",
					ControlPlane:  ptr.To(true),
				},
				Status: infrav1.VSphereDeploymentZoneStatus{},
			}
			Expect(testEnv.Create(ctx, zoneOne)).To(Succeed())

			By("Create the VSphere Cluster")
			instance = &infrav1.VSphereCluster{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "vsphere-test2",
					Namespace: namespace.Name,
					OwnerReferences: []metav1.OwnerReference{{
						Kind:       "Cluster",
						APIVersion: clusterv1.GroupVersion.String(),
						Name:       capiCluster.Name,
						UID:        "blah",
					}},
				},
				Spec: infrav1.VSphereClusterSpec{
					FailureDomainSelector: &metav1.LabelSelector{MatchLabels: map[string]string{}},
					Server:                testEnv.Simulator.ServerURL().Host,
				},
			}
			Expect(testEnv.Create(ctx, instance)).To(Succeed())
		})

		AfterEach(func() {
			// Note: Make sure VSphereCluster is deleted before the Cluster is deleted.
			// Otherwise reconcileDelete in VSphereCluster reconciler will fail because the Cluster cannot be found.
			Expect(testEnv.CleanupAndWait(ctx, instance, zoneOne)).To(Succeed())
			Expect(testEnv.CleanupAndWait(ctx, capiCluster)).To(Succeed())
			Expect(testEnv.CleanupAndWait(ctx, namespace)).To(Succeed())
		})

		It("should reconcile a cluster", func() {
			key := client.ObjectKey{Namespace: instance.Namespace, Name: instance.Name}
			Eventually(func() bool {
				if err := testEnv.Get(ctx, key, instance); err != nil {
					return false
				}
				return conditions.Has(instance, infrav1.FailureDomainsAvailableCondition) &&
					conditions.IsFalse(instance, infrav1.FailureDomainsAvailableCondition) &&
					conditions.Get(instance, infrav1.FailureDomainsAvailableCondition).Reason == infrav1.WaitingForFailureDomainStatusReason
			}, timeout).Should(BeTrue())

			By("Setting the status of the Deployment Zone to true")
			Eventually(func() error {
				ph, err := patch.NewHelper(zoneOne, testEnv)
				Expect(err).ShouldNot(HaveOccurred())
				zoneOne.Status.Ready = ptr.To(true)
				return ph.Patch(ctx, zoneOne, patch.WithStatusObservedGeneration{})
			}, timeout).Should(BeNil())

			Eventually(func() bool {
				if err := testEnv.Get(ctx, key, instance); err != nil {
					return false
				}
				return conditions.Has(instance, infrav1.FailureDomainsAvailableCondition) &&
					conditions.IsTrue(instance, infrav1.FailureDomainsAvailableCondition)
			}, timeout).Should(BeTrue())
		})

		Context("when deployment zones are deleted", func() {
			BeforeEach(func() {
				By("Setting the status of the Deployment Zone to true")
				Eventually(func() error {
					ph, err := patch.NewHelper(zoneOne, testEnv)
					Expect(err).ShouldNot(HaveOccurred())
					zoneOne.Status.Ready = ptr.To(true)
					return ph.Patch(ctx, zoneOne, patch.WithStatusObservedGeneration{})
				}, timeout).Should(BeNil())
			})

			It("should remove the FailureDomainsAvailable condition from the cluster", func() {
				key := client.ObjectKey{Namespace: instance.Namespace, Name: instance.Name}
				Eventually(func() bool {
					if err := testEnv.Get(ctx, key, instance); err != nil {
						return false
					}
					return conditions.Has(instance, infrav1.FailureDomainsAvailableCondition) &&
						conditions.IsTrue(instance, infrav1.FailureDomainsAvailableCondition)
				}, timeout).Should(BeTrue())

				By("Deleting the Deployment Zone", func() {
					Expect(testEnv.Delete(ctx, zoneOne)).To(Succeed())
				})

				Eventually(func() bool {
					if err := testEnv.Get(ctx, key, instance); err != nil {
						return false
					}
					return conditions.Has(instance, infrav1.FailureDomainsAvailableCondition)
				}, timeout).Should(BeFalse())
			})
		})
	})
})

func TestClusterReconciler_ControlPlaneNodesRegistered(t *testing.T) {
	tests := []struct {
		name          string
		desired       int32
		existingNodes int32
		listNodeErr   error
		wantReady     bool
	}{
		{
			name:          "all control plane Nodes are registered",
			desired:       3,
			existingNodes: 3,
			wantReady:     true,
		},
		{
			name:          "some control plane Nodes are not registered",
			desired:       3,
			existingNodes: 2,
			wantReady:     false,
		},
		{
			name:        "workload cluster Nodes cannot be listed",
			desired:     1,
			listNodeErr: fmt.Errorf("workload cluster is not ready"),
			wantReady:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewWithT(t)
			cluster := &clusterv1.Cluster{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: "default",
					Name:      "test-cluster",
				},
				Spec: clusterv1.ClusterSpec{
					ControlPlaneRef: &corev1.ObjectReference{
						APIVersion: controlplanev1.GroupVersion.String(),
						Kind:       "KubeadmControlPlane",
						Name:       "test-kcp",
					},
				},
			}
			objects := []client.Object{
				cluster,
				&controlplanev1.KubeadmControlPlane{
					ObjectMeta: metav1.ObjectMeta{
						Namespace: cluster.Namespace,
						Name:      cluster.Spec.ControlPlaneRef.Name,
					},
					Spec: controlplanev1.KubeadmControlPlaneSpec{
						Replicas: ptr.To(tt.desired),
					},
				},
			}
			workloadObjects := make([]runtime.Object, 0, tt.existingNodes)
			for i := int32(0); i < tt.existingNodes; i++ {
				workloadObjects = append(workloadObjects, &corev1.Node{ObjectMeta: metav1.ObjectMeta{
					Name: fmt.Sprintf("test-control-plane-%d", i),
					Labels: map[string]string{
						"node-role.kubernetes.io/control-plane": "",
					},
				}})
			}

			controllerManagerContext := fake.NewControllerManagerContext(objects...)
			workloadClient := kubernetesfake.NewSimpleClientset(workloadObjects...)
			if tt.listNodeErr != nil {
				workloadClient.PrependReactor("list", "nodes", func(k8stesting.Action) (bool, runtime.Object, error) {
					return true, nil, tt.listNodeErr
				})
			}
			r := clusterReconciler{Client: controllerManagerContext.Client}
			controlPlaneNodes, ready, err := r.controlPlaneNodesRegistered(ctx, cluster, workloadClient)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(ready).To(Equal(tt.wantReady))
			if tt.wantReady {
				g.Expect(controlPlaneNodes).To(Equal([]string{"test-control-plane-0", "test-control-plane-1", "test-control-plane-2"}))
			}
		})
	}
}

func TestBuildKubeOvnAppReleaseSetsPullSecrets(t *testing.T) {
	g := NewWithT(t)
	cluster := &clusterv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{Name: "test-cluster"},
		Spec: clusterv1.ClusterSpec{
			ControlPlaneEndpoint: clusterv1.APIEndpoint{Host: "api.test.example.com"},
		},
	}

	appRelease := buildKubeOvnAppRelease(
		cluster,
		"registry.example.com",
		"",
		"v1.0.0",
		"192.168.0.0/16",
		"10.96.0.0/12",
		"100.64.0.0/16",
		"global-registry-auth",
		[]any{"global-registry-auth", "extra-registry-auth"},
		nil,
	)

	chartPullSecret, found, err := unstructured.NestedString(appRelease.Object, "spec", "source", "chartPullSecret")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(found).To(BeTrue())
	g.Expect(chartPullSecret).To(Equal("global-registry-auth"))

	imagePullSecrets, found, err := unstructured.NestedSlice(appRelease.Object, "spec", "values", "global", "registry", "imagePullSecrets")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(found).To(BeTrue())
	g.Expect(imagePullSecrets).To(Equal([]any{"global-registry-auth", "extra-registry-auth"}))

	charts, found, err := unstructured.NestedSlice(appRelease.Object, "spec", "source", "charts")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(found).To(BeTrue())
	g.Expect(charts).To(HaveLen(1))
	g.Expect(charts[0].(map[string]interface{})["name"]).To(Equal(kubeOvnLegacyChartName))
}

func TestBuildKubeOvnAppReleaseUsesRequestedChartName(t *testing.T) {
	g := NewWithT(t)
	appRelease := buildKubeOvnAppRelease(
		&clusterv1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "test-cluster"}},
		"registry.example.com",
		kubeOvnChartNameV44,
		"v4.4.0",
		"192.168.0.0/16",
		"10.96.0.0/12",
		"100.64.0.0/16",
		"",
		nil,
		nil,
	)

	charts, found, err := unstructured.NestedSlice(appRelease.Object, "spec", "source", "charts")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(found).To(BeTrue())
	g.Expect(charts).To(HaveLen(1))
	g.Expect(charts[0].(map[string]interface{})["name"]).To(Equal(kubeOvnChartNameV44))
}

func TestBuildKubeOvnAppReleaseSetsControlPlaneNodes(t *testing.T) {
	g := NewWithT(t)
	appRelease := buildKubeOvnAppRelease(
		&clusterv1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "test-cluster"}},
		"registry.example.com",
		kubeOvnChartNameV44,
		"v4.4.0",
		"192.168.0.0/16",
		"10.96.0.0/12",
		"100.64.0.0/16",
		"",
		nil,
		[]string{"cp-0", "cp-1", "cp-2"},
	)

	controlPlaneNodes, found, err := unstructured.NestedSlice(appRelease.Object, "spec", "values", "controlPlaneNodes")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(found).To(BeTrue())
	g.Expect(controlPlaneNodes).To(Equal([]any{"cp-0", "cp-1", "cp-2"}))
}

func TestKubeOvnAppReleaseReadiness(t *testing.T) {
	condition := func(conditionType string, status corev1.ConditionStatus, reason, message string) map[string]any {
		return map[string]any{
			"type":    conditionType,
			"status":  string(status),
			"reason":  reason,
			"message": message,
		}
	}
	appRelease := func(generation, observedGeneration int64, conditions []any) *unstructured.Unstructured {
		return &unstructured.Unstructured{Object: map[string]any{
			"metadata": map[string]any{"generation": generation},
			"status": map[string]any{
				"observedGeneration": observedGeneration,
				"conditions":         conditions,
			},
		}}
	}

	tests := []struct {
		name        string
		appRelease  *unstructured.Unstructured
		wantReady   bool
		wantReason  string
		wantMessage string
	}{
		{
			name:       "ready when top-level observedGeneration is current",
			appRelease: appRelease(2, 2, []any{condition("Sync", corev1.ConditionTrue, "Synced", ""), condition("Health", corev1.ConditionTrue, "Ready", "")}),
			wantReady:  true,
			wantReason: infrav1.KubeOvnAppReleaseReadyReason,
		},
		{
			name:        "waiting when Sync condition is missing",
			appRelease:  appRelease(1, 1, []any{condition("Health", corev1.ConditionTrue, "Ready", "")}),
			wantReason:  infrav1.KubeOvnAppReleaseReconcilingReason,
			wantMessage: "waiting for kube-ovn AppRelease Sync condition",
		},
		{
			name:        "waiting when top-level observedGeneration is stale",
			appRelease:  appRelease(2, 1, []any{condition("Sync", corev1.ConditionTrue, "Synced", ""), condition("Health", corev1.ConditionTrue, "Ready", "")}),
			wantReason:  infrav1.KubeOvnAppReleaseReconcilingReason,
			wantMessage: "waiting for kube-ovn AppRelease to observe generation 2",
		},
		{
			name: "waiting when top-level observedGeneration is missing",
			appRelease: &unstructured.Unstructured{Object: map[string]any{
				"metadata": map[string]any{"generation": int64(1)},
				"status": map[string]any{
					"conditions": []any{condition("Sync", corev1.ConditionTrue, "Synced", ""), condition("Health", corev1.ConditionTrue, "Ready", "")},
				},
			}},
			wantReason:  infrav1.KubeOvnAppReleaseReconcilingReason,
			wantMessage: "waiting for kube-ovn AppRelease observedGeneration",
		},
		{
			name:        "not ready when Health is false",
			appRelease:  appRelease(1, 1, []any{condition("Sync", corev1.ConditionTrue, "Synced", ""), condition("Health", corev1.ConditionFalse, "Progressing", "waiting for pods")}),
			wantReason:  infrav1.KubeOvnAppReleaseNotReadyReason,
			wantMessage: "kube-ovn AppRelease Health condition is Progressing: waiting for pods",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewWithT(t)
			got := kubeOvnAppReleaseReadiness(tt.appRelease)
			g.Expect(got.ready).To(Equal(tt.wantReady))
			g.Expect(got.reason).To(Equal(tt.wantReason))
			if tt.wantMessage != "" {
				g.Expect(got.message).To(Equal(tt.wantMessage))
			}
		})
	}
}

func TestClusterReconciler_SetKubeOvnAppReleaseConditionEmitsEventsOnChanges(t *testing.T) {
	g := NewWithT(t)
	recorder := apirecord.NewFakeRecorder(10)
	r := &clusterReconciler{Recorder: recorder}
	vsphereCluster := &infrav1.VSphereCluster{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "test-cluster"}}

	r.setKubeOvnAppReleaseCondition(vsphereCluster, corev1.ConditionFalse, infrav1.KubeOvnAppReleaseNotReadyReason, clusterv1.ConditionSeverityWarning, "waiting for pods")
	g.Expect(<-recorder.Events).To(ContainSubstring(infrav1.KubeOvnAppReleaseNotReadyReason))

	r.setKubeOvnAppReleaseCondition(vsphereCluster, corev1.ConditionFalse, infrav1.KubeOvnAppReleaseNotReadyReason, clusterv1.ConditionSeverityWarning, "waiting for pods")
	select {
	case event := <-recorder.Events:
		t.Fatalf("expected no duplicate event, got %q", event)
	default:
	}

	r.setKubeOvnAppReleaseCondition(vsphereCluster, corev1.ConditionTrue, infrav1.KubeOvnAppReleaseReadyReason, clusterv1.ConditionSeverityInfo, "")
	g.Expect(<-recorder.Events).To(ContainSubstring(infrav1.KubeOvnAppReleaseReadyReason))
}

func TestClusterReconciler_ReconcileKubeOvnAppReleaseRequeuesUntilControlPlaneNodesRegister(t *testing.T) {
	g := NewWithT(t)
	cluster := &clusterv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default",
			Name:      "test-cluster",
			Annotations: map[string]string{
				"cpaas.io/network-type":     "kube-ovn",
				"cpaas.io/kube-ovn-version": "v1.0.0",
				"cpaas.io/registry-address": "registry.example.com",
			},
		},
		Spec: clusterv1.ClusterSpec{
			ControlPlaneRef: &corev1.ObjectReference{
				APIVersion: controlplanev1.GroupVersion.String(),
				Kind:       "KubeadmControlPlane",
				Name:       "test-kcp",
			},
			ClusterNetwork: &clusterv1.ClusterNetwork{
				Pods: &clusterv1.NetworkRanges{
					CIDRBlocks: []string{"192.168.0.0/16"},
				},
				Services: &clusterv1.NetworkRanges{
					CIDRBlocks: []string{"10.96.0.0/12"},
				},
			},
		},
	}
	objects := []client.Object{
		cluster,
		kubeconfig.GenerateSecret(cluster, kubeconfig.FromEnvTestConfig(testEnv.Config, cluster)),
		&controlplanev1.KubeadmControlPlane{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: cluster.Namespace,
				Name:      cluster.Spec.ControlPlaneRef.Name,
			},
			Spec: controlplanev1.KubeadmControlPlaneSpec{
				Replicas: ptr.To[int32](3),
			},
		},
	}
	for i := range 3 {
		machine := &clusterv1.Machine{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: cluster.Namespace,
				Name:      fmt.Sprintf("test-control-plane-%d", i),
				Labels: map[string]string{
					clusterv1.ClusterNameLabel:         cluster.Name,
					clusterv1.MachineControlPlaneLabel: "",
				},
			},
		}
		if i < 2 {
			machine.Status.NodeRef = &corev1.ObjectReference{Name: machine.Name}
		}
		objects = append(objects, machine)
	}
	controllerManagerContext := fake.NewControllerManagerContext(objects...)
	r := clusterReconciler{Client: controllerManagerContext.Client}
	vsphereCluster := &infrav1.VSphereCluster{
		ObjectMeta: metav1.ObjectMeta{Namespace: cluster.Namespace, Name: cluster.Name},
		Status:     infrav1.VSphereClusterStatus{Ready: true},
	}

	result, err := r.reconcileKubeOvnAppRelease(ctx, &capvcontext.ClusterContext{Cluster: cluster, VSphereCluster: vsphereCluster})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(Equal(10 * time.Second))
	g.Expect(vsphereCluster.Status.Ready).To(BeTrue())
	g.Expect(conditions.Get(vsphereCluster, infrav1.KubeOvnAppReleaseReadyCondition).Reason).To(Equal(infrav1.KubeOvnAppReleaseReconcilingReason))
	condition := v1beta2conditions.Get(vsphereCluster, infrav1.VSphereClusterKubeOvnAppReleaseReadyV1Beta2Condition)
	g.Expect(condition).NotTo(BeNil())
	g.Expect(condition.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(condition.Reason).To(Equal(infrav1.KubeOvnAppReleaseReconcilingReason))
}

func TestClusterReconciler_ReconcileDeploymentZones(t *testing.T) {
	server := "vcenter123.foo.com"

	t.Run("with nil selectors", func(t *testing.T) {
		g := NewWithT(t)
		tests := []struct {
			name       string
			initObjs   []client.Object
			reconciled bool
			assert     func(*infrav1.VSphereCluster)
		}{
			{
				name:       "with no deployment zones",
				reconciled: true,
				assert: func(vsphereCluster *infrav1.VSphereCluster) {
					g.Expect(conditions.Has(vsphereCluster, infrav1.FailureDomainsAvailableCondition)).To(BeFalse())
				},
			},
			{
				name:       "with all deployment zone statuses as ready",
				reconciled: true,
				initObjs: []client.Object{
					deploymentZone(server, "zone-1", ptr.To(false), ptr.To(true)),
					deploymentZone(server, "zone-2", ptr.To(true), ptr.To(true)),
				},
				assert: func(vsphereCluster *infrav1.VSphereCluster) {
					g.Expect(conditions.Has(vsphereCluster, infrav1.FailureDomainsAvailableCondition)).To(BeFalse())
				},
			},
		}

		for _, tt := range tests {
			// Looks odd, but need to reinit test variable
			tt := tt
			t.Run(tt.name, func(t *testing.T) {
				g := NewWithT(t)
				controllerManagerContext := fake.NewControllerManagerContext(tt.initObjs...)
				clusterCtx := fake.NewClusterContext(ctx, controllerManagerContext)
				clusterCtx.VSphereCluster.Spec.Server = server

				r := clusterReconciler{
					ControllerManagerContext: controllerManagerContext,
					Client:                   controllerManagerContext.Client,
				}
				reconciled, err := r.reconcileDeploymentZones(ctx, clusterCtx)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(reconciled).To(Equal(tt.reconciled))
				tt.assert(clusterCtx.VSphereCluster)
			})
		}
	})

	t.Run("with empty selectors", func(t *testing.T) {
		g := NewWithT(t)
		tests := []struct {
			name       string
			initObjs   []client.Object
			reconciled bool
			assert     func(*infrav1.VSphereCluster)
		}{
			{
				name:       "with no deployment zones",
				reconciled: true,
				assert: func(vsphereCluster *infrav1.VSphereCluster) {
					g.Expect(conditions.Has(vsphereCluster, infrav1.FailureDomainsAvailableCondition)).To(BeFalse())
				},
			},
			{
				name: "with deployment zone status not reported",
				initObjs: []client.Object{
					deploymentZone(server, "zone-1", ptr.To(false), nil),
					deploymentZone(server, "zone-2", ptr.To(true), ptr.To(false)),
				},
				assert: func(vsphereCluster *infrav1.VSphereCluster) {
					g.Expect(conditions.IsFalse(vsphereCluster, infrav1.FailureDomainsAvailableCondition)).To(BeTrue())
					g.Expect(conditions.Get(vsphereCluster, infrav1.FailureDomainsAvailableCondition).Reason).To(Equal(infrav1.WaitingForFailureDomainStatusReason))
				},
			},
			{
				name:       "with some deployment zones statuses as not ready",
				reconciled: true,
				initObjs: []client.Object{
					deploymentZone(server, "zone-1", ptr.To(false), ptr.To(false)),
					deploymentZone(server, "zone-2", ptr.To(true), ptr.To(true)),
				},
				assert: func(vsphereCluster *infrav1.VSphereCluster) {
					g.Expect(conditions.IsFalse(vsphereCluster, infrav1.FailureDomainsAvailableCondition)).To(BeTrue())
					g.Expect(conditions.Get(vsphereCluster, infrav1.FailureDomainsAvailableCondition).Reason).To(Equal(infrav1.FailureDomainsSkippedReason))
				},
			},
			{
				name:       "with all deployment zone statuses as ready",
				reconciled: true,
				initObjs: []client.Object{
					deploymentZone(server, "zone-1", ptr.To(false), ptr.To(true)),
					deploymentZone(server, "zone-2", ptr.To(true), ptr.To(true)),
				},
				assert: func(vsphereCluster *infrav1.VSphereCluster) {
					g.Expect(conditions.IsTrue(vsphereCluster, infrav1.FailureDomainsAvailableCondition)).To(BeTrue())
				},
			},
		}

		for _, tt := range tests {
			// Looks odd, but need to reinit test variable
			tt := tt
			t.Run(tt.name, func(t *testing.T) {
				g := NewWithT(t)
				controllerManagerContext := fake.NewControllerManagerContext(tt.initObjs...)
				clusterCtx := fake.NewClusterContext(ctx, controllerManagerContext)
				clusterCtx.VSphereCluster.Spec.Server = server
				clusterCtx.VSphereCluster.Spec.FailureDomainSelector = &metav1.LabelSelector{MatchLabels: map[string]string{}}

				r := clusterReconciler{
					ControllerManagerContext: controllerManagerContext,
					Client:                   controllerManagerContext.Client,
				}
				reconciled, err := r.reconcileDeploymentZones(ctx, clusterCtx)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(reconciled).To(Equal(tt.reconciled))
				tt.assert(clusterCtx.VSphereCluster)
			})
		}
	})

	t.Run("with zone selectors", func(t *testing.T) {
		g := NewWithT(t)

		zoneOne := deploymentZone(server, "zone-1", ptr.To(false), ptr.To(true))
		zoneOne.Labels = map[string]string{
			"zone":       "rack-one",
			"datacenter": "ohio",
		}
		zoneTwo := deploymentZone(server, "zone-2", ptr.To(false), ptr.To(true))
		zoneTwo.Labels = map[string]string{
			"zone":       "rack-two",
			"datacenter": "ohio",
		}
		zoneThree := deploymentZone(server, "zone-3", ptr.To(false), ptr.To(true))
		zoneThree.Labels = map[string]string{
			"datacenter": "oregon",
		}

		assertNumberOfZones := func(selector *metav1.LabelSelector, selectedZones int) {
			controllerManagerContext := fake.NewControllerManagerContext(zoneOne, zoneTwo, zoneThree)
			clusterCtx := fake.NewClusterContext(ctx, controllerManagerContext)
			clusterCtx.VSphereCluster.Spec.Server = server
			clusterCtx.VSphereCluster.Spec.FailureDomainSelector = selector

			r := clusterReconciler{
				ControllerManagerContext: controllerManagerContext,
				Client:                   controllerManagerContext.Client,
			}
			_, err := r.reconcileDeploymentZones(ctx, clusterCtx)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(clusterCtx.VSphereCluster.Status.FailureDomains).To(HaveLen(selectedZones))
		}

		t.Run("with no zones matching labels", func(_ *testing.T) {
			assertNumberOfZones(&metav1.LabelSelector{MatchLabels: map[string]string{"foo": "bar"}}, 0)
		})

		t.Run("with all zones matching some labels", func(_ *testing.T) {
			assertNumberOfZones(&metav1.LabelSelector{MatchLabels: map[string]string{"datacenter": "ohio"}}, 2)
		})

		t.Run("with selector and all matching labels", func(_ *testing.T) {
			assertNumberOfZones(&metav1.LabelSelector{MatchLabels: map[string]string{
				"zone":       "rack-two",
				"datacenter": "ohio",
			}}, 1)
		})

		t.Run("with no selector", func(_ *testing.T) {
			assertNumberOfZones(nil, 0)
		})

		t.Run("with selector and a negation label matcher", func(_ *testing.T) {
			assertNumberOfZones(&metav1.LabelSelector{
				MatchExpressions: []metav1.LabelSelectorRequirement{
					{
						Key:      "datacenter",
						Operator: metav1.LabelSelectorOpNotIn,
						Values:   []string{"ohio"},
					},
				},
			}, 1)
		})

		t.Run("with selector and a key-only label matcher", func(_ *testing.T) {
			assertNumberOfZones(&metav1.LabelSelector{
				MatchExpressions: []metav1.LabelSelectorRequirement{
					{
						Key:      "zone",
						Operator: metav1.LabelSelectorOpExists,
					},
				},
			}, 2)
		})

		t.Run("with selector and a multi value label matcher", func(_ *testing.T) {
			assertNumberOfZones(&metav1.LabelSelector{
				MatchExpressions: []metav1.LabelSelectorRequirement{
					{
						Key:      "datacenter",
						Operator: metav1.LabelSelectorOpIn,
						Values:   []string{"ohio", "oregon"},
					},
				},
			}, 3)
		})
	})
}

func deploymentZone(server, fdName string, cp, ready *bool) *infrav1.VSphereDeploymentZone {
	return &infrav1.VSphereDeploymentZone{
		ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("zone-%s", fdName)},
		Spec: infrav1.VSphereDeploymentZoneSpec{
			Server:        server,
			FailureDomain: fdName,
			ControlPlane:  cp,
		},
		Status: infrav1.VSphereDeploymentZoneStatus{Ready: ready},
	}
}

func startVcenter() *vcsim.Simulator {
	model := simulator.VPX()
	model.Pool = 1

	simr, err := vcsim.NewBuilder().WithModel(model).Build()
	if err != nil {
		panic(fmt.Sprintf("unable to create simulator %s", err))
	}

	return simr
}
