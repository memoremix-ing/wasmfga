package tests

import (
	"encoding/json"
	wasmfga "github.com/memoremix-ing/wasmfga/sdk/go"
	"os"
	"testing"
)

// The benchmark target requires this gate for the exact bytes/workloads/artifact
// measured. Fixture creation is outside timing and native OpenFGA is the oracle.
func TestBenchmarkOracle(t *testing.T) {
	path := os.Getenv("WASMFGA_BENCH_FIXTURES")
	if path == "" {
		t.Skip("run make benchmark for generated benchmark-fixture oracle gate")
	}
	bytes, err := os.ReadFile(path)
	must(t, err)
	var cases []struct {
		Name    string
		Model   json.RawMessage
		Tuples  []json.RawMessage
		Check   json.RawMessage
		Allowed bool
	}
	must(t, json.Unmarshal(bytes, &cases))
	e := openEngine(t)
	for _, c := range cases {
		t.Run(c.Name, func(t *testing.T) {
			oracle := newOracle(t, c.Model)
			expected, err := oracle.check(c.Tuples, c.Check)
			must(t, err)
			if expected != c.Allowed {
				t.Fatal("benchmark expectation disagrees with OpenFGA")
			}
			db := openDB(t)
			setup(t, e, db, c.Model)
			tx, err := db.Begin()
			must(t, err)
			defer tx.Rollback()
			for _, tk := range c.Tuples {
				var tuple struct{ Object, Relation, User string }
				must(t, json.Unmarshal(tk, &tuple))
				_, err = tx.Exec("INSERT INTO wasmfga_tuples (store_id, object, relation, user, body) VALUES (?, ?, ?, ?, ?)", storeID, tuple.Object, tuple.Relation, tuple.User, string(tk))
				must(t, err)
			}
			res, err := do(e, tx, wasmfga.Request{Operation: "check", StoreID: storeID, ModelID: modelID, Check: c.Check})
			must(t, err)
			if res.Allowed == nil || *res.Allowed != expected {
				t.Fatal("benchmark artifact disagrees with OpenFGA")
			}
		})
	}
}
