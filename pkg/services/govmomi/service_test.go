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
	scsiKeys := map[int32]struct{}{scsiKey: {}}

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

	t.Run("tier1: match by VolumePath ignoring controller", func(t *testing.T) {
		g := NewWithT(t)
		// Disk on IDE controller but VolumePath matches — tier 1 should find it.
		disk := newDisk(1, ideKey, int32Ptr(0), 20, "[ds] vm/data.vmdk")
		disks := object.VirtualDeviceList{disk}
		pd := &infrav1.PersistentDisk{Name: "d", SizeGiB: 20, VolumePath: "[ds] vm/data.vmdk"}
		result := findPersistentDiskDevice(pd, disks, map[int32]struct{}{}, map[int32]struct{}{}, scsiKeys)
		g.Expect(result).NotTo(BeNil())
		g.Expect(result.Key).To(Equal(int32(1)))
	})

	t.Run("tier2: match by UnitNumber on SCSI only", func(t *testing.T) {
		g := NewWithT(t)
		osDisk := newDisk(1, ideKey, int32Ptr(0), 300, "[ds] vm/os.vmdk")
		dataDisk := newDisk(2, scsiKey, int32Ptr(0), 20, "[ds] vm/data.vmdk")
		disks := object.VirtualDeviceList{osDisk, dataDisk}

		pd := &infrav1.PersistentDisk{Name: "d", SizeGiB: 20, UnitNumber: int32Ptr(0)}
		result := findPersistentDiskDevice(pd, disks, map[int32]struct{}{}, map[int32]struct{}{}, scsiKeys)
		g.Expect(result).NotTo(BeNil())
		g.Expect(result.Key).To(Equal(int32(2)), "should match SCSI disk, not IDE OS disk")
	})

	t.Run("tier2: skips IDE disk even with matching unit number", func(t *testing.T) {
		g := NewWithT(t)
		osDisk := newDisk(1, ideKey, int32Ptr(1), 20, "[ds] vm/os.vmdk")
		disks := object.VirtualDeviceList{osDisk}

		pd := &infrav1.PersistentDisk{Name: "d", SizeGiB: 20, UnitNumber: int32Ptr(1)}
		result := findPersistentDiskDevice(pd, disks, map[int32]struct{}{}, map[int32]struct{}{}, scsiKeys)
		g.Expect(result).To(BeNil(), "should not match IDE disk by unit number")
	})

	t.Run("tier3: capacity match on SCSI only", func(t *testing.T) {
		g := NewWithT(t)
		osDisk := newDisk(1, ideKey, int32Ptr(0), 20, "[ds] vm/os.vmdk")
		dataDisk := newDisk(2, scsiKey, int32Ptr(0), 20, "[ds] vm/data.vmdk")
		disks := object.VirtualDeviceList{osDisk, dataDisk}

		pd := &infrav1.PersistentDisk{Name: "d", SizeGiB: 20}
		result := findPersistentDiskDevice(pd, disks, map[int32]struct{}{}, map[int32]struct{}{}, scsiKeys)
		g.Expect(result).NotTo(BeNil())
		g.Expect(result.Key).To(Equal(int32(2)), "should match SCSI disk by capacity, not IDE")
	})

	t.Run("tier3: duplicate capacity chooses first deterministic unused candidate", func(t *testing.T) {
		g := NewWithT(t)
		d1 := newDisk(2, scsiKey, int32Ptr(1), 20, "[ds] vm/d2.vmdk")
		d2 := newDisk(1, scsiKey, int32Ptr(0), 20, "[ds] vm/d1.vmdk")
		disks := object.VirtualDeviceList{d1, d2}

		pd := &infrav1.PersistentDisk{Name: "d", SizeGiB: 20}
		result := findPersistentDiskDevice(pd, disks, map[int32]struct{}{}, map[int32]struct{}{}, scsiKeys)
		g.Expect(result).NotTo(BeNil())
		g.Expect(result.Key).To(Equal(int32(1)))
	})

	t.Run("tier3: skips used duplicate capacity candidates", func(t *testing.T) {
		g := NewWithT(t)
		d1 := newDisk(1, scsiKey, int32Ptr(0), 20, "[ds] vm/d1.vmdk")
		d2 := newDisk(2, scsiKey, int32Ptr(1), 20, "[ds] vm/d2.vmdk")
		disks := object.VirtualDeviceList{d1, d2}

		used := map[int32]struct{}{1: {}}
		pd := &infrav1.PersistentDisk{Name: "d", SizeGiB: 20}
		result := findPersistentDiskDevice(pd, disks, used, map[int32]struct{}{}, scsiKeys)
		g.Expect(result).NotTo(BeNil())
		g.Expect(result.Key).To(Equal(int32(2)))
	})

	t.Run("tier3: skips referenced duplicate capacity candidates", func(t *testing.T) {
		g := NewWithT(t)
		d1 := newDisk(1, scsiKey, int32Ptr(0), 20, "[ds] vm/d1.vmdk")
		d2 := newDisk(2, scsiKey, int32Ptr(1), 20, "[ds] vm/d2.vmdk")
		disks := object.VirtualDeviceList{d1, d2}

		referenced := findReferencedPersistentDiskKeys([]infrav1.PersistentDisk{{Name: "other", UnitNumber: int32Ptr(0)}}, disks, scsiKeys)
		pd := &infrav1.PersistentDisk{Name: "d", SizeGiB: 20}
		result := findPersistentDiskDevice(pd, disks, map[int32]struct{}{}, referenced, scsiKeys)
		g.Expect(result).NotTo(BeNil())
		g.Expect(result.Key).To(Equal(int32(2)))
	})

	t.Run("tier3: input device order does not affect deterministic selection", func(t *testing.T) {
		g := NewWithT(t)
		d1 := newDisk(1, scsiKey, int32Ptr(0), 20, "[ds] vm/d1.vmdk")
		d2 := newDisk(2, scsiKey, int32Ptr(1), 20, "[ds] vm/d2.vmdk")
		pd := &infrav1.PersistentDisk{Name: "d", SizeGiB: 20}

		resultA := findPersistentDiskDevice(pd, object.VirtualDeviceList{d1, d2}, map[int32]struct{}{}, map[int32]struct{}{}, scsiKeys)
		resultB := findPersistentDiskDevice(pd, object.VirtualDeviceList{d2, d1}, map[int32]struct{}{}, map[int32]struct{}{}, scsiKeys)
		g.Expect(resultA).NotTo(BeNil())
		g.Expect(resultB).NotTo(BeNil())
		g.Expect(resultA.Key).To(Equal(int32(1)))
		g.Expect(resultB.Key).To(Equal(int32(1)))
	})

	t.Run("nil pd returns nil", func(t *testing.T) {
		g := NewWithT(t)
		result := findPersistentDiskDevice(nil, object.VirtualDeviceList{}, map[int32]struct{}{}, map[int32]struct{}{}, scsiKeys)
		g.Expect(result).To(BeNil())
	})
}
