package engine

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func fixture(t *testing.T) Request {
	t.Helper()
	model, err := os.ReadFile("../tests/fixtures/model.json")
	if err != nil {
		t.Fatal(err)
	}
	return Request{Operation: "put_model", StoreID: "s", ModelID: "m", Model: model}
}
func hydrated(t *testing.T, op string) Request {
	t.Helper()
	r := fixture(t)
	res, err := Process(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	results := [][][]string{{{res.Statements[0].Params[2]}}}
	r.Operation = op
	r.Results = &results
	return r
}

func TestSQLBoundary(t *testing.T) {
	ctx := context.Background()
	r := fixture(t)
	r.StoreID = "'; COMMIT; --"
	for _, op := range []string{"schema", "put_model", "check", "write"} {
		r.Operation = op
		res, err := Process(ctx, r)
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range res.Statements {
			first := strings.Fields(s.SQL)[0]
			if first != "SELECT" && first != "INSERT" && first != "DELETE" && first != "CREATE" {
				t.Fatalf("unexpected effect: %s", s.SQL)
			}
			if strings.Contains(s.SQL, r.StoreID) {
				t.Fatalf("SQL interpolated an identifier: %s", s.SQL)
			}
		}
	}
}

func TestFailClosed(t *testing.T) {
	tests := map[string]func() Request{
		"unknown dialect": func() Request { r := fixture(t); r.Dialect = "postgres"; return r },
		"invalid model":   func() Request { r := fixture(t); r.Model = json.RawMessage(`{}`); return r },
		"missing model": func() Request {
			r := fixture(t)
			r.Operation = "check"
			results := [][][]string{{}, {}}
			r.Results = &results
			return r
		},
		"missing query": func() Request {
			r := hydrated(t, "check")
			*r.Results = append(*r.Results, [][]string{})
			r.Check = json.RawMessage(`{}`)
			return r
		},
		"snapshot overflow": func() Request {
			r := hydrated(t, "check")
			*r.Results = append(*r.Results, make([][]string, MaxTuples+1))
			return r
		},
		"wrong result count": func() Request { return hydrated(t, "check") },
		"wrong row shape": func() Request {
			r := hydrated(t, "check")
			*r.Results = append(*r.Results, [][]string{{"a", "b"}})
			return r
		},
		"contextual tuple limit": func() Request {
			r := hydrated(t, "check")
			*r.Results = append(*r.Results, [][]string{})
			tuples := make([]map[string]string, 101)
			for i := range tuples {
				tuples[i] = map[string]string{"object": "document:1", "relation": "owner", "user": "user:alice"}
			}
			body, _ := json.Marshal(map[string]any{"tuple_key": tuples[0], "contextual_tuples": map[string]any{"tuple_keys": tuples}})
			r.Check = body
			return r
		},
		"empty write": func() Request { return hydrated(t, "write") },
		"invalid tuple": func() Request {
			r := hydrated(t, "write")
			r.Writes = []json.RawMessage{json.RawMessage(`{"object":"document:1","relation":"parent","user":"user:alice"}`)}
			return r
		},
		"duplicate tuple": func() Request {
			r := hydrated(t, "write")
			tk := json.RawMessage(`{"object":"document:1","relation":"owner","user":"user:alice"}`)
			r.Writes = []json.RawMessage{tk, tk}
			return r
		},
	}
	for name, makeReq := range tests {
		t.Run(name, func(t *testing.T) {
			res, err := Process(context.Background(), makeReq())
			if err == nil {
				t.Fatal("expected error")
			}
			if res.Allowed != nil || len(res.Statements) > 0 {
				t.Fatal("error produced effects/decision")
			}
		})
	}
	if _, err := Decode(Run([]byte(`{"operation":`))); err == nil {
		t.Fatal("invalid JSON accepted")
	}
}
