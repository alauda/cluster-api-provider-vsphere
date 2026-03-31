# Live Debug Workflow

This document records the workflow used on 2026-03-24 to debug and advance the
`capv-test` cluster on the management cluster behind the `proxy-connect`
`kubectl` context.

## Scope

This workflow is for live debugging against a real management cluster when:

- CAPV is already installed in-cluster.
- You want to run the current workspace version of CAPV instead of the image in
  the `Deployment`.
- You need to inspect and advance a stuck `VSphereVM` or `VSphereMachine`.

## Current Environment Notes

- Management cluster context: `proxy-connect`
- CAPV namespace: `capv-system`
- Workload namespace: `cpaas-system`
- Sample environment path: `sample/env-192.168.254.211`
- Control plane template path: `/Datacenter1/vm/slemicro-template`
- `ClusterResourceSet=true` on the management cluster `capi-controller-manager` is mandatory for this sample. Without it, CAPV can still create and boot the VMs, but the workload cluster will never receive the CPI / CCM manifests, nodes remain `NotReady`, and `Machine.status.nodeRef` may stay unset because `Node.spec.providerID` is never populated.

## Key Findings From This Session

The following progression happened during this session:

1. `VirtualMachineProvisioned=False` was initially blocked before clone started.
2. Root cause 1 was a CAPV condition bug when no IP claims were required:
   `IPAddressClaimsFulfilled=True` existed in v1beta2, but legacy
   `IPAddressClaimed=True` was not set.
3. After fixing that, clone progressed and exposed root cause 2:
   `diskGiB=40` was smaller than the template boot disk, which is about `300GiB`.
4. After setting `diskGiB=300`, clone progressed and exposed root cause 3:
   `A specified parameter was not correct: unitNumber`.
5. Root cause 3 came from the template hardware layout:
   the primary disk is on `IDE`, while CAPV also needed to add an etcd data
   disk. Data disks must go on a SCSI controller, not follow the IDE boot disk.
6. After fixing CAPV to prefer an existing SCSI controller for data disks, clone
   and reconfigure both succeeded.
7. Current status moved to `WaitingForIPAllocation`.
8. Latest live check shows the VM is powered on and CAPV can find it by BIOS
   UUID, but vCenter reports:
   - `toolsStatus=toolsNotInstalled`
   - `toolsRunningStatus=guestToolsNotRunning`
   - no guest IP addresses are reported
   This is the current reason `WaitingForIPAllocation` is not clearing.

## Current Meaning Of `WaitingForIPAllocation`

This is expected as a transient next step after:

- clone succeeded,
- the VM exists,
- power-on succeeded,
- metadata reconfigure succeeded.

It is not the final desired state. It means CAPV has a VM reference and MAC
addresses, but no guest IP address has been reported yet.

In code this is the branch in
`controllers/vspherevm_controller.go` where CAPV finds
`len(vmCtx.VSphereVM.Status.Addresses) == 0` and requeues every 10 seconds.

## Checking Live Status

Use:

```bash
kubectl -n cpaas-system get vspherevm capv-test-5r4zm -o yaml
kubectl -n cpaas-system get vspheremachine capv-test-5r4zm -o yaml
kubectl -n cpaas-system get machine capv-test-5r4zm -o yaml
```

Useful quick checks:

```bash
kubectl -n cpaas-system get vspherevm capv-test-5r4zm \
  -o jsonpath='{.status.taskRef}{"\n"}{.status.vmRef}{"\n"}{.status.host}{"\n"}{.status.addresses}{"\n"}'

kubectl -n cpaas-system get vspherevm capv-test-5r4zm \
  -o jsonpath='{.status.conditions[?(@.type=="VMProvisioned")].reason}{"\n"}{.status.conditions[?(@.type=="VMProvisioned")].message}{"\n"}'
```

## Running The Workspace Version Against The Cluster

### 1. Scale down the in-cluster CAPV deployment

```bash
kubectl -n capv-system scale deployment controller-manager --replicas=0
kubectl -n capv-system rollout status deployment/controller-manager --timeout=120s
```

This avoids two CAPV managers reconciling the same objects at once.

### 2. Prepare local credentials and webhook certs

