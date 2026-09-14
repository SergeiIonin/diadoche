BINARY  := diadoche
CMD     := ./cmd/diadoche
PKGS    := ./...

.PHONY: all
all: fmt vet test build

.PHONY: build
build:
	go build -trimpath -o $(BINARY) $(CMD)

.PHONY: run
run: build
	./$(BINARY) $(ARGS)

.PHONY: install
install:
	go install $(CMD)

.PHONY: test
test:
	go test $(PKGS)

.PHONY: test-verbose
test-verbose:
	go test -v $(PKGS)

.PHONY: cover
cover:
	go test -coverprofile=coverage.out $(PKGS)
	go tool cover -func=coverage.out

.PHONY: vet
vet:
	go vet $(PKGS)

.PHONY: fmt
fmt:
	gofmt -w .

.PHONY: fmt-check
fmt-check:
	@unformatted=$$(gofmt -l .); \
	if [ -n "$$unformatted" ]; then \
		echo "gofmt needed on:"; echo "$$unformatted"; exit 1; \
	fi

.PHONY: lint
lint:
	@if command -v golangci-lint >/dev/null 2>&1; then \
		golangci-lint run; \
	else \
		echo "golangci-lint not installed, skipping (see https://golangci-lint.run)"; \
	fi

.PHONY: tidy
tidy:
	go mod tidy

.PHONY: check
check: fmt-check vet test

.PHONY: clean
clean:
	rm -f $(BINARY) coverage.out

.PHONY: help
help:
	@echo "Targets:"
	@echo "  build        build the $(BINARY) binary"
	@echo "  run          build and run $(BINARY) (pass flags via ARGS=...)"
	@echo "  install      go install the $(BINARY) binary"
	@echo "  test         run tests"
	@echo "  test-verbose run tests with -v"
	@echo "  cover        run tests with coverage report"
	@echo "  vet          go vet"
	@echo "  fmt          gofmt -w ."
	@echo "  fmt-check    fail if any file is not gofmt-formatted"
	@echo "  lint         golangci-lint (if installed)"
	@echo "  tidy         go mod tidy"
	@echo "  check        fmt-check + vet + test"
	@echo "  clean        remove build artifacts"
	@echo "  all          fmt + vet + test + build (default)"
