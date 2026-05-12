# mini-docker Makefile

BIN_DIR := bin
MYDOCKER     := $(BIN_DIR)/mydocker
MYDOCKER_CRI := $(BIN_DIR)/mydocker-cri

GOFLAGS := -trimpath
LDFLAGS := -s -w

.PHONY: all build build-linux build-linux-arm64 clean tidy vet test

all: build

## 本机构建（本机是 Linux 时才能运行产物）
build:
	@mkdir -p $(BIN_DIR)
	go build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $(MYDOCKER)     ./cmd/mydocker
	go build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $(MYDOCKER_CRI) ./cmd/mydocker-cri

## 从任意平台交叉编译到 linux/amd64
build-linux:
	@mkdir -p $(BIN_DIR)
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 \
		go build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $(MYDOCKER)     ./cmd/mydocker
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 \
		go build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $(MYDOCKER_CRI) ./cmd/mydocker-cri

## 交叉编译到 linux/arm64
build-linux-arm64:
	@mkdir -p $(BIN_DIR)
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 \
		go build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $(MYDOCKER)     ./cmd/mydocker
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 \
		go build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $(MYDOCKER_CRI) ./cmd/mydocker-cri

tidy:
	go mod tidy

vet:
	go vet ./...

test:
	go test ./...

clean:
	rm -rf $(BIN_DIR)
