GO ?= go
VERSION ?= dev
LDFLAGS := -s -w -buildid= -X main.version=$(VERSION)

.PHONY: build check test race vet format-check release-snapshot docker-build clean

build:
	CGO_ENABLED=0 $(GO) build -buildvcs=false -trimpath -ldflags '$(LDFLAGS)' -o cloudfs ./cmd/cloudfs

test:
	$(GO) test ./... -count=1 -timeout 300s

race:
	$(GO) test -race ./... -count=1 -timeout 300s

vet:
	$(GO) vet ./...

format-check:
	@test -z "$$(gofmt -l $$(find . -name '*.go' -not -path './vendor/*'))" || \
		(gofmt -l $$(find . -name '*.go' -not -path './vendor/*'); exit 1)

check: format-check vet test

release-snapshot:
	./scripts/release.sh $(VERSION)

docker-build:
	docker build --build-arg VERSION=$(VERSION) -t cloudfs:$(VERSION) .

clean:
	$(GO) clean
