.PHONY: build test race integration
build:
	CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o bin/q-agent ./cmd/q-agent
test:
	go test ./...
race:
	go test -race ./...
integration: build
	Q_AGENT_INTEGRATION=1 Q_AGENT_WORKER=$(CURDIR)/bin/q-agent go test -v ./internal/sandbox ./cmd/q-agent -run Integration -timeout 10m
