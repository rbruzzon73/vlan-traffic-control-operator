#!/usr/bin/env bash
set -e

OLD_VER="$1"
NEW_VER="$2"

if [ -z "$OLD_VER" ] || [ -z "$NEW_VER" ]; then
    echo "Usage: ./bump-to-new-version.sh <OLD_VERSION> <NEW_VERSION>"
    echo "Example: ./bump-to-new-version.sh 0.2.3 0.2.4"
    exit 1
fi

echo "=================================================="
echo " Bumping project references: v${OLD_VER} -> v${NEW_VER}"
echo "=================================================="

# 1. Update image tags in Go Controller code
CONTROLLER_FILE="pkg/controller/vlantrafficcontrol_controller.go"
if [ -f "$CONTROLLER_FILE" ]; then
    echo "--> Updating Agent image tag in ${CONTROLLER_FILE}..."
    sed -i "s|vlan-traffic-control-agent:v${OLD_VER}|vlan-traffic-control-agent:v${NEW_VER}|g" "$CONTROLLER_FILE"
fi

# 2. Update default versions inside release.sh
RELEASE_SCRIPT="release.sh"
if [ -f "$RELEASE_SCRIPT" ]; then
    echo "--> Updating default versions in ${RELEASE_SCRIPT}..."
    sed -i "s|OLD_VER=\"\${1:-${OLD_VER}}\"|OLD_VER=\"\${1:-${NEW_VER}}\"|g" "$RELEASE_SCRIPT"
fi

# 3. Update CSV / Bundle Manifests if present
CSV_CONFIG="config/manifests/bases/vlan-traffic-control.clusterserviceversion.yaml"
CSV_BUNDLE="bundle/manifests/vlan-traffic-control.clusterserviceversion.yaml"

for csv in "$CSV_CONFIG" "$CSV_BUNDLE"; do
    if [ -f "$csv" ]; then
        echo "--> Updating version in ${csv}..."
        sed -i "s|name: vlan-traffic-control.v${OLD_VER}|name: vlan-traffic-control.v${NEW_VER}|g" "$csv"
        sed -i "s|replaces: vlan-traffic-control.v[0-9]*\.[0-9]*\.[0-9]*|replaces: vlan-traffic-control.v${OLD_VER}|g" "$csv"
        sed -i "s|version: ${OLD_VER}|version: ${NEW_VER}|g" "$csv"
        sed -i "s|vlan-traffic-control-manager:v${OLD_VER}|vlan-traffic-control-manager:v${NEW_VER}|g" "$csv"
    fi
done

# 4. Verify Go compilation
echo "--> Verifying Go code compiles with new references..."
go mod tidy
go build -o /dev/null cmd/manager/main.go
go build -o /dev/null cmd/agent/main.go

echo "=================================================="
echo " Project successfully bumped to v${NEW_VER}!"
echo " Next step: Run './release.sh ${OLD_VER} ${NEW_VER}' to build and publish."
echo "=================================================="
