.PHONY: build test test-go test-rust test-typescript test-browser typescript-deps fmt

build:
	mkdir -p dist
	GOTOOLCHAIN=local python3 scripts/wasm-overlay.py
	cd engine && GOTOOLCHAIN=local GOOS=wasip1 GOARCH=wasm CGO_ENABLED=0 go build -modfile=../dist/wasm.mod -trimpath -buildmode=c-shared -o ../dist/wasmfga.wasm ./cmd/wasmfga

test: test-go test-rust test-typescript test-browser

test-go: build
	cd engine && GOTOOLCHAIN=local go test ./...
	GOTOOLCHAIN=local go test ./...

test-rust: build
	cargo test --manifest-path sdk/rust/Cargo.toml --locked

typescript-deps:
	cd sdk/typescript && npm ci

test-typescript: build typescript-deps
	cd sdk/typescript && npm test

test-browser: build typescript-deps
	cd sdk/typescript && npm run test:browser

fmt:
	gofmt -w engine sdk/go tests
	cargo fmt --manifest-path sdk/rust/Cargo.toml
