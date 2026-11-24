SHELL := /bin/bash
LDFLAGS :=

# Options for 'bundle-build'
ifneq ($(origin CHANNELS), undefined)
BUNDLE_CHANNELS := --channels=$(CHANNELS)
endif
ifneq ($(origin DEFAULT_CHANNEL), undefined)
BUNDLE_DEFAULT_CHANNEL := --default-channel=$(DEFAULT_CHANNEL)
endif
BUNDLE_METADATA_OPTS ?= $(BUNDLE_CHANNELS) $(BUNDLE_DEFAULT_CHANNEL)

# Generate apiextensions.k8s.io/v1 CRDs.
CRD_OPTIONS ?= crd

# Default to Openshift for the platform, which means for example that the Antrea
# image will default to antrea-ubi in generate manifests. To use the operator
# with a "vanilla" (non-Openshift) K8s cluster, you will need to explicitly set
# ANTREA_PLATFORM=kubernetes when generating resources.
ifndef ANTREA_PLATFORM
	ANTREA_PLATFORM=openshift
endif

ifndef IS_CERTIFICATION
	IS_CERTIFICATION=false
endif

.PHONY: all
all: generate golangci manager

include versioning.mk
LDFLAGS += $(VERSION_LDFLAGS)

# Get the currently used golang install path (in GOPATH/bin, unless GOBIN is set)
ifeq (,$(shell go env GOBIN))
GOBIN=$(shell go env GOPATH)/bin
else
GOBIN=$(shell go env GOBIN)
endif

# Options for "packagemanifests".
ifneq ($(origin FROM_VERSION), undefined)
PKG_FROM_VERSION := --from-version=$(FROM_VERSION)
endif
ifneq ($(origin CHANNEL), undefined)
PKG_CHANNELS := --channel=$(CHANNEL)
endif
ifeq ($(IS_CHANNEL_DEFAULT), 1)
PKG_IS_DEFAULT_CHANNEL := --default-channel
endif
PKG_MAN_OPTS ?= $(FROM_VERSION) $(PKG_CHANNELS) $(PKG_IS_DEFAULT_CHANNEL)

GOLANGCI_LINT_VERSION := v2.11.4
GOLANGCI_LINT_BINDIR  := $(CURDIR)/.golangci-bin
GOLANGCI_LINT_BIN     := $(GOLANGCI_LINT_BINDIR)/$(GOLANGCI_LINT_VERSION)/golangci-lint

$(GOLANGCI_LINT_BIN):
	@echo "===> Installing Golangci-lint <==="
	@curl -sSfL https://raw.githubusercontent.com/golangci/golangci-lint/$(GOLANGCI_LINT_VERSION)/install.sh | sh -s -- -b $(GOLANGCI_LINT_BINDIR)/$(GOLANGCI_LINT_VERSION) $(GOLANGCI_LINT_VERSION)

.PHONY: golangci
golangci: $(GOLANGCI_LINT_BIN)
	@echo "===> Running golangci <==="
	@GOOS=linux $(GOLANGCI_LINT_BIN) run -c $(CURDIR)/.golangci.yml

.PHONY: golangci-fix
golangci-fix: $(GOLANGCI_LINT_BIN)
	@echo "===> Running golangci-fix <==="
	@GOOS=linux $(GOLANGCI_LINT_BIN) run -c $(CURDIR)/.golangci.yml --fix

# Unit tests render the checked-in operand manifests.
.PHONY: test
test:
	go test ./... -coverprofile cover.out

# Build manager binary
manager:
	@echo "===> Building antrea-operator binary <==="
	go build -o bin/manager -ldflags '$(LDFLAGS)' main.go

# Run against the configured Kubernetes cluster in ~/.kube/config
run: generate golangci manifests
	go run -ldflags '$(LDFLAGS)' ./main.go

# Install CRDs into a cluster
install: manifests kustomize
	$(KUSTOMIZE) build config/crd | kubectl apply -f -

# Uninstall CRDs from a cluster
uninstall: manifests kustomize
	$(KUSTOMIZE) build config/crd | kubectl delete -f -

# Deploy controller in the configured Kubernetes cluster in ~/.kube/config
deploy: manifests kustomize
	cd config/manager && $(KUSTOMIZE) edit set image controller=${IMG}
	$(KUSTOMIZE) build config/default | kubectl apply -f -

# Generate operator API artifacts independently of the operand resources.
.PHONY: manifests crds generate
manifests: crds antrea-resources
	$(CONTROLLER_GEN) rbac:roleName=antrea-operator webhook paths="./controllers/..." output:rbac:artifacts:config=config/rbac

crds: controller-gen kustomize
	$(CONTROLLER_GEN) $(CRD_OPTIONS) paths="./api/..." output:crd:artifacts:config=config/crd/bases
	$(KUSTOMIZE) build config/crd > config/crd/operator.antrea.vmware.com_antreainstalls.yaml
	cp config/crd/operator.antrea.vmware.com_antreainstalls.yaml bundle/manifests/
	cp config/crd/operator.antrea.vmware.com_antreainstalls.yaml deploy/kubernetes/operator.antrea.vmware.com_antreainstalls_crd.yaml
	cp config/crd/operator.antrea.vmware.com_antreainstalls.yaml deploy/openshift/operator.antrea.vmware.com_antreainstalls_crd.yaml

