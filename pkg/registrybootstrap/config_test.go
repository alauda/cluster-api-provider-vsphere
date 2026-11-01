// Copyright 2026 Alauda. Licensed under the Apache License, Version 2.0.

package registrybootstrap

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestLoadForClusterMirror(t *testing.T) {
	cluster := &clusterv1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "cluster", Namespace: "cpaas-system", UID: "cluster-uid", Annotations: map[string]string{
		RegistryModeAnnotation:   "mirror",
		RegistryConfigAnnotation: "registry-bootstrap",
	}}}
	ca := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "registry-ca", Namespace: cluster.Namespace, UID: "ca-uid", ResourceVersion: "11"}, Type: corev1.SecretTypeOpaque, Data: map[string][]byte{"ca.crt": []byte("-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n")}}
	// The CA must be parseable; use no CA in this test and cover the reference
	// revision checks with the dockerconfig Secret below.
	ca.Data["ca.crt"] = nil
	creds := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "registry-creds", Namespace: cluster.Namespace, UID: "creds-uid", ResourceVersion: "12"}, Type: corev1.SecretTypeDockerConfigJson, Data: map[string][]byte{corev1.DockerConfigJsonKey: []byte(`{"auths":{"mirror.example:443":{"auth":"` + base64.StdEncoding.EncodeToString([]byte("user:pass")) + `"}}}`)}}
	config := map[string]interface{}{
		"schemaVersion": "v1",
		"mode":          "mirror",
		"source":        RegistrySource,
		"endpoints":     []string{"mirror.example"},
		"clusterUID":    string(cluster.UID),
		"configVersion": "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		"bootstrapCredentialsSecretRef": map[string]string{
			"name": "registry-creds",
		},
	}
	payload, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	bootstrap := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "registry-bootstrap", Namespace: cluster.Namespace}, Type: corev1.SecretTypeOpaque, Data: map[string][]byte{"config.json": payload}}
	scheme := runtime.NewScheme()
	if err := clusterv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cluster, bootstrap, creds, ca).Build()
	got, err := LoadForCluster(context.Background(), c, cluster)
	if err != nil {
		t.Fatal(err)
	}
	if got.Source != RegistrySource || len(got.Endpoints) != 1 || got.Endpoints[0] != "mirror.example" {
		t.Fatalf("unexpected config: %#v", got)
	}
	if got.CA != "" {
		t.Fatalf("credential/config loader must not return CA from invalid empty test Secret")
	}
}

func TestLoadForClusterAcceptsNameOnlyInputSecretReferences(t *testing.T) {
	cluster := &clusterv1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "cluster", Namespace: "cpaas-system", UID: "cluster-uid", Annotations: map[string]string{
		RegistryModeAnnotation:   "mirror",
		RegistryConfigAnnotation: "registry-bootstrap",
	}}}
	creds := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "registry-creds", Namespace: cluster.Namespace}, Type: corev1.SecretTypeDockerConfigJson, Data: map[string][]byte{corev1.DockerConfigJsonKey: []byte(`{"auths":{"mirror.example":{"username":"user","password":"pass"}}}`)}}
	config := map[string]interface{}{
		"mode": "mirror", "source": RegistrySource, "endpoints": []string{"mirror.example"},
		"clusterUID": string(cluster.UID), "configVersion": "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		"bootstrapCredentialsSecretRef": map[string]string{"name": "registry-creds"},
	}
	payload, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	bootstrap := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "registry-bootstrap", Namespace: cluster.Namespace}, Type: corev1.SecretTypeOpaque, Data: map[string][]byte{"config.json": payload}}
	scheme := runtime.NewScheme()
	if err := clusterv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cluster, bootstrap, creds).Build()
	if _, err := LoadForCluster(context.Background(), c, cluster); err != nil {
		t.Fatal(err)
	}
}

func TestLoadForClusterIgnoresUnrelatedCredentialEntries(t *testing.T) {
	cluster := &clusterv1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "cluster", Namespace: "cpaas-system", UID: "cluster-uid", Annotations: map[string]string{
		RegistryModeAnnotation:   "mirror",
		RegistryConfigAnnotation: "registry-bootstrap",
	}}}
	creds := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "registry-creds", Namespace: cluster.Namespace}, Type: corev1.SecretTypeDockerConfigJson, Data: map[string][]byte{corev1.DockerConfigJsonKey: []byte(`{"auths":{"mirror.example":{"username":"user","password":"pass"},"https://index.docker.io/v1/":{}}}`)}}
	config := map[string]interface{}{
		"mode": "mirror", "source": RegistrySource, "endpoints": []string{"mirror.example"},
		"clusterUID": string(cluster.UID), "configVersion": "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		"bootstrapCredentialsSecretRef": map[string]string{"name": "registry-creds"},
	}
	payload, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	bootstrap := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "registry-bootstrap", Namespace: cluster.Namespace}, Type: corev1.SecretTypeOpaque, Data: map[string][]byte{"config.json": payload}}
	scheme := runtime.NewScheme()
	if err := clusterv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cluster, bootstrap, creds).Build()
	if _, err := LoadForCluster(context.Background(), c, cluster); err != nil {
		t.Fatal(err)
	}
}

func TestLoadForClusterDirectIsNoop(t *testing.T) {
	cluster := &clusterv1.Cluster{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Annotations: map[string]string{RegistryModeAnnotation: "direct"}}}
	got, err := LoadForCluster(context.Background(), fake.NewClientBuilder().Build(), cluster)
	if err != nil {
		t.Fatal(err)
	}
	if got.Source != "" {
		t.Fatalf("direct cluster returned mirror config: %#v", got)
	}
}

func TestEndpointKeyCanonicalizesDefaultPortAndIPv6(t *testing.T) {
	left, err := EndpointKey("EXAMPLE.com")
	if err != nil {
		t.Fatal(err)
	}
	right, err := EndpointKey("example.com:443")
	if err != nil {
		t.Fatal(err)
	}
	if left != right {
		t.Fatalf("default HTTPS port did not canonicalize: %q != %q", left, right)
	}
	ip1, err := EndpointKey("[2001:0db8::1]")
	if err != nil {
		t.Fatal(err)
	}
	ip2, err := EndpointKey("[2001:db8:0:0:0:0:0:1]:443")
	if err != nil {
		t.Fatal(err)
	}
	if ip1 != ip2 {
		t.Fatalf("IPv6 aliases did not canonicalize: %q != %q", ip1, ip2)
	}
}
