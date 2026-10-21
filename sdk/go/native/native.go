//go:build !wasip1

// Package native embeds the Go evaluator directly, with no WASM runtime.
// The host supplies an active transaction and owns its commit/rollback.
package native

import (
	"context"
	engine "github.com/openfga/openfga/wasmfga"
)

type Transaction = engine.Transaction
type SQLTransaction = engine.SQLTransaction
type Request = engine.Request
type Response = engine.Response

// Do calls the evaluator using only the supplied transaction. Roll back on error.
func Do(ctx context.Context, tx Transaction, request Request) (Response, error) {
	return engine.Do(ctx, tx, request)
}
