#!/usr/bin/env bash
set -eo pipefail

OLD_VERSION="${1}"
NEW_VERSION="${2}"

if [ -z "$OLD_VERSION" ] || [ -z "$NEW_VERSION" ]; then
  echo "Usage: $0 <OLD_VERSION> <NEW_VERSION>"
  echo "Example: $0 0.2.35 0.2.36"
  exit 1
fi

REGISTRY="ghcr.io/rbruzzon73"
NEW_TAG="v${NEW_VERSION}"

echo "=================================================="
echo "==> Starting release workflow: v${OLD_VERSION} -> v${NEW_VERSION}"
echo "=================================================="

# ------------------------------------------------------------------------------
# 1. Update version references across the project via bump script & sed
# ------------------------------------------------------------------------------
echo "==> Step 1: Bumping project version references..."
./bump-to-new-version.sh "${OLD_VERSION}" "${NEW_VERSION}"

echo "==> Step 1a: Enforcing exact image tag updates across manifests & Go code..."
find config/ -type f \( -name "*.yaml" -o -name "*.yml" \) -exec sed -i "s|${REGISTRY}/vlan-traffic-control-agent:v[0-9]*\.[0-9]*\.[0-9]*|${REGISTRY}/vlan-traffic-control-agent:${NEW_TAG}|g" {} +
find config/ -type f \( -name "*.yaml" -o -name "*.yml" \) -exec sed -i "s|${REGISTRY}/vlan-traffic-control-operator:v[0-9]*\.[0-9]*\.[0-9]*|${REGISTRY}/vlan-traffic-control-operator:${NEW_TAG}|g" {} +

# Update fallback defaults in Go files if present
find pkg/ controllers/ -type f -name "*.go" -exec sed -i "s|vlan-traffic-control-agent:v[0-9]*\.[0-9]*\.[0-9]*|vlan-traffic-control-agent:${NEW_TAG}|g" {} + 2>/dev/null || true
find pkg/ controllers/ -type f -name "*.go" -exec sed -i "s|vlan-traffic-control-operator:v[0-9]*\.[0-9]*\.[0-9]*|vlan-traffic-control-operator:${NEW_TAG}|g" {} + 2>/dev/null || true

# ------------------------------------------------------------------------------
# 1b. Pre-flight Check: Ensure target version alignment before building
# ------------------------------------------------------------------------------
echo "==> Step 1b: Running pre-flight version alignment checks..."

CHECK_FILES=(
  "config/deploy/deployment.yaml"
  "config/manifests/bases/vlan-traffic-control.clusterserviceversion.yaml"
)

ERRORS=0
for file in "${CHECK_FILES[@]}"; do
  if [ ! -f "$file" ]; then
    echo "⚠️ WARNING: File not found during check: $file (skipping)"
    continue
  fi

  # Search for ANY image tag version that does NOT match NEW_VERSION
  STALE_REFS=$(grep -En "vlan-traffic-control-(operator|agent):v[0-9]+\.[0-9]+\.[0-9]+" "$file" | grep -v "${NEW_TAG}" || true)

  if [ -n "$STALE_REFS" ]; then
    echo "❌ MISMATCH IN: $file"
    echo "    Found stale reference(s) not matching ${NEW_TAG}:"
    echo "$STALE_REFS" | sed 's/^/      /'
    ERRORS=$((ERRORS + 1))
  else
    echo "    ✓ Aligned: $file"
  fi
done

if [ $ERRORS -gt 0 ]; then
  echo ""
  echo "💥 Pre-flight check failed! $ERRORS file(s) contain image tags different from ${NEW_TAG}."
  echo "Aborting build process to prevent publishing mismatched container images."
  exit 1
fi

echo "✅ Pre-flight check passed. All target files are aligned to ${NEW_TAG}."

# ------------------------------------------------------------------------------
# 2. Regenerate CRDs from Go API types
# ------------------------------------------------------------------------------
echo "==> Step 2: Regenerating CRDs..."
make manifests

mkdir -p config/deploy
cp config/rbac/service_account.yaml config/deploy/service_account.yaml 2>/dev/null || true

# ------------------------------------------------------------------------------
# 3. Regenerate OLM Bundle manifests
# ------------------------------------------------------------------------------
echo "==> Step 3: Regenerating OLM bundle..."
operator-sdk generate bundle \
  --version "${NEW_VERSION}" \
  --deploy-dir config/deploy \
  --crds-dir config/crd/bases \
  --output-dir bundle \
  --overwrite

CSV_FILE="bundle/manifests/vlan-traffic-control.clusterserviceversion.yaml"
BASE_CSV="config/manifests/bases/vlan-traffic-control.clusterserviceversion.yaml"

echo "--> Injecting RELATED_IMAGE_AGENT, base metadata, icon, and clusterPermissions into bundle CSV..."
python3 -c "
import yaml

with open('$BASE_CSV') as f:
    base = yaml.safe_load(f)

with open('$CSV_FILE') as f:
    bundle = yaml.safe_load(f)

# Copy base metadata annotations
bundle['metadata']['annotations'].update(base['metadata'].get('annotations', {}))

# Copy icon, maintainers, links, provider, keywords, etc.
for key in ['icon', 'links', 'maintainers', 'provider', 'keywords', 'installModes', 'description', 'displayName']:
    if key in base['spec']:
        bundle['spec'][key] = base['spec'][key]

