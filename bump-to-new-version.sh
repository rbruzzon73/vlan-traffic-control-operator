#!/usr/bin/env bash
set -eo pipefail

OLD_VERSION="${1}"
NEW_VERSION="${2}"

if [ -z "$OLD_VERSION" ] || [ -z "$NEW_VERSION" ]; then
  echo "Usage: $0 <OLD_VERSION> <NEW_VERSION>"
  echo "Example: $0 0.2.35 0.2.36"
  exit 1
fi

echo "=================================================="
echo " ==> Bumping project references: v${OLD_VERSION} -> v${NEW_VERSION}"
echo "=================================================="

# 1. Update Go Controller fallback image tag (Agent Image)
if [ -f "pkg/controller/vlantrafficcontrol_controller.go" ]; then
  echo "--> Updating agent fallback image in pkg/controller/vlantrafficcontrol_controller.go..."
  sed -i -E "s|(vlan-traffic-control-agent:v)[0-9]+\.[0-9]+\.[0-9]+|\1${NEW_VERSION}|g" pkg/controller/vlantrafficcontrol_controller.go
fi

# 2. Update config/deploy manifests
if [ -d "config/deploy" ]; then
  echo "--> Updating image tags in config/deploy manifests..."
  find config/deploy -type f \( -name "*.yaml" -o -name "*.yml" \) \
    -exec sed -i -E "s|(vlan-traffic-control-operator:v)[0-9]+\.[0-9]+\.[0-9]+|\1${NEW_VERSION}|g" {} +
fi

# 3. Update base CSV manifests (Includes CSV metadata name, version field, and image tags)
if [ -d "config/manifests" ]; then
  echo "--> Updating CSV base manifests..."
  find config/manifests -type f \( -name "*.yaml" -o -name "*.yml" \) \
    -exec sed -i "s/vlan-traffic-control.v${OLD_VERSION}/vlan-traffic-control.v${NEW_VERSION}/g" {} + \
    -exec sed -i -E "s/^(  version: ).*/\1${NEW_VERSION}/g" {} + \
    -exec sed -i -E "s|(vlan-traffic-control-operator:v)[0-9]+\.[0-9]+\.[0-9]+|\1${NEW_VERSION}|g" {} + \
    -exec sed -i -E "s|(vlan-traffic-control-agent:v)[0-9]+\.[0-9]+\.[0-9]+|\1${NEW_VERSION}|g" {} +
fi

# 4. Update bundle manifests
if [ -d "bundle" ]; then
  echo "--> Updating references in bundle manifests..."
  find bundle/ -type f \( -name "*.yaml" -o -name "*.yml" \) \
    -exec sed -i "s/vlan-traffic-control.v${OLD_VERSION}/vlan-traffic-control.v${NEW_VERSION}/g" {} + \
    -exec sed -i -E "s/^(  version: ).*/\1${NEW_VERSION}/g" {} + \
    -exec sed -i -E "s|(vlan-traffic-control-operator:v)[0-9]+\.[0-9]+\.[0-9]+|\1${NEW_VERSION}|g" {} + \
    -exec sed -i -E "s|(vlan-traffic-control-agent:v)[0-9]+\.[0-9]+\.[0-9]+|\1${NEW_VERSION}|g" {} + \
    -exec sed -i -E "s|(vlan-traffic-control-bundle:v)[0-9]+\.[0-9]+\.[0-9]+|\1${NEW_VERSION}|g" {} +
fi

echo "=================================================="
echo " Project successfully bumped to v${NEW_VERSION}!"
echo "=================================================="