Create a temporary credentials file from the in-cluster secret:

```bash
kubectl -n capv-system get secret manager-bootstrap-credentials -o jsonpath='{.data.credentials\.yaml}' | base64 -d > /tmp/capv-credentials.yaml
```

Extract the webhook certificate used by the running deployment:

```bash
mkdir -p /tmp/capv-webhook
kubectl -n capv-system get secret webhook-service-cert -o jsonpath='{.data.tls\.crt}' | base64 -d > /tmp/capv-webhook/tls.crt
kubectl -n capv-system get secret webhook-service-cert -o jsonpath='{.data.tls\.key}' | base64 -d > /tmp/capv-webhook/tls.key
```

### 3. Build the current workspace binary

```bash
env GOTOOLCHAIN=local \
  GOCACHE=/tmp/capv-gocache \
  GOMODCACHE=/Users/mac/go/workdir/pkg/mod \
  go build -o /tmp/capv-manager .
```

### 3.1 Clean orphan VMDKs before reusing the same cluster name

Before recreating `capv-test`, inspect the target datastores for leftover
directories from older control-plane or worker VMs:

```bash
env GOVC_DATACENTER=Datacenter1 govc datastore.ls -l -R -ds vm-store capv-test*
env GOVC_DATACENTER=Datacenter2 govc datastore.ls -l -R -ds vm-store capv-test*
```

If the old cluster has already been deleted from Kubernetes and the
corresponding VMs are gone from inventory, but datastore directories such as
`capv-test-sjm8d/`, `capv-test-4t9kv/`, or old worker directories still remain,
remove those orphan directories before rebuilding:

```bash
env GOVC_DATACENTER=Datacenter2 govc datastore.rm -ds vm-store <orphan-dir>...
```

This matters because a rebuild can fail even when the old `VSphereResourcePool`
status looks clean. In one live case, the second control-plane machine failed
with:

- `Insufficient disk space on datastore 'vm-store'.`

The root cause was leftover `capv-test*` directories containing old `.vmdk`
files on `Datacenter2/vm-store`, which prevented CAPV from provisioning the
next persistent data disk for the replacement control-plane VM.

### 4. Run the manager locally

```bash
/tmp/capv-manager \
  --kubeconfig=$HOME/.kube/config \
  --credentials-file=/tmp/capv-credentials.yaml \
  --leader-elect=false \
  --webhook-port=0 \
  --webhook-cert-dir=/tmp/capv-webhook \
  --health-addr=:19440 \
  --diagnostics-address=:18443 \
  --insecure-diagnostics \
  --v=4
```

Notes:

- This must keep running for the workspace version to keep reconciling.
- `--leader-elect=false` is safe only because the in-cluster deployment was
  scaled to zero.
- If the session ends, start this command again.
- Before starting the local manager, clear `http_proxy`, `https_proxy`,
  `HTTP_PROXY`, and `HTTPS_PROXY`. In this environment, inherited proxy
  settings such as `http://127.0.0.1:8118` caused `govmomi` to route vCenter
  traffic through the proxy, making thumbprint-based trust look broken and
  producing misleading errors like
  `tls: failed to verify certificate: x509: "192.168.254.211" certificate is not trusted`.
- The same proxy caveat applies when using a workload kubeconfig. If
  `kubectl --kubeconfig=/tmp/capv-test.kubeconfig ...` fails with TLS errors
  against the workload control-plane VIP even though:
  - the kubeconfig `server:` points to the expected VIP
  - the kubeconfig CA matches the cluster CA secret
  - the API server certificate SAN already includes that VIP
  then first retry with proxy variables removed:

```bash
env -u http_proxy -u https_proxy -u HTTP_PROXY -u HTTPS_PROXY \
  kubectl --kubeconfig=/tmp/capv-test.kubeconfig get --raw=/version
```

  In this environment, the local proxy on `127.0.0.1:8118` can intercept the
  HTTPS connection and surface misleading errors such as
  `x509: certificate signed by unknown authority`, even when the workload
  kubeconfig and API server certificate are otherwise correct.

## Updating The Sample Environment

The sample files changed during this session:

