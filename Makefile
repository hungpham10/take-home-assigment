.PHONY: test lint build fmt tidy clean

test:
	go test ./... -race

lint-transitions:
	@./scripts/lint-transitions.sh

lint:
	golangci-lint run

build:
	go build -o bin/loadtest-controller ./cmds/loadtest-controller

fmt:
	go fmt ./...

tidy:
	go mod tidy

clean:
	rm -rf bin/
