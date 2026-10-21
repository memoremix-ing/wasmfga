package tests

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"testing"

	wasmfga "github.com/memoremix-ing/wasmfga/sdk/go"
	engine "github.com/openfga/openfga/wasmfga"
	"github.com/openfga/openfga/wasmfga/upstream"
)

func TestPreparedUpstreamReadsTentativeGrantsAndRevokes(t *testing.T) {
	ctx := context.Background()
	body, err := os.ReadFile("fixtures/model.json")
	must(t, err)
	db := openDB(t)
	initial, err := db.Begin()
	must(t, err)
	binding := engine.SQLTransaction{Tx: initial}
	_, err = engine.Do(ctx, binding, engine.Request{Operation: "schema"})
	must(t, err)
	_, err = engine.Do(ctx, binding, engine.Request{Operation: "put_model", StoreID: storeID, ModelID: modelID, Model: body})
	must(t, err)
	must(t, initial.Commit())
	query := raw(map[string]any{"tuple_key": map[string]string{"object": "document:1", "relation": "can_view", "user": "user:alice"}})
	for _, commit := range []bool{false, true} {
		tx, err := db.Begin()
		must(t, err)
		func(tx *sql.Tx) {
			defer tx.Rollback()
			binding := engine.SQLTransaction{Tx: tx}
			prepared, err := upstream.Prepare(ctx, binding, storeID, modelID)
			must(t, err)
			defer prepared.Close()
			assertDecision := func(want bool) {
				t.Helper()
				got, err := prepared.Check(ctx, query)
				must(t, err)
				if got != want {
					t.Fatalf("cached-model decision %v, want %v", got, want)
				}
			}
			assertDecision(false)
			grant := engine.Request{Operation: "write", StoreID: storeID, ModelID: modelID, Writes: []json.RawMessage{tuple("document:1", "owner", "user:alice")}}
			_, err = engine.Do(ctx, binding, grant)
			must(t, err)
			assertDecision(true)
			_, err = engine.Do(ctx, binding, engine.Request{Operation: "write", StoreID: storeID, ModelID: modelID, Deletes: grant.Writes})
			must(t, err)
			assertDecision(false)
			_, err = engine.Do(ctx, binding, grant)
			must(t, err)
			assertDecision(true)
			if commit {
				must(t, tx.Commit())
			} else {
				must(t, tx.Rollback())
			}
		}(tx)
		tx, err = db.Begin()
		must(t, err)
		got, err := upstream.Check(ctx, engine.SQLTransaction{Tx: tx}, storeID, modelID, query)
		must(t, err)
		if got != commit {
			t.Fatalf("committed decision %v, want %v", got, commit)
		}
		must(t, tx.Rollback())
	}
}

type nativeEngine struct {
	kind   string
	input  io.WriteCloser
	output *bufio.Reader
	cmd    *exec.Cmd
	mu     sync.Mutex
}

func openNativeEngine(t testing.TB, name string) testEngine {
	t.Helper()
	e := &nativeEngine{kind: name}
	if name == "rust" {
		path := os.Getenv("WASMFGA_NATIVE_RUST")
		if path == "" {
			path = "../dist/native/rust-bench"
		}
		e.cmd = exec.Command(path, "--serve")
		e.cmd.Stderr = os.Stderr
		in, err := e.cmd.StdinPipe()
		must(t, err)
		e.input = in
		out, err := e.cmd.StdoutPipe()
		must(t, err)
		e.output = bufio.NewReader(out)
		must(t, e.cmd.Start())
	} else if name != "go" && name != "upstream" {
		t.Fatalf("unknown native evaluator: %s", name)
	}
	t.Cleanup(func() { must(t, e.Close(context.Background())) })
	return e
}
func (e *nativeEngine) Close(context.Context) error {
	if e.cmd == nil {
		return nil
	}
	_ = e.input.Close()
	err := e.cmd.Wait()
	e.cmd = nil
	return err
}
func (e *nativeEngine) Do(ctx context.Context, tx wasmfga.Transaction, r wasmfga.Request) (wasmfga.Response, error) {
	req := engine.Request{Operation: r.Operation, Dialect: r.Dialect, StoreID: r.StoreID, ModelID: r.ModelID, Model: r.Model, Check: r.Check, Writes: r.Writes, Deletes: r.Deletes}
	var res engine.Response
	var err error
	if e.kind == "go" {
		res, err = engine.Do(ctx, tx, req)
	} else if e.kind == "upstream" {
		if r.Operation == "check" {
			var allowed bool
			allowed, err = upstream.Check(ctx, tx, r.StoreID, r.ModelID, r.Check)
			if err == nil {
				res.Allowed = &allowed
			}
		} else {
			res, err = engine.Do(ctx, tx, req)
		}
	} else {
		e.mu.Lock()
		defer e.mu.Unlock()
		for round := 0; round < 2; round++ {
			b, merr := json.Marshal(req)
			if merr != nil {
				return wasmfga.Response{}, merr
			}
			if _, err = fmt.Fprintln(e.input, string(b)); err != nil {
				break
			}
			var bline []byte
			bline, err = e.output.ReadBytes('\n')
			if err != nil {
				break
			}
			res, err = engine.Decode(bline)
			if err != nil {
				break
			}
			results := [][][]string{}
			for _, s := range res.Statements {
				if s.Kind == "query" {
					var rows [][]string
					rows, err = tx.Query(ctx, s.SQL, s.Params)
					if err != nil {
						break
					}
					if rows == nil {
						rows = [][]string{}
					}
					results = append(results, rows)
				} else if s.Kind == "execute" {
					var n int64
					n, err = tx.Execute(ctx, s.SQL, s.Params)
					if err != nil {
						break
					}
					if s.ExpectedChanges != nil && n != int64(*s.ExpectedChanges) {
						err = fmt.Errorf("unexpected affected rows: %d", n)
						break
					}
				} else {
					err = fmt.Errorf("unknown effect: %s", s.Kind)
					break
				}
			}
			if err != nil || !res.NeedsResults {
				break
			}
			req.Results = &results
			if round == 1 {
				err = fmt.Errorf("native protocol exceeded two rounds")
			}
		}
	}
	return wasmfga.Response{Allowed: res.Allowed, NeedsResults: res.NeedsResults}, err
}
