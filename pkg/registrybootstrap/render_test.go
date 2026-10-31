// Copyright 2026 Alauda. Licensed under the Apache License, Version 2.0.

package registrybootstrap

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v2"
)

func TestRenderCloudConfig(t *testing.T) {
	data, err := RenderCloudConfig(Config{
		Source:                     RegistrySource,
		Endpoints:                  []string{"first.example:8443", "second.example"},
		CA:                         "ca-data",
		ConfigVersion:              "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		EndpointInsecureSkipVerify: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	var config map[interface{}]interface{}
	if err := yaml.Unmarshal(data, &config); err != nil {
		t.Fatal(err)
	}
	if len(config["write_files"].([]interface{})) < 7 {
		t.Fatalf("expected registry files, got %#v", config["write_files"])
	}
	run := config["runcmd"].([]interface{})
	if len(run) != 1 || !strings.Contains(run[0].(string), "acp-registry-bootstrap.service") {
		t.Fatalf("registry service is not first bootstrap command: %#v", run)
	}
	files := data
	for _, expected := range []string{
		"server = \"https://second.example\"",
		"[host.\"https://first.example:8443\"]",
		"skip_verify = true",
		"/etc/containerd/certs.d/cpaas-registry.cpaas-system/ca.crt",
		"containerd --version",
		"/root/images/pause.tar",
	} {
		if !strings.Contains(string(files), expected) {
			t.Fatalf("rendered config missing %q:\n%s", expected, files)
		}
	}
}
