GO ?= go
BINARY := adbb
MAIN := ./src/cmd/adb-tcp-bridge
DIST_DIR := dist
RELEASE_FLAGS := -trimpath -ldflags "-s -w"

.DEFAULT_GOAL := build

.PHONY: fmt test build release release-cross \
	release-linux-amd64 release-linux-arm64 \
	release-darwin-amd64 release-darwin-arm64 \
	release-windows-amd64 release-windows-arm64

fmt:
	$(GO) fmt ./...

test:
	$(GO) test ./...

build:
	$(GO) build -o $(BINARY) $(MAIN)

release:
	$(GO) build $(RELEASE_FLAGS) -o $(BINARY) $(MAIN)

release-cross: release-linux-amd64 release-linux-arm64 release-darwin-amd64 release-darwin-arm64 release-windows-amd64 release-windows-arm64

$(DIST_DIR):
	mkdir -p $(DIST_DIR)

release-linux-amd64: | $(DIST_DIR)
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build $(RELEASE_FLAGS) -o $(DIST_DIR)/$(BINARY)-linux-amd64 $(MAIN)

release-linux-arm64: | $(DIST_DIR)
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 $(GO) build $(RELEASE_FLAGS) -o $(DIST_DIR)/$(BINARY)-linux-arm64 $(MAIN)

release-darwin-amd64: | $(DIST_DIR)
	CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 $(GO) build $(RELEASE_FLAGS) -o $(DIST_DIR)/$(BINARY)-darwin-amd64 $(MAIN)

release-darwin-arm64: | $(DIST_DIR)
	CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 $(GO) build $(RELEASE_FLAGS) -o $(DIST_DIR)/$(BINARY)-darwin-arm64 $(MAIN)

release-windows-amd64: | $(DIST_DIR)
	CGO_ENABLED=0 GOOS=windows GOARCH=amd64 $(GO) build $(RELEASE_FLAGS) -o $(DIST_DIR)/$(BINARY)-windows-amd64.exe $(MAIN)

release-windows-arm64: | $(DIST_DIR)
	CGO_ENABLED=0 GOOS=windows GOARCH=arm64 $(GO) build $(RELEASE_FLAGS) -o $(DIST_DIR)/$(BINARY)-windows-arm64.exe $(MAIN)
