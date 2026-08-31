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
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	"sigs.k8s.io/cluster-api/util/conditions"
	"sigs.k8s.io/cluster-api/util/patch"
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
						{DHCP4: true},
						{DHCP4: true},
					},
				},
			},
		},
	}
	vmCtx.MachineConfigSlot = &infrav1.MachineConfigSlot{
		Hostname: "worker-01",
	}
	vmCtx.State = &infrav1.VirtualMachine{
		Network: []infrav1.NetworkStatus{
			{IPAddrs: []string{"192.168.132.20"}, NetworkName: "net-a"},
			{IPAddrs: []string{"192.168.164.245"}, NetworkName: "net-b"},
		},
	}
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
	block, _ := pem.Decode(updatedSecret.Data[bootstrapSecretKubeletServingCertKey])
	g.Expect(block).NotTo(BeNil())
	cert, err := x509.ParseCertificate(block.Bytes)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(cert.DNSNames).To(ContainElement("worker-01"))
	g.Expect(certIPStrings(cert)).To(ContainElement("192.168.132.20"))
	g.Expect(certIPStrings(cert)).To(ContainElement("192.168.164.245"))

	firstCert := append([]byte(nil), updatedSecret.Data[bootstrapSecretKubeletServingCertKey]...)
	firstKey := append([]byte(nil), updatedSecret.Data[bootstrapSecretKubeletServingKeyKey]...)

	config, err = vms.buildKubeletServingCertCloudConfig(context.Background(), vmCtx)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(config).ToNot(BeEmpty())

	err = vmCtx.Client.Get(context.Background(), client.ObjectKey{Name: "bootstrap-secret", Namespace: "cpaas-system"}, updatedSecret)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(updatedSecret.Data[bootstrapSecretKubeletServingCertKey]).To(Equal(firstCert))
	g.Expect(updatedSecret.Data[bootstrapSecretKubeletServingKeyKey]).To(Equal(firstKey))

	vmCtx.State.Network = []infrav1.NetworkStatus{
		{IPAddrs: []string{"192.168.132.21"}, NetworkName: "net-a"},
		{IPAddrs: []string{"192.168.164.245"}, NetworkName: "net-b"},
	}

	config, err = vms.buildKubeletServingCertCloudConfig(context.Background(), vmCtx)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(config).ToNot(BeEmpty())

	err = vmCtx.Client.Get(context.Background(), client.ObjectKey{Name: "bootstrap-secret", Namespace: "cpaas-system"}, updatedSecret)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(updatedSecret.Data[bootstrapSecretKubeletServingCertKey]).NotTo(Equal(firstCert))
	g.Expect(updatedSecret.Data[bootstrapSecretKubeletServingKeyKey]).NotTo(Equal(firstKey))

	block, _ = pem.Decode(updatedSecret.Data[bootstrapSecretKubeletServingCertKey])
	g.Expect(block).NotTo(BeNil())
	cert, err = x509.ParseCertificate(block.Bytes)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(cert.DNSNames).To(ContainElement("worker-01"))
	g.Expect(certIPStrings(cert)).To(ContainElement("192.168.132.21"))
	g.Expect(certIPStrings(cert)).ToNot(ContainElement("192.168.132.20"))
}

func certIPStrings(cert *x509.Certificate) []string {
	out := make([]string, 0, len(cert.IPAddresses))
	for _, ip := range cert.IPAddresses {
		out = append(out, ip.String())
	}
	return out
}

func TestResolveNodeIdentityAllowsMissingNodeIP(t *testing.T) {
	t.Run("slot without network resolved yet", func(t *testing.T) {
		g := NewWithT(t)
		vmCtx := emptyVirtualMachineContext()
		vmCtx.VSphereVM = &infrav1.VSphereVM{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "capv-test-md-0-abcde",
				Namespace: "cpaas-system",
			},
		}
		vmCtx.MachineConfigSlot = &infrav1.MachineConfigSlot{
			Hostname: "worker-01",
		}

		identity, err := resolveNodeIdentity(vmCtx)
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(identity.Hostname).To(Equal("worker-01"))
		g.Expect(identity.NodeIP).To(BeEmpty())
	})

	t.Run("primary slot network without usable ip yet", func(t *testing.T) {
		g := NewWithT(t)
		vmCtx := emptyVirtualMachineContext()
		vmCtx.VSphereVM = &infrav1.VSphereVM{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "capv-test-md-0-abcde",
				Namespace: "cpaas-system",
			},
			Spec: infrav1.VSphereVMSpec{
				VirtualMachineCloneSpec: infrav1.VirtualMachineCloneSpec{
					Network: infrav1.NetworkSpec{
						Devices: []infrav1.NetworkDeviceSpec{
							{NetworkName: "net-a"},
						},
					},
				},
			},
		}
		vmCtx.MachineConfigSlot = &infrav1.MachineConfigSlot{
			Hostname: "worker-02",
			Network: &infrav1.MachineConfigSlotNetwork{
				Primary: infrav1.NetworkConfig{NetworkName: "net-a"},
			},
		}
		vmCtx.State = &infrav1.VirtualMachine{
			Network: []infrav1.NetworkStatus{
				{NetworkName: "net-a"},
			},
		}

		identity, err := resolveNodeIdentity(vmCtx)
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(identity.Hostname).To(Equal("worker-02"))
		g.Expect(identity.NodeIP).To(BeEmpty())
	})
}