if 'clusterPermissions' in base['spec']['install']['spec']:
    bundle['spec']['install']['spec']['clusterPermissions'] = base['spec']['install']['spec']['clusterPermissions']

# ENSURE RELATED_IMAGE_AGENT IS UPDATED IN CSV SPEC
agent_img = '${REGISTRY}/vlan-traffic-control-agent:${NEW_TAG}'
bundle['spec']['relatedImages'] = [
    {'name': 'operator', 'image': '${REGISTRY}/vlan-traffic-control-operator:${NEW_TAG}'},
    {'name': 'agent', 'image': agent_img}
]

# INJECT RELATED_IMAGE_AGENT INTO MANAGER CONTAINER ENV
for dep in bundle['spec']['install']['spec']['deployments']:
    for container in dep['spec']['template']['spec']['containers']:
        env_list = container.setdefault('env', [])
        # Replace existing or append
        updated = False
        for env_item in env_list:
            if env_item.get('name') == 'RELATED_IMAGE_AGENT':
                env_item['value'] = agent_img
                updated = True
        if not updated:
            env_list.append({'name': 'RELATED_IMAGE_AGENT', 'value': agent_img})

with open('$CSV_FILE', 'w') as f:
    yaml.dump(bundle, f, default_flow_style=False)
"

# ------------------------------------------------------------------------------
# 4. Build and Push Controller & Bundle Containers
# ------------------------------------------------------------------------------
echo "==> Step 4: Building & pushing operator controller image..."
podman build --no-cache -t "${REGISTRY}/vlan-traffic-control-operator:${NEW_TAG}" -f Dockerfile .
podman push "${REGISTRY}/vlan-traffic-control-operator:${NEW_TAG}"

echo "==> Step 4b: Building & pushing agent image..."
podman build --no-cache -t "${REGISTRY}/vlan-traffic-control-agent:${NEW_TAG}" -f Dockerfile.agent .
podman push "${REGISTRY}/vlan-traffic-control-agent:${NEW_TAG}"

echo "==> Step 5: Building & pushing OLM bundle image..."
podman build --no-cache -t "${REGISTRY}/vlan-traffic-control-bundle:${NEW_TAG}" -f bundle.Dockerfile .
podman push "${REGISTRY}/vlan-traffic-control-bundle:${NEW_TAG}"

# ------------------------------------------------------------------------------
# 6. Build File-Based Catalog & Render index.json
# ------------------------------------------------------------------------------
echo "==> Step 6: Rendering File-Based Catalog..."

rm -rf catalog catalog.Dockerfile
mkdir -p catalog

opm init vlan-traffic-control \
  --default-channel alpha \
  --output json > catalog/index.json

cat <<EOF >> catalog/index.json
{"schema":"olm.channel","package":"vlan-traffic-control","name":"alpha","entries":[{"name":"vlan-traffic-control.v${OLD_VERSION}"},{"name":"vlan-traffic-control.v${NEW_VERSION}","replaces":"vlan-traffic-control.v${OLD_VERSION}"}]}
EOF

# Render bundle objects
opm render "${REGISTRY}/vlan-traffic-control-bundle:v${OLD_VERSION}" | jq -c 'select(.schema == "olm.bundle")' >> catalog/index.json
opm render "${REGISTRY}/vlan-traffic-control-bundle:${NEW_TAG}" | jq -c 'select(.schema == "olm.bundle")' >> catalog/index.json

# ------------------------------------------------------------------------------
# 7. Inject Icon directly into File-Based Catalog (FBC) base64 payload
# ------------------------------------------------------------------------------
echo "==> Step 7: Injecting icon into Catalog index via base64 payload patcher..."

python3 -c "
import json, base64, yaml

BASE_CSV = '$BASE_CSV'
with open(BASE_CSV) as f:
    base = yaml.safe_load(f)

icon_list = base['spec'].get('icon', [])

output_lines = []
with open('catalog/index.json', 'r') as f:
    for line in f:
        line_str = line.strip()
        if not line_str:
            continue
        try:
            obj = json.loads(line_str)
            if obj.get('schema') == 'olm.bundle':
                for item in obj.get('objects', []):
                    if item.get('kind') == 'ClusterServiceVersion':
                        csv_raw = base64.b64decode(item['data']).decode('utf-8')
                        csv_json = json.loads(csv_raw)
                        csv_json.setdefault('spec', {})['icon'] = icon_list
                        updated_csv_raw = json.dumps(csv_json)
                        item['data'] = base64.b64encode(updated_csv_raw.encode('utf-8')).decode('utf-8')
            output_lines.append(json.dumps(obj))
        except Exception as e:
            output_lines.append(line_str)

with open('catalog/index.json', 'w') as f:
    f.write('\n'.join(output_lines) + '\n')
"

echo "✅ Step 7 complete: Icon successfully injected into catalog/index.json base64 payload."

# ------------------------------------------------------------------------------
# 8. Build & Push Catalog Image
# ------------------------------------------------------------------------------
opm generate dockerfile catalog

echo "--> Building catalog container image..."
podman build --no-cache -t "${REGISTRY}/vlan-traffic-control-catalog:${NEW_TAG}" -f catalog.Dockerfile .
podman push "${REGISTRY}/vlan-traffic-control-catalog:${NEW_TAG}"

echo "=================================================="
echo "🎉 Release v${NEW_VERSION} successfully built and published!"
echo "=================================================="
