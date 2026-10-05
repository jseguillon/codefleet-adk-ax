GO ?= go

.PHONY: build test demo verify clean
build:
	mkdir -p bin
	$(GO) build -buildvcs=false -trimpath -o bin/codefleet ./cmd/codefleet
	$(GO) build -buildvcs=false -trimpath -o bin/codefleet-worker ./cmd/codefleet-worker
	$(GO) build -buildvcs=false -trimpath -o bin/ax-task-runner ./cmd/ax-task-runner
test:
	$(GO) test -race ./...
demo: build
	./bin/codefleet demo
verify: build test
	python3 scripts/verify_demo.py
clean:
	rm -rf bin
