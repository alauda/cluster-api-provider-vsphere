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
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"regexp"
	"strings"
	"text/template"
	"time"

	"github.com/pkg/errors"
	"gopkg.in/yaml.v2"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	apitypes "k8s.io/apimachinery/pkg/types"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
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

func NewKubeletServingCertData(hostname string, vsphereVM infrav1.VSphereVM, ipamState map[string]infrav1.NetworkDeviceSpec, caCertPEM, caKeyPEM []byte, networkStatuses ...infrav1.NetworkStatus) ([]byte, []byte, error) {
	devices := effectiveNetworkDevices(vsphereVM, ipamState, networkStatuses...)
	sans := uniqueIPv4SANs(devices)
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
			map[interface{}]interface{}{
				"path":        "/etc/kubernetes/patches/kubeletconfiguration1+strategic.json",
				"permissions": "0644",
				"owner":       "root:root",
				"content": "{\n" +
					"  \"apiVersion\": \"kubelet.config.k8s.io/v1beta1\",\n" +
					"  \"kind\": \"KubeletConfiguration\",\n" +
					"  \"tlsCertFile\": \"/etc/kubernetes/pki/kubelet.crt\",\n" +
					"  \"tlsPrivateKeyFile\": \"/etc/kubernetes/pki/kubelet.key\"\n" +
					"}\n",
			},
		},
	}
	return yaml.Marshal(config)
}

func newKubeletServingCert(hostname string, sans []string, caCertPEM, caKeyPEM []byte) ([]byte, []byte, error) {
	caCert, err := parseCertificatePEM(caCertPEM)
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

func parseCertificatePEM(certPEM []byte) (*x509.Certificate, error) {
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

func GetPersistentDiskCloudConfig(persistentDisks []infrav1.PersistentDisk) ([]byte, error) {
	normalizedPersistentDisks := make([]infrav1.PersistentDisk, 0, len(persistentDisks))
	for i := range persistentDisks {
		disk := persistentDisks[i]
		if disk.UnitNumber == nil {
			continue
		}
		if disk.FSFormat == "" {
			disk.FSFormat = "ext4"
		}
		normalizedPersistentDisks = append(normalizedPersistentDisks, disk)
	}
	if len(normalizedPersistentDisks) == 0 {
		return nil, nil
	}

	var configFile strings.Builder
	for _, disk := range normalizedPersistentDisks {
		options := "defaults"
		if len(disk.MountOptions) > 0 {
			options = strings.Join(disk.MountOptions, ",")
		}
		fmt.Fprintf(
			&configFile,
			"%s\t%d\t%s\t%s\t%s\t%s\n",
			disk.Name,
			*disk.UnitNumber,
			disk.MountPath,
			disk.FSFormat,
			options,
			disk.DiskUUID,
		)
	}

	reconcileScript := persistentDiskReconcileScript()
	serviceUnit := persistentDiskServiceUnit()

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
		},
		"runcmd": []interface{}{
			[]interface{}{"systemctl", "daemon-reload"},
			[]interface{}{"systemctl", "enable", "--now", "capv-persistent-disk-reconcile.service"},
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

func persistentDiskReconcileScript() string {
	return `#!/bin/sh
set -u

CONFIG_FILE="/etc/capv/persistent-disks.tsv"
DEVICE_DIR="/dev/disk/by-capv"

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

while true; do
  if [ ! -f "${CONFIG_FILE}" ]; then
    sleep 30
    continue
  fi

  while IFS='	' read -r disk_name unit_number mount_path fs_format mount_options disk_uuid; do
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

    if [ -n "${mount_path}" ]; then
      mkdir -p "${mount_path}"
      if ! mountpoint -q "${mount_path}"; then
        if [ -n "${mount_options}" ] && [ "${mount_options}" != "defaults" ]; then
          mount -o "${mount_options}" "${link_path}" "${mount_path}" || true
        else
          mount "${link_path}" "${mount_path}" || true
        fi
      fi
    fi
  done < "${CONFIG_FILE}"

  sleep 30
done
`
}

func persistentDiskServiceUnit() string {
	return `[Unit]
Description=CAPV Persistent Disk Reconcile
After=local-fs.target
Wants=local-fs.target

[Service]
Type=simple
ExecStart=/usr/local/bin/capv-persistent-disk-reconcile.sh
Restart=always
RestartSec=15

[Install]
WantedBy=multi-user.target
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
