BINARY := file-edit-mcp
VERSION ?= dev
GOFLAGS_BUILD := -trimpath -ldflags "-s -w -X main.version=$(VERSION)"

DIST := dist
LINUX_NAME := file-edit-mcp-linux-amd64
LINUX_BIN := $(DIST)/$(LINUX_NAME)

.PHONY: build test vet linux release clean

build:
	go build $(GOFLAGS_BUILD) -o $(BINARY) ./cmd/$(BINARY)

test:
	go test ./...

vet:
	go vet ./...

# Cross-compile the static linux/amd64 binary shipped via Dockerfile.prebuilt.
linux:
	mkdir -p $(DIST)
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
		go build $(GOFLAGS_BUILD) -o $(LINUX_BIN) ./cmd/$(BINARY)
	@file $(LINUX_BIN)
	@file $(LINUX_BIN) | grep -q 'x86-64' \
		|| { echo "ERROR: $(LINUX_BIN) is not an x86-64 binary" >&2; exit 1; }
	@file $(LINUX_BIN) | grep -q 'statically linked' \
		|| { echo "ERROR: $(LINUX_BIN) is not statically linked" >&2; exit 1; }

# Build the release image locally (scratch + prebuilt binary, Dockerfile.prebuilt)
# and push it to Docker Hub. Deploy hosts only ever pull.
REGISTRY ?= docker.io/myl7
release: linux
	docker buildx build --platform linux/amd64 -f Dockerfile.prebuilt \
		-t $(REGISTRY)/$(BINARY):$(VERSION) --load .
	docker push $(REGISTRY)/$(BINARY):$(VERSION)
	@echo '>> Pushed $(REGISTRY)/$(BINARY):$(VERSION). Bump the image tag wherever you deploy.'

clean:
	rm -f $(BINARY)
	rm -rf $(DIST)
