# butterstack-connector (issue #1575 spike)
#
# Go is run in a container by default so no host toolchain is required.
# Set GO=go to use a host Go instead.

GO          ?= docker
GO_IMAGE    ?= golang:1.23-alpine
RUBY        ?= ruby
BIN         := build/butterstack-connector
VERSION     ?= 0.1.0-spike

ifeq ($(GO),docker)
GORUN = docker run --rm -v "$(CURDIR)":/w -w /w \
	-v /tmp/butterstack-connector-gocache:/root/.cache/go-build \
	-v /tmp/butterstack-connector-gomod:/go/pkg/mod $(GO_IMAGE) sh -c
else
GORUN = sh -c
endif

.PHONY: all build test drills check vocabulary fmt clean

all: build

build:
	@mkdir -p build
	$(GORUN) 'go build -ldflags "-X main.Version=$(VERSION)" -o $(BIN) ./cmd/butterstack-connector'
	@echo "built $(BIN)"

fmt:
	$(GORUN) 'gofmt -l -w .'

test:
	$(GORUN) 'gofmt -l . && go vet ./... && go test ./...'

# The seven drills from the design note section 4.3, against the mock broker.
drills: build
	CONNECTOR_BIN=$(CURDIR)/$(BIN) $(RUBY) test/drills.rb

check: test drills

vocabulary: build
	@$(BIN) -print-vocabulary

clean:
	rm -rf build
