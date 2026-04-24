.PHONY: build run test clean

build:
	go build -trimpath -ldflags="-s -w" -o bin/agent-wecom-bridge ./cmd/bridge

run:
	go run ./cmd/bridge

test:
	go test ./...

clean:
	go clean
	rm -rf ./bin ./dist
