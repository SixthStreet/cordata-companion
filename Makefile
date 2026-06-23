.PHONY: build release clean fmt vet test run

BIN := cordata-companion
VERSION := 0.4.0
LDFLAGS := -s -w -X main.version=$(VERSION)

build:
	go build -ldflags "$(LDFLAGS)" -o $(BIN) .

# Cross-compile every release artifact at once. Output lands under
# dist/ as `cordata-companion-<version>-<os>-<arch>` ready to ship via
# GitHub Releases. CGO disabled so each artifact is a pure static
# binary the user can drop on their HQP server with no runtime
# dependencies beyond ffmpeg.
release: clean
	mkdir -p dist
	GOOS=linux  GOARCH=amd64 CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o dist/$(BIN)-$(VERSION)-linux-amd64  .
	GOOS=linux  GOARCH=arm64 CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o dist/$(BIN)-$(VERSION)-linux-arm64  .
	GOOS=darwin GOARCH=amd64 CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o dist/$(BIN)-$(VERSION)-darwin-amd64 .
	GOOS=darwin GOARCH=arm64 CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o dist/$(BIN)-$(VERSION)-darwin-arm64 .
	GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o dist/$(BIN)-$(VERSION)-windows-amd64.exe .

clean:
	rm -rf dist/ $(BIN)

fmt:
	go fmt ./...

vet:
	go vet ./...

run: build
	./$(BIN)
