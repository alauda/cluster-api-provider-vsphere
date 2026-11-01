// Copyright 2026 Alauda. Licensed under the Apache License, Version 2.0.

// Package registrybootstrap contains the provider-side contract for the
// versioned registry bootstrap Secret published by cluster-transformer.
package registrybootstrap

import (
	"context"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	RegistryModeAnnotation   = "cpaas.io/registry-mode"
	RegistryConfigAnnotation = "cpaas.io/registry-config-secret"
	RegistrySource           = "cpaas-registry.cpaas-system"
)

var configVersionPattern = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)

// Config is the non-secret snapshot needed to bootstrap one immutable node.
// Registry credentials are validated while loading but are intentionally not
// copied into this value or rendered into cloud-init.
type Config struct {
	Source                     string
	Endpoints                  []string
	CA                         string
	ConfigVersion              string
	EndpointInsecureSkipVerify bool
}

type authEntry struct {
	Auth          string `json:"auth"`
	Username      string `json:"username"`
	Password      string `json:"password"`
	IdentityToken string `json:"identitytoken"`
	RegistryToken string `json:"registrytoken"`
}

type secretReference struct {
	Name            string `json:"name"`
	UID             string `json:"uid,omitempty"`
	ResourceVersion string `json:"resourceVersion,omitempty"`
}

type payload struct {
	SchemaVersion string   `json:"schemaVersion,omitempty"`
	Mode          string   `json:"mode"`
	Source        string   `json:"source"`
	Endpoints     []string `json:"endpoints"`
	ClusterUID    string   `json:"clusterUID"`
	ConfigVersion string   `json:"configVersion"`
	TLSPolicy     struct {
		EndpointInsecureSkipVerify bool `json:"endpointInsecureSkipVerify,omitempty"`
	} `json:"tlsPolicy,omitempty"`
	CASecretRef                   *secretReference `json:"caSecretRef,omitempty"`
	SandboxCredentialsSecretRef   *secretReference `json:"sandboxCredentialsSecretRef,omitempty"`
	BootstrapCredentialsSecretRef *secretReference `json:"bootstrapCredentialsSecretRef,omitempty"`
}

// LoadForCluster reads the version-bound bootstrap Secret named by the CAPI
// Cluster annotation. It does not read a Registry CR and does not require the
// workload API or DNS to be available.
func LoadForCluster(ctx context.Context, c client.Client, cluster *clusterv1.Cluster) (Config, error) {
	empty := Config{}
	if cluster == nil {
		return empty, nil
	}
	mode := ""
	if cluster.Annotations != nil {
		mode = strings.TrimSpace(cluster.Annotations[RegistryModeAnnotation])
	}
	if mode == "direct" || mode == "" {
		return empty, nil
	}
	if mode != "mirror" {
		return empty, fmt.Errorf("unsupported registry mode %q", mode)
	}
	name := strings.TrimSpace(cluster.Annotations[RegistryConfigAnnotation])
	if name == "" {
		return empty, fmt.Errorf("waiting for complete mirror registry bootstrap input")
	}
	if c == nil {
		return empty, fmt.Errorf("registry bootstrap client is nil")
	}
	secret := &corev1.Secret{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: cluster.Namespace, Name: name}, secret); err != nil {
		return empty, fmt.Errorf("read registry bootstrap Secret: %w", err)
	}
	if secret.Type != corev1.SecretTypeOpaque {
		return empty, fmt.Errorf("registry bootstrap Secret must be opaque")
	}
	var in payload
	if err := json.Unmarshal(secret.Data["config.json"], &in); err != nil {
		return empty, fmt.Errorf("decode registry bootstrap Secret: %w", err)
	}
	if in.SchemaVersion != "" && in.SchemaVersion != "v1" {
		return empty, fmt.Errorf("unsupported registry bootstrap schema version %q", in.SchemaVersion)
	}
	if in.Mode != "mirror" {
		return empty, fmt.Errorf("unsupported registry bootstrap mode %q", in.Mode)
	}
	if in.Source != RegistrySource {
		return empty, fmt.Errorf("invalid mirror registry source")
	}
	if cluster.UID == "" || in.ClusterUID != string(cluster.UID) {
		return empty, fmt.Errorf("registry bootstrap cluster UID mismatch")
	}
	if !configVersionPattern.MatchString(in.ConfigVersion) {
		return empty, fmt.Errorf("invalid registry bootstrap config version")
	}
	if len(in.Endpoints) == 0 || len(in.Endpoints) > 16 {
		return empty, fmt.Errorf("mirror requires between 1 and 16 endpoints")
	}
	seen := map[string]struct{}{}
	sourceKey, err := EndpointKey(in.Source)
	if err != nil {
		return empty, fmt.Errorf("invalid registry source: %w", err)
	}
	for _, endpoint := range in.Endpoints {
		key, err := EndpointKey(endpoint)
		if err != nil || key == sourceKey {
			return empty, fmt.Errorf("invalid or duplicate registry bootstrap endpoint")
		}
		if _, ok := seen[key]; ok {
			return empty, fmt.Errorf("invalid or duplicate registry bootstrap endpoint")
		}
		seen[key] = struct{}{}
	}
	result := Config{
		Source:                     in.Source,
		Endpoints:                  append([]string(nil), in.Endpoints...),
		ConfigVersion:              in.ConfigVersion,
		EndpointInsecureSkipVerify: in.TLSPolicy.EndpointInsecureSkipVerify,
	}
	if in.CASecretRef != nil {
		caSecret, err := inputSecret(ctx, c, cluster.Namespace, in.CASecretRef)
		if err != nil {
			return empty, err
		}
		ca := caSecret.Data["ca.crt"]
		if len(ca) == 0 || !x509.NewCertPool().AppendCertsFromPEM(ca) {
			return empty, fmt.Errorf("invalid registry CA certificate bundle")
		}
		result.CA = string(ca)
	}
	if in.BootstrapCredentialsSecretRef != nil {
		credentials, err := inputSecret(ctx, c, cluster.Namespace, in.BootstrapCredentialsSecretRef)
		if err != nil {
			return empty, err
		}
		if err := validateBootstrapCredentials(credentials, in.Endpoints); err != nil {
			return empty, err
		}
	}
	if in.SandboxCredentialsSecretRef != nil {
		return empty, fmt.Errorf("immutable mirror bootstrap must not receive SSH sandbox credentials")
	}
	return result, nil
}

