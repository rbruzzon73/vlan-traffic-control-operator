#!/usr/bin/env bash
set -eo pipefail

OLD_VERSION="${1}"
NEW_VERSION="${2}"

if [ -z "$OLD_VERSION" ] || [ -z "$NEW_VERSION" ]; then
  echo "Usage: $0 <OLD_VERSION> <NEW_VERSION>"
  echo "Example: $0 0.2.63 0.2.64"
  exit 1
fi

REGISTRY="ghcr.io/rbruzzon73"
NEW_TAG="v${NEW_VERSION}"
BASE_CSV="config/manifests/bases/vlan-traffic-control.clusterserviceversion.yaml"

# Corrected Red Hat Base64 SVG string
CLEAN_BASE64_ICON="PHN2ZyBpZD0iTGF5ZXJfMSIgZGF0YS1uYW1lPSJMYXllciAxIiB4bWxucz0iaHR0cDovL3d3dy53My5vcmcvMjAwMC9zdmciIHZpZXdCb3g9IjAgMCAxOTIgMTQ1Ij48ZGVmcz48c3R5bGU+LmNscy0xe2ZpbGw6I2UwMDt9PC9zdHlsZT48L2RlZnM+PHRpdGxlPlJlZEhhdC1Mb2dvLUhhdC1Db2xvcjwvdGl0bGU+PHBhdGggZD0iTTE1Ny43Nyw2Mi42MWExNCwxNCwwLDAsMSwuMzEsMy40MmMwLDE0Ljg4LTE4LjEsMTcuNDYtMzAuNjEsMTcuNDZDNzguODMsODMuNDksNDIuNTMsNTMuMjYsNDIuNTMsNDRhNi40Myw2LjQzLDAsMCwxLC4yMi0xLjk0bC0zLjY2LDkuMDZhMTguNDUsMTguNDUsMCwwLDAtMS41MSw3LjMzYzAsMTguMTEsNDEsNDUuNDgsODcuNzQsNDUuNDgsMjAuNjksMCwzNi40My03Ljc2LDM2LjQzLTIxLjc3LDAtMS4wOCwwLTEuOTQtMS43My0xMC4xM1oiLz48cGF0aCBjbGFzcz0iY2xzLTEiIGQ9Ik0xMjcuNDcsODMuNDljMTIuNTEsMCwzMC42MS0yLjU4LDMwLjYxLTE3LjQ2YTE0LDE0LDAsMCwwLS4zMS0zLjQybC03LjQ1LTMyLjM2Yy0xLjcyLTcuMTItMy4yMy0xMC4zNS0xNS43My0xNi42QzEyNC44OSw4LjY5LDEwMy43Ni41LDk3LjUxLjUsOTEuNjkuNSw5MCw4LDgzLjA2LDhjLTYuNjgsMC0xMS42NC01LjYtMTcuODktNS42LTYsMC05LjkxLDQuMDktMTIuOTMsMTIuNSwwLDAtOC40MSwyMy43Mi05LjQ5LDI3LjE2QTYuNDMsNi40MywwLDAsMCw0Mi41Myw0NGMwLDkuMjIsMzYuMywzOS40NSw4NC45NCwzOS40NU0xNjAsNzIuMDdjMS43Myw4LjE5LDEuNzMsOS4wNSwxLjczLDEwLjEzLDAsMTQtMTUuNzQsMjEuNzctMzYuNDMsMjEuNzdDNzguNTQsMTA0LDM3LjU4LDc2LjYsMzcuNTgsNTguNDlhMTguNDUsMTguNDUsMCwwLDEsMS41MS03LjMzQzIyLjI3LDUyLC41LDU1LC41LDc0LjIyYzAsMzEuNDgsNzQuNTksNzAuMjgsMTMzLjY1LDcwLjI4LDQ1LjI4LDAsNTYuNy0yMC40OCw1Ni43LTM2LjY1LDAtMTIuNzAtMTEtMjcuMTYtMzAuODMtMzUuNzgiLz48L3N2Zz4="

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
# 1c. Sanitize & Ensure Base CSV Icon String BEFORE builds start
# ------------------------------------------------------------------------------
echo "==> Step 1c: Repairing and synchronizing base CSV icon (${BASE_CSV})..."

python3 -c "
import yaml

base_csv_path = '$BASE_CSV'
clean_b64 = '$CLEAN_BASE64_ICON'

with open(base_csv_path, 'r') as f:
    data = yaml.safe_load(f)

# Force clean single-line icon in base CSV
data.setdefault('spec', {})['icon'] = [
    {
        'base64data': clean_b64,
        'mediatype': 'image/svg+xml'
    }
]