- `sample/env-192.168.254.211/20-control-plane.yaml`
  already used `diskGiB: 300`
- `sample/env-192.168.254.211/30-workers.yaml`
  changed from `40` to `300`
- `sample/env-192.168.254.211/41-standalone-vm.yaml`
  changed from `40` to `300`

Apply them with:

```bash
kubectl apply -f sample/env-192.168.254.211/30-workers.yaml
kubectl apply -f sample/env-192.168.254.211/41-standalone-vm.yaml
```

For the already-created control-plane machine and VM, patch them directly:

```bash
kubectl -n cpaas-system patch vspheremachine capv-test-5r4zm --type=merge -p '{"spec":{"diskGiB":300}}'
kubectl -n cpaas-system patch vspherevm capv-test-5r4zm --type=merge -p '{"spec":{"diskGiB":300}}'
```

## Temporary Official OVA Fallback

When the custom `slemicro-template` proved unsuitable for CAPV testing, the
environment was temporarily switched to CAPV's official Photon template for
`v1.33.0`.

Official release page:

- `templates/v1.33.0` on the CAPV releases page

The applied fallback template path is:

- `/Datacenter1/vm/photon-5-kube-v1.33.0`

Practical workflow used:

```bash
# 1. Download the official OVA locally.
# 2. Import it to vCenter.
govc import.spec /tmp/photon-5-kube-v1.33.0.ova > /tmp/photon-import.json
govc import.ova -options=/tmp/photon-import.json /tmp/photon-5-kube-v1.33.0.ova

# 3. Create a snapshot for linked-clone use.
govc snapshot.create -vm /Datacenter1/vm/photon-5-kube-v1.33.0 root

# 4. Mark it as a template.
govc vm.markastemplate /Datacenter1/vm/photon-5-kube-v1.33.0

# 5. Point control plane and worker templates to the new template.
kubectl apply -f sample/env-192.168.254.211/20-control-plane.yaml
kubectl apply -f sample/env-192.168.254.211/30-workers.yaml

# 6. Delete the stuck control-plane Machine so KCP recreates it.
kubectl -n cpaas-system delete machine capv-test-5r4zm
```

The replacement Machine created by KCP in this session was:

- `Machine/capv-test-2vrd9`
- `VSphereMachine/capv-test-2vrd9`
- `VSphereVM/capv-test-2vrd9`

The replacement VM from the Photon template reached these states:

- clone succeeded
- reconfigure succeeded
- `open-vm-tools` is installed and running
- vCenter reports `guest.toolsStatus=toolsOk`
- vCenter reports `guest.toolsRunningStatus=guestToolsRunning`
- cloud-init reports `DataSourceVMware [seed=guestinfo]`
- the guest actually applied the intended static addresses on both NICs
- SSH access with the injected `capv` key works

If CAPV still reports `WaitingForIPAllocation` after this point, the issue is
no longer "missing VMware Tools". Instead, investigate why vCenter network
status does not yet include guest IP addresses for the VM NICs.

## Clarified Root Cause: SLE Micro Template

The most accurate conclusion from this session is:

- `open-vm-tools` is not the direct reason SSH was missing.
- The stronger root cause on the original `slemicro-template` was that
  cloud-init did not successfully consume the VMware `guestinfo.*` data CAPV
  injected.

Symptoms on the original template:

- CAPV injected `guestinfo.metadata` and `guestinfo.userdata`
- the guest did not end up with the expected static IPs
- the `capv` user SSH key was not usable
- CAPV remained stuck at `WaitingForIPAllocation`

What Photon proved:

- once `cloud-init` + VMware datasource worked, the injected IPs and SSH key
  both worked as expected.
- `open-vm-tools` then allowed vCenter/CAPV to observe guest IP information.

Practical guidance:

- for SSH/user/network bootstrap correctness:
  `cloud-init + VMware guestinfo datasource` is essential.
- for CAPV to observe guest IPs reliably:
  `open-vm-tools` should still be treated as required.

## Manual Bootstrap Fix On The New Control Plane Node

After the Photon replacement control-plane node was up, bootstrap still stalled
 because `kubeadm init` could not pull control-plane images from
 `registry.k8s.io`.

The following was verified from inside the VM:

