/*
Copyright 2022 The Kubernetes Authors.

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
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	"github.com/vmware/govmomi/find"
	"github.com/vmware/govmomi/object"
	pbmsimulator "github.com/vmware/govmomi/pbm/simulator"
	"github.com/vmware/govmomi/simulator"
	"github.com/vmware/govmomi/vim25"
	"github.com/vmware/govmomi/vim25/types"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	infrav1 "sigs.k8s.io/cluster-api-provider-vsphere/apis/v1beta1"
	capvcontext "sigs.k8s.io/cluster-api-provider-vsphere/pkg/context"
	"sigs.k8s.io/cluster-api-provider-vsphere/pkg/session"
)

const (
	defaultStoragePolicy = "vSAN Default Storage Policy"
)

func emptyVirtualMachineContext() *virtualMachineContext {
	return &virtualMachineContext{
		VMContext: capvcontext.VMContext{
			ControllerManagerContext: &capvcontext.ControllerManagerContext{},
		},
	}
}

func Test_reconcilePCIDevices(t *testing.T) {
	var vmCtx *virtualMachineContext
	var g *WithT
	var vms *VMService

	before := func() {
		vmCtx = emptyVirtualMachineContext()
		vmCtx.Client = fake.NewClientBuilder().Build()

		vms = &VMService{}
	}

	t.Run("when powered off VM has no PCI devices", func(t *testing.T) {
		g = NewWithT(t)
		before()

		simulator.Run(func(ctx context.Context, c *vim25.Client) error {
			finder := find.NewFinder(c)
			vm, err := finder.VirtualMachine(ctx, "DC0_H0_VM0")
			g.Expect(err).ToNot(HaveOccurred())
			_, err = vm.PowerOff(ctx)
			g.Expect(err).ToNot(HaveOccurred())

			vmCtx.Obj = vm
			vmCtx.VSphereVM = &infrav1.VSphereVM{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "vsphereVM1",
					Namespace: "my-namespace",
				},
				Spec: infrav1.VSphereVMSpec{
					VirtualMachineCloneSpec: infrav1.VirtualMachineCloneSpec{
						PciDevices: []infrav1.PCIDeviceSpec{
							{DeviceID: ptr.To[int32](1234), VendorID: ptr.To[int32](5678)},
							{DeviceID: ptr.To[int32](1234), VendorID: ptr.To[int32](5678)},
						},
					},
				},
			}

			g.Expect(vms.reconcilePCIDevices(ctx, vmCtx)).ToNot(HaveOccurred())

			// get the VM's virtual device list
			devices, err := vm.Device(ctx)
			g.Expect(err).ToNot(HaveOccurred())
			// filter the device with the given backing info
			pciDevices := devices.SelectByBackingInfo(&types.VirtualPCIPassthroughDynamicBackingInfo{
				AllowedDevice: []types.VirtualPCIPassthroughAllowedDevice{
					{DeviceId: 1234, VendorId: 5678},
				},
			})
			g.Expect(pciDevices).To(HaveLen(2))
			return nil
		})
	})
}

func Test_ReconcileStoragePolicy(t *testing.T) {
	var vmCtx *virtualMachineContext
	var g *WithT
	var vms *VMService

	before := func() {
		vmCtx = emptyVirtualMachineContext()
		vmCtx.Client = fake.NewClientBuilder().Build()

		vms = &VMService{}
	}
	t.Run("when VM has no storage policy spec", func(t *testing.T) {
		g = NewWithT(t)
		before()
		vmCtx.VSphereVM = &infrav1.VSphereVM{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "vsphereVM1",
				Namespace: "my-namespace",
			},
			Spec: infrav1.VSphereVMSpec{
				VirtualMachineCloneSpec: infrav1.VirtualMachineCloneSpec{},
			},
		}
		g.Expect(vms.reconcileStoragePolicy(context.Background(), vmCtx)).ToNot(HaveOccurred())
		g.Expect(vmCtx.VSphereVM.Status.TaskRef).To(BeEmpty())
	})

	t.Run("when the requested storage policy does not exists should fail", func(t *testing.T) {
		g = NewWithT(t)
		before()
		model, err := storagePolicyModel()
		g.Expect(err).ToNot(HaveOccurred())

		simulator.Run(func(ctx context.Context, c *vim25.Client) error {
			authSession, err := getAuthSession(ctx, model.Service.Listen.Host)
			g.Expect(err).ToNot(HaveOccurred())
			vmCtx.Session = authSession
			vm, err := getPoweredoffVM(ctx, c)
			g.Expect(err).ToNot(HaveOccurred())

			vmCtx.Obj = vm
			vmCtx.VSphereVM = &infrav1.VSphereVM{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "vsphereVM1",
					Namespace: "my-namespace",
				},
				Spec: infrav1.VSphereVMSpec{
					VirtualMachineCloneSpec: infrav1.VirtualMachineCloneSpec{
						StoragePolicyName: "non-existing-storagepolicy",
					},
				},
			}
			err = vms.reconcileStoragePolicy(context.Background(), vmCtx)
			g.Expect(err.Error()).To(ContainSubstring("no pbm profile found with name"))
			return nil
		}, model)
	})

	t.Run("when the requested storage policy exists should pass", func(t *testing.T) {
		// This Method should be implemented on Govmomi vcsim and then we can unskip this test
		t.Skip("PbmQueryAssociatedProfiles is not yet implemented on PBM simulator")
		g = NewWithT(t)
		before()
		model, err := storagePolicyModel()
		g.Expect(err).ToNot(HaveOccurred())

		simulator.Run(func(ctx context.Context, c *vim25.Client) error {
			authSession, err := getAuthSession(ctx, model.Service.Listen.Host)
			g.Expect(err).ToNot(HaveOccurred())
			vmCtx.Session = authSession
			vm, err := getPoweredoffVM(ctx, c)
			g.Expect(err).ToNot(HaveOccurred())

			vmCtx.Obj = vm
			vmCtx.VSphereVM = &infrav1.VSphereVM{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "vsphereVM1",
					Namespace: "my-namespace",
				},
				Spec: infrav1.VSphereVMSpec{
					VirtualMachineCloneSpec: infrav1.VirtualMachineCloneSpec{
						StoragePolicyName: defaultStoragePolicy,
					},
				},
			}
			err = vms.reconcileStoragePolicy(context.Background(), vmCtx)
			g.Expect(err).ToNot(HaveOccurred())
			return nil
		}, model)
	})
}

func Test_buildKubeletServingCertCloudConfig(t *testing.T) {
	g := NewWithT(t)
	vms := &VMService{}
	caCertPEM, caKeyPEM := newTestCA(t)
	bootstrapSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "bootstrap-secret",
			Namespace: "cpaas-system",
		},
		Data: map[string][]byte{
			"value": []byte("#cloud-config\n"),
		},
	}

	vmCtx := emptyVirtualMachineContext()
	vmCtx.Client = fake.NewClientBuilder().WithObjects(
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "capv-test-ca",
				Namespace: "cpaas-system",
			},
			Data: map[string][]byte{
				"tls.crt": caCertPEM,
				"tls.key": caKeyPEM,
			},
		},
		bootstrapSecret,
	).Build()
	vmCtx.VSphereVM = &infrav1.VSphereVM{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "capv-test-md-0-abcde",
			Namespace: "cpaas-system",
			Labels: map[string]string{
				clusterv1.ClusterNameLabel: "capv-test",
			},
		},
		Spec: infrav1.VSphereVMSpec{
			VirtualMachineCloneSpec: infrav1.VirtualMachineCloneSpec{
				Network: infrav1.NetworkSpec{
					Devices: []infrav1.NetworkDeviceSpec{
						{IPAddrs: []string{"192.168.132.20/20"}},
						{IPAddrs: []string{"192.168.164.245/24"}},
					},
				},
			},
		},
	}
	vmCtx.ResourceSlot = &infrav1.ResourceSlot{Hostname: "worker-01"}
	vmCtx.State = &infrav1.VirtualMachine{}
	vmCtx.VSphereVM.Spec.BootstrapRef = &corev1.ObjectReference{
		Kind:      "Secret",
		Namespace: "cpaas-system",
		Name:      "bootstrap-secret",
	}

	config, err := vms.buildKubeletServingCertCloudConfig(context.Background(), vmCtx)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(config).ToNot(BeEmpty())
	g.Expect(string(config)).To(ContainSubstring("/etc/kubernetes/pki/kubelet.crt"))
	g.Expect(string(config)).To(ContainSubstring("/etc/kubernetes/pki/kubelet.key"))
	g.Expect(string(config)).ToNot(ContainSubstring("kubelet-serving-openssl.cnf"))

	updatedSecret := &corev1.Secret{}
	err = vmCtx.Client.Get(context.Background(), client.ObjectKey{Name: "bootstrap-secret", Namespace: "cpaas-system"}, updatedSecret)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(updatedSecret.Data[bootstrapSecretKubeletServingCertKey]).NotTo(BeEmpty())
	g.Expect(updatedSecret.Data[bootstrapSecretKubeletServingKeyKey]).NotTo(BeEmpty())

	firstCert := append([]byte(nil), updatedSecret.Data[bootstrapSecretKubeletServingCertKey]...)
	firstKey := append([]byte(nil), updatedSecret.Data[bootstrapSecretKubeletServingKeyKey]...)

	config, err = vms.buildKubeletServingCertCloudConfig(context.Background(), vmCtx)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(config).ToNot(BeEmpty())

	err = vmCtx.Client.Get(context.Background(), client.ObjectKey{Name: "bootstrap-secret", Namespace: "cpaas-system"}, updatedSecret)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(updatedSecret.Data[bootstrapSecretKubeletServingCertKey]).To(Equal(firstCert))
	g.Expect(updatedSecret.Data[bootstrapSecretKubeletServingKeyKey]).To(Equal(firstKey))
}

func getAuthSession(ctx context.Context, server string) (*session.Session, error) {
	password, _ := simulator.DefaultLogin.Password()
	return session.GetOrCreate(
		ctx,
		session.NewParams().
			WithUserInfo(simulator.DefaultLogin.Username(), password).
			WithServer(fmt.Sprintf("http://%s", server)).
			WithDatacenter("*"))
}

func newTestCA(t *testing.T) ([]byte, []byte) {
	t.Helper()

	caKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("failed to generate CA key: %v", err)
	}
	serialNumber, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		t.Fatalf("failed to generate CA serial number: %v", err)
	}
	caTemplate := &x509.Certificate{
		SerialNumber: serialNumber,
		Subject: pkix.Name{
			CommonName: "test-ca",
		},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().AddDate(10, 0, 0),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("failed to create CA cert: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}),
		pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(caKey)})
}

func getPoweredoffVM(ctx context.Context, c *vim25.Client) (*object.VirtualMachine, error) {
	finder := find.NewFinder(c)
	vm, err := finder.VirtualMachine(ctx, "DC0_H0_VM0")
	if err != nil {
		return nil, err
	}

	_, err = vm.PowerOff(ctx)
	return vm, err
}

func storagePolicyModel() (*simulator.Model, error) {
	model := simulator.VPX()
	err := model.Create()
	if err != nil {
		return nil, err
	}
	model.Service.RegisterSDK(pbmsimulator.New())
	model.Machine = 1
	model.Datacenter = 1
	model.Host = 1
	return model, nil
}
