VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X github.com/lyelgate/kdeck/internal/app.Version=$(VERSION)
PREFIX  ?= $(shell go env GOPATH)/bin
PLATFORMS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64

.PHONY: build test vet install dist clean

build:
	go build -ldflags "$(LDFLAGS)" -o bin/kdeck ./cmd/kdeck

test:
	go test ./...

vet:
	go vet ./...

install: build
	install -m 0755 bin/kdeck $(PREFIX)/kdeck
	@echo "Installed $(PREFIX)/kdeck. Next: eval \"\$$(kdeck shell-init zsh)\""

dist:
	@mkdir -p dist
	@for p in $(PLATFORMS); do \
		os=$${p%/*}; arch=$${p#*/}; ext=""; [ $$os = windows ] && ext=".exe"; \
		echo "building $$os/$$arch"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -trimpath -ldflags "$(LDFLAGS)" -o dist/kdeck-$$os-$$arch$$ext ./cmd/kdeck || exit 1; \
	done
	@cd dist && shasum -a 256 kdeck-* > SHA256SUMS

clean:
	rm -rf bin dist
