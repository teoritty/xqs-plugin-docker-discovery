# xqs-plugin-docker-discovery
#
# CGO is off everywhere: the plugin must be a single static binary the host can copy into its data
# directory and run, on a machine that may have no toolchain at all.
BINARY := xqs-docker
BUILD_FLAGS := -trimpath -ldflags="-s -w"
export CGO_ENABLED := 0

.PHONY: all build build-all test vet fmt stage pack clean

all: vet test build

build:
	go build $(BUILD_FLAGS) -o dist/bin/$(BINARY)$(shell go env GOEXE) ./cmd/xqs-docker

# Four platforms, matching the host's own release matrix.
build-all:
	GOOS=windows GOARCH=amd64 go build $(BUILD_FLAGS) -o dist/bin/windows-amd64/$(BINARY).exe ./cmd/xqs-docker
	GOOS=linux   GOARCH=amd64 go build $(BUILD_FLAGS) -o dist/bin/linux-amd64/$(BINARY) ./cmd/xqs-docker
	GOOS=darwin  GOARCH=amd64 go build $(BUILD_FLAGS) -o dist/bin/darwin-amd64/$(BINARY) ./cmd/xqs-docker
	GOOS=darwin  GOARCH=arm64 go build $(BUILD_FLAGS) -o dist/bin/darwin-arm64/$(BINARY) ./cmd/xqs-docker

test:
	go test ./... -count=1

race:
	go test ./... -count=1 -race

vet:
	go vet ./...

fmt:
	gofmt -w .

# stage assembles what a bundle contains for one platform. The manifest's engine.entry names the
# Windows binary, so a non-Windows bundle rewrites it — the host refuses an entry that is not there.
stage: build-all
	@mkdir -p dist/stage/windows-amd64/ui dist/stage/linux-amd64/ui dist/stage/darwin-amd64/ui dist/stage/darwin-arm64/ui
	@for target in windows-amd64 linux-amd64 darwin-amd64 darwin-arm64; do \
		cp -r ui/icons dist/stage/$$target/ui/; \
	done
	cp plugin.json dist/stage/windows-amd64/plugin.json
	cp dist/bin/windows-amd64/$(BINARY).exe dist/stage/windows-amd64/$(BINARY).exe
	@for target in linux-amd64 darwin-amd64 darwin-arm64; do \
		sed 's/"entry": "$(BINARY).exe"/"entry": "$(BINARY)"/' plugin.json > dist/stage/$$target/plugin.json; \
		cp dist/bin/$$target/$(BINARY) dist/stage/$$target/$(BINARY); \
	done

pack: stage
	go run ./cmd/xqs-docker-pack -src dist/stage/windows-amd64 -out dist/$(BINARY)-windows-amd64.xqsp
	go run ./cmd/xqs-docker-pack -src dist/stage/linux-amd64   -out dist/$(BINARY)-linux-amd64.xqsp
	go run ./cmd/xqs-docker-pack -src dist/stage/darwin-amd64  -out dist/$(BINARY)-darwin-amd64.xqsp
	go run ./cmd/xqs-docker-pack -src dist/stage/darwin-arm64  -out dist/$(BINARY)-darwin-arm64.xqsp

clean:
	rm -rf dist