func inputSecret(ctx context.Context, c client.Client, namespace string, ref *secretReference) (*corev1.Secret, error) {
	if ref == nil || ref.Name == "" {
		return nil, fmt.Errorf("registry input Secret reference must include a name")
	}
	secret := &corev1.Secret{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: ref.Name}, secret); err != nil {
		return nil, fmt.Errorf("read registry input Secret: %w", err)
	}
	if ref.UID != "" && string(secret.UID) != ref.UID {
		return nil, fmt.Errorf("registry input Secret revision mismatch")
	}
	if ref.ResourceVersion != "" && secret.ResourceVersion != ref.ResourceVersion {
		return nil, fmt.Errorf("registry input Secret revision mismatch")
	}
	return secret, nil
}

// EndpointKey returns a stable authority key used to compare endpoint and
// dockerconfigjson aliases. Default HTTPS is represented as port 443.
func EndpointKey(endpoint string) (string, error) {
	if !validEndpoint(endpoint) {
		return "", fmt.Errorf("invalid registry endpoint")
	}
	u, err := url.Parse("https://" + endpoint)
	if err != nil || u.Host != endpoint || u.Hostname() == "" {
		return "", fmt.Errorf("invalid registry endpoint")
	}
	host := strings.ToLower(u.Hostname())
	if ip := net.ParseIP(host); ip != nil {
		host = ip.String()
	}
	port := 443
	if value := u.Port(); value != "" {
		port, err = strconv.Atoi(value)
		if err != nil || port < 1 || port > 65535 {
			return "", fmt.Errorf("invalid registry endpoint port")
		}
	}
	return net.JoinHostPort(host, strconv.Itoa(port)), nil
}

func validEndpoint(endpoint string) bool {
	if endpoint == "" || strings.ContainsAny(endpoint, "/\\?#@\"'\n\r\t ") {
		return false
	}
	u, err := url.Parse("https://" + endpoint)
	if err != nil || u.Host != endpoint || u.Hostname() == "" {
		return false
	}
	host := u.Hostname()
	if net.ParseIP(host) == nil {
		if len(host) > 253 || strings.Contains(host, ":") {
			return false
		}
		for _, label := range strings.Split(host, ".") {
			if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
				return false
			}
			for _, ch := range label {
				if !(ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || ch == '-') {
					return false
				}
			}
		}
	} else if strings.Contains(host, ":") && !strings.HasPrefix(endpoint, "[") {
		return false
	}
	if port := u.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return false
		}
	} else if strings.HasSuffix(endpoint, ":") {
		return false
	}
	return true
}

func validateBootstrapCredentials(secret *corev1.Secret, endpoints []string) error {
	if secret.Type != corev1.SecretTypeDockerConfigJson {
		return fmt.Errorf("invalid bootstrap credential Secret type")
	}
	var config struct {
		Auths map[string]authEntry `json:"auths"`
	}
	if json.Unmarshal(secret.Data[corev1.DockerConfigJsonKey], &config) != nil || config.Auths == nil {
		return fmt.Errorf("invalid bootstrap dockerconfigjson")
	}
	if len(config.Auths) == 0 {
		return nil
	}
	for _, endpoint := range endpoints {
		key, err := EndpointKey(endpoint)
		if err != nil {
			return err
		}
		var matched *authEntry
		for alias, candidate := range config.Auths {
			aliasKey, aliasErr := EndpointKey(alias)
			if aliasErr != nil || aliasKey != key {
				continue
			}
			candidateCopy := candidate
			if candidateCopy.Auth != "" {
				raw, decodeErr := base64.StdEncoding.DecodeString(candidateCopy.Auth)
				parts := strings.SplitN(string(raw), ":", 2)
				if decodeErr != nil || len(parts) != 2 {
					return fmt.Errorf("invalid bootstrap credential encoding")
				}
				if (candidateCopy.Username != "" && candidateCopy.Username != parts[0]) || (candidateCopy.Password != "" && candidateCopy.Password != parts[1]) {
					return fmt.Errorf("conflicting bootstrap credential encodings")
				}
				candidateCopy.Username, candidateCopy.Password, candidateCopy.Auth = parts[0], parts[1], ""
			}
			if candidateCopy.Username == "" && candidateCopy.Password == "" && candidateCopy.IdentityToken == "" && candidateCopy.RegistryToken == "" {
				return fmt.Errorf("empty bootstrap credential entry")
			}
			if matched != nil && *matched != candidateCopy {
				return fmt.Errorf("conflicting bootstrap credential aliases")
			}
			matched = &candidateCopy
		}
		if matched == nil {
			return fmt.Errorf("bootstrap credentials missing configured endpoint")
		}
	}
	return nil
}
