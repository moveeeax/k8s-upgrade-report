BINARY := k8s-upgrade-report
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -X main.version=$(VERSION)

.PHONY: build test lint sample clean

build:
	go build -ldflags "$(LDFLAGS)" -o bin/$(BINARY) ./cmd/$(BINARY)

test:
	go test ./...

lint:
	gofmt -l . | tee /dev/stderr | (! read)
	go vet ./...

# Regenerates the published sample report from the deliberately broken fixture.
sample: build
	./bin/$(BINARY) scan \
		--manifests testdata/broken-cluster \
		--current 1.24 --target 1.29 --provider eks \
		--html docs/sample-report.html \
		--patch docs/sample-report.patch >/dev/null

clean:
	rm -rf bin dist