with open(base_csv_path, 'w') as f:
    yaml.dump(data, f, default_flow_style=False, width=1000)
"
echo "✓ Base CSV Icon successfully synchronized in ${BASE_CSV}."

# ------------------------------------------------------------------------------
# 1b. Pre-flight Check: Ensure target version alignment before building
# ------------------------------------------------------------------------------
echo "==> Step 1b: Running pre-flight version alignment checks..."

CHECK_FILES=(
  "config/deploy/manager.yaml"
  "$BASE_CSV"
)

ERRORS=0
for file in "${CHECK_FILES[@]}"; do
  if [ ! -f "$file" ]; then
    echo "⚠️ WARNING: File not found during check: $file (skipping)"
    continue
  fi

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
  exit 1
fi

echo "✅ Pre-flight check passed. All target files are aligned to ${NEW_TAG}."

# ------------------------------------------------------------------------------
# 2. Regenerate CRDs from Go API types
# ------------------------------------------------------------------------------
echo "==> Step 2: Regenerating CRDs..."
make generate
make manifests

mkdir -p config/deploy
cp config/rbac/service_account.yaml config/deploy/service_account.yaml 2>/dev/null || true
rm -f config/deploy/deployment.yaml

# ------------------------------------------------------------------------------
# 3. Regenerate OLM Bundle manifests
# ------------------------------------------------------------------------------
echo "==> Step 3: Regenerating OLM bundle..."
rm -rf bundle/manifests/*.yaml

operator-sdk generate bundle \
  --version "${NEW_VERSION}" \
  --deploy-dir config/deploy \
  --crds-dir config/crd/bases \
  --output-dir bundle \
  --overwrite

CSV_FILE="bundle/manifests/vlan-traffic-control.clusterserviceversion.yaml"

echo "--> Injecting RELATED_IMAGE_AGENT, base metadata, icon, and clusterPermissions into bundle CSV..."
python3 -c "
import yaml

with open('$BASE_CSV') as f:
    base = yaml.safe_load(f)

with open('$CSV_FILE') as f:
    bundle = yaml.safe_load(f)

bundle['metadata']['annotations'].update(base['metadata'].get('annotations', {}))

for key in ['icon', 'links', 'maintainers', 'provider', 'keywords', 'installModes', 'description', 'displayName']:
    if key in base['spec']:
        bundle['spec'][key] = base['spec'][key]

if 'clusterPermissions' in base['spec']['install']['spec']:
    bundle['spec']['install']['spec']['clusterPermissions'] = base['spec']['install']['spec']['clusterPermissions']

agent_img = '${REGISTRY}/vlan-traffic-control-agent:${NEW_TAG}'
bundle['spec']['relatedImages'] = [
    {'name': 'operator', 'image': '${REGISTRY}/vlan-traffic-control-operator:${NEW_TAG}'},
    {'name': 'agent', 'image': agent_img}
]

for dep in bundle['spec']['install']['spec']['deployments']:
    for container in dep['spec']['template']['spec']['containers']:
        env_list = container.setdefault('env', [])
        updated = False
        for env_item in env_list:
            if env_item.get('name') == 'RELATED_IMAGE_AGENT':
                env_item['value'] = agent_img
                updated = True
        if not updated:
            env_list.append({'name': 'RELATED_IMAGE_AGENT', 'value': agent_img})

with open('$CSV_FILE', 'w') as f:
    yaml.dump(bundle, f, default_flow_style=False, width=1000)
"

# ------------------------------------------------------------------------------
# 4. Build and Push Controller & Bundle Containers
# ------------------------------------------------------------------------------
echo "==> Step 4: Building & pushing operator controller image..."
podman build -t "${REGISTRY}/vlan-traffic-control-operator:${NEW_TAG}" -f Dockerfile .
podman push "${REGISTRY}/vlan-traffic-control-operator:${NEW_TAG}"

echo "==> Step 4b: Building & pushing agent image..."
podman build -t "${REGISTRY}/vlan-traffic-control-agent:${NEW_TAG}" -f Dockerfile.agent .
podman push "${REGISTRY}/vlan-traffic-control-agent:${NEW_TAG}"

echo "==> Step 5: Building & pushing OLM bundle image..."
podman build -t "${REGISTRY}/vlan-traffic-control-bundle:${NEW_TAG}" -f bundle.Dockerfile .
podman push "${REGISTRY}/vlan-traffic-control-bundle:${NEW_TAG}"

# ------------------------------------------------------------------------------
# 6. Build File-Based Catalog & Render index.json
# ------------------------------------------------------------------------------
echo "==> Step 6: Rendering File-Based Catalog..."

rm -rf catalog catalog.Dockerfile
mkdir -p catalog

opm init vlan-traffic-control \
  --default-channel alpha \
  --output json | jq -c '.' > catalog/index.json

cat <<EOF >> catalog/index.json
{"schema":"olm.channel","package":"vlan-traffic-control","name":"alpha","entries":[{"name":"vlan-traffic-control.v${OLD_VERSION}"},{"name":"vlan-traffic-control.v${NEW_VERSION}","replaces":"vlan-traffic-control.v${OLD_VERSION}"}]}
EOF

opm render "${REGISTRY}/vlan-traffic-control-bundle:v${OLD_VERSION}" | jq -c 'select(.schema == "olm.bundle")' >> catalog/index.json
opm render "${REGISTRY}/vlan-traffic-control-bundle:${NEW_TAG}" | jq -c 'select(.schema == "olm.bundle")' >> catalog/index.json

# ------------------------------------------------------------------------------
# 7. Inject Icon directly into File-Based Catalog (FBC) base64 payload
# ------------------------------------------------------------------------------
echo "==> Step 7: Injecting icon into Catalog index via base64 payload patcher..."

python3 -c "
import json, base64

clean_b64 = '$CLEAN_BASE64_ICON'
icon_list = [{
    'base64data': clean_b64,
    'mediatype': 'image/svg+xml'
}]

output_lines = []
with open('catalog/index.json', 'r') as f:
    for line in f:
        line_str = line.strip()
        if not line_str:
            continue
        try:
            obj = json.loads(line_str)
            if obj.get('schema') == 'olm.bundle':
                props = obj.get('properties', [])
                for prop in props:
                    if prop.get('type') == 'olm.bundle.object' and 'value' in prop and 'data' in prop['value']:
                        csv_raw = base64.b64decode(prop['value']['data']).decode('utf-8')
                        csv_json = json.loads(csv_raw)
                        if csv_json.get('kind') == 'ClusterServiceVersion':
                            csv_json.setdefault('spec', {})['icon'] = icon_list
                            updated_csv_raw = json.dumps(csv_json, separators=(',', ':'))
                            prop['value']['data'] = base64.b64encode(updated_csv_raw.encode('utf-8')).decode('utf-8')
            output_lines.append(json.dumps(obj, separators=(',', ':')))
        except Exception:
            output_lines.append(line_str)

with open('catalog/index.json', 'w') as f:
    f.write('\n'.join(output_lines) + '\n')
"

# ------------------------------------------------------------------------------
# 7b. Post-flight Check: Verify Icon Integrity in catalog/index.json
# ------------------------------------------------------------------------------
echo "==> Step 7b: Verifying icon consistency in catalog/index.json..."

python3 -c "
import json, base64

expected_clean = ''.join('$CLEAN_BASE64_ICON'.split())
found_valid_icon = False

with open('catalog/index.json', 'r') as f:
    for line in f:
        line_str = line.strip()
        if not line_str:
            continue
        try:
            obj = json.loads(line_str)
            if obj.get('schema') == 'olm.bundle' and obj.get('name') == 'vlan-traffic-control.v${NEW_VERSION}':
                props = obj.get('properties', [])
                for prop in props:
                    if prop.get('type') == 'olm.bundle.object' and 'value' in prop and 'data' in prop['value']:
                        csv_raw = base64.b64decode(prop['value']['data']).decode('utf-8')
                        csv_json = json.loads(csv_raw)
                        if csv_json.get('kind') == 'ClusterServiceVersion':
                            icons = csv_json.get('spec', {}).get('icon', [])
                            if len(icons) > 0:
                                actual_b64 = ''.join(icons[0].get('base64data', '').split())
                                if actual_b64 == expected_clean:
                                    found_valid_icon = True
                                    break
        except Exception:
            pass

if not found_valid_icon:
    print('❌ ERROR: Icon consistency check failed! New CSV bundle in catalog/index.json does not match expected icon.')
    exit(1)
else:
    print('✅ Icon consistency check passed: catalog/index.json matches valid SVG Base64 icon.')
"

# ------------------------------------------------------------------------------
# 8. Build & Push Catalog Image
# ------------------------------------------------------------------------------
opm generate dockerfile catalog

echo "--> Building catalog container image..."
podman build -t "${REGISTRY}/vlan-traffic-control-catalog:${NEW_TAG}" -f catalog.Dockerfile .
podman push "${REGISTRY}/vlan-traffic-control-catalog:${NEW_TAG}"

echo "=================================================="
echo "🎉 Release v${NEW_VERSION} successfully built and published!"
echo "=================================================="