- `containerd` was healthy
- `cloud-init` completed far enough to configure the VM and launch `kubeadm`
- `kubeadm init` was stuck on image pulls
- kubelet kept restarting because `/var/lib/kubelet/config.yaml` was not yet
  generated

The workload node could pull from:

- `registry.alauda.cn:60070`
- `registry.aliyuncs.com`

It could not reliably bootstrap from:

- `registry.k8s.io`

### Registry Override Used

The following registry layout was validated from inside the VM:

- `registry.alauda.cn:60070/tkestack/kube-apiserver:v1.33.7-2`
- `registry.alauda.cn:60070/tkestack/kube-controller-manager:v1.33.7-2`
- `registry.alauda.cn:60070/tkestack/kube-scheduler:v1.33.7-2`
- `registry.alauda.cn:60070/tkestack/kube-proxy:v1.33.7-2`
- `registry.alauda.cn:60070/tkestack/pause:3.10`
- `registry.alauda.cn:60070/tkestack/etcd:v3.5.27-260305`
- `registry.alauda.cn:60070/tkestack/coredns:1.12.4-v4.2.8`

The live `KubeadmControlPlane` object was patched to persist:

- `imageRepository: registry.alauda.cn:60070/tkestack`
- `dns.imageRepository: registry.alauda.cn:60070/tkestack`
- `dns.imageTag: 1.12.4-v4.2.8`
- `etcd.local.imageRepository: registry.alauda.cn:60070/tkestack`
- `etcd.local.imageTag: v3.5.27-260305`

The sample file was updated to match:

- `sample/env-192.168.254.211/20-control-plane.yaml`
- `sample/env-192.168.254.211/30-workers.yaml`

### Manual Recovery Steps Used On The Node

The replacement node (`master-01`) was repaired manually with:

1. pre-pull all required images from the internal registry
2. switch `containerd` sandbox image to
   `registry.alauda.cn:60070/tkestack/pause:3.10`
3. restart `containerd`
4. stop the previously stuck `kubeadm init`
5. re-run `kubeadm init` with a corrected kubeadm config

Result:

- static pod manifests were created under `/etc/kubernetes/manifests`
- `etcd`, `kube-apiserver`, `kube-controller-manager`, and `kube-scheduler`
  all started
- `/etc/kubernetes/admin.conf` became usable locally on the node
- the node registered successfully as `master-01`

## Control Plane Endpoint Workaround

The original cluster endpoint was:

- `192.168.139.101:6443`

The node itself was reachable at:

- `192.168.130.219:6443`

Because the original endpoint was not yet being served, the management cluster
could not talk to the workload control plane.

Temporary workaround used:

- add `192.168.139.101/20` to `eth0` on the control-plane node
- patch `Cluster.spec.controlPlaneEndpoint` to use `192.168.130.219:6443`
- delete and re-create the workload kubeconfig secret so a new kubeconfig was
  generated with the patched endpoint

## Control Plane Node Label Root Cause

After the control plane node was repaired, `KubeadmControlPlane` still reported:

- `APIServerPodHealthy=False, reason=PodFailed, message=Missing Node`
- `EtcdMemberHealthy=False, message=Etcd member reports the cluster is composed by members [], but the member hosted on this Machine is not included`

The root cause was not a broken kubeconfig or a bad etcd port-forward path.
The actual issue was that the workload `Node` no longer had the
`node-role.kubernetes.io/control-plane` label.

Important observations:

- `kubeadm init` inside the VM did run `mark-control-plane`
- the node initially registered as `master-01`
- `KubeadmControlPlane` looks up control plane nodes by node labels, not by
  `Machine.status.nodeRef`
- `cluster-api` later reconciles node labels from the owning `Machine`
- the generated control plane `Machine` did not contain
  `node-role.kubernetes.io/control-plane`, so the node was eventually
  reconciled into a state without the control plane role label

Why this broke KCP:

- `getControlPlaneNodes()` returned an empty list because `master-01` had no
  control plane role label
- static pod health checks then treated the machine as having a missing node
- etcd member checks also used the empty control plane node list and ended up
  surfacing `members []`

Permanent fix:

