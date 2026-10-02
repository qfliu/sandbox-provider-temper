# Run from code/ after `source ../env.sh`. Build output goes to bin/ (D15).

BIN := bin

.PHONY: build test vet fmt check run-api clean

## build: compile all commands into bin/
build:
	go build -o $(BIN)/ ./cmd/...

## test: run unit tests
test:
	go test ./...

## vet: static checks
vet:
	go vet ./...

## fmt: format all Go code
fmt:
	gofmt -w .

## check: formatting, vet and tests (what a PR should pass)
check:
	@test -z "$$(gofmt -l .)" || { echo "needs gofmt:"; gofmt -l .; exit 1; }
	go vet ./...
	go test ./...

## run-api: build and run the API locally on :8080
run-api: build
	$(BIN)/api

clean:
	rm -rf $(BIN)
