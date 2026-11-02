#!/bin/bash
set -euo pipefail
sandbox=$(cat /etc/acp/registry-bootstrap/sandbox-image)
if ! crictl inspecti "$sandbox" >/dev/null 2>&1; then
  ctr -n k8s.io images import /root/images/pause.tar >/dev/null
fi
crictl inspecti "$sandbox" >/dev/null
ctr -n k8s.io images label "$sandbox" io.cri-containerd.pinned=pinned >/dev/null
