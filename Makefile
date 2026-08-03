VERSION ?= 0.2.4
IMG_MANAGER ?= ghcr.io/rbruzzon73/vlan-traffic-control-manager:v$(VERSION)
IMG_AGENT ?= ghcr.io/rbruzzon73/vlan-traffic-control-agent:v$(VERSION)
BUNDLE_IMG ?= ghcr.io/rbruzzon73/vlan-traffic-control-bundle:v$(VERSION)
CATALOG_IMG ?= ghcr.io/rbruzzon73/vlan-traffic-control-catalog:v$(VERSION)

# Tooling configuration
LOCALBIN ?= $(shell pwd)/bin
CONTROLLER_GEN ?= $(LOCALBIN)/controller-gen
CONTROLLER_TOOLS_VERSION ?= v0.17.1

.PHONY: all build build-images push-images deploy generate manifests controller-gen catalog-build

all: generate manifests build

# --- Code Generation & Manifests ---

.PHONY: controller-gen
controller-gen: $(CONTROLLER_GEN) ## Download controller-gen locally if necessary.
$(CONTROLLER_GEN): $(LOCALBIN)
	test -s $(LOCALBIN)/controller-gen || GOBIN=$(LOCALBIN) go install sigs.k8s.io/controller-tools/cmd/controller-gen@$(CONTROLLER_TOOLS_VERSION)

$(LOCALBIN):
	mkdir -p $(LOCALBIN)

.PHONY: generate
generate: controller-gen ## Generate DeepCopy, DeepCopyInto, and DeepCopyObject code.
	$(CONTROLLER_GEN) object paths="./..."

.PHONY: manifests
manifests: controller-gen ## Generate CRD YAMLs and RBAC rules.
	$(CONTROLLER_GEN) crd:crdVersions=v1 paths="./api/..." output:crd:artifacts:config=config/crd/bases
	$(CONTROLLER_GEN) rbac:roleName=manager-role paths="./..." output:rbac:artifacts:config=config/rbac
	$(CONTROLLER_GEN) object paths="./..."

# --- Build Targets ---

build:
	go build -o bin/manager cmd/manager/main.go
	go build -o bin/agent cmd/agent/main.go

build-images:
	podman build --no-cache -t $(IMG_MANAGER) -f Dockerfile .
	podman build --no-cache -t $(IMG_AGENT) -f Dockerfile.agent .
	podman build --no-cache -t $(BUNDLE_IMG) -f bundle.Dockerfile .

push-images:
	podman push $(IMG_MANAGER)
	podman push $(IMG_AGENT)
	podman push $(BUNDLE_IMG)

catalog-build:
	rm -rf catalog catalog.Dockerfile
	mkdir -p catalog
	opm generate dockerfile catalog
	opm render $(BUNDLE_IMG) --output=yaml > catalog/operator.yaml
	printf -- "---\nschema: olm.package\nname: vlan-traffic-control\ndefaultChannel: alpha\n---\nschema: olm.channel\npackage: vlan-traffic-control\nname: alpha\nentries:\n  - name: vlan-traffic-control.v$(VERSION)\n    replaces: vlan-traffic-control.v0.2.3\n" >> catalog/operator.yaml
	podman build --no-cache -t $(CATALOG_IMG) -f catalog.Dockerfile .
	podman push $(CATALOG_IMG)

deploy: manifests
	oc apply -f config/crd/networking.med.io_vlantrafficcontrols.yaml
	oc delete pod -n openshift-marketplace -l olm.catalogSource=vlan-traffic-control-catalog --ignore-not-found
