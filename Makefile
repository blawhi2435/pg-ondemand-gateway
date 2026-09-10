MODULE  := github.com/blawhi2435/pg-ondemand-gateway
BIN     := bin/pg-proxy
IMAGE   := pg-proxy:dev
GOARCH  ?= arm64
GOOS    ?= linux

.PHONY: test lint build image

test:
	go test ./... -race

lint:
	go vet ./...
	gofmt -l .

build:
	CGO_ENABLED=0 GOOS=$(GOOS) GOARCH=$(GOARCH) go build -o $(BIN) ./cmd/pg-proxy

image:
	docker build -t $(IMAGE) .