func TestReconcilePowerStateInitialPowerOnLatch(t *testing.T) {
	newVSphereVM := func() *infrav1.VSphereVM {
		return &infrav1.VSphereVM{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "vspherevm1",
				Namespace: "default",
			},
		}
	}

	t.Run("powered-on VM latches InitialPowerOnCompleted and reflects PoweredOn", func(t *testing.T) {
		g := NewWithT(t)
		simulator.Run(func(ctx context.Context, c *vim25.Client) error {
			finder := find.NewFinder(c)
			vm, err := finder.VirtualMachine(ctx, "DC0_H0_VM0") // powered on by default
			g.Expect(err).ToNot(HaveOccurred())

			vmCtx := emptyVirtualMachineContext()
			vmCtx.Obj = vm
			vmCtx.VSphereVM = newVSphereVM()

			ok, err := (&VMService{}).reconcilePowerState(ctx, vmCtx)
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(ok).To(BeTrue())
			g.Expect(conditions.IsTrue(vmCtx.VSphereVM, infrav1.InitialPowerOnCompletedCondition)).To(BeTrue())
			g.Expect(conditions.IsTrue(vmCtx.VSphereVM, infrav1.PoweredOnCondition)).To(BeTrue())
			return nil
		})
	})

	t.Run("once latched a powered-off VM is not powered back on and surfaces not ready", func(t *testing.T) {
		g := NewWithT(t)
		simulator.Run(func(ctx context.Context, c *vim25.Client) error {
			vm, err := getPoweredoffVM(ctx, c)
			g.Expect(err).ToNot(HaveOccurred())

			vmCtx := emptyVirtualMachineContext()
			vmCtx.Obj = vm
			vmCtx.VSphereVM = newVSphereVM()
			// Simulate a VM that has already completed its initial power-on and is now
			// stopped out of band by an operator.
			conditions.MarkTrue(vmCtx.VSphereVM, infrav1.InitialPowerOnCompletedCondition)
			vmCtx.VSphereVM.Status.Ready = true

			ok, err := (&VMService{}).reconcilePowerState(ctx, vmCtx)
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(ok).To(BeFalse())

			// The controller must not power the VM back on.
			powerState, err := vm.PowerState(ctx)
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(powerState).To(Equal(types.VirtualMachinePowerStatePoweredOff))

			// The powered-off state is reflected and drags Ready down.
			g.Expect(conditions.IsFalse(vmCtx.VSphereVM, infrav1.PoweredOnCondition)).To(BeTrue())
			g.Expect(conditions.GetReason(vmCtx.VSphereVM, infrav1.PoweredOnCondition)).To(Equal(infrav1.PoweredOffReason))
			g.Expect(vmCtx.VSphereVM.Status.Ready).To(BeFalse())
			// The latch itself is never cleared.
			g.Expect(conditions.IsTrue(vmCtx.VSphereVM, infrav1.InitialPowerOnCompletedCondition)).To(BeTrue())
			return nil
		})
	})

	t.Run("before the latch a powered-off VM is powered on as part of provisioning", func(t *testing.T) {
		g := NewWithT(t)
		simulator.Run(func(ctx context.Context, c *vim25.Client) error {
			vm, err := getPoweredoffVM(ctx, c)
			g.Expect(err).ToNot(HaveOccurred())

			scheme := runtime.NewScheme()
			g.Expect(infrav1.AddToScheme(scheme)).To(Succeed())
			vsphereVM := newVSphereVM()
			cl := fake.NewClientBuilder().WithScheme(scheme).
				WithObjects(vsphereVM).WithStatusSubresource(vsphereVM).Build()
			helper, err := patch.NewHelper(vsphereVM, cl)
			g.Expect(err).ToNot(HaveOccurred())

			vmCtx := emptyVirtualMachineContext()
			vmCtx.Obj = vm
			vmCtx.VSphereVM = vsphereVM
			vmCtx.PatchHelper = helper

			ok, err := (&VMService{}).reconcilePowerState(ctx, vmCtx)
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(ok).To(BeFalse())
			// A power-on task was triggered and the latch is not set yet: it only latches
			// once the VM is actually observed powered on.
			g.Expect(vmCtx.VSphereVM.Status.TaskRef).ToNot(BeEmpty())
			g.Expect(conditions.IsTrue(vmCtx.VSphereVM, infrav1.InitialPowerOnCompletedCondition)).To(BeFalse())
			return nil
		})
	})
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

