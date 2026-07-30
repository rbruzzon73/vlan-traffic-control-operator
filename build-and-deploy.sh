#!/usr/bin/env bash
set -eo pipefail

# ==============================================================================
# Configuration Variables
# ==============================================================================
VERSION="0.2.4"
PREV_VERSION="0.2.3"
REGISTRY="ghcr.io/rbruzzon73"
OPERATOR_NAME="vlan-traffic-control"

BUNDLE_IMG="${REGISTRY}/${OPERATOR_NAME}-bundle:v${VERSION}"
PREV_BUNDLE_IMG="${REGISTRY}/${OPERATOR_NAME}-bundle:v${PREV_VERSION}"
CATALOG_IMG="${REGISTRY}/${OPERATOR_NAME}-catalog:v${VERSION}"

echo "======================================================================"
echo "Starting Build & Deploy Process for ${OPERATOR_NAME} v${VERSION}"
echo "======================================================================"

# ------------------------------------------------------------------------------
# Step 1: Sync Base CSV to Bundle Manifests
# ------------------------------------------------------------------------------
echo "==> Step 1: Syncing CSV base to bundle/manifests..."
if [ -f "config/manifests/bases/${OPERATOR_NAME}.clusterserviceversion.yaml" ]; then
    cp "config/manifests/bases/${OPERATOR_NAME}.clusterserviceversion.yaml" \
       "bundle/manifests/${OPERATOR_NAME}.clusterserviceversion.yaml"
else
    echo "ERROR: Base CSV file not found in config/manifests/bases/"
    exit 1
fi

# ------------------------------------------------------------------------------
# Step 2: Build and Push Bundle Container Image
# ------------------------------------------------------------------------------
echo "==> Step 2: Building and pushing bundle image (${BUNDLE_IMG})..."
podman build --no-cache -t "${BUNDLE_IMG}" -f bundle.Dockerfile .
podman push "${BUNDLE_IMG}"

# ------------------------------------------------------------------------------
# Step 3: Re-render Catalog Workspace
# ------------------------------------------------------------------------------
echo "==> Step 3: Generating catalog directory and Dockerfile..."
rm -rf catalog catalog.Dockerfile
mkdir -p catalog
opm generate dockerfile catalog

echo "==> Step 4: Rendering bundle manifests into catalog/operator.yaml..."
opm render "${PREV_BUNDLE_IMG}" --output=yaml > catalog/operator.yaml
opm render "${BUNDLE_IMG}" --output=yaml >> catalog/operator.yaml

echo "==> Step 5: Appending package and channel entries..."
printf -- "---\nschema: olm.package\nname: ${OPERATOR_NAME}\ndefaultChannel: alpha\n---\nschema: olm.channel\npackage: ${OPERATOR_NAME}\nname: alpha\nentries:\n  - name: ${OPERATOR_NAME}.v${PREV_VERSION}\n  - name: ${OPERATOR_NAME}.v${VERSION}\n    replaces: ${OPERATOR_NAME}.v${PREV_VERSION}\n" >> catalog/operator.yaml

# ------------------------------------------------------------------------------
# Step 4: Build and Push Catalog Container Image
# ------------------------------------------------------------------------------
echo "==> Step 6: Building and pushing catalog image (${CATALOG_IMG})..."
podman build --no-cache -t "${CATALOG_IMG}" -f catalog.Dockerfile .
podman push "${CATALOG_IMG}"

# ------------------------------------------------------------------------------
# Step 5: Refresh OpenShift CatalogSource Pod
# ------------------------------------------------------------------------------
echo "==> Step 7: Triggering OpenShift CatalogSource refresh..."
if command -v oc &> /dev/null; then
    oc delete pod -n openshift-marketplace \
      -l olm.catalogSource=${OPERATOR_NAME}-catalog \
      --ignore-not-found
    echo "==> CatalogSource pod restarted successfully."
else
    echo "WARNING: 'oc' CLI not found. Skipping OpenShift pod restart."
fi

echo "======================================================================"
echo "Successfully built and pushed version v${VERSION}!"
echo "======================================================================"
