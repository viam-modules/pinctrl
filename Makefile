TOOL_BIN = bin/gotools/$(shell uname -s)-$(shell uname -m)
EXECUTABLE_BIN = bin/$(shell uname -s)-$(shell uname -m)

build:
	go build -o "$(EXECUTABLE_BIN)/"

tool-install:
	GOBIN=`pwd`/$(TOOL_BIN) go install \
	github.com/edaniels/golinters/cmd/combined \
	github.com/golangci/golangci-lint/cmd/golangci-lint \
	github.com/rhysd/actionlint/cmd/actionlint

lint: tool-install
	go mod tidy
	$(TOOL_BIN)/golangci-lint run -v --fix --config=./etc/.golangci.yaml
