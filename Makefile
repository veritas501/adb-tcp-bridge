GO      ?= go
MODULE  := adb-tcp-bridge
BIN     := atb
CMD     := ./src/cmd/adb-tcp-bridge
DIST_DIR := dist

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

# 注入 internal/version 包变量（勿注入 main，便于 CLI 与后续复用入口共享）。
LDFLAGS         := -X $(MODULE)/src/internal/version.Version=$(VERSION) \
                   -X $(MODULE)/src/internal/version.Commit=$(COMMIT) \
                   -X $(MODULE)/src/internal/version.BuildDate=$(DATE)
RELEASE_LDFLAGS := -s -w $(LDFLAGS)

.DEFAULT_GOAL := build

.PHONY: build run version tidy fmt test clean release release-cross \
	release-linux-amd64 release-linux-arm64 \
	release-darwin-amd64 release-darwin-arm64 \
	release-windows-amd64 release-windows-arm64

build:
	$(GO) build -ldflags "$(LDFLAGS)" -o $(BIN) $(CMD)

run: build
	./$(BIN) version

version: build
	./$(BIN) version

tidy:
	$(GO) mod tidy

fmt:
	gofmt -w ./src/cmd ./src/internal

test:
	$(GO) test ./...

clean:
	rm -rf $(BIN) $(DIST_DIR)

release:
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags "$(RELEASE_LDFLAGS)" -o $(BIN) $(CMD)

release-cross: release-linux-amd64 release-linux-arm64 release-darwin-amd64 release-darwin-arm64 release-windows-amd64 release-windows-arm64

$(DIST_DIR):
	mkdir -p $(DIST_DIR)

release-linux-amd64: | $(DIST_DIR)
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build -trimpath -ldflags "$(RELEASE_LDFLAGS)" -o $(DIST_DIR)/$(BIN)-linux-amd64 $(CMD)

release-linux-arm64: | $(DIST_DIR)
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 $(GO) build -trimpath -ldflags "$(RELEASE_LDFLAGS)" -o $(DIST_DIR)/$(BIN)-linux-arm64 $(CMD)

release-darwin-amd64: | $(DIST_DIR)
	CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 $(GO) build -trimpath -ldflags "$(RELEASE_LDFLAGS)" -o $(DIST_DIR)/$(BIN)-darwin-amd64 $(CMD)

release-darwin-arm64: | $(DIST_DIR)
	CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 $(GO) build -trimpath -ldflags "$(RELEASE_LDFLAGS)" -o $(DIST_DIR)/$(BIN)-darwin-arm64 $(CMD)

release-windows-amd64: | $(DIST_DIR)
	CGO_ENABLED=0 GOOS=windows GOARCH=amd64 $(GO) build -trimpath -ldflags "$(RELEASE_LDFLAGS)" -o $(DIST_DIR)/$(BIN)-windows-amd64.exe $(CMD)

release-windows-arm64: | $(DIST_DIR)
	CGO_ENABLED=0 GOOS=windows GOARCH=arm64 $(GO) build -trimpath -ldflags "$(RELEASE_LDFLAGS)" -o $(DIST_DIR)/$(BIN)-windows-arm64.exe $(CMD)
