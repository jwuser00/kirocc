export GOEXPERIMENT := jsonv2

BIN := dist/kirocc

.PHONY: build install run debug test test-e2e lint vet fmt fix clean \
	service-install service-uninstall service-restart service-status service-logs

build:
	go build -o $(BIN) ./cmd/kirocc

install:
	go install ./cmd/kirocc

run:
	go run ./cmd/kirocc $(ARGS)

debug:
	go run ./cmd/kirocc -debug $(ARGS)

test:
	go test -race ./...

test-e2e:
	go test -tags e2e -race -timeout 120s ./internal/e2e/

lint:
	golangci-lint run

vet:
	go vet ./...

fmt:
	golangci-lint fmt

fix:
	go fix ./...

clean:
	rm -f $(BIN)

# Background service (macOS launchd user agent).
# Use SHELL_ENV=1 to also write the Claude Code env vars to your shell rc.
service-install:
	./scripts/service.sh install $(if $(SHELL_ENV),--with-shell-env,)

service-uninstall:
	./scripts/service.sh uninstall

service-restart:
	./scripts/service.sh restart

service-status:
	./scripts/service.sh status

service-logs:
	./scripts/service.sh logs
