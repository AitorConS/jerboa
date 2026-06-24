BINARY_UNI   := jerboa
BINARY_UNID  := jerboad
BUILD_DIR    := dist
VERSION      ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo "dev")
LDFLAGS      := -ldflags="-s -w -X main.version=$(VERSION)"

.PHONY: build kernel test test-integration test-kernel lint tidy-check e2e smoke coverage clean

build:
	go build $(LDFLAGS) -o $(BUILD_DIR)/$(BINARY_UNI)  ./cmd/jerboa
	go build $(LDFLAGS) -o $(BUILD_DIR)/$(BINARY_UNID) ./cmd/jerboad

kernel:
	$(MAKE) -C kernel all

test:
	go test $(shell go list ./... | grep -v '/kernel/') -race -coverprofile=coverage.out -covermode=atomic

test-integration:
	go test -tags integration -timeout 10m ./tests/integration/...

test-kernel:
	$(MAKE) -C kernel/test/unit test

lint:
	golangci-lint run ./...

tidy-check:
	go mod tidy
	git diff --exit-code go.mod go.sum || (echo "go.mod/go.sum out of sync — commit the result of 'go mod tidy'" && exit 1)

e2e:
	go test -tags e2e -timeout 30m ./tests/e2e/...

smoke:
	go build $(LDFLAGS) -o $(BUILD_DIR)/$(BINARY_UNI) ./cmd/jerboa
	go build $(LDFLAGS) -o $(BUILD_DIR)/uni-smoke ./cmd/uni-smoke
	./$(BUILD_DIR)/uni-smoke --jerboa ./$(BUILD_DIR)/$(BINARY_UNI)

coverage: test
	go tool cover -html=coverage.out -o coverage.html
	@echo "Coverage report: coverage.html"

clean:
	rm -rf $(BUILD_DIR) coverage.out coverage.html
	$(MAKE) -C kernel clean