- add `node-role.kubernetes.io/control-plane: ""` to
  `KubeadmControlPlane.spec.machineTemplate.metadata.labels`

Files updated in this repository:

- `sample/20-control-plane.yaml`
- `sample/env-192.168.254.211/20-control-plane.yaml`
- `templates/cluster-template.yaml`
- `templates/cluster-template-node-ipam.yaml`
- `templates/cluster-template-ignition.yaml`
- `templates/cluster-template-external-loadbalancer.yaml`

Live validation after the fix:

- the workload node recovered the `node-role.kubernetes.io/control-plane` label
- `APIServerPodHealthy`, `ControllerManagerPodHealthy`, `SchedulerPodHealthy`,
  `EtcdPodHealthy`, and `EtcdMemberHealthy` all flipped back to `True`
- `KubeadmControlPlane` recovered `ControlPlaneComponentsHealthy=True` and
  `EtcdClusterHealthy=True`

## Platform Registry Prerequisite

For this environment, imported-cluster module reconciliation also depends on a
platform-side public registry credential. This is not configured by the CAPV
manifests in this repository.

Observed behavior when the prerequisite was missing:

- `cluster-transformer` continuously logged
  `need registry address in common config`
- `ClusterModule/capv-test` could not progress
- manually patching `cpaas.io/registry-address` onto
  `cluster.x-k8s.io/Cluster` did not stick
- the same annotation on `platform.tkestack.io/Cluster` also remained empty

The reason is that `cpaas.io/registry-address` is reconciled from platform
"common config", not treated as a free-form user annotation.

The concrete prerequisite identified in-cluster was:

- `Secret/cpaas-system/public-registry-credential`
- `type: CloudCredential`
- `cpaas.io/status: pending`
- no `data`

Required action:

- complete the platform-side or external operation that populates
  `public-registry-credential` with real registry credential data before
  expecting `cpaas.io/registry-address` to appear on cluster resources

Until that external setup is completed, controllers may continuously reconcile
`cpaas.io/registry-address` back to an empty string.

## Cleanup Old ClusterCredential Before Reusing The Same Cluster Name

When re-creating a workload cluster with the same name, verify that the
platform-side `ClusterCredential` has also been rotated to a fresh object for
the new cluster instance.

Observed failure mode:

- platform cluster creation or import validation failed with errors like:
  - `imported: Internal error: Unauthorized`
  - `version: Internal error: the server has asked for the client to provide credentials`
  - `nodes.architecture: Internal error: Unauthorized`

Root cause found in this session:

- the old `ClusterCredential` object was reused across two different
  `capv-test` cluster lifecycles
- its `token` pointed to a previous cluster instance and was no longer valid
- `cluster-api-provider-alauda` prefers `BearerToken` over client certs if both
  are present, so the stale token overrode an otherwise usable client
  certificate

Practical checks:

```bash
kubectl get clustercredentials.platform.tkestack.io
kubectl get clusters.platform.tkestack.io capv-test -o jsonpath='{.spec.clusterCredentialRef.name}{"\n"}'
kubectl -n cpaas-system get secret <cluster-credential-secret> -o yaml
```

Safe expectation after a clean re-create:

- the new workload cluster should reference a newly created
  `ClusterCredential`, not an older reused one
- if an older credential is still referenced, remove or rotate it before
  continuing platform-side reconciliation

Temporary workaround used during debugging:

- remove `bearerToken` from the underlying credential secret so only client cert
  authentication remains

Example:

```bash
kubectl -n cpaas-system patch secret <cluster-credential-secret> \
  --type=json \
  -p='[{"op":"remove","path":"/data/bearerToken"}]'
```

This is a workaround, not a full fix. The durable fix is to ensure old
cluster credentials are not reused across same-name cluster rebuilds, or to
change the platform controller logic so a stale token does not override a valid
client certificate.

After re-generating the secret, the workload kubeconfig became usable from the
management environment.

Important cleanup note discovered on 2026-03-25:

- this workaround must be reverted once the dedicated `haproxy` VM is serving
  `192.168.139.101:6443`
- if `192.168.139.101/20` remains on the control-plane node while the separate
  `haproxy` VM also owns `192.168.139.101`, the environment has an IP conflict
