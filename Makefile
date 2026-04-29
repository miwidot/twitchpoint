.PHONY: build build-all build-debug clean run

BINARY_NAME=twitchpoint
BUILD_DIR=bin

# Release linker flags:
#   -s strips the symbol table
#   -w strips the DWARF debug-info table
# Combined they shave ~25-30% off the binary size with zero behavior
# change. Stack traces from panics still resolve to function names
# (Go embeds those separately) but won't show file:line numbers.
RELEASE_LDFLAGS=-s -w

# `make build` — current platform, release-flavored (stripped, no debug tag).
build:
	go build -ldflags="$(RELEASE_LDFLAGS)" -o $(BUILD_DIR)/$(BINARY_NAME) ./cmd/twitchpoint

# `make build-debug` — current platform, with -tags=debug (UI shows
# every prober/heartbeat tick, file:line in stack traces). For
# diagnosing pick / credit issues; never used for releases.
build-debug:
	go build -tags=debug -o $(BUILD_DIR)/$(BINARY_NAME)-debug ./cmd/twitchpoint

# `make build-all` — cross-compile every release target. ALL targets
# are stripped + tag-free. Naming kept stable for existing release URLs:
#   twitchpoint-macos       = darwin/arm64 (Apple Silicon — primary)
#   twitchpoint-macos-intel = darwin/amd64 (Apple Intel)
#   twitchpoint-linux       = linux/amd64  (Intel/AMD x86_64)
#   twitchpoint-linux-arm64 = linux/arm64  (Raspberry Pi, ARM servers)
#   twitchpoint-windows.exe = windows/amd64
build-all:
	GOOS=darwin  GOARCH=arm64 go build -ldflags="$(RELEASE_LDFLAGS)" -o $(BUILD_DIR)/$(BINARY_NAME)-macos       ./cmd/twitchpoint
	GOOS=darwin  GOARCH=amd64 go build -ldflags="$(RELEASE_LDFLAGS)" -o $(BUILD_DIR)/$(BINARY_NAME)-macos-intel ./cmd/twitchpoint
	GOOS=linux   GOARCH=amd64 go build -ldflags="$(RELEASE_LDFLAGS)" -o $(BUILD_DIR)/$(BINARY_NAME)-linux       ./cmd/twitchpoint
	GOOS=linux   GOARCH=arm64 go build -ldflags="$(RELEASE_LDFLAGS)" -o $(BUILD_DIR)/$(BINARY_NAME)-linux-arm64 ./cmd/twitchpoint
	GOOS=windows GOARCH=amd64 go build -ldflags="$(RELEASE_LDFLAGS)" -o $(BUILD_DIR)/$(BINARY_NAME)-windows.exe ./cmd/twitchpoint

run:
	go run ./cmd/twitchpoint

clean:
	rm -rf $(BUILD_DIR)
