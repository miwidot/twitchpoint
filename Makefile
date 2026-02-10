.PHONY: build build-all clean run

BINARY_NAME=twitchpoint
BUILD_DIR=bin

build:
	go build -o $(BUILD_DIR)/$(BINARY_NAME) ./cmd/twitchpoint

build-all:
	GOOS=darwin  GOARCH=arm64 go build -o $(BUILD_DIR)/$(BINARY_NAME)-macos       ./cmd/twitchpoint
	GOOS=linux   GOARCH=amd64 go build -o $(BUILD_DIR)/$(BINARY_NAME)-linux       ./cmd/twitchpoint
	GOOS=windows GOARCH=amd64 go build -o $(BUILD_DIR)/$(BINARY_NAME)-windows.exe ./cmd/twitchpoint

run:
	go run ./cmd/twitchpoint

clean:
	rm -rf $(BUILD_DIR)
