/*
Copyright The Kubernetes Authors.

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

// Package ovn contains helpers for interacting with kube-ovn deployments in
// workload clusters. Currently this is limited to evicting a single OVN raft
// member when its node is being removed.
package ovn

import (
	"bytes"
	"context"
	"fmt"
	"net/netip"
	"regexp"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	k8sscheme "k8s.io/client-go/kubernetes/scheme"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
)

const (
	// CentralNamespace is the namespace where ovn-central pods live.
	CentralNamespace = "kube-system"
	// CentralSelector is the label selector for ovn-central pods.
	CentralSelector = "app=ovn-central"

	kubeOvnAppReleaseNamespace = "cpaas-system"
	kubeOvnAppReleaseName      = "cni-kube-ovn"
)

var (
	bracketedIPRe = regexp.MustCompile(`\[([^\]]+)\]`)
	ipv4Re        = regexp.MustCompile(`\b(?:\d{1,3}\.){3}\d{1,3}\b`)
)

type raftDB struct {
	ctlPath string
	dbName  string
}

var raftDBs = []raftDB{
	{"/var/run/ovn/ovnnb_db.ctl", "OVN_Northbound"},
	{"/var/run/ovn/ovnsb_db.ctl", "OVN_Southbound"},
}

var appReleaseGVR = schema.GroupVersionResource{
	Group:    "operator.alauda.io",
	Version:  "v1alpha1",
	Resource: "appreleases",
}

// execInPod runs cmd inside container of pod/ns and returns stdout (or stderr
// when stdout is empty).
func execInPod(ctx context.Context, conf *rest.Config, ns, pod, container string, cmd ...string) (string, error) {
	rc, err := typedcorev1.NewForConfig(conf)
	if err != nil {
		return "", err
	}
	req := rc.RESTClient().Post().Resource("pods").Namespace(ns).Name(pod).SubResource("exec").Param("container", container)
	req.VersionedParams(&corev1.PodExecOptions{
		Container: container,
		Command:   cmd,
		Stdin:     false,
		Stdout:    true,
		Stderr:    true,
		TTY:       false,
	}, k8sscheme.ParameterCodec)

	exec, err := remotecommand.NewSPDYExecutor(conf, "POST", req.URL())
	if err != nil {
		return "", err
	}

	var stdout, stderr bytes.Buffer
	if err := exec.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdout: &stdout,
		Stderr: &stderr,
	}); err != nil {
		return "", err
	}
	if stdout.Len() > 0 {
		return stdout.String(), nil
	}
	return stderr.String(), nil
}

// selectPod returns a running, ready pod matching selector in ns whose HostIP
// is not in excludeHostIPs. It returns an error when no such pod exists.
func selectPod(ctx context.Context, clientset kubernetes.Interface, ns, selector string, excludeHostIPs []string) (podName, containerName string, err error) {
	pods, err := clientset.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return "", "", err
	}

	excluded := make(map[string]struct{}, len(excludeHostIPs))
	for _, ip := range excludeHostIPs {
		excluded[ip] = struct{}{}
	}

	for _, pod := range pods.Items {
		if _, skip := excluded[pod.Status.HostIP]; skip {
			continue
		}
		if pod.Status.Phase != corev1.PodRunning {
			continue
		}
		for _, c := range pod.Status.ContainerStatuses {
			if c.Ready {
				return pod.Name, c.Name, nil
			}
		}
	}
	return "", "", fmt.Errorf("cannot find running pod with selector %q in namespace %q", selector, ns)
}

func isKubeOvnAppReleaseDeployed(ctx context.Context, config *rest.Config) (bool, error) {
	discoveryClient, err := discovery.NewDiscoveryClientForConfig(config)
	if err != nil {
		return false, err
	}
	if _, err = discoveryClient.ServerResourcesForGroupVersion(appReleaseGVR.GroupVersion().String()); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}

	dynamicClient, err := dynamic.NewForConfig(config)
	if err != nil {
		return false, err
	}
	_, err = dynamicClient.Resource(appReleaseGVR).Namespace(kubeOvnAppReleaseNamespace).Get(ctx, kubeOvnAppReleaseName, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func normalizeCandidateIPs(candidateIPs []string) []string {
	seen := make(map[string]struct{}, len(candidateIPs))
	ips := make([]string, 0, len(candidateIPs))
	for _, candidate := range candidateIPs {
		addr, err := netip.ParseAddr(strings.TrimSpace(candidate))
		if err != nil {
			continue
		}
		ip := addr.String()
		if _, ok := seen[ip]; ok {
			continue
		}
		seen[ip] = struct{}{}
		ips = append(ips, ip)
	}
	return ips
}

func ipsInRaftStatus(status string) map[string]struct{} {
	ips := map[string]struct{}{}
	for _, match := range bracketedIPRe.FindAllStringSubmatch(status, -1) {
		addr, err := netip.ParseAddr(match[1])
		if err == nil {
			ips[addr.String()] = struct{}{}
		}
	}
	for _, match := range ipv4Re.FindAllString(status, -1) {
		addr, err := netip.ParseAddr(match)
		if err == nil {
			ips[addr.String()] = struct{}{}
		}
	}
	return ips
}

func lineHasIP(line, ip string) bool {
	_, ok := ipsInRaftStatus(line)[ip]
	return ok
}

func ipsInRaftServers(status string) map[string]struct{} {
	ips := map[string]struct{}{}
	inServers := false
	for line := range strings.SplitSeq(status, "\n") {
		line = strings.TrimSpace(line)
		if line == "Servers:" {
			inServers = true
			continue
		}
		if !inServers || line == "" {
			continue
		}
		for ip := range ipsInRaftStatus(line) {
			ips[ip] = struct{}{}
		}
	}
	return ips
}

func findMatchingRaftMemberIP(candidateIPs []string, statuses ...string) (string, error) {
	candidates := normalizeCandidateIPs(candidateIPs)
	if len(candidates) == 0 {
		return "", nil
	}

	raftIPs := map[string]struct{}{}
	for _, status := range statuses {
		for ip := range ipsInRaftServers(status) {
			raftIPs[ip] = struct{}{}
		}
	}

	matches := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		if _, ok := raftIPs[candidate]; ok {
			matches = append(matches, candidate)
		}
	}

	switch len(matches) {
	case 0:
		return "", nil
	case 1:
		return matches[0], nil
	default:
		return "", fmt.Errorf("multiple Machine IPs match OVN raft members: %s", strings.Join(matches, ", "))
	}
}

func serverIDForMemberIP(status, memberIP string) (string, error) {
	memberIPs := normalizeCandidateIPs([]string{memberIP})
	if len(memberIPs) == 0 {
		return "", nil
	}
	memberIP = memberIPs[0]
	var serverIDs []string
	inServers := false
	for line := range strings.SplitSeq(status, "\n") {
		line = strings.TrimSpace(line)
		if line == "Servers:" {
			inServers = true
			continue
		}
		if !inServers || line == "" || !lineHasIP(line, memberIP) {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		serverIDs = append(serverIDs, fields[0])
	}

	switch len(serverIDs) {
	case 0:
		return "", nil
	case 1:
		return serverIDs[0], nil
	default:
		return "", fmt.Errorf("multiple OVN raft server IDs match %s: %s", memberIP, strings.Join(serverIDs, ", "))
	}
}

func clusterStatus(ctx context.Context, config *rest.Config, ns, podName, containerName string, db raftDB) (string, error) {
	return execInPod(ctx, config, ns, podName, containerName, "ovs-appctl", "-t", db.ctlPath, "cluster/status", db.dbName)
}

func kickRaftMemberFromPod(ctx context.Context, config *rest.Config, ns, podName, containerName, memberIP string) ([]string, error) {
	kickedDBs := make([]string, 0, len(raftDBs))
	for _, db := range raftDBs {
		status, err := clusterStatus(ctx, config, ns, podName, containerName, db)
		if err != nil {
			return kickedDBs, fmt.Errorf("failed to query %s server id for %s: %w", db.dbName, memberIP, err)
		}
		serverID, err := serverIDForMemberIP(status, memberIP)
		if err != nil {
			return kickedDBs, fmt.Errorf("failed to resolve %s server id for %s: %w", db.dbName, memberIP, err)
		}
		if serverID == "" {
			continue
		}
		if _, err := execInPod(ctx, config, ns, podName, containerName, "ovs-appctl", "-t", db.ctlPath, "cluster/kick", db.dbName, serverID); err != nil {
			return kickedDBs, fmt.Errorf("failed to kick %s server %s for %s: %w", db.dbName, serverID, memberIP, err)
		}
		kickedDBs = append(kickedDBs, db.dbName)
	}
	return kickedDBs, nil
}

// KickRaftMemberFromCandidates resolves the single Machine-reported IP that is
// present in OVN raft status and evicts that member. It returns an empty matched
// IP when none of the candidates are OVN raft members.
func KickRaftMemberFromCandidates(ctx context.Context, clientset kubernetes.Interface, config *rest.Config, ns string, candidateIPs []string) (string, []string, error) {
	deployed, err := isKubeOvnAppReleaseDeployed(ctx, config)
	if err != nil || !deployed {
		return "", nil, err
	}

	candidates := normalizeCandidateIPs(candidateIPs)
	if len(candidates) == 0 {
		return "", nil, nil
	}

	statusPodName, statusContainerName, err := selectPod(ctx, clientset, ns, CentralSelector, nil)
	if err != nil {
		return "", nil, err
	}

	statuses := make([]string, 0, len(raftDBs))
	for _, db := range raftDBs {
		status, err := clusterStatus(ctx, config, ns, statusPodName, statusContainerName, db)
		if err != nil {
			return "", nil, fmt.Errorf("failed to query %s raft status: %w", db.dbName, err)
		}
		statuses = append(statuses, status)
	}

	memberIP, err := findMatchingRaftMemberIP(candidates, statuses...)
	if err != nil || memberIP == "" {
		return memberIP, nil, err
	}

	podName, containerName, err := selectPod(ctx, clientset, ns, CentralSelector, []string{memberIP})
	if err != nil {
		return memberIP, nil, err
	}
	kickedDBs, err := kickRaftMemberFromPod(ctx, config, ns, podName, containerName, memberIP)
	if err != nil {
		return memberIP, kickedDBs, err
	}
	return memberIP, kickedDBs, nil
}

// KickRaftMember asks a healthy ovn-central pod (not running on memberIP) to
// evict memberIP from both the OVN_Northbound and OVN_Southbound raft clusters.
// Calling this on a member that has already left or never joined is a no-op.
func KickRaftMember(ctx context.Context, clientset kubernetes.Interface, config *rest.Config, ns, memberIP string) error {
	memberIPs := normalizeCandidateIPs([]string{memberIP})
	if len(memberIPs) == 0 {
		return nil
	}
	memberIP = memberIPs[0]

	podName, containerName, err := selectPod(ctx, clientset, ns, CentralSelector, []string{memberIP})
	if err != nil {
		return err
	}

	_, err = kickRaftMemberFromPod(ctx, config, ns, podName, containerName, memberIP)
	return err
}
