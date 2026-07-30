#!/usr/bin/env bash
set -e

OLD_VER="${1:-0.2.3}"
NEW_VER="${2:-0.2.4}"
REGISTRY="ghcr.io/rbruzzon73"

echo "=========================================================================="
echo " Starting Release Process: v${OLD_VER} -> v${NEW_VER}"
echo "=========================================================================="

# 1. Verify Go compilation before building images
echo "--> [1/6] Compiling Go binaries..."
go mod tidy
go build -o /dev/null cmd/manager/main.go
go build -o /dev/null cmd/agent/main.go

# 2. Build and Push Manager Image
echo "--> [2/6] Building & Pushing Manager Image (v${NEW_VER})..."
podman build -t ${REGISTRY}/vlan-traffic-control-manager:v${NEW_VER} -f Dockerfile .
podman push ${REGISTRY}/vlan-traffic-control-manager:v${NEW_VER}

# 3. Build and Push Agent Image
echo "--> [3/6] Building & Pushing Agent Image (v${NEW_VER})..."
podman build -t ${REGISTRY}/vlan-traffic-control-agent:v${NEW_VER} -f Dockerfile.agent .
podman push ${REGISTRY}/vlan-traffic-control-agent:v${NEW_VER}

# 4. Generate Bundle & Build Bundle Image
echo "--> [4/6] Building & Pushing OLM Bundle Image..."
make bundle IMG=${REGISTRY}/vlan-traffic-control-manager:v${NEW_VER} VERSION=${NEW_VER}
podman build -t ${REGISTRY}/vlan-traffic-control-bundle:v${NEW_VER} -f bundle.Dockerfile .
podman push ${REGISTRY}/vlan-traffic-control-bundle:v${NEW_VER}

# 5. Build and Push Catalog Image (FBC Method)
echo "--> [5/6] Generating and Building Catalog Image..."
rm -rf catalog catalog.Dockerfile catalog.dockerfile
mkdir -p catalog

# Render the bundle definition to catalog.yaml
opm render ${REGISTRY}/vlan-traffic-control-bundle:v${NEW_VER} --output=yaml > catalog/catalog.yaml

# Extract the exact bundle name (handles name at top of file or under schema)
BUNDLE_NAME=$(grep -E "^name: vlan-traffic-control\.v" catalog/catalog.yaml | head -n 1 | awk '{print $2}')

if [ -z "$BUNDLE_NAME" ]; then
    BUNDLE_NAME=$(grep -E "name: vlan-traffic-control\.v" catalog/catalog.yaml | head -n 1 | awk '{print $2}')
fi

echo "--> Detected bundle identifier: ${BUNDLE_NAME}"

# Append matching olm.package and olm.channel definitions
cat <<EOF >> catalog/catalog.yaml

---
schema: olm.package
name: vlan-traffic-control
defaultChannel: stable

---
schema: olm.channel
package: vlan-traffic-control
name: stable
entries:
  - name: ${BUNDLE_NAME}
EOF

# Generate the catalog Dockerfile
opm generate dockerfile catalog

# Detect generated Dockerfile name and build using Podman
DOCKERFILE_NAME="catalog.Dockerfile"
if [ ! -f "$DOCKERFILE_NAME" ]; then
    DOCKERFILE_NAME="catalog.dockerfile"
fi

podman build -t ${REGISTRY}/vlan-traffic-control-catalog:v${NEW_VER} -f ${DOCKERFILE_NAME} .
podman push ${REGISTRY}/vlan-traffic-control-catalog:v${NEW_VER}

# 6. Apply to OpenShift
echo "--> [6/6] Updating OpenShift CatalogSource..."
oc apply -f - <<MANIFEST
apiVersion: operators.coreos.com/v1alpha1
kind: CatalogSource
metadata:
  name: vlan-traffic-control-catalog
  namespace: openshift-marketplace
spec:
  displayName: VLAN Traffic Control Catalog
  image: ${REGISTRY}/vlan-traffic-control-catalog:v${NEW_VER}
  publisher: rbruzzon73
  sourceType: grpc
  updateStrategy:
    registryPoll:
      interval: 1m
MANIFEST

echo "=========================================================================="
echo " Release v${NEW_VER} completed and CatalogSource updated in OpenShift!"
echo " Watch rollout status using:"
echo "   oc get pods -n openshift-vlan-tc-operator -w"
echo "=========================================================================="
