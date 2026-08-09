/*
Copyright 2019 The Kubernetes Authors.

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

package util

import (
	"bufio"
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"regexp"
	"strings"
	"text/template"
	"time"

	"github.com/blang/semver/v4"
	"github.com/pkg/errors"
	"gopkg.in/yaml.v2"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	apitypes "k8s.io/apimachinery/pkg/types"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	bootstrapv1 "sigs.k8s.io/cluster-api/bootstrap/kubeadm/api/v1beta1"
	kubeadmtypes "sigs.k8s.io/cluster-api/bootstrap/kubeadm/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrav1 "sigs.k8s.io/cluster-api-provider-vsphere/apis/v1beta1"
	vmwarev1 "sigs.k8s.io/cluster-api-provider-vsphere/apis/vmware/v1beta1"
)

// GetVSphereMachine gets a vmware.infrastructure.cluster.x-k8s.io.VSphereMachine resource for the given CAPI Machine.
func GetVSphereMachine(
	ctx context.Context,
	controllerClient client.Client,
	namespace, machineName string) (*vmwarev1.VSphereMachine, error) {
	machine := &vmwarev1.VSphereMachine{}
	namespacedName := apitypes.NamespacedName{
		Namespace: namespace,
		Name:      machineName,
	}
	if err := controllerClient.Get(ctx, namespacedName, machine); err != nil {
		return nil, err
	}
	return machine, nil
}

// ErrNoMachineIPAddr indicates that no valid IP addresses were found in a machine context.
var ErrNoMachineIPAddr = errors.New("no IP addresses found for machine")

// ErrPrimaryMachineIPAddr indicates that a configured primary slot network did
// not resolve to a usable IP address.
var ErrPrimaryMachineIPAddr = errors.New("no IP addresses found for primary slot network")

var (
	kubernetesVersionRe = regexp.MustCompile(`(?m)^\s*kubernetesVersion:\s*("?[^"\n]+"?)\s*$`)
	kubeadmAPIVersionRe = regexp.MustCompile(`(?m)^\s*apiVersion:\s*kubeadm\.k8s\.io/(v1beta\d+)\s*$`)

	// kubeadmAPIVersionMinKubeVersion maps kubeadm API version suffixes to the
	// minimum Kubernetes version that introduced each API version. These are the
	// boundary values used by kubeadmtypes.KubeVersionToKubeadmAPIGroupVersion
	// and MUST be kept in sync — see TestKubeadmAPIVersionMinKubeVersionMapping.
	kubeadmAPIVersionMinKubeVersion = map[string]semver.Version{
		"v1beta4": semver.MustParse("1.31.0"),
		"v1beta3": semver.MustParse("1.22.0"),
	}
)

// GetMachinePreferredIPAddress returns the preferred IP address for a
// VSphereMachine resource.
func GetMachinePreferredIPAddress(machine *infrav1.VSphereMachine) (string, error) {
	var cidr *net.IPNet
	if cidrString := machine.Spec.Network.PreferredAPIServerCIDR; cidrString != "" {
		var err error
		if _, cidr, err = net.ParseCIDR(cidrString); err != nil {
			return "", errors.New("error parsing preferred API server CIDR")
		}
	}

	for _, machineAddr := range machine.Status.Addresses {
		if machineAddr.Type != clusterv1.MachineExternalIP {
			continue
		}
		if cidr == nil {
			return machineAddr.Address, nil
		}
		if cidr.Contains(net.ParseIP(machineAddr.Address)) {
			return machineAddr.Address, nil
		}
	}

	return "", ErrNoMachineIPAddr
}

// GetPrimaryNodeIPAddress returns the kubelet node IP for the given VM and slot.
// When a slot network is configured, the primary device must resolve to an IP.
// Otherwise, the first resolved device IP is used.
func GetPrimaryNodeIPAddress(vsphereVM infrav1.VSphereVM, slot *infrav1.MachineConfigSlot, ipamState map[string]infrav1.NetworkDeviceSpec, networkStatuses ...infrav1.NetworkStatus) (string, error) {
	devices := observedNetworkDevices(vsphereVM, ipamState, networkStatuses...)

	if slot != nil && slot.Network != nil {
		if len(devices) == 0 {
			return "", errors.Wrapf(ErrPrimaryMachineIPAddr, "primary slot network %q has no resolved device", slot.Network.Primary.NetworkName)
		}
		// devices[0] is the primary network — mergeSlotNetwork always places
		// slot.Network.Primary at index 0 followed by Additional entries.
		if ip := firstUsableDeviceIP(devices[0].IPAddrs); ip != "" {
			return ip, nil
		}
		return "", errors.Wrapf(ErrPrimaryMachineIPAddr, "primary slot network %q has no usable IP addresses", slot.Network.Primary.NetworkName)
	}

	for i := range devices {
		if ip := firstUsableDeviceIP(devices[i].IPAddrs); ip != "" {
			return ip, nil
		}
	}

	return "", ErrNoMachineIPAddr
}

// IsControlPlaneMachine returns true if the provided resource is
// a member of the control plane.
func IsControlPlaneMachine(machine metav1.Object) bool {
	_, ok := machine.GetLabels()[clusterv1.MachineControlPlaneLabel]
	return ok
}

// GetMachineMetadata the cloud-init metadata as a base-64 encoded
// string for a given VSphereMachine.
// IPAM state includes IP and Gateways that should be added to each device.
func GetMachineMetadata(hostname string, vsphereVM infrav1.VSphereVM, ipamState map[string]infrav1.NetworkDeviceSpec, persistentDisks []infrav1.PersistentDisk, networkStatuses ...infrav1.NetworkStatus) ([]byte, error) {
	// Create a copy of the devices and add their MAC addresses from a network status.
	devices := effectiveNetworkDevices(vsphereVM, ipamState, networkStatuses...)

	var waitForIPv4, waitForIPv6 bool
	for i := range vsphereVM.Spec.Network.Devices {
		if waitForIPv4 && waitForIPv6 {
			// break early as we already wait for ipv4 and ipv6
			continue
		}

		// check static IPs
		for _, ipStr := range vsphereVM.Spec.Network.Devices[i].IPAddrs {
			ip := net.ParseIP(ipStr)
			// check the IP family
			if ip != nil {
				if ip.To4() == nil {
					waitForIPv6 = true
				} else {
					waitForIPv4 = true
				}
			}
		}
		// check if DHCP is enabled
		if vsphereVM.Spec.Network.Devices[i].DHCP4 {
			waitForIPv4 = true
		}
		if vsphereVM.Spec.Network.Devices[i].DHCP6 {
			waitForIPv6 = true
		}
	}

	buf := &bytes.Buffer{}
	tpl := template.Must(template.New("t").Funcs(
		template.FuncMap{
			"nameservers": func(spec infrav1.NetworkDeviceSpec) bool {
				return len(spec.Nameservers) > 0 || len(spec.SearchDomains) > 0
			},
		}).Parse(metadataFormat))
	if err := tpl.Execute(buf, struct {
		Hostname        string
		Devices         []infrav1.NetworkDeviceSpec
		Routes          []infrav1.NetworkRouteSpec
		PersistentDisks []infrav1.PersistentDisk
		WaitForIPv4     bool
		WaitForIPv6     bool
	}{
		Hostname:        hostname, // note that hostname determines the Kubernetes node name
		Devices:         devices,
		Routes:          vsphereVM.Spec.Network.Routes,
		PersistentDisks: persistentDisks,
		WaitForIPv4:     waitForIPv4,
		WaitForIPv6:     waitForIPv6,
	}); err != nil {
		return nil, errors.Wrapf(
			err,
			"error getting cloud init metadata for vsphereVM %s/%s",
			vsphereVM.Namespace, vsphereVM.Name)
	}
	return buf.Bytes(), nil
}

func GetKubeletServingCertCloudConfig(hostname string, vsphereVM infrav1.VSphereVM, ipamState map[string]infrav1.NetworkDeviceSpec, caCertPEM, caKeyPEM []byte, networkStatuses ...infrav1.NetworkStatus) ([]byte, error) {
	kubeletCertPEM, kubeletKeyPEM, err := NewKubeletServingCertData(hostname, vsphereVM, ipamState, caCertPEM, caKeyPEM, networkStatuses...)
	if err != nil {
		return nil, err
	}
	return GetKubeletServingCertCloudConfigFromPEM(kubeletCertPEM, kubeletKeyPEM)
}

func GetKubeletServingCertIPs(vsphereVM infrav1.VSphereVM, ipamState map[string]infrav1.NetworkDeviceSpec, networkStatuses ...infrav1.NetworkStatus) []string {
	devices := observedNetworkDevices(vsphereVM, ipamState, networkStatuses...)
	return uniqueIPv4SANs(devices)
}

func NewKubeletServingCertData(hostname string, vsphereVM infrav1.VSphereVM, ipamState map[string]infrav1.NetworkDeviceSpec, caCertPEM, caKeyPEM []byte, networkStatuses ...infrav1.NetworkStatus) ([]byte, []byte, error) {
	sans := GetKubeletServingCertIPs(vsphereVM, ipamState, networkStatuses...)
	if hostname == "" || len(sans) == 0 {
		return nil, nil, nil
	}

	kubeletCertPEM, kubeletKeyPEM, err := newKubeletServingCert(hostname, sans, caCertPEM, caKeyPEM)
	if err != nil {
		return nil, nil, err
	}
	return kubeletCertPEM, kubeletKeyPEM, nil
}

func GetKubeletServingCertCloudConfigFromPEM(kubeletCertPEM, kubeletKeyPEM []byte) ([]byte, error) {
	if len(kubeletCertPEM) == 0 || len(kubeletKeyPEM) == 0 {
		return nil, nil
	}

	config := map[interface{}]interface{}{
		"write_files": []interface{}{
			map[interface{}]interface{}{
				"path":        "/etc/kubernetes/pki/kubelet.crt",
				"permissions": "0600",
				"owner":       "root:root",
				"encoding":    "b64",
				"content":     base64.StdEncoding.EncodeToString(kubeletCertPEM),
			},
			map[interface{}]interface{}{
				"path":        "/etc/kubernetes/pki/kubelet.key",
				"permissions": "0600",
				"owner":       "root:root",
				"encoding":    "b64",
				"content":     base64.StdEncoding.EncodeToString(kubeletKeyPEM),
			},
		},
	}
	return yaml.Marshal(config)
}

// UpdateKubeadmNodeRegistration updates kubeadm init/join configs embedded in
// cloud-config write_files so kubelet bootstrap identity uses the requested node name
// and node IP.
func UpdateKubeadmNodeRegistration(userData []byte, nodeName, nodeIP string) ([]byte, error) {
	if len(userData) == 0 || nodeName == "" {
		return userData, nil
	}

	lines := strings.Split(string(userData), "\n")
	header := make([]string, 0, 2)
	bodyStart := 0
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if len(header) == 0 && trimmed == "" {
			continue
		}
		if strings.HasPrefix(line, "## template:") || trimmed == "#cloud-config" {
			header = append(header, line)
			bodyStart = i + 1
			continue
		}
		break
	}

	body := strings.Join(lines[bodyStart:], "\n")
	config := map[interface{}]interface{}{}
	if err := yaml.Unmarshal([]byte(body), &config); err != nil {
		return nil, errors.Wrap(err, "failed to parse bootstrap cloud-config")
	}

	writeFiles, ok := config["write_files"].([]interface{})
	if !ok || len(writeFiles) == 0 {
		return userData, nil
	}

	updated := false
	for i := range writeFiles {
		entry, ok := writeFiles[i].(map[interface{}]interface{})
		if !ok {
			continue
		}
		path, _ := entry["path"].(string)
		switch path {
		case "/run/kubeadm/kubeadm.yaml":
			content, err := decodeCloudConfigWriteFileContent(entry)
			if err != nil {
				return nil, err
			}
			newContent, err := updateInitConfigurationNodeRegistration(content, nodeName, nodeIP)
			if err != nil {
				return nil, err
			}
			encodeCloudConfigWriteFileContent(entry, newContent)
			updated = true
		case "/run/kubeadm/kubeadm-join-config.yaml":
			content, err := decodeCloudConfigWriteFileContent(entry)
			if err != nil {
				return nil, err
			}
			newContent, err := updateJoinConfigurationNodeRegistration(content, nodeName, nodeIP)
			if err != nil {
				return nil, err
			}
			encodeCloudConfigWriteFileContent(entry, newContent)
			updated = true
		}
	}

	if !updated {
		return userData, nil
	}

	outBody, err := yaml.Marshal(config)
	if err != nil {
		return nil, errors.Wrap(err, "failed to marshal bootstrap cloud-config")
	}

	var out strings.Builder
	if len(header) > 0 {
		out.WriteString(strings.Join(header, "\n"))
		out.WriteString("\n\n")
	}
	out.Write(outBody)
	return []byte(out.String()), nil
}

func decodeCloudConfigWriteFileContent(entry map[interface{}]interface{}) (string, error) {
	content, _ := entry["content"].(string)
	encoding, _ := entry["encoding"].(string)
	if strings.EqualFold(encoding, "b64") || strings.EqualFold(encoding, "base64") {
		decoded, err := base64.StdEncoding.DecodeString(content)
		if err != nil {
			return "", errors.Wrap(err, "failed to decode cloud-config write_files content")
		}
		return string(decoded), nil
	}
	return content, nil
}

func encodeCloudConfigWriteFileContent(entry map[interface{}]interface{}, content string) {
	encoding, _ := entry["encoding"].(string)
	if strings.EqualFold(encoding, "b64") || strings.EqualFold(encoding, "base64") {
		entry["content"] = base64.StdEncoding.EncodeToString([]byte(content))
		return
	}
	entry["content"] = content
}

func updateInitConfigurationNodeRegistration(content, nodeName, nodeIP string) (string, error) {
	clusterConfiguration := &bootstrapv1.ClusterConfiguration{}
	// The upstream UnmarshalInitConfiguration uses UniversalDeserializer which
	// only decodes a single YAML document. Real kubeadm configs are multi-doc
	// (ClusterConfiguration + InitConfiguration), so we must split and try each
	// document individually.
	docs, err := splitYAMLDocuments(content)
	if err != nil {
		return "", errors.Wrap(err, "failed to split kubeadm init configuration documents")
	}
	var initConfiguration *bootstrapv1.InitConfiguration
	for _, doc := range docs {
		init, unmarshalErr := kubeadmtypes.UnmarshalInitConfiguration(doc, clusterConfiguration)
		if unmarshalErr == nil {
			initConfiguration = init
			break
		}
	}
	if initConfiguration == nil {
		return "", errors.New("failed to parse kubeadm init configuration: InitConfiguration not found")
	}
	initConfiguration.NodeRegistration.Name = nodeName
	if initConfiguration.NodeRegistration.KubeletExtraArgs == nil {
		initConfiguration.NodeRegistration.KubeletExtraArgs = map[string]string{}
	}
	if nodeIP != "" {
		initConfiguration.NodeRegistration.KubeletExtraArgs["node-ip"] = nodeIP
	}
	versionString := clusterConfiguration.KubernetesVersion
	if versionString == "" {
		versionString = extractKubernetesVersion(content)
	}
	if versionString == "" {
		versionString = kubeadmAPIVersionToKubernetesVersion(content)
	}
	if versionString == "" {
		return "", errors.New("failed to determine kubernetesVersion from kubeadm init configuration")
	}
	version, err := semver.ParseTolerant(versionString)
	if err != nil {
		return "", errors.Wrap(err, "failed to parse kubeadm init kubernetesVersion")
	}
	out, err := kubeadmtypes.MarshalInitConfigurationForVersion(clusterConfiguration, initConfiguration, version)
	if err != nil {
		return "", errors.Wrap(err, "failed to marshal kubeadm init configuration")
	}

	updatedContent, err := replaceYAMLDocument(content, "InitConfiguration", out)
	if err != nil {
		return "", errors.Wrap(err, "failed to preserve kubeadm cluster configuration")
	}
	return updatedContent, nil
}

func updateJoinConfigurationNodeRegistration(content, nodeName, nodeIP string) (string, error) {
	clusterConfiguration := &bootstrapv1.ClusterConfiguration{}
	docs, err := splitYAMLDocuments(content)
	if err != nil {
		return "", errors.Wrap(err, "failed to split kubeadm join configuration documents")
	}
	var joinConfiguration *bootstrapv1.JoinConfiguration
	for _, doc := range docs {
		join, unmarshalErr := kubeadmtypes.UnmarshalJoinConfiguration(doc, clusterConfiguration)
		if unmarshalErr == nil {
			joinConfiguration = join
			break
		}
	}
	if joinConfiguration == nil {
		return "", errors.New("failed to parse kubeadm join configuration: JoinConfiguration not found")
	}
	joinConfiguration.NodeRegistration.Name = nodeName
	if joinConfiguration.NodeRegistration.KubeletExtraArgs == nil {
		joinConfiguration.NodeRegistration.KubeletExtraArgs = map[string]string{}
	}
	if nodeIP != "" {
		joinConfiguration.NodeRegistration.KubeletExtraArgs["node-ip"] = nodeIP
	}
	versionString := clusterConfiguration.KubernetesVersion
	if versionString == "" {
		versionString = extractKubernetesVersion(content)
	}
	if versionString == "" {
		versionString = kubeadmAPIVersionToKubernetesVersion(content)
	}
	if versionString == "" {
		return "", errors.New("failed to determine kubernetesVersion from kubeadm join configuration")
	}
	version, err := semver.ParseTolerant(versionString)
	if err != nil {
		return "", errors.Wrap(err, "failed to parse kubeadm join kubernetesVersion")
	}
	out, err := kubeadmtypes.MarshalJoinConfigurationForVersion(clusterConfiguration, joinConfiguration, version)
	if err != nil {
		return "", errors.Wrap(err, "failed to marshal kubeadm join configuration")
	}
	return out, nil
}

func extractKubernetesVersion(content string) string {
	matches := kubernetesVersionRe.FindStringSubmatch(content)
	if len(matches) < 2 {
		return ""
	}
	return strings.Trim(matches[1], `"`)
}

// kubeadmAPIVersionToKubernetesVersion extracts the kubeadm apiVersion from a
// YAML document and returns the minimum Kubernetes version that uses that API
// version. This is used as a fallback when kubernetesVersion is not available
// (e.g. JoinConfiguration without a ClusterConfiguration). The mapping is
// defined in kubeadmAPIVersionMinKubeVersion and validated against the upstream
// kubeadmtypes.KubeVersionToKubeadmAPIGroupVersion in tests.
func kubeadmAPIVersionToKubernetesVersion(content string) string {
	matches := kubeadmAPIVersionRe.FindStringSubmatch(content)
	if len(matches) < 2 {
		return ""
	}
	v, ok := kubeadmAPIVersionMinKubeVersion[matches[1]]
	if !ok {
		return ""
	}
	return v.String()
}

func replaceYAMLDocument(content, kind, replacement string) (string, error) {
	docs, err := splitYAMLDocuments(content)
	if err != nil {
		return "", err
	}

	replaced := false
	for i := range docs {
		meta := struct {
			Kind string `yaml:"kind"`
		}{}
		if err := yaml.Unmarshal([]byte(docs[i]), &meta); err != nil {
			return "", errors.Wrap(err, "failed to inspect yaml document")
		}
		if meta.Kind == kind {
			docs[i] = strings.TrimSpace(replacement)
			replaced = true
			break
		}
	}

	if !replaced {
		return strings.TrimSpace(replacement), nil
	}
	return strings.Join(docs, "\n---\n"), nil
}

func splitYAMLDocuments(content string) ([]string, error) {
	reader := utilyaml.NewYAMLReader(bufio.NewReader(strings.NewReader(content)))
	docs := []string{}
	for {
		doc, err := reader.Read()
		if err != nil {
			if err == io.EOF {
				break
			}
			return nil, errors.Wrap(err, "failed to read yaml document")
		}
		trimmed := strings.TrimSpace(string(doc))
		if trimmed == "" {
			continue
		}
		docs = append(docs, trimmed)
	}
	return docs, nil
}

func newKubeletServingCert(hostname string, sans []string, caCertPEM, caKeyPEM []byte) ([]byte, []byte, error) {
	caCert, err := ParseCertificatePEM(caCertPEM)
	if err != nil {
		return nil, nil, err
	}
	caKey, err := parsePrivateKeyPEM(caKeyPEM)
	if err != nil {
		return nil, nil, err
	}

	serialNumberLimit := new(big.Int).Lsh(big.NewInt(1), 128)
	serialNumber, err := rand.Int(rand.Reader, serialNumberLimit)
	if err != nil {
		return nil, nil, errors.Wrap(err, "failed to generate kubelet serving cert serial number")
	}

	kubeletKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, nil, errors.Wrap(err, "failed to generate kubelet serving private key")
	}

	template := &x509.Certificate{
		SerialNumber: serialNumber,
		Subject: pkix.Name{
			CommonName: "kubelet",
		},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().AddDate(10, 0, 0),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              []string{hostname},
	}
	for _, san := range sans {
		template.IPAddresses = append(template.IPAddresses, net.ParseIP(san))
	}

	kubeletCertDER, err := x509.CreateCertificate(rand.Reader, template, caCert, &kubeletKey.PublicKey, caKey)
	if err != nil {
		return nil, nil, errors.Wrap(err, "failed to sign kubelet serving cert")
	}

	kubeletCertPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: kubeletCertDER})
	kubeletKeyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(kubeletKey)})
	return kubeletCertPEM, kubeletKeyPEM, nil
}

// ParseCertificatePEM decodes the first PEM block from certPEM and parses it as an X.509 certificate.
func ParseCertificatePEM(certPEM []byte) (*x509.Certificate, error) {
	block, _ := pem.Decode(certPEM)
	if block == nil {
		return nil, errors.New("failed to decode CA certificate PEM")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, errors.Wrap(err, "failed to parse CA certificate")
	}
	return cert, nil
}

func parsePrivateKeyPEM(keyPEM []byte) (crypto.PrivateKey, error) {
	block, _ := pem.Decode(keyPEM)
	if block == nil {
		return nil, errors.New("failed to decode CA private key PEM")
	}

	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	if key, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	if key, err := x509.ParseECPrivateKey(block.Bytes); err == nil {
		return key, nil
	}

	return nil, errors.New("failed to parse CA private key")
}

func effectiveNetworkDevices(vsphereVM infrav1.VSphereVM, ipamState map[string]infrav1.NetworkDeviceSpec, networkStatuses ...infrav1.NetworkStatus) []infrav1.NetworkDeviceSpec {
	devices := make([]infrav1.NetworkDeviceSpec, max(len(vsphereVM.Spec.Network.Devices), len(networkStatuses)))

	for i := range vsphereVM.Spec.Network.Devices {
		vsphereVM.Spec.Network.Devices[i].DeepCopyInto(&devices[i])

		if len(networkStatuses) > i {
			devices[i].MACAddr = networkStatuses[i].MACAddr
		}

		if state, ok := ipamState[devices[i].MACAddr]; ok {
			devices[i].IPAddrs = append(devices[i].IPAddrs, state.IPAddrs...)
			devices[i].Gateway4 = state.Gateway4
			devices[i].Gateway6 = state.Gateway6
		}
	}

	for i, status := range networkStatuses {
		devices[i].MACAddr = status.MACAddr
	}

	return devices
}

func observedNetworkDevices(vsphereVM infrav1.VSphereVM, ipamState map[string]infrav1.NetworkDeviceSpec, networkStatuses ...infrav1.NetworkStatus) []infrav1.NetworkDeviceSpec {
	devices := effectiveNetworkDevices(vsphereVM, ipamState, networkStatuses...)
	for i := range networkStatuses {
		devices[i].IPAddrs = append(devices[i].IPAddrs, networkStatuses[i].IPAddrs...)
	}
	return devices
}

func firstUsableDeviceIP(ipAddrs []string) string {
	for _, ipAddr := range ipAddrs {
		host := ipAddr
		if strings.Contains(ipAddr, "/") {
			ip, _, err := net.ParseCIDR(ipAddr)
			if err != nil {
				continue
			}
			host = ip.String()
		}
		if ip := net.ParseIP(host); ip != nil {
			return host
		}
	}
	return ""
}

func uniqueIPv4SANs(devices []infrav1.NetworkDeviceSpec) []string {
	seen := map[string]struct{}{}
	out := []string{}
	for _, device := range devices {
		for _, ipAddr := range device.IPAddrs {
			host := ipAddr
			if strings.Contains(ipAddr, "/") {
				ip, _, err := net.ParseCIDR(ipAddr)
				if err != nil {
					continue
				}
				host = ip.String()
			}
			ip := net.ParseIP(host)
			if ip == nil || ip.To4() == nil {
				continue
			}
			if _, ok := seen[host]; ok {
				continue
			}
			seen[host] = struct{}{}
			out = append(out, host)
		}
	}
	return out
}

func ValidatePersistentDiskBackfill(persistentDisks []infrav1.PersistentDisk) error {
	var incomplete []string
	for i := range persistentDisks {
		disk := persistentDisks[i]
		var missing []string
		if disk.UnitNumber == nil {
			missing = append(missing, "unitNumber")
		}
		if disk.VolumePath == "" {
			missing = append(missing, "volumePath")
		}
		if disk.DiskUUID == "" {
			missing = append(missing, "diskUUID")
		}
		if len(missing) > 0 {
			incomplete = append(incomplete, fmt.Sprintf("disk %q missing %s", disk.Name, strings.Join(missing, ", ")))
		}
	}
	if len(incomplete) > 0 {
		return errors.Errorf("persistent disk metadata incomplete: %s", strings.Join(incomplete, "; "))
	}
	return nil
}

// ValidateEphemeralDiskBackfill checks that every ephemeral disk has its
// controller-assigned SCSI unit observed. Unlike persistent disks there is no
// VolumePath/DiskUUID to backfill (the backing is always created fresh), so the
// unit number is the only prerequisite for writing the guest disk table.
func ValidateEphemeralDiskBackfill(ephemeralDisks []infrav1.EphemeralDisk) error {
	var incomplete []string
	for i := range ephemeralDisks {
		disk := ephemeralDisks[i]
		if disk.UnitNumber == nil {
			incomplete = append(incomplete, fmt.Sprintf("disk %q missing unitNumber", disk.Name))
		}
	}
	if len(incomplete) > 0 {
		return errors.Errorf("ephemeral disk metadata incomplete: %s", strings.Join(incomplete, "; "))
	}
	return nil
}

// GetPersistentDiskCloudConfig builds the cloud-init config that provisions the
// slot's data disks inside the guest. Persistent and ephemeral disks share the
// same reconcile script and disk table (/etc/capv/persistent-disks.tsv): each
// row carries name, unit, mount path, fs format, mount options, disk UUID, and
// a wipe flag. Ephemeral rows leave the disk-UUID column empty (so the guest
// script's UUID lookup fails and falls back to addressing the disk by SCSI
// unit) and never wipe (a freshly created disk is always empty).
func GetPersistentDiskCloudConfig(persistentDisks []infrav1.PersistentDisk, ephemeralDisks []infrav1.EphemeralDisk) ([]byte, error) {
	if len(persistentDisks) == 0 && len(ephemeralDisks) == 0 {
		return nil, nil
	}
	if err := ValidatePersistentDiskBackfill(persistentDisks); err != nil {
		return nil, err
	}
	if err := ValidateEphemeralDiskBackfill(ephemeralDisks); err != nil {
		return nil, err
	}

	var configFile strings.Builder
	// writeDiskRow emits one /etc/capv/persistent-disks.tsv row. Persistent and
	// ephemeral disks share the row format; they differ only in the disk-UUID and
	// wipe columns, passed in by the caller.
	writeDiskRow := func(name string, unit int32, mountPath, fsFormat string, mountOptions []string, diskUUID string, wipe bool) {
		if fsFormat == "" && mountPath != "" {
			fsFormat = "ext4"
		}
		options := "defaults"
		if len(mountOptions) > 0 {
			options = strings.Join(mountOptions, ",")
		}
		wipeFs := "false"
		if wipe {
			wipeFs = "true"
		}
		fmt.Fprintf(
			&configFile,
			"%s\t%d\t%s\t%s\t%s\t%s\t%s\n",
			name, unit, mountPath, fsFormat, options, diskUUID, wipeFs,
		)
	}

	for i := range persistentDisks {
		disk := persistentDisks[i]
		wipe := disk.WipeFilesystem != nil && *disk.WipeFilesystem
		writeDiskRow(disk.Name, *disk.UnitNumber, disk.MountPath, disk.FSFormat, disk.MountOptions, disk.DiskUUID, wipe)
	}
	// Ephemeral disks use two fixed columns: an empty disk-UUID (so the guest
	// script's UUID lookup fails and it addresses the disk by SCSI unit) and
	// wipe=false (a freshly created disk is always empty).
	for i := range ephemeralDisks {
		disk := ephemeralDisks[i]
		writeDiskRow(disk.Name, *disk.UnitNumber, disk.MountPath, disk.FSFormat, disk.MountOptions, "", false)
	}

	reconcileScript := persistentDiskReconcileScript()
	serviceUnit := persistentDiskServiceUnit()
	containerdDropin := persistentDiskContainerdDropin()
	kubeletDropin := persistentDiskKubeletDropin()

	config := map[interface{}]interface{}{
		"write_files": []interface{}{
			map[interface{}]interface{}{
				"path":        "/etc/capv/persistent-disks.tsv",
				"permissions": "0600",
				"owner":       "root:root",
				"encoding":    "b64",
				"content":     base64.StdEncoding.EncodeToString([]byte(configFile.String())),
			},
			map[interface{}]interface{}{
				"path":        "/usr/local/bin/capv-persistent-disk-reconcile.sh",
				"permissions": "0755",
				"owner":       "root:root",
				"encoding":    "b64",
				"content":     base64.StdEncoding.EncodeToString([]byte(reconcileScript)),
			},
			map[interface{}]interface{}{
				"path":        "/etc/systemd/system/capv-persistent-disk-reconcile.service",
				"permissions": "0644",
				"owner":       "root:root",
				"encoding":    "b64",
				"content":     base64.StdEncoding.EncodeToString([]byte(serviceUnit)),
			},
			map[interface{}]interface{}{
				"path":        "/etc/systemd/system/containerd.service.d/10-wait-disks.conf",
				"permissions": "0644",
				"owner":       "root:root",
				"encoding":    "b64",
				"content":     base64.StdEncoding.EncodeToString([]byte(containerdDropin)),
			},
			map[interface{}]interface{}{
				"path":        "/etc/systemd/system/kubelet.service.d/10-wait-disks.conf",
				"permissions": "0644",
				"owner":       "root:root",
				"encoding":    "b64",
				"content":     base64.StdEncoding.EncodeToString([]byte(kubeletDropin)),
			},
		},
		"runcmd": []interface{}{
			[]interface{}{"systemctl", "daemon-reload"},
			[]interface{}{"systemctl", "enable", "--now", "capv-persistent-disk-reconcile.service"},
			// Restart containerd after persistent disks are mounted.  containerd
			// starts early at boot (before cloud-init writes the drop-in files),
			// so it initializes its data directories on the root filesystem.
			// When the disk service later mounts persistent storage over
			// /var/lib/containerd, containerd's in-memory state becomes stale.
			// A restart forces containerd to re-initialize on the persistent disk.
			[]interface{}{"systemctl", "restart", "containerd"},
		},
	}
	return yaml.Marshal(config)
}

func MergeCloudConfigUserData(userData []byte, extraConfig []byte) ([]byte, error) {
	if len(extraConfig) == 0 {
		return userData, nil
	}

	lines := strings.Split(string(userData), "\n")
	header := make([]string, 0, 2)
	bodyStart := 0
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if len(header) == 0 && trimmed == "" {
			continue
		}
		if strings.HasPrefix(line, "## template:") || trimmed == "#cloud-config" {
			header = append(header, line)
			bodyStart = i + 1
			continue
		}
		break
	}

	body := strings.Join(lines[bodyStart:], "\n")
	mergedBody, err := mergeCloudConfigBodies([]byte(body), extraConfig)
	if err != nil {
		return nil, err
	}

	var out strings.Builder
	if len(header) > 0 {
		out.WriteString(strings.Join(header, "\n"))
		out.WriteString("\n\n")
	}
	out.Write(mergedBody)
	return []byte(out.String()), nil
}

func mergeCloudConfigBodies(baseBody []byte, extraBody []byte) ([]byte, error) {
	baseConfig := map[interface{}]interface{}{}
	if len(strings.TrimSpace(string(baseBody))) > 0 {
		if err := yaml.Unmarshal(baseBody, &baseConfig); err != nil {
			return nil, errors.Wrap(err, "failed to parse bootstrap cloud-config")
		}
	}

	extraConfig := map[interface{}]interface{}{}
	if err := yaml.Unmarshal(extraBody, &extraConfig); err != nil {
		return nil, errors.Wrap(err, "failed to parse persistent disk cloud-config")
	}

	mergeSequenceBefore(baseConfig, extraConfig, "bootcmd")
	mergeMap(baseConfig, extraConfig, "disk_setup")
	mergeSequenceBefore(baseConfig, extraConfig, "fs_setup")
	mergeSequenceBefore(baseConfig, extraConfig, "mounts")
	mergeWriteFiles(baseConfig, extraConfig)
	mergeSequenceBefore(baseConfig, extraConfig, "runcmd")

	return yaml.Marshal(baseConfig)
}

func mergeSequenceBefore(base map[interface{}]interface{}, extra map[interface{}]interface{}, key string) {
	extraVal, ok := extra[key]
	if !ok {
		return
	}
	extraSeq, ok := extraVal.([]interface{})
	if !ok {
		base[key] = extraVal
		return
	}

	if baseVal, ok := base[key]; ok {
		if baseSeq, ok := baseVal.([]interface{}); ok {
			merged := make([]interface{}, 0, len(extraSeq)+len(baseSeq))
			merged = append(merged, extraSeq...)
			for _, item := range baseSeq {
				if containsYAMLEqual(merged, item) {
					continue
				}
				merged = append(merged, item)
			}
			base[key] = merged
			return
		}
	}
	base[key] = extraSeq
}

// mergeWriteFiles merges write_files entries from extra into base, replacing
// base entries that share the same "path" with the extra version.  This ensures
// that updated file content (e.g. refreshed DiskUUID in persistent-disks.tsv)
// overwrites the stale entry instead of being appended alongside it.
func mergeWriteFiles(base, extra map[interface{}]interface{}) {
	const key = "write_files"
	extraVal, ok := extra[key]
	if !ok {
		return
	}
	extraSeq, ok := extraVal.([]interface{})
	if !ok {
		base[key] = extraVal
		return
	}

	baseVal, ok := base[key]
	if !ok {
		base[key] = extraSeq
		return
	}
	baseSeq, ok := baseVal.([]interface{})
	if !ok {
		base[key] = extraSeq
		return
	}

	// Index extra entries by path for O(1) lookup.
	extraPaths := map[string]struct{}{}
	for _, item := range extraSeq {
		if p := writeFilePath(item); p != "" {
			extraPaths[p] = struct{}{}
		}
	}

	// Start with extra entries (they take precedence), then append base entries
	// whose path is not overridden by extra.
	merged := make([]interface{}, 0, len(extraSeq)+len(baseSeq))
	merged = append(merged, extraSeq...)
	for _, item := range baseSeq {
		p := writeFilePath(item)
		if p != "" {
			if _, overridden := extraPaths[p]; overridden {
				continue
			}
		} else if containsYAMLEqual(merged, item) {
			continue
		}
		merged = append(merged, item)
	}
	base[key] = merged
}

func writeFilePath(item interface{}) string {
	m, ok := item.(map[interface{}]interface{})
	if !ok {
		return ""
	}
	p, _ := m["path"].(string)
	return p
}

func mergeMap(base map[interface{}]interface{}, extra map[interface{}]interface{}, key string) {
	extraVal, ok := extra[key]
	if !ok {
		return
	}
	extraMap, ok := extraVal.(map[interface{}]interface{})
	if !ok {
		base[key] = extraVal
		return
	}

	if baseVal, ok := base[key]; ok {
		if baseMap, ok := baseVal.(map[interface{}]interface{}); ok {
			for k, v := range extraMap {
				baseMap[k] = v
			}
			base[key] = baseMap
			return
		}
	}
	base[key] = extraMap
}

func capvDiskPath(name string) string {
	return "/dev/disk/by-capv/" + name
}

// DeterministicDiskPath derives the datastore path of a slot-managed data disk
// that CAPV creates itself, so status carries a Tier-1 VolumePath from the moment
// of clone instead of relying on vCenter's auto-generated vmdk name and a later
// capacity guess. The layout is "[datastore] <vmName>/<name>.vmdk", where the
// per-VM directory (an exact vmName, a DNS-1123 object name that is always a safe
// path component) namespaces disks across VMs, and the file is named by
// DeterministicDiskName(vmName, diskName). A retained persistent disk keeps living
// under this directory across VM recreations because the directory persists as
// long as the retained vmdk does, and the next VM reattaches by the recorded full
// path. Used ONLY when first creating a fresh disk on a known datastore; existing
// disks keep their recorded path. The datastore name is trusted verbatim (it
// originates from vSphere and is already used unguarded elsewhere).
//
// Returns "" only when a required input (datastore, vmName, diskName) is empty.
func DeterministicDiskPath(vmName, datastore, diskName string) string {
	ds := strings.TrimSpace(datastore)
	vm := strings.TrimSpace(vmName)
	if ds == "" || vm == "" || strings.TrimSpace(diskName) == "" {
		return ""
	}
	return DatastorePrefix(ds) + " " + vm + "/" + DeterministicDiskName(vmName, diskName) + ".vmdk"
}

// DatastorePrefix returns the "[datastore]" token that opens a datastore path, or
// "" when the datastore name is empty. It is the single builder shared by
// DeterministicDiskPath and clone.go's datastoreFileHint.
func DatastorePrefix(datastore string) string {
	ds := strings.TrimSpace(datastore)
	if ds == "" {
		return ""
	}
	return "[" + ds + "]"
}

// DeterministicDiskName composes an idempotent, path-safe vmdk file-name base
// (without the ".vmdk" suffix) for a slot-managed data disk from the creating
// VM's hostname and the disk name. The readable form is "<hostname>-<diskName>".
//
// The name must be a single datastore path component within the 255-byte
// VMFS/NFS limit and must never collapse two distinct (hostname, diskName) pairs
// onto one name (which would match the wrong disk). To guarantee both without
// rejecting any input: every byte outside [A-Za-z0-9._-] is replaced with '-',
// and whenever that replacement changes the string or the result would exceed the
// budget (255 minus the ".vmdk" the caller appends), a collision-resistant suffix
// "-<first 5 hex of SHA-256(hostname 0x00 diskName)>" is appended to a truncated
// prefix. The hash is taken over the raw inputs, so distinct pairs never share a
// name even when their sanitized/truncated prefixes coincide. The function is
// pure: identical inputs always yield the identical name.
func DeterministicDiskName(hostname, diskName string) string {
	const maxBase = 255 - len(".vmdk") // leave room for the ".vmdk" the caller appends
	host := strings.TrimSpace(hostname)
	disk := strings.TrimSpace(diskName)
	readable := host + "-" + disk

	sanitized := sanitizeDiskPathComponent(readable)
	if sanitized == readable && len(sanitized) <= maxBase {
		return sanitized
	}

	sum := sha256.Sum256([]byte(host + "\x00" + disk))
	suffix := "-" + hex.EncodeToString(sum[:])[:5]
	if prefixBudget := maxBase - len(suffix); len(sanitized) > prefixBudget {
		sanitized = sanitized[:prefixBudget]
	}
	return sanitized + suffix
}

// sanitizeDiskPathComponent replaces every byte outside [A-Za-z0-9._-] with '-'
// so the result is safe as a single datastore path component. Each non-ASCII rune
// collapses to a single '-'. It never returns "" for a non-empty input.
func sanitizeDiskPathComponent(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '.' || r == '_' || r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	return b.String()
}

func persistentDiskReconcileScript() string {
	return `#!/bin/sh
set -u

CONFIG_FILE="/etc/capv/persistent-disks.tsv"
DEVICE_DIR="/dev/disk/by-capv"
CONTAINERD_MOUNT_PATH="/var/lib/containerd"
POD_LOG_TARGET_PATH="${CONTAINERD_MOUNT_PATH}/logs"
POD_LOG_PATH="/var/log/pods"

mkdir -p "${DEVICE_DIR}"

normalize_value() {
  printf '%s' "${1:-}" | tr '[:upper:]' '[:lower:]' | tr -cd '[:alnum:]'
}

find_device_by_uuid() {
  target_uuid="$(normalize_value "${1:-}")"
  [ -n "${target_uuid}" ] || return 1

  matched_devices=""

  for sysdev in /sys/block/*; do
    base="$(basename "${sysdev}")"
    case "${base}" in
      loop*|ram*|fd*|sr*)
        continue
        ;;
    esac

    device_path="/dev/${base}"
    [ -b "${device_path}" ] || continue

    identity_text=""
    if command -v udevadm >/dev/null 2>&1; then
      identity_text="$(udevadm info --query=property --name "${device_path}" 2>/dev/null || true)"
    fi

    for link in /dev/disk/by-id/* /dev/disk/by-path/*; do
      [ -e "${link}" ] || continue
      resolved="$(readlink -f "${link}" 2>/dev/null || true)"
      if [ "${resolved}" = "${device_path}" ]; then
        identity_text="${identity_text}
$(basename "${link}")"
      fi
    done

    normalized_identity="$(normalize_value "${identity_text}")"
    case "${normalized_identity}" in
      *"${target_uuid}"*)
        matched_devices="${matched_devices}
${device_path}"
        ;;
    esac
  done

  if [ -z "${matched_devices}" ]; then
    return 1
  fi

  # Prefer a device-mapper node when multipath is configured, because it is the
  # canonical block device the guest should format and mount.
  preferred_device=""
  unique_devices=""
  for device_path in ${matched_devices}; do
    case " ${unique_devices} " in
      *" ${device_path} "*) continue ;;
    esac
    unique_devices="${unique_devices} ${device_path}"

    base="$(basename "${device_path}")"
    if [ -d "/sys/block/${base}/dm" ]; then
      if [ -f "/sys/block/${base}/dm/uuid" ] && grep -qi '^mpath-' "/sys/block/${base}/dm/uuid"; then
        printf '%s\n' "${device_path}"
        return 0
      fi
      [ -z "${preferred_device}" ] && preferred_device="${device_path}"
    fi
  done

  if [ -n "${preferred_device}" ]; then
    printf '%s\n' "${preferred_device}"
    return 0
  fi

  set -- ${unique_devices}
  if [ "$#" -eq 1 ]; then
    printf '%s\n' "$1"
    return 0
  fi

  return 1
}

find_device_by_unit() {
  unit_number="${1:-}"
  [ -n "${unit_number}" ] || return 1
  raw_device="$(readlink -f /dev/disk/by-path/*-scsi-0:0:${unit_number}:0 2>/dev/null | head -n 1 || true)"
  [ -n "${raw_device}" ] || return 1

  # When multipath is active the raw SCSI device (e.g. /dev/sdc) is claimed by
  # device-mapper.  Formatting or mounting the raw device fails with "apparently
  # in use".  Walk the holders to find the corresponding dm node.
  raw_base="$(basename "${raw_device}")"
  holder_dir="/sys/block/${raw_base}/holders"
  if [ -d "${holder_dir}" ]; then
    for holder in "${holder_dir}"/*; do
      [ -e "${holder}" ] || continue
      holder_name="$(basename "${holder}")"
      dm_uuid_file="/sys/block/${holder_name}/dm/uuid"
      if [ -f "${dm_uuid_file}" ] && grep -qi '^mpath-' "${dm_uuid_file}"; then
        printf '/dev/%s\n' "${holder_name}"
        return 0
      fi
    done
  fi

  printf '%s\n' "${raw_device}"
}

ensure_pod_log_symlink() {
  [ "${1:-}" = "${CONTAINERD_MOUNT_PATH}" ] || return 0
  if ! mountpoint -q "${CONTAINERD_MOUNT_PATH}"; then
    echo "containerd path ${CONTAINERD_MOUNT_PATH} is not a mount point; skip pod log symlink" >&2
    return 0
  fi

  mkdir -p /var/log "${POD_LOG_TARGET_PATH}"
  if [ -L "${POD_LOG_PATH}" ]; then
    current_target="$(readlink "${POD_LOG_PATH}" 2>/dev/null || true)"
    if [ "${current_target}" = "${POD_LOG_TARGET_PATH}" ]; then
      return 0
    fi
    if [ "${current_target}" = "${CONTAINERD_MOUNT_PATH}" ]; then
      rm -f "${POD_LOG_PATH}"
    else
      echo "pod log path ${POD_LOG_PATH} points to unexpected target ${current_target}; skip symlink" >&2
      return 0
    fi
  elif [ -e "${POD_LOG_PATH}" ]; then
    if [ -d "${POD_LOG_PATH}" ]; then
      for item in "${POD_LOG_PATH}"/* "${POD_LOG_PATH}"/.[!.]* "${POD_LOG_PATH}"/..?*; do
        [ -e "${item}" ] || continue
        target="${POD_LOG_TARGET_PATH}/$(basename "${item}")"
        if [ -e "${target}" ]; then
          echo "pod log migration target ${target} already exists; keep ${item}" >&2
          continue
        fi
        mv "${item}" "${POD_LOG_TARGET_PATH}/"
      done
      if ! rmdir "${POD_LOG_PATH}" 2>/dev/null; then
        echo "pod log path ${POD_LOG_PATH} could not be migrated cleanly; skip symlink" >&2
        return 0
      fi
    else
      rm -f "${POD_LOG_PATH}"
    fi
  fi

  ln -s "${POD_LOG_TARGET_PATH}" "${POD_LOG_PATH}"
}

while true; do
  if [ ! -f "${CONFIG_FILE}" ]; then
    sleep 30
    continue
  fi

  while IFS='	' read -r disk_name unit_number mount_path fs_format mount_options disk_uuid wipe_fs; do
    [ -n "${disk_name}" ] || continue

    link_path="${DEVICE_DIR}/${disk_name}"
    device_path=""
    if [ -n "${disk_uuid}" ]; then
      device_path="$(find_device_by_uuid "${disk_uuid}" || true)"
    fi
    if [ -z "${device_path}" ] && [ -n "${unit_number}" ]; then
      device_path="$(find_device_by_unit "${unit_number}" || true)"
    fi
    if [ -z "${device_path}" ]; then
      continue
    fi

    ln -sf "${device_path}" "${link_path}"

    if [ -n "${mount_path}" ]; then
      if ! blkid "${device_path}" >/dev/null 2>&1; then
        mkfs_cmd="mkfs.${fs_format}"
        if ! command -v "${mkfs_cmd}" >/dev/null 2>&1; then
          echo "missing formatter ${mkfs_cmd} for disk ${disk_name}" >&2
          continue
        fi
        if ! "${mkfs_cmd}" -F "${device_path}"; then
          echo "failed to format ${device_path} for disk ${disk_name}" >&2
          continue
        fi
      fi
      mkdir -p "${mount_path}"
      if ! mountpoint -q "${mount_path}"; then
        if [ -n "${mount_options}" ] && [ "${mount_options}" != "defaults" ]; then
          mount -o "${mount_options}" "${link_path}" "${mount_path}" || true
        else
          mount "${link_path}" "${mount_path}" || true
        fi
      fi
      # If wipeFilesystem is true, wipe disk content on first boot of a
      # new VM.  The marker lives on the system disk (not a persistent disk),
      # so it is absent after VM recreation but survives normal reboots and
      # manual service restarts.
      if [ "${wipe_fs}" = "true" ]; then
        marker_dir="/var/lib/capv"
        marker="${marker_dir}/disk-initialized-${disk_name}"
        if [ ! -f "${marker}" ]; then
          find "${mount_path}" -mindepth 1 -delete 2>/dev/null || true
        fi
        mkdir -p "${marker_dir}"
        touch "${marker}"
      fi
      # ext4 creates lost+found on mkfs; etcd refuses to start if its
      # data-dir is not empty, so remove it after mounting.
      rm -rf "${mount_path}/lost+found"
      ensure_pod_log_symlink "${mount_path}"
    fi
  done < "${CONFIG_FILE}"

  # First pass complete — all disks mounted. Exit successfully so that
  # systemd marks this oneshot unit as "active (exited)", unblocking
  # dependent services (containerd, kubelet) that require the mounts.
  exit 0
done
`
}

func persistentDiskServiceUnit() string {
	return `[Unit]
Description=CAPV Persistent Disk Reconcile
After=local-fs.target
Before=containerd.service kubelet.service
Wants=local-fs.target

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/usr/local/bin/capv-persistent-disk-reconcile.sh

[Install]
WantedBy=multi-user.target
`
}

func persistentDiskContainerdDropin() string {
	return `[Unit]
After=capv-persistent-disk-reconcile.service
Requires=capv-persistent-disk-reconcile.service
`
}

func persistentDiskKubeletDropin() string {
	return `[Unit]
After=capv-persistent-disk-reconcile.service
Requires=capv-persistent-disk-reconcile.service
`
}

func containsYAMLEqual(items []interface{}, candidate interface{}) bool {
	for _, item := range items {
		if yamlEqual(item, candidate) {
			return true
		}
	}
	return false
}

func yamlEqual(a interface{}, b interface{}) bool {
	left, err := yaml.Marshal(a)
	if err != nil {
		return false
	}
	right, err := yaml.Marshal(b)
	if err != nil {
		return false
	}
	return string(left) == string(right)
}

// GetOwnerVSphereMachine returns the VSphereMachine owner for the passed object.
func GetOwnerVSphereMachine(ctx context.Context, c client.Client, obj metav1.ObjectMeta) (*infrav1.VSphereMachine, error) {
	ref, err := GetOwnerVSphereMachineRef(obj)
	if err != nil {
		return nil, err
	}
	if ref == nil {
		return nil, nil
	}
	return getVSphereMachineByName(ctx, c, obj.Namespace, ref.Name)
}

// GetOwnerVSphereMachineRef returns the owner reference for the VSphereMachine owner, if present.
func GetOwnerVSphereMachineRef(obj metav1.ObjectMeta) (*corev1.ObjectReference, error) {
	for _, ref := range obj.OwnerReferences {
		gv, err := schema.ParseGroupVersion(ref.APIVersion)
		if err != nil {
			return nil, err
		}
		if ref.Kind == "VSphereMachine" && gv.Group == infrav1.GroupVersion.Group {
			return &corev1.ObjectReference{
				APIVersion: ref.APIVersion,
				Kind:       ref.Kind,
				Namespace:  obj.Namespace,
				Name:       ref.Name,
				UID:        ref.UID,
			}, nil
		}
	}
	return nil, nil
}

func getVSphereMachineByName(ctx context.Context, c client.Client, namespace, name string) (*infrav1.VSphereMachine, error) {
	m := &infrav1.VSphereMachine{}
	key := client.ObjectKey{Name: name, Namespace: namespace}
	if err := c.Get(ctx, key, m); err != nil {
		return nil, err
	}
	return m, nil
}

const (
	// ProviderIDPrefix is the string data prefixed to a BIOS UUID in order
	// to build a provider ID.
	ProviderIDPrefix = "vsphere://"

	// ProviderIDPattern is a regex pattern and is used by ConvertProviderIDToUUID
	// to convert a providerID into a UUID string.
	ProviderIDPattern = `(?i)^` + ProviderIDPrefix + `([a-f\d]{8}-[a-f\d]{4}-[a-f\d]{4}-[a-f\d]{4}-[a-f\d]{12})$`

	// UUIDPattern is a regex pattern and is used by ConvertUUIDToProviderID
	// to convert a UUID into a providerID string.
	UUIDPattern = `(?i)^[a-f\d]{8}-[a-f\d]{4}-[a-f\d]{4}-[a-f\d]{4}-[a-f\d]{12}$`
)

// ConvertProviderIDToUUID transforms a provider ID into a UUID string.
// If providerID is nil, empty, or invalid, then an empty string is returned.
// A valid providerID should adhere to the format specified by
// ProviderIDPattern.
func ConvertProviderIDToUUID(providerID *string) string {
	if providerID == nil || *providerID == "" {
		return ""
	}
	pattern := regexp.MustCompile(ProviderIDPattern)
	matches := pattern.FindStringSubmatch(*providerID)
	if len(matches) < 2 {
		return ""
	}
	return matches[1]
}

// ConvertUUIDToProviderID transforms a UUID string into a provider ID.
// If the supplied UUID is empty or invalid then an empty string is returned.
// A valid UUID should adhere to the format specified by UUIDPattern.
func ConvertUUIDToProviderID(uuid string) string {
	if uuid == "" {
		return ""
	}
	pattern := regexp.MustCompile(UUIDPattern)
	if !pattern.MatchString(uuid) {
		return ""
	}
	return ProviderIDPrefix + uuid
}

// MachinesAsString constructs a string (with correct punctuations) to be
// used in logging and error messages.
func MachinesAsString(machines []*clusterv1.Machine) string {
	var message string
	count := 1
	for _, m := range machines {
		if count == 1 {
			message = fmt.Sprintf("%s/%s", m.Namespace, m.Name)
		} else {
			var format string
			if count > 1 && count != len(machines) {
				format = "%s, %s/%s"
			} else if count == len(machines) {
				format = "%s and %s/%s"
			}
			message = fmt.Sprintf(format, message, m.Namespace, m.Name)
		}
		count++
	}
	return message
}
