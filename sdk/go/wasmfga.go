// Package wasmfga embeds the WASI evaluator and borrows an already-open host transaction.
package wasmfga

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
)

// Transaction deliberately has no Begin, Commit, Rollback, or connection access.
type Transaction interface {
	Query(context.Context, string, []string) ([][]string, error)
	Execute(context.Context, string, []string) (int64, error)
}

// SQLTransaction adapts a caller-owned database/sql transaction (SQLite or libSQL).
type SQLTransaction struct{ Tx *sql.Tx }

func (t SQLTransaction) Query(ctx context.Context, query string, params []string) ([][]string, error) {
	args := make([]any, len(params))
	for i, p := range params {
		args[i] = p
	}
	rows, err := t.Tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := [][]string{}
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		result = append(result, []string{s})
	}
	return result, rows.Err()
}
func (t SQLTransaction) Execute(ctx context.Context, query string, params []string) (int64, error) {
	args := make([]any, len(params))
	for i, p := range params {
		args[i] = p
	}
	result, err := t.Tx.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

type Request struct {
	Operation string            `json:"operation"`
	Dialect   string            `json:"dialect,omitempty"`
	StoreID   string            `json:"store_id,omitempty"`
	ModelID   string            `json:"model_id,omitempty"`
	Model     json.RawMessage   `json:"model,omitempty"`
	Check     json.RawMessage   `json:"check,omitempty"`
	Writes    []json.RawMessage `json:"writes,omitempty"`
	Deletes   []json.RawMessage `json:"deletes,omitempty"`
}

type statement struct {
	Kind            string   `json:"kind"`
	SQL             string   `json:"sql"`
	Params          []string `json:"params"`
	ExpectedChanges *int64   `json:"expected_changes,omitempty"`
}
type Response struct {
	NeedsResults bool        `json:"needs_results,omitempty"`
	Statements   []statement `json:"statements,omitempty"`
	Allowed      *bool       `json:"allowed,omitempty"`
	Error        string      `json:"error,omitempty"`
}

type Engine struct {
	mu      sync.Mutex
	runtime wazero.Runtime
	module  api.Module
}

func New(ctx context.Context, wasm []byte) (*Engine, error) {
	rt := wazero.NewRuntimeWithConfig(ctx, wazero.NewRuntimeConfig().WithMemoryLimitPages(8192).WithCloseOnContextDone(true))
	fail := func(err error) (*Engine, error) { _ = rt.Close(ctx); return nil, err }
	if _, err := wasi_snapshot_preview1.Instantiate(ctx, rt); err != nil {
		return fail(err)
	}
	mod, err := rt.InstantiateWithConfig(ctx, wasm, wazero.NewModuleConfig().WithName("").WithArgs("wasmfga").WithStartFunctions("_initialize").WithRandSource(rand.Reader).WithSysWalltime().WithSysNanotime().WithSysNanosleep())
	if err != nil {
		return fail(err)
	}
	return &Engine{runtime: rt, module: mod}, nil
}

func (e *Engine) Close(ctx context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.runtime.Close(ctx)
}

func (e *Engine) call(ctx context.Context, request any) (Response, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	var response Response
	data, err := json.Marshal(request)
	if err != nil {
		return response, err
	}
	if len(data) > 16<<20 {
		return response, errors.New("input exceeds 16 MiB")
	}
	p, err := e.module.ExportedFunction("alloc").Call(ctx, uint64(len(data)))
	if err != nil {
		return response, err
	}
	if p[0] == 0 || !e.module.Memory().Write(uint32(p[0]), data) {
		return response, errors.New("WASM allocation failed")
	}
	packed, err := e.module.ExportedFunction("run").Call(ctx)
	if err != nil {
		return response, err
	}
	output, ok := e.module.Memory().Read(uint32(packed[0]>>32), uint32(packed[0]))
	if !ok {
		return response, errors.New("invalid WASM output")
	}
	if err := json.Unmarshal(output, &response); err != nil {
		return response, err
	}
	if response.Error != "" {
		return response, fmt.Errorf("wasmfga: %s", response.Error)
	}
	return response, nil
}

// Do executes one operation using only the supplied transaction. On ANY error the
// caller must roll back its transaction; a multi-statement write may be partial.
// The transaction must not be used concurrently until Do returns.
func (e *Engine) Do(ctx context.Context, tx Transaction, request Request) (Response, error) {
	if tx == nil {
		return Response{}, errors.New("an active transaction binding is required")
	}
	envelope := struct {
		Request
		Results *[][][]string `json:"results,omitempty"`
	}{Request: request}
	for round := 0; round < 2; round++ {
		res, err := e.call(ctx, envelope)
		if err != nil {
			return res, err
		}
		results := [][][]string{}
		for _, s := range res.Statements {
			switch s.Kind {
			case "query":
				rows, err := tx.Query(ctx, s.SQL, s.Params)
				if err != nil {
					return Response{}, fmt.Errorf("query: %w", err)
				}
				if rows == nil {
					rows = [][]string{}
				}
				results = append(results, rows)
			case "execute":
				n, err := tx.Execute(ctx, s.SQL, s.Params)
				if err != nil {
					return Response{}, fmt.Errorf("execute (caller must roll back): %w", err)
				}
				if s.ExpectedChanges != nil && n != *s.ExpectedChanges {
					return Response{}, fmt.Errorf("expected %d changes, got %d (caller must roll back)", *s.ExpectedChanges, n)
				}
			default:
				return Response{}, errors.New("unknown SQL effect")
			}
		}
		if !res.NeedsResults {
			return res, nil
		}
		envelope.Results = &results
	}
	return Response{}, errors.New("WASM protocol exceeded two rounds")
}
