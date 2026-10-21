//go:build !wasip1

package engine

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// Transaction borrows the host's active transaction without lifecycle authority.
// The caller must roll back on any error and serialize access until Do returns.
type Transaction interface {
	Query(context.Context, string, []string) ([][]string, error)
	Execute(context.Context, string, []string) (int64, error)
}

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
	res, err := t.Tx.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// Do is the native entry point. It calls Process directly, without WASM or outer
// JSON transport. Results supplied by the caller are discarded; SQL reads always
// go through the transaction binding. Serialized-input limits apply to Run, while
// the model, tuple, batch and graph limits in Process also apply here.
func Do(ctx context.Context, tx Transaction, request Request) (Response, error) {
	if tx == nil {
		return Response{}, errors.New("an active transaction binding is required")
	}
	request.Results = nil
	for round := 0; round < 2; round++ {
		if err := ctx.Err(); err != nil {
			return Response{}, err
		}
		response, err := Process(ctx, request)
		if err != nil {
			return Response{}, err
		}
		results := [][][]string{}
		for _, s := range response.Statements {
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
				changed, err := tx.Execute(ctx, s.SQL, s.Params)
				if err != nil {
					return Response{}, fmt.Errorf("execute (caller must roll back): %w", err)
				}
				if s.ExpectedChanges != nil && changed != int64(*s.ExpectedChanges) {
					return Response{}, fmt.Errorf("expected %d changes, got %d (caller must roll back)", *s.ExpectedChanges, changed)
				}
			default:
				return Response{}, errors.New("unknown SQL effect")
			}
		}
		if !response.NeedsResults {
			return response, nil
		}
		request.Results = &results
	}
	return Response{}, errors.New("native protocol exceeded two rounds")
}
