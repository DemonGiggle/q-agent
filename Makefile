VERSION ?= $(shell sh ./scripts/version.sh)

.PHONY: build static-build release test race integration integration-host learning-acceptance
build:
	CGO_ENABLED=0 go build -trimpath -ldflags='-s -w -X main.version=$(VERSION)' -o bin/q-agent ./cmd/q-agent
# Explicit alias for the static binary (CGO_ENABLED=0 above), so release
# verification can request it by name: `make build static-build`.
static-build: build
release:
	VERSION=$(VERSION) sh ./scripts/build-all.sh
test:
	go test ./...
race:
	go test -race ./...
learning-acceptance:
	go test -count=1 -timeout 5m -v -run '^Test(Learning|RankQuestionAndConversationDedup|RankNoteKeywords)' \
		./internal/learning ./internal/state ./internal/runtime ./internal/llm ./internal/localtools ./cmd/q-agent
integration-host:
	go test -v ./internal/host ./cmd/q-agent -run Host -timeout 2m
integration: build
	Q_AGENT_INTEGRATION=1 Q_AGENT_WORKER=$(CURDIR)/bin/q-agent go test -v ./internal/sandbox ./cmd/q-agent -run Integration -timeout 10m
