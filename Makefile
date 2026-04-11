BINARY := eam-collector
VERSION := 0.1.0
BUILD := $(shell git rev-parse --short HEAD 2>/dev/null || echo "dev")
LDFLAGS := -s -w -X main.version=$(VERSION) -X main.build=$(BUILD)
OUT := dist

.PHONY: build build-all clean test

build:
	go build -ldflags "$(LDFLAGS)" -o $(OUT)/$(BINARY) ./cmd/eam-collector/

build-all: clean
	GOOS=darwin  GOARCH=arm64 go build -ldflags "$(LDFLAGS)" -o $(OUT)/$(BINARY)-darwin-arm64  ./cmd/eam-collector/
	GOOS=darwin  GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o $(OUT)/$(BINARY)-darwin-amd64  ./cmd/eam-collector/
	GOOS=linux   GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o $(OUT)/$(BINARY)-linux-amd64   ./cmd/eam-collector/
	GOOS=windows GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o $(OUT)/$(BINARY)-windows-amd64.exe ./cmd/eam-collector/

clean:
	rm -rf $(OUT)

test:
	go test ./...
