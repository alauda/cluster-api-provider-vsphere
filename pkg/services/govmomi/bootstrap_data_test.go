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

package govmomi

import (
	"context"
	"testing"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	bootstrapv1 "sigs.k8s.io/cluster-api/bootstrap/kubeadm/api/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	infrav1 "sigs.k8s.io/cluster-api-provider-vsphere/apis/v1beta1"
)

// classifyBootstrapReason mirrors the reason selection performed at the getBootstrapData call site in
// ReconcileVM, so the mapping from typed error to BootstrapReady reason is covered without a vCenter session.
func classifyBootstrapReason(err error) (string, string) {
	reason := infrav1.BootstrapSecretGetFailedReason
	v1beta2Reason := infrav1.VSphereVMBootstrapSecretGetFailedV1Beta2Reason
	if _, ok := err.(*bootstrapSecretContentError); ok {
		reason = infrav1.BootstrapSecretContentInvalidReason
		v1beta2Reason = infrav1.VSphereVMBootstrapSecretContentInvalidV1Beta2Reason
	}
	return reason, v1beta2Reason
}

func Test_getBootstrapData(t *testing.T) {
	const ns = "cpaas-system"
	bootstrapRef := &corev1.ObjectReference{Kind: "Secret", Namespace: ns, Name: "bootstrap-secret"}

	newSecret := func(data map[string][]byte) *corev1.Secret {
		return &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "bootstrap-secret", Namespace: ns},
			Data:       data,
		}
	}

	t.Run("no bootstrap ref returns no data and no error", func(t *testing.T) {
		g := NewWithT(t)
		vmCtx := emptyVirtualMachineContext()
		vmCtx.Client = fake.NewClientBuilder().Build()
		vmCtx.VSphereVM = &infrav1.VSphereVM{ObjectMeta: metav1.ObjectMeta{Namespace: ns}}

		data, format, err := (&VMService{}).getBootstrapData(context.Background(), &vmCtx.VMContext)
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(data).To(BeNil())
		g.Expect(format).To(BeEquivalentTo(""))
	})

	t.Run("missing secret yields a get error classified as BootstrapSecretGetFailed", func(t *testing.T) {
		g := NewWithT(t)
		vmCtx := emptyVirtualMachineContext()
		vmCtx.Client = fake.NewClientBuilder().Build()
		vmCtx.VSphereVM = &infrav1.VSphereVM{ObjectMeta: metav1.ObjectMeta{Namespace: ns}}
		vmCtx.VSphereVM.Spec.BootstrapRef = bootstrapRef

		_, _, err := (&VMService{}).getBootstrapData(context.Background(), &vmCtx.VMContext)
		g.Expect(err).To(HaveOccurred())
		g.Expect(err).To(BeAssignableToTypeOf(&bootstrapSecretGetError{}))

		reason, v1beta2Reason := classifyBootstrapReason(err)
		g.Expect(reason).To(Equal(infrav1.BootstrapSecretGetFailedReason))
		g.Expect(v1beta2Reason).To(Equal(infrav1.VSphereVMBootstrapSecretGetFailedV1Beta2Reason))
	})

	t.Run("secret without value key yields a content error classified as BootstrapSecretContentInvalid", func(t *testing.T) {
		g := NewWithT(t)
		vmCtx := emptyVirtualMachineContext()
		vmCtx.Client = fake.NewClientBuilder().WithObjects(newSecret(map[string][]byte{"format": []byte("cloud-config")})).Build()
		vmCtx.VSphereVM = &infrav1.VSphereVM{ObjectMeta: metav1.ObjectMeta{Namespace: ns}}
		vmCtx.VSphereVM.Spec.BootstrapRef = bootstrapRef

		_, _, err := (&VMService{}).getBootstrapData(context.Background(), &vmCtx.VMContext)
		g.Expect(err).To(HaveOccurred())
		g.Expect(err).To(BeAssignableToTypeOf(&bootstrapSecretContentError{}))

		reason, v1beta2Reason := classifyBootstrapReason(err)
		g.Expect(reason).To(Equal(infrav1.BootstrapSecretContentInvalidReason))
		g.Expect(v1beta2Reason).To(Equal(infrav1.VSphereVMBootstrapSecretContentInvalidV1Beta2Reason))
	})

	t.Run("valid secret returns the bootstrap value", func(t *testing.T) {
		g := NewWithT(t)
		vmCtx := emptyVirtualMachineContext()
		vmCtx.Client = fake.NewClientBuilder().WithObjects(newSecret(map[string][]byte{"value": []byte("#cloud-config\n")})).Build()
		vmCtx.VSphereVM = &infrav1.VSphereVM{ObjectMeta: metav1.ObjectMeta{Namespace: ns}}
		vmCtx.VSphereVM.Spec.BootstrapRef = bootstrapRef

		data, format, err := (&VMService{}).getBootstrapData(context.Background(), &vmCtx.VMContext)
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(string(data)).To(Equal("#cloud-config\n"))
		g.Expect(format).To(Equal(bootstrapv1.CloudConfig))
	})
}
