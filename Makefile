GOCMDS := $(notdir $(patsubst %/,%,$(dir $(shell find cmd -name 'main.go'))))

CLOUDSDK_CONFIG?=${HOME}/.config/gcloud
PROJECT_ID?=$(shell gcloud config get-value core/project)
GMP_CLUSTER?=gmp-test-cluster
GMP_LOCATION?=us-central1-c
API_DIR=pkg/operator/apis
# For now assume the docker daemon is mounted through a unix socket.
# TODO(pintohutch): will this work if using a remote docker over tcp?
DOCKER_HOST?=unix:///var/run/docker.sock
DOCKER_VOLUME:=$(DOCKER_HOST:unix://%=%)

TAG_NAME?=$(shell date "+gmp-%Y%d%m_%H%M")

define docker_build
	DOCKER_BUILDKIT=1 docker build $(1)
endef

help:        ## Show this help.
	@fgrep -h "##" $(MAKEFILE_LIST) | fgrep -v fgrep | sed -e 's/\\$$//' | sed -e 's/##//'

clean:       ## Clean build time resources, primarily docker resources.
	docker container prune -f
	docker volume prune -f
	for i in `docker image ls | grep ^gmp/ | awk '{print $$1}'`; do docker image rm -f $$i; done

lint:        ## Lint code.
	@echo ">> linting code"
	DOCKER_BUILDKIT=1 docker run --rm -v $(shell pwd):/app -w /app golangci/golangci-lint:v1.43.0 golangci-lint run -v --timeout=5m

cloudbuild:  ## Build images on Google Cloud Build.
	@echo ">> building GMP images on Cloud Build with tag: $(TAG_NAME)"
	gcloud builds submit --config build.yaml --timeout=30m --substitutions=TAG_NAME="$(TAG_NAME)"

$(GOCMDS):   ## Build go binary in cmd/ (e.g. 'operator').
             ## Set NO_DOCKER=1 env var to build natively without Docker.
	@echo ">> building binaries"
ifeq ($(NO_DOCKER), 1)
	if [ "$@" = "frontend" ]; then pkg/ui/build.sh; fi
	CGO_ENABLED=0 go build -tags builtinassets -mod=vendor -o ./build/bin/$@ ./cmd/$@/*.go
else
	$(call docker_build, --tag gmp/$@ -f ./cmd/$@/Dockerfile .)
	mkdir -p build/bin
	@echo ">> exporting built image to local 'build/' dir"
	printf 'FROM scratch\nCOPY --from=gmp/$@ /bin/$@ /$@' | $(call docker_build, -o ./build/bin -)
endif

bin:         ## Build all go binaries.
             ## Set NO_DOCKER=1 env var to build natively without Docker.
bin: $(GOCMDS)

regen:       ## Refresh autogenerated files and reformat code.
             ## Use DRY_RUN=1 to only validate without generating changes.
regen:
ifeq ($(DRY_RUN), 1)
	$(call docker_build, -f ./hack/Dockerfile --target hermetic -t gmp/hermetic \
		--build-arg RUNCMD='./hack/presubmit.sh all diff' .)
else
	$(call docker_build, -f ./hack/Dockerfile --target sync -o . -t gmp/sync \
		--build-arg RUNCMD='./hack/presubmit.sh' .)
	rm -rf vendor && mv vendor.tmp vendor
endif

test:        ## Run all tests. Setting NO_DOCKER=1 writes real data to GCM API under PROJECT_ID environment variable.
             ## Use GMP_CLUSTER, GMP_LOCATION to specify timeseries labels.
	@echo ">> running tests"
ifeq ($(NO_DOCKER), 1)
	kubectl apply -f manifests/setup.yaml
	kubectl apply -f cmd/operator/deploy/operator/01-priority-class.yaml
	kubectl apply -f cmd/operator/deploy/operator/03-role.yaml
	go test `go list ./... | grep -v operator/e2e | grep -v export/bench`
	go test `go list ./... | grep operator/e2e` -args -project-id=${PROJECT_ID} -cluster=${GMP_CLUSTER} -location=${GMP_LOCATION}
else
	$(call docker_build, -f ./hack/Dockerfile --target sync -o . -t gmp/hermetic \
		--build-arg RUNCMD='./hack/presubmit.sh test' .)
	rm -rf vendor.tmp
endif

kindtest:    ## Run e2e test suite against fresh kind k8s cluster.
	@echo ">> building image"
	$(call docker_build, -f hack/Dockerfile --target kindtest -t gmp/kindtest .)
	@echo ">> running container"
# We lose some isolation by sharing the host network with the kind containers.
# However, we avoid a gcloud-shell "Dockerception" and save on build times.
	docker run --network host --rm -v $(DOCKER_VOLUME):/var/run/docker.sock gmp/kindtest ./hack/kind-test.sh

presubmit:   ## Run all checks and tests before submitting a change 
             ## Use DRY_RUN=1 to only validate without regenerating changes.
presubmit: regen bin test kindtest
