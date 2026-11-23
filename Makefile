APP_NAME = vsno

vet:
	go vet ./...

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")

build:
	go build -ldflags "-X main.version=$(VERSION)" -o $(APP_NAME)

# Remove before copying: overwriting the file in place leaves macOS with a stale
# code signature for it, and the new binary gets killed (SIGKILL) when run.
INSTALL_DIR ?= $(if $(GOPATH),$(GOPATH),$(HOME)/go)/bin

install: build
	mkdir -p $(INSTALL_DIR)
	rm -f $(INSTALL_DIR)/$(APP_NAME)
	cp $(APP_NAME) $(INSTALL_DIR)/$(APP_NAME)

lint:
	golangci-lint run ./...

test:
	go test ./... -v

# Runs the built binary as a real process and asserts on its exit code, stdout
# and stderr. No cluster: a fake API server stands in for one. Also runs under
# `make test`, since it is an ordinary package.
integration-test:
	go test ./test/integration/... -v -count=1

coverage:
	go test ./... -coverprofile=coverage.out
	go tool cover -func=coverage.out

coverage-html: coverage
	go tool cover -html=coverage.out -o coverage.html

fmt:
	gofmt -w .
	goimports -w .

clean:
	rm -f $(APP_NAME) coverage.out coverage.html

.PHONY: vet build install lint test integration-test coverage coverage-html fmt clean
