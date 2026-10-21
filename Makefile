.PHONY: build test test-go test-rust test-typescript test-browser typescript-deps fmt build-go build-rust test-engines benchmark benchmark-gate conformance build-native benchmark-native test-native

build: build-go build-rust

build-native:
	mkdir -p dist/native
	GOTOOLCHAIN=local go build -buildvcs=false -trimpath -ldflags="-s -w" -o dist/native/go-bench ./benchmarks/native/go
	cargo build --manifest-path benchmarks/native/rust/Cargo.toml --locked --release
	cp benchmarks/native/rust/target/release/wasmfga-native-bench dist/native/rust-bench
	node benchmarks/native/fixtures.mjs

test-native: build-native
	cd engine && GOTOOLCHAIN=local go test ./...
	cargo test --manifest-path engine-rust/Cargo.toml --features sqlite --locked
	WASMFGA_NATIVE=go GOTOOLCHAIN=local go test ./tests -count=1
	WASMFGA_NATIVE=upstream GOTOOLCHAIN=local go test ./tests -count=1
	WASMFGA_NATIVE=rust GOTOOLCHAIN=local go test ./tests -count=1

benchmark-native: test-native
	node benchmarks/native/gate.mjs
	node benchmarks/native/run.mjs
	node benchmarks/native/report.mjs

# Strict: includes known gaps and intentionally fails for the current Rust guest.
conformance: build
	python3 scripts/conformance.py

build-go:
	mkdir -p dist
	GOTOOLCHAIN=local python3 scripts/wasm-overlay.py
	cd engine && GOTOOLCHAIN=local GOOS=wasip1 GOARCH=wasm CGO_ENABLED=0 go build -ldflags="-s -w" -modfile=../dist/wasm.mod -trimpath -buildmode=c-shared -o ../dist/wasmfga.wasm ./cmd/wasmfga

build-rust:
	mkdir -p dist
	cargo build --manifest-path engine-rust/Cargo.toml --locked --release --target wasm32-wasip1
	cp engine-rust/target/wasm32-wasip1/release/wasmfga_engine.wasm dist/wasmfga-rust.wasm

benchmark-gate: build typescript-deps
	node benchmarks/gate.mjs

benchmark: benchmark-gate
	node benchmarks/run.mjs
	node benchmarks/report.mjs

test-engines: build
	cd engine && GOTOOLCHAIN=local go test ./...
	cargo test --manifest-path engine-rust/Cargo.toml --locked
	GOTOOLCHAIN=local go test ./...
	WASMFGA_WASM=$(CURDIR)/dist/wasmfga-rust.wasm GOTOOLCHAIN=local go test ./... -count=1

test: test-engines test-rust test-typescript test-browser

test-go: build
	cd engine && GOTOOLCHAIN=local go test ./...
	GOTOOLCHAIN=local go test ./...

test-rust: build
	cargo test --manifest-path sdk/rust/Cargo.toml --locked
	WASMFGA_WASM=$(CURDIR)/dist/wasmfga-rust.wasm cargo test --manifest-path sdk/rust/Cargo.toml --locked

typescript-deps:
	cd sdk/typescript && npm ci

test-typescript: build typescript-deps
	cd sdk/typescript && npm test
	cd sdk/typescript && WASMFGA_WASM=$(CURDIR)/dist/wasmfga-rust.wasm npm test

test-browser: build typescript-deps
	cd sdk/typescript && npm run test:browser

fmt:
	gofmt -w engine sdk/go tests
	cargo fmt --manifest-path sdk/rust/Cargo.toml
	cargo fmt --manifest-path engine-rust/Cargo.toml