func TestDetachPersistentDisksIgnoresEphemeralDisks(t *testing.T) {
	vms := &VMService{}

	dataDiskCount := func(ctx context.Context, vm *object.VirtualMachine) (int, int32) {
		devices, err := vm.Device(ctx)
		if err != nil {
			return -1, 0
		}
		disks := devices.SelectByType((*types.VirtualDisk)(nil))
		var unit int32
		if len(disks) > 0 {
			if u := disks[0].(*types.VirtualDisk).UnitNumber; u != nil {
				unit = *u
			}
		}
		return len(disks), unit
	}

	newVMCtx := func(vm *object.VirtualMachine, slot *infrav1.MachineConfigSlot) *virtualMachineContext {
		vmCtx := emptyVirtualMachineContext()
		vmCtx.Obj = vm
		vmCtx.VSphereVM = &infrav1.VSphereVM{
			ObjectMeta: metav1.ObjectMeta{Name: "vspherevm1", Namespace: "default"},
		}
		vmCtx.MachineConfigSlot = slot
		return vmCtx
	}

	t.Run("a slot with only ephemeral disks detaches nothing", func(t *testing.T) {
		g := NewWithT(t)
		simulator.Run(func(ctx context.Context, c *vim25.Client) error {
			vm, err := getPoweredoffVM(ctx, c)
			g.Expect(err).ToNot(HaveOccurred())

			before, unit := dataDiskCount(ctx, vm)
			g.Expect(before).To(BeNumerically(">", 0))

			// The ephemeral disk even names the attached disk's unit; detach must
			// still leave it in place because it only ever operates on persistent
			// disks.
			slot := &infrav1.MachineConfigSlot{
				Hostname:       "worker-01",
				EphemeralDisks: []infrav1.EphemeralDisk{{Name: "cache-1", SizeGiB: 10, UnitNumber: ptr.To(unit)}},
			}
			g.Expect(vms.detachPersistentDisks(ctx, newVMCtx(vm, slot))).To(Succeed())

			after, _ := dataDiskCount(ctx, vm)
			g.Expect(after).To(Equal(before), "ephemeral disks must not be detached")
			return nil
		})
	})

	t.Run("control: a persistent disk at the same unit is detached", func(t *testing.T) {
		g := NewWithT(t)
		simulator.Run(func(ctx context.Context, c *vim25.Client) error {
			vm, err := getPoweredoffVM(ctx, c)
			g.Expect(err).ToNot(HaveOccurred())

			before, unit := dataDiskCount(ctx, vm)
			g.Expect(before).To(BeNumerically(">", 0))

			slot := &infrav1.MachineConfigSlot{
				Hostname:        "worker-01",
				PersistentDisks: []infrav1.PersistentDisk{{Name: "data-1", SizeGiB: 10, UnitNumber: ptr.To(unit)}},
			}
			g.Expect(vms.detachPersistentDisks(ctx, newVMCtx(vm, slot))).To(Succeed())

			after, _ := dataDiskCount(ctx, vm)
			g.Expect(after).To(Equal(before-1), "the persistent disk at the matched unit should be detached")
			return nil
		})
	})
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

func TestReconcilePersistentDiskStatusesRequiresCompleteMetadata(t *testing.T) {
	g := NewWithT(t)
	vms := &VMService{}

	simulator.Run(func(ctx context.Context, c *vim25.Client) error {
		vm, err := getPoweredoffVM(ctx, c)
		g.Expect(err).ToNot(HaveOccurred())

		vmCtx := emptyVirtualMachineContext()
		vmCtx.Obj = vm
		vmCtx.VSphereVM = &infrav1.VSphereVM{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "vspherevm1",
				Namespace: "default",
			},
		}
		vmCtx.MachineConfigSlot = &infrav1.MachineConfigSlot{
			Hostname: "worker-01",
			PersistentDisks: []infrav1.PersistentDisk{{
				Name:    "data-missing",
				SizeGiB: 999,
			}},
		}

		err = vms.reconcilePersistentDiskStatuses(ctx, vmCtx)
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("persistent disk metadata incomplete for machine config slot \"worker-01\""))
		g.Expect(err.Error()).To(ContainSubstring("refusing to generate persistent disk user-data or power on VM"))
		g.Expect(err.Error()).To(ContainSubstring("data-missing"))
		g.Expect(err.Error()).To(ContainSubstring("unitNumber"))
		g.Expect(err.Error()).To(ContainSubstring("volumePath"))
		g.Expect(err.Error()).To(ContainSubstring("diskUUID"))
		return nil
	})
}