generate: controller-gen
	$(CONTROLLER_GEN) object:headerFile="hack/boilerplate.go.txt" paths="./api/..."

# Build the docker image
docker-build:
	docker build -f build/Dockerfile --label version="$(VERSION)" . -t ${IMG}
	docker tag ${IMG} antrea/antrea-operator

CONTROLLER_GEN_VERSION := v0.20.1
CONTROLLER_GEN_BINDIR  := $(CURDIR)/.controller-gen
CONTROLLER_GEN         := $(CONTROLLER_GEN_BINDIR)/$(CONTROLLER_GEN_VERSION)/controller-gen

$(CONTROLLER_GEN):
	@echo "===> Installing Controller-gen  <==="
	GOBIN=$(CONTROLLER_GEN_BINDIR)/$(CONTROLLER_GEN_VERSION) go install sigs.k8s.io/controller-tools/cmd/controller-gen@$(CONTROLLER_GEN_VERSION)

.PHONY: controller-gen
controller-gen: $(CONTROLLER_GEN)

KUSTOMIZE_VERSION := 5.3.0
KUSTOMIZE_BINDIR  := $(CURDIR)/.kustomize
KUSTOMIZE         := $(KUSTOMIZE_BINDIR)/$(KUSTOMIZE_VERSION)/kustomize

$(KUSTOMIZE):
	@echo "===> Installing Kustomize <==="
	GOBIN=$(KUSTOMIZE_BINDIR)/$(KUSTOMIZE_VERSION) go install sigs.k8s.io/kustomize/kustomize/v5@v$(KUSTOMIZE_VERSION)

.PHONY: kustomize
kustomize: $(KUSTOMIZE)

# Generate bundle manifests and metadata, then validate generated files.
.PHONY: bundle
bundle: manifests kustomize
	operator-sdk generate kustomize manifests -q
	# OCP requires that an image will be identified by its digest hash
	if [ "$(IS_CERTIFICATION)" == "true" ]; then \
	        $(KUSTOMIZE) build config/manifests | operator-sdk generate bundle -q --use-image-digests --overwrite $(BUNDLE_METADATA_OPTS) --version $(VERSION) ; \
	else \
		pushd config/manager && $(KUSTOMIZE) edit set image antrea/antrea-operator:v$(VERSION) && popd;\
	        $(KUSTOMIZE) build config/manifests | operator-sdk generate bundle -q --overwrite $(BUNDLE_METADATA_OPTS) --version $(VERSION) ; \
	fi
	operator-sdk bundle validate ./bundle
	cp config/samples/operator_v1_antreainstall.yaml ./deploy/$(ANTREA_PLATFORM)/operator.antrea.vmware.com_v1_antreainstall_cr.yaml
	cp config/crd/operator.antrea.vmware.com_antreainstalls.yaml deploy/$(ANTREA_PLATFORM)/operator.antrea.vmware.com_antreainstalls_crd.yaml

.PHONY: ocpbundle
ocpbundle: ANTREA_PLATFORM=openshift
ocpbundle: bundle
	./hack/edit_bundle_metadata_ocp.sh

ocpcertification: IS_CERTIFICATION=true
ocpcertification: ocpbundle

# Build the bundle image.
.PHONY: bundle-build
bundle-build:
	docker build -f bundle.Dockerfile -t $(BUNDLE_IMG) .
	docker tag ${BUNDLE_IMG} antrea/antrea-operator-bundle

.PHONY: antrea-resources
antrea-resources: kustomize
	KUSTOMIZE=$(KUSTOMIZE) ./hack/generate-antrea-resources.sh --platform $(ANTREA_PLATFORM) --version $(VERSION)
	cp ./config/rbac/role.yaml ./deploy/$(ANTREA_PLATFORM)/role.yaml
	cp ./config/samples/operator_v1_antreainstall.yaml ./deploy/$(ANTREA_PLATFORM)/operator.antrea.vmware.com_v1_antreainstall_cr.yaml

# Generate package manifests.
packagemanifests: kustomize manifests
	operator-sdk generate kustomize manifests -q
	cd config/manager && $(KUSTOMIZE) edit set image antrea-operator=$(IMG)
	$(KUSTOMIZE) build config/manifests | operator-sdk generate packagemanifests -q --version $(VERSION) $(PKG_MAN_OPTS)

test-tidy:
	@echo
	@echo "===> Checking go.mod tidiness <==="
	./hack/tidy-check.sh

.PHONY: tidy
tidy:
	go mod tidy

.PHONY: bin
bin: manager

.PHONY: clean
clean:
	@rm -rf bin
	@rm -rf $(GOLANGCI_LINT_BINDIR)
	@rm -rf $(KUSTOMIZE_BINDIR)
	@rm -rf $(CONTROLLER_GEN_BINDIR)
