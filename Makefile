.PHONY: build build-all clean run

BINARY_NAME=twitchpoint
BUILD_DIR=bin

build:
	go build -o $(BUILD_DIR)/$(BINARY_NAME) ./cmd/twitchpoint

# Cross-compile for every supported platform/arch.
# Naming kept stable for existing release URLs:
#   twitchpoint-macos       = darwin/arm64 (Apple Silicon — primary)
#   twitchpoint-macos-intel = darwin/amd64 (Apple Intel)
#   twitchpoint-linux       = linux/amd64  (Intel/AMD x86_64)
#   twitchpoint-linux-arm64 = linux/arm64  (Raspberry Pi, ARM servers)
#   twitchpoint-windows.exe = windows/amd64
build-all:
	GOOS=darwin  GOARCH=arm64 go build -o $(BUILD_DIR)/$(BINARY_NAME)-macos       ./cmd/twitchpoint
	GOOS=darwin  GOARCH=amd64 go build -o $(BUILD_DIR)/$(BINARY_NAME)-macos-intel ./cmd/twitchpoint
	GOOS=linux   GOARCH=amd64 go build -o $(BUILD_DIR)/$(BINARY_NAME)-linux       ./cmd/twitchpoint
	GOOS=linux   GOARCH=arm64 go build -o $(BUILD_DIR)/$(BINARY_NAME)-linux-arm64 ./cmd/twitchpoint
	GOOS=windows GOARCH=amd64 go build -o $(BUILD_DIR)/$(BINARY_NAME)-windows.exe ./cmd/twitchpoint

run:
	go run ./cmd/twitchpoint

clean:
	rm -rf $(BUILD_DIR)
