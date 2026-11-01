// Copyright 2026 Alauda. Licensed under the Apache License, Version 2.0.

package registrybootstrap

import (
	"bytes"
	_ "embed"
	"fmt"
	"regexp"
	"strings"
	"text/template"

	"gopkg.in/yaml.v2"
)

const bootstrapDirectory = "/etc/acp/registry-bootstrap"

//go:embed bootstrap.sh.tmpl
var bootstrapScript string

//go:embed restore-pause.sh
var restoreScript string

// RenderCloudConfig returns a cloud-config fragment. Its runcmd entries are
// deliberately prepended by the provider's existing cloud-config merge helper,
// so containerd and the sandbox are ready before kubeadm commands run.
func RenderCloudConfig(config Config) ([]byte, error) {
	if config.Source == "" {
		return nil, nil
	}
	if config.Source != RegistrySource || len(config.Endpoints) == 0 || !regexp.MustCompile(`^sha256:[a-f0-9]{64}$`).MatchString(config.ConfigVersion) {
		return nil, fmt.Errorf("invalid mirror render input")
	}
	var hosts strings.Builder
	fmt.Fprintf(&hosts, "server = %q\ncapabilities = [\"pull\", \"resolve\"]\nskip_verify = %t\n", "https://"+config.Endpoints[len(config.Endpoints)-1], config.EndpointInsecureSkipVerify)
	if config.CA != "" {
		fmt.Fprintf(&hosts, "ca = %q\n", bootstrapDirectory+"/ca.crt")
	}
	for _, endpoint := range config.Endpoints {
		if _, err := EndpointKey(endpoint); err != nil {
			return nil, err
		}
	}
	for _, endpoint := range config.Endpoints[:len(config.Endpoints)-1] {
		fmt.Fprintf(&hosts, "[host.%q]\n  capabilities = [\"pull\", \"resolve\"]\n  skip_verify = %t\n", "https://"+endpoint, config.EndpointInsecureSkipVerify)
		if config.CA != "" {
			fmt.Fprintf(&hosts, "  ca = %q\n", bootstrapDirectory+"/ca.crt")
		}
	}
	var script bytes.Buffer
	if err := template.Must(template.New("bootstrap").Parse(bootstrapScript)).Execute(&script, struct {
		Config
		Insecure bool
	}{Config: config, Insecure: config.EndpointInsecureSkipVerify}); err != nil {
		return nil, err
	}

	writeFiles := []map[string]string{
		{"path": bootstrapDirectory + "/hosts.toml", "permissions": "0644", "owner": "root:root", "content": hosts.String()},
		{"path": bootstrapDirectory + "/bootstrap.sh", "permissions": "0700", "owner": "root:root", "content": script.String()},
		{"path": bootstrapDirectory + "/restore-pause.sh", "permissions": "0700", "owner": "root:root", "content": restoreScript},
		{"path": "/etc/systemd/system/acp-registry-bootstrap.service", "permissions": "0644", "owner": "root:root", "content": bootstrapUnit},
		{"path": "/etc/systemd/system/acp-registry-pause-restore.service", "permissions": "0644", "owner": "root:root", "content": restoreUnit},
		{"path": "/etc/systemd/system/kubelet.service.d/15-registry-bootstrap.conf", "permissions": "0644", "owner": "root:root", "content": bootDependency},
		{"path": "/etc/systemd/system/kubeadm.service.d/15-registry-bootstrap.conf", "permissions": "0644", "owner": "root:root", "content": bootDependency},
	}
	if config.CA != "" {
		writeFiles = append(writeFiles, map[string]string{"path": bootstrapDirectory + "/ca.crt", "permissions": "0644", "owner": "root:root", "content": config.CA})
	}
	fragment := map[string]interface{}{
		"write_files": writeFiles,
		"bootcmd":     []string{"if [ ! -e /etc/containerd/.acp-registry-mco-managed ] && [ ! -e /etc/containerd/.acp-registry-bootstrap-version ]; then systemctl mask --runtime --now kubelet.service || true; systemctl stop containerd.service || true; fi"},
		"runcmd":      []string{"systemctl daemon-reload && systemctl enable acp-registry-bootstrap.service acp-registry-pause-restore.service && systemctl start acp-registry-bootstrap.service acp-registry-pause-restore.service && systemctl unmask --runtime kubelet.service"},
	}
	return yaml.Marshal(fragment)
}

const bootDependency = `[Unit]
Requires=acp-registry-bootstrap.service acp-registry-pause-restore.service
After=acp-registry-bootstrap.service acp-registry-pause-restore.service
`

const bootstrapUnit = `[Unit]
Description=Prepare vSphere registry mirror before Kubernetes bootstrap
Wants=network-online.target containerd.service
After=network-online.target containerd.service cloud-init.service
Before=kubelet.service kubeadm.service
RequiresMountsFor=/var/lib/containerd
[Service]
Type=oneshot
RemainAfterExit=yes
TimeoutStartSec=15min
ExecStart=/bin/bash /etc/acp/registry-bootstrap/bootstrap.sh
[Install]
WantedBy=multi-user.target
`

const restoreUnit = `[Unit]
Description=Restore OS sandbox image locally
Wants=containerd.service
Requires=acp-registry-bootstrap.service
After=containerd.service acp-registry-bootstrap.service
Before=kubelet.service kubeadm.service
RequiresMountsFor=/var/lib/containerd
[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/bin/bash /etc/acp/registry-bootstrap/restore-pause.sh
[Install]
WantedBy=multi-user.target
`