- the symptom on the `haproxy` VM is that HAProxy listens on `6443`, but logs
  repeated `kube_apiserver_backend/<NOSRV>` or backend flapping because traffic
  to the VIP may still land on `master-01`

The concrete state observed during this follow-up was:

- `master-01` had both `192.168.130.219/20` and `192.168.139.101/20` on `eth0`
- the manually deployed `haproxy` VM also had `192.168.139.101/20`
- from the `haproxy` VM, backend `192.168.130.219:6443` initially looked
  unreachable
- from `master-01`, `https://192.168.139.101:6443/version` returned the local
  kube-apiserver response, proving the VIP was still local to the node

Resolution used:

```bash
# 1. Remove the temporary VIP from the control-plane node.
ssh capv@192.168.130.219 sudo ip addr del 192.168.139.101/20 dev eth0

# 2. Verify HAProxy can reach the backend again.
curl -k https://192.168.139.101:6443/version

# 3. Restore the Cluster endpoint back to the HAProxy VIP.
kubectl -n cpaas-system patch cluster capv-test --type=merge \
  -p '{"spec":{"controlPlaneEndpoint":{"host":"192.168.139.101","port":6443}}}'

# 4. Re-generate the workload kubeconfig so it points back to the VIP.
kubectl -n cpaas-system delete secret capv-test-kubeconfig
```

After this cleanup:

- HAProxy reported `Server kube_apiserver_backend/master-01 is UP`
- the regenerated workload kubeconfig used `https://192.168.139.101:6443`
- `kubectl --kubeconfig=/tmp/capv-test.kubeconfig get --raw=/version` succeeded
  through HAProxy

## CNI Follow-Up

Once the control plane was available, the next blocker was that the cluster had
no CNI. `coredns` remained pending until a CNI was installed.

### Calico Attempt

The Calico manifest from:

- `test/e2e/data/cni/calico/calico.yaml`

was adapted by:

- replacing upstream `quay.io/calico/*` images with internal images:
  - `registry.alauda.cn:60070/acp/calico-node:v3.31.4-a9c64230`
  - `registry.alauda.cn:60070/acp/calico-cni:v3.31.4-a9c64230`
  - `registry.alauda.cn:60070/acp/calico-kube-controllers:v3.31.4-a9c64230`
- setting `__CNI_MTU__` to `1450`

Initial problem:

- the `set-mtu` init container used `/bin/bash`
- the internal `acp/calico-node` image did not contain `/bin/bash`

Fix:

- patch the `calico-node` daemonset to remove the first init container

### Tolerations And Taints

Requirement from the user:

- CNI should tolerate all taints
- do not delete node taints as the long-term solution

Applied changes:

- patch `DaemonSet/calico-node` to use `tolerations: [{operator: Exists}]`
- patch `Deployment/calico-kube-controllers` the same way
- restore the node taint
  `node.cloudprovider.kubernetes.io/uninitialized=true:NoSchedule`

### kube-ovn CRD Dependency In Internal Calico Images

The internal `acp/calico-*` images are not clean upstream images.
They attempted to access:

- `ips.kubeovn.io`

This caused pod sandbox creation to fail until the CRD existed in the workload
cluster.

Fix:

- copy the management-cluster CRD `ips.kubeovn.io` into the workload cluster

After that:

- `coredns` became `Running`
- `calico-kube-controllers` became schedulable and progressed
- the workload cluster node stayed `Ready`

### Remaining CNI Caveat

Even after `coredns` was recovered, `calico-node` itself still showed
instability (`Running` / `CrashLoopBackOff` across checks). This indicates the
internal `acp/calico-*` images are still customized and may need additional
environment-specific resources beyond plain upstream Calico behavior.

This should be treated as a separate follow-up from the original CAPV/template
debugging flow.

## Worker Machine Reconciliation Gotcha

Two worker machines stayed broken even after the live worker template had
already been fixed:

- `capv-test-md-0-nlfqs-cz7fq`
- `capv-test-md-0-nlfqs-tcfj2`

Observed symptom:

