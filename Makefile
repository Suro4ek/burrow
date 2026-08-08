VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: all build web web-dev test race lint check clean server-linux agent-all install-agent

all: build

# The admin panel is compiled into the burrowd binary, so `make web` must run
# before any build that should ship an up-to-date panel.
web:
	npm --prefix web ci
	npm --prefix web run build

web-dev:
	npm --prefix web run dev

build:
	go build -ldflags "$(LDFLAGS)" -o bin/burrowd ./cmd/burrowd
	go build -ldflags "$(LDFLAGS)" -o bin/burrow ./cmd/burrow

test:
	go test ./...

race:
	go test -race ./...

lint:
	go vet ./...
	@unformatted=$$(gofmt -l .); \
	if [ -n "$$unformatted" ]; then echo "needs gofmt:"; echo "$$unformatted"; exit 1; fi

# Everything CI runs, so a green local check means a green pull request.
check: lint race
	npm --prefix web run typecheck

# The server only ever runs on the VPS.
server-linux: web
	GOOS=linux GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o bin/burrowd-linux-amd64 ./cmd/burrowd
	GOOS=linux GOARCH=arm64 go build -ldflags "$(LDFLAGS)" -o bin/burrowd-linux-arm64 ./cmd/burrowd

# The agent runs wherever you develop.
agent-all:
	GOOS=darwin GOARCH=arm64 go build -ldflags "$(LDFLAGS)" -o bin/burrow-darwin-arm64 ./cmd/burrow
	GOOS=darwin GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o bin/burrow-darwin-amd64 ./cmd/burrow
	GOOS=linux  GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o bin/burrow-linux-amd64 ./cmd/burrow
	GOOS=linux  GOARCH=arm64 go build -ldflags "$(LDFLAGS)" -o bin/burrow-linux-arm64 ./cmd/burrow

install-agent:
	go install -ldflags "$(LDFLAGS)" ./cmd/burrow

clean:
	rm -rf bin
