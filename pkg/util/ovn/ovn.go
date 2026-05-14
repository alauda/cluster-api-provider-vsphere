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
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
)

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

// KickRaftMember asks a healthy ovn-central pod (not running on memberIP) to
// evict memberIP from both the OVN_Northbound and OVN_Southbound raft clusters.
// Calling this on a member that has already left or never joined is a no-op
// (the cluster/status grep returns no server ID, so cluster/kick is skipped).
func KickRaftMember(ctx context.Context, clientset kubernetes.Interface, config *rest.Config, ns, memberIP string) error {
	podName, containerName, err := selectPod(ctx, clientset, ns, CentralSelector, []string{memberIP})
	if err != nil {
		return err
	}

	for _, db := range []struct {
		ctlPath string
		dbName  string
	}{
		{"/var/run/ovn/ovnnb_db.ctl", "OVN_Northbound"},
		{"/var/run/ovn/ovnsb_db.ctl", "OVN_Southbound"},
	} {
		// Find the raft server ID for memberIP. grep -v Address strips the
		// header line that also matches the IP literal when the cluster
		// happens to expose its members on a column called Address.
		statusCmd := fmt.Sprintf(
			`ovs-appctl -t %s cluster/status %s | grep %s | grep -v Address | awk '{print $1}'`,
			db.ctlPath, db.dbName, memberIP,
		)
		serverID, err := execInPod(ctx, config, ns, podName, containerName, "sh", "-c", statusCmd)
		if err != nil {
			return fmt.Errorf("failed to query %s server id for %s: %w", db.dbName, memberIP, err)
		}
		serverID = strings.TrimSpace(serverID)
		if serverID == "" {
			continue
		}
		kickCmd := fmt.Sprintf(`ovs-appctl -t %s cluster/kick %s %s`, db.ctlPath, db.dbName, serverID)
		if _, err := execInPod(ctx, config, ns, podName, containerName, "sh", "-c", kickCmd); err != nil {
			return fmt.Errorf("failed to kick %s server %s for %s: %w", db.dbName, serverID, memberIP, err)
		}
	}

	return nil
}