- `VSphereMachine.spec.template` still pointed to `/Datacenter1/vm/slemicro-template`
- `VSphereMachine.spec.diskGiB` was still `40`
- `VSphereVM.spec.diskGiB` was still `40`
- both failed with:
  `can't resize template disk down, initial capacity is larger: 314572800KiB > 41943040KiB`

Important nuance:

- the live `VSphereMachineTemplate/capv-test-worker` had already been updated to:
  - template `/Datacenter1/vm/photon-5-kube-v1.33.0`
  - `diskGiB: 300`
- however, the existing `Machine` / `VSphereMachine` objects had been created
  earlier and already contained the old cloned infrastructure spec.
- updating the template in-place did **not** retroactively rewrite those
  existing worker objects.

Practical fix used:

```bash
kubectl -n cpaas-system delete machine \
  capv-test-md-0-nlfqs-cz7fq \
  capv-test-md-0-nlfqs-tcfj2
```

The `MachineSet` then recreated fresh workers using the current live template.

Replacement workers created in this session:

- `capv-test-md-0-nlfqs-5wrj4`
- `capv-test-md-0-nlfqs-rqltw`

Their new specs confirmed the fix:

- template `/Datacenter1/vm/photon-5-kube-v1.33.0`
- `diskGiB: 300`
- `VSphereVM` moved past the old disk-size failure and into
  `WaitingForIPAllocation`

## Inspecting Template Hardware

Use `govc` to inspect the template used by CAPV:

```bash
env GOVC_URL=https://<vcenter> \
  GOVC_USERNAME='<username>' \
  GOVC_PASSWORD='<password>' \
  GOVC_INSECURE=1 \
  govc device.info -vm /Datacenter1/vm/slemicro-template
```

In this session the important result was:

- boot disk on `IDE 0`
- an existing `ParaVirtualSCSIController` with key `1000`

That hardware layout is what exposed the `unitNumber` bug.

## Why `unitNumber` Failed

Old behavior:

- CAPV picked the primary disk controller.
- The primary template disk was on IDE.
- CAPV tried to place the etcd data disk on that same bus.
- vSphere rejected the resulting clone spec with
  `A specified parameter was not correct: unitNumber`.

Fixed behavior:

- If the primary disk controller is already SCSI, keep using it.
- If the primary disk controller is not SCSI, search the VM hardware for an
  existing SCSI controller and use that for data disks.
- If no SCSI controller exists, fail early with a clear CAPV error instead of
  a vSphere task failure.

## Code Touchpoints

- IP claim condition fix:
  `controllers/vspherevm_ipaddress_reconciler.go`
- IDE-to-SCSI data disk controller fix:
  `pkg/services/govmomi/vcenter/clone.go`
- Regression test for IDE boot disk plus SCSI data disk:
  `pkg/services/govmomi/vcenter/clone_test.go`

## Test Commands Used

Focused test for the SCSI-controller selection fix:

```bash
env GOCACHE=/tmp/capv-gocache \
  GOMODCACHE=/Users/mac/go/workdir/pkg/mod \
  go test ./pkg/services/govmomi/vcenter \
  -run TestCreateDataDisksUsesSCSIControllerWhenPrimaryDiskIsIDE
```

## If `WaitingForIPAllocation` Persists

Then the likely next areas to inspect are:

- guest network configuration inside the VM
- VMware Tools / open-vm-tools reporting
- whether the expected NICs are connected to the right port groups
- whether the guest OS actually receives the static or DHCP config you expect

At this stage CAPV has already created and powered on the VM; the remaining work
is guest-IP discovery.

## Latest Live Finding For `capv-test-5r4zm`

As of 2026-03-24 18:20 CST:

- `VSphereVM.status.vmRef = VirtualMachine:vm-115`
- vCenter runtime power state is `poweredOn`
- both disks and both NICs are present and connected
- guest metadata and userdata were written to `guestinfo.*`
- CAPV sees MAC addresses but not guest IP addresses
- vCenter reports:
  - `guest.toolsStatus = toolsNotInstalled`
  - `guest.toolsRunningStatus = guestToolsNotRunning`

This strongly suggests the current blocker is guest-side IP reporting via
VMware Tools / open-vm-tools, not CAPV reconciliation logic.
