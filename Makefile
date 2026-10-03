APP     := GHOSTWIRE
VERSION ?= $(shell git describe --tags --always 2>/dev/null || echo 0.1.0)
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: build linux-amd64 linux-arm64 linux-arm test dev clean

build:
	go build -ldflags "$(LDFLAGS)" -o $(APP) .

linux-amd64:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "$(LDFLAGS)" -o dist/amd64/$(APP) .

linux-arm64:
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags "$(LDFLAGS)" -o dist/arm64/$(APP) .

# Raspberry Pi OS 32-bit
linux-arm:
	CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7 go build -trimpath -ldflags "$(LDFLAGS)" -o dist/armv7/$(APP) .

test:
	go vet ./...
	GOOS=linux go vet ./...
	go test ./...

# Runs locally with the traffic simulator (non-Linux) on http://127.0.0.1:8080
dev: build
	mkdir -p dev
	test -f dev/config.json || echo '{"web":{"listen":"127.0.0.1:8080","tls":{"mode":"off"}},"server":{"endpoint":"vpn.example.net"}}' > dev/config.json
	./$(APP) -config dev/config.json

clean:
	rm -rf $(APP) dist dev