func TestFindSCSIControllerKeys(t *testing.T) {
	g := NewWithT(t)

	scsi1 := &types.ParaVirtualSCSIController{
		VirtualSCSIController: types.VirtualSCSIController{
			VirtualController: types.VirtualController{
				VirtualDevice: types.VirtualDevice{Key: 1000},
			},
		},
	}
	scsi2 := &types.ParaVirtualSCSIController{
		VirtualSCSIController: types.VirtualSCSIController{
			VirtualController: types.VirtualController{
				VirtualDevice: types.VirtualDevice{Key: 1001},
			},
		},
	}
	ide := &types.VirtualIDEController{
		VirtualController: types.VirtualController{
			VirtualDevice: types.VirtualDevice{Key: 200},
		},
	}

	keys := findSCSIControllerKeys(object.VirtualDeviceList{scsi1, ide, scsi2})
	g.Expect(keys).To(HaveLen(2))
	g.Expect(keys).To(HaveKey(int32(1000)))
	g.Expect(keys).To(HaveKey(int32(1001)))

	empty := findSCSIControllerKeys(object.VirtualDeviceList{ide})
	g.Expect(empty).To(BeEmpty())
}

func TestFindPersistentDiskDevice(t *testing.T) {
	scsiKey := int32(1000)
	ideKey := int32(200)

	newDisk := func(key, controllerKey int32, unitNumber *int32, capacityGiB int32, fileName string) *types.VirtualDisk {
		backing := &types.VirtualDiskFlatVer2BackingInfo{
			VirtualDeviceFileBackingInfo: types.VirtualDeviceFileBackingInfo{FileName: fileName},
		}
		return &types.VirtualDisk{
			VirtualDevice: types.VirtualDevice{
				Key:           key,
				ControllerKey: controllerKey,
				UnitNumber:    unitNumber,
				Backing:       backing,
			},
			CapacityInKB: int64(capacityGiB) * 1024 * 1024,
		}
	}
	int32Ptr := func(v int32) *int32 { return &v }

	t.Run("match by expectedPath ignoring controller", func(t *testing.T) {
		g := NewWithT(t)
		// Disk on IDE controller but expectedPath matches — path identity is enough.
		disk := newDisk(1, ideKey, int32Ptr(0), 20, "[ds] vm/data.vmdk")
		disks := object.VirtualDeviceList{disk}
		pd := &infrav1.PersistentDisk{Name: "d", SizeGiB: 20, VolumePath: "[ds] vm/data.vmdk"}
		result := findPersistentDiskDevice(pd, "[ds] vm/data.vmdk", "", disks, map[int32]struct{}{})
		g.Expect(result).NotTo(BeNil())
		g.Expect(result.Key).To(Equal(int32(1)))
	})

	t.Run("tier1 self-heal: match by derived expectedPath when VolumePath lost", func(t *testing.T) {
		g := NewWithT(t)
		// Status write was lost so pd.VolumePath is empty, but the disk was created
		// at the deterministic path; expectedPath re-derives it and matches.
		disk := newDisk(1, scsiKey, int32Ptr(3), 20, "[ds] vm/data.vmdk")
		disks := object.VirtualDeviceList{disk}
		pd := &infrav1.PersistentDisk{Name: "d", SizeGiB: 20}
		result := findPersistentDiskDevice(pd, "[ds] vm/data.vmdk", "", disks, map[int32]struct{}{})
		g.Expect(result).NotTo(BeNil())
		g.Expect(result.Key).To(Equal(int32(1)))
	})

	t.Run("only expectedPath identifies the disk", func(t *testing.T) {
		g := NewWithT(t)
		recorded := newDisk(1, scsiKey, int32Ptr(0), 20, "[ds] vm/recorded.vmdk")
		derived := newDisk(2, scsiKey, int32Ptr(1), 20, "[ds] vm/derived.vmdk")
		disks := object.VirtualDeviceList{recorded, derived}
		pd := &infrav1.PersistentDisk{Name: "d", SizeGiB: 20, VolumePath: "[ds] vm/recorded.vmdk"}
		result := findPersistentDiskDevice(pd, "[ds] vm/derived.vmdk", "", disks, map[int32]struct{}{})
		g.Expect(result).NotTo(BeNil())
		g.Expect(result.Key).To(Equal(int32(2)))
	})

	t.Run("match by exact VMDK name when datastore is unknown", func(t *testing.T) {
		g := NewWithT(t)
		disk := newDisk(1, scsiKey, int32Ptr(3), 20, "[selected-ds] vm/data.vmdk")
		pd := &infrav1.PersistentDisk{Name: "d", SizeGiB: 20}
		result := findPersistentDiskDevice(pd, "", "data.vmdk", object.VirtualDeviceList{disk}, map[int32]struct{}{})
		g.Expect(result).NotTo(BeNil())
		g.Expect(result.Key).To(Equal(int32(1)))
	})

	t.Run("ambiguous VMDK name returns nil", func(t *testing.T) {
		g := NewWithT(t)
		d1 := newDisk(1, scsiKey, int32Ptr(3), 20, "[ds-a] vm/data.vmdk")
		d2 := newDisk(2, scsiKey, int32Ptr(4), 20, "[ds-b] vm/data.vmdk")
		pd := &infrav1.PersistentDisk{Name: "d", SizeGiB: 20}
		result := findPersistentDiskDevice(pd, "", "data.vmdk", object.VirtualDeviceList{d1, d2}, map[int32]struct{}{})
		g.Expect(result).To(BeNil())
	})

	t.Run("unit number alone does not identify a disk", func(t *testing.T) {
		g := NewWithT(t)
		osDisk := newDisk(1, ideKey, int32Ptr(0), 300, "[ds] vm/os.vmdk")
		dataDisk := newDisk(2, scsiKey, int32Ptr(0), 20, "[ds] vm/data.vmdk")
		disks := object.VirtualDeviceList{osDisk, dataDisk}

		pd := &infrav1.PersistentDisk{Name: "d", SizeGiB: 20, UnitNumber: int32Ptr(0)}
		result := findPersistentDiskDevice(pd, "", "", disks, map[int32]struct{}{})
		g.Expect(result).To(BeNil())
	})

	t.Run("unit number fallback is disabled even for a single disk", func(t *testing.T) {
		g := NewWithT(t)
		osDisk := newDisk(1, ideKey, int32Ptr(1), 20, "[ds] vm/os.vmdk")
		disks := object.VirtualDeviceList{osDisk}

		pd := &infrav1.PersistentDisk{Name: "d", SizeGiB: 20, UnitNumber: int32Ptr(1)}
		result := findPersistentDiskDevice(pd, "", "", disks, map[int32]struct{}{})
		g.Expect(result).To(BeNil())
	})

	t.Run("no capacity fallback: same-size SCSI disks without path or unit return nil", func(t *testing.T) {
		g := NewWithT(t)
		// Two same-capacity SCSI disks, but pd carries neither a path nor a unit —
		// the removed tier 3 would have guessed one; now the match must fail so
		// ValidatePersistentDiskBackfill refuses power-on instead of guessing.
		d1 := newDisk(1, scsiKey, int32Ptr(0), 20, "[ds] vm/d1.vmdk")
		d2 := newDisk(2, scsiKey, int32Ptr(1), 20, "[ds] vm/d2.vmdk")
		disks := object.VirtualDeviceList{d1, d2}

		pd := &infrav1.PersistentDisk{Name: "d", SizeGiB: 20}
		result := findPersistentDiskDevice(pd, "", "", disks, map[int32]struct{}{})
		g.Expect(result).To(BeNil())
	})

	t.Run("no capacity fallback: single same-size candidate still returns nil", func(t *testing.T) {
		g := NewWithT(t)
		// Even a lone same-size candidate is not enough: identity must come from
		// the expected path, not capacity.
		only := newDisk(1, scsiKey, int32Ptr(0), 20, "[ds] vm/d1.vmdk")
		disks := object.VirtualDeviceList{only}

		pd := &infrav1.PersistentDisk{Name: "d", SizeGiB: 20}
		result := findPersistentDiskDevice(pd, "", "", disks, map[int32]struct{}{})
		g.Expect(result).To(BeNil())
	})

	t.Run("nil pd returns nil", func(t *testing.T) {
		g := NewWithT(t)
		result := findPersistentDiskDevice(nil, "", "", object.VirtualDeviceList{}, map[int32]struct{}{})
		g.Expect(result).To(BeNil())
	})
}
