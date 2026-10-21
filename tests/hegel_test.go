package tests

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"sort"
	"testing"

	wasmfga "github.com/memoremix-ing/wasmfga/sdk/go"
	"hegel.dev/go/hegel"
)

// Hegel shrinks both the authorization rewrite and the entire transaction trace.
// The model tracks committed vs. tentative tuples/application writes independently
// of SQL. Native OpenFGA Server.Check is the permission oracle at every step.
func TestHegelTransactionModel(t *testing.T) {
	e := openEngine(t)
	original, err := os.ReadFile("fixtures/model.json")
	must(t, err)
	pool := []json.RawMessage{
		tuple("document:1", "owner", "user:alice"), tuple("document:1", "viewer", "user:bob"), tuple("document:1", "viewer", "user:*"),
		tuple("document:1", "viewer", "group:1#member"), tuple("group:1", "member", "user:alice"), tuple("group:1", "member", "group:2#member"),
		tuple("group:2", "member", "group:1#member"), tuple("group:2", "member", "user:bob"), tuple("document:1", "parent", "folder:1"),
		tuple("folder:1", "parent", "folder:2"), tuple("folder:2", "parent", "folder:1"), tuple("folder:2", "viewer", "user:alice"),
		tuple("document:1", "editor", "user:alice"), tuple("document:1", "restricted", "user:alice"), tuple("document:1", "blocked", "user:alice"),
		tuple("document:2", "owner", "user:bob"),
		raw(map[string]any{"object": "document:1", "relation": "conditional", "user": "user:alice", "condition": map[string]any{"name": "granted", "context": map[string]any{"required": 3}}}),
	}
	hegel.Test(t, func(ht *hegel.T) {
		require := func(err error) {
			if err != nil {
				ht.Fatalf("%v", err)
			}
		}
		// Generate valid, nested rewrites with independently chosen set operators.
		var model map[string]any
		require(json.Unmarshal(original, &model))
		computed := func(r string) any { return map[string]any{"computedUserset": map[string]any{"relation": r}} }
		refs := []string{"viewer", "owner", "both", "conditional"}
		var expr any = computed(refs[hegel.Draw(ht, hegel.Integers(0, len(refs)-1))])
		depth := hegel.Draw(ht, hegel.Integers(0, 3))
		for i := 0; i < depth; i++ {
			rhs := computed(refs[hegel.Draw(ht, hegel.Integers(0, len(refs)-1))])
			switch hegel.Draw(ht, hegel.Integers(0, 2)) {
			case 0:
				expr = map[string]any{"union": map[string]any{"child": []any{expr, rhs}}}
			case 1:
				expr = map[string]any{"intersection": map[string]any{"child": []any{expr, rhs}}}
			case 2:
				expr = map[string]any{"difference": map[string]any{"base": expr, "subtract": rhs}}
			}
		}
		// Include tuple-to-userset paths alongside the generated expression.
		relations := model["type_definitions"].([]any)[3].(map[string]any)["relations"].(map[string]any)
		relations["can_view"] = map[string]any{"union": map[string]any{"child": []any{expr, map[string]any{"tupleToUserset": map[string]any{"tupleset": map[string]any{"relation": "parent"}, "computedUserset": map[string]any{"relation": "viewer"}}}}}}
		mb := raw(model)
		o := newOracle(t, mb)
		db, err := sql.Open("sqlite", ":memory:")
		require(err)
		defer db.Close()
		db.SetMaxOpenConns(1)
		setup(t, e, db, mb)
		tx, err := db.Begin()
		require(err)
		defer func() { _ = tx.Rollback() }()
		committed := map[int]bool{}
		working := map[int]bool{}
		committedAudit, workingAudit := 0, 0
		steps := hegel.Draw(ht, hegel.Integers(5, 35))
		for step := 0; step < steps; step++ {
			op := hegel.Draw(ht, hegel.Integers(0, 4))
			idx := hegel.Draw(ht, hegel.Integers(0, len(pool)-1))
			ht.Logf("step=%d op=%d tuple=%s", step, op, pool[idx])
			switch op {
			case 0, 1:
				if (op == 0 && !working[idx]) || (op == 1 && working[idx]) {
					req := wasmfga.Request{Operation: "write", StoreID: storeID, ModelID: modelID}
					if op == 0 {
						req.Writes = []json.RawMessage{pool[idx]}
						working[idx] = true
					} else {
						var tk map[string]any
						require(json.Unmarshal(pool[idx], &tk))
						delete(tk, "condition")
						req.Deletes = []json.RawMessage{raw(tk)}
						delete(working, idx)
					}
					_, err = do(e, tx, req)
					require(err)
					_, err = tx.Exec("INSERT INTO app_audit VALUES (?)", fmt.Sprint(step))
					require(err)
					workingAudit++
				}
			case 3:
				require(tx.Commit())
				committed = maps.Clone(working)
				committedAudit = workingAudit
				tx, err = db.Begin()
				require(err)
			case 4:
				require(tx.Rollback())
				working = maps.Clone(committed)
				workingAudit = committedAudit
				tx, err = db.Begin()
				require(err)
			}
			var n int
			require(tx.QueryRow("SELECT count(*) FROM app_audit").Scan(&n))
			if n != workingAudit {
				ht.Fatalf("app rows: SQL=%d model=%d", n, workingAudit)
			}
			object := []string{"document:1", "document:2"}[hegel.Draw(ht, hegel.Integers(0, 1))]
			relation := []string{"can_view", "viewer", "both"}[hegel.Draw(ht, hegel.Integers(0, 2))]
			user := []string{"user:alice", "user:bob", "user:carol"}[hegel.Draw(ht, hegel.Integers(0, 2))]
			query := map[string]any{"tuple_key": map[string]string{"object": object, "relation": relation, "user": user}, "context": map[string]any{"level": hegel.Draw(ht, hegel.Integers(0, 5))}}
			if hegel.Draw(ht, hegel.Integers(0, 3)) == 0 {
				query["contextual_tuples"] = map[string]any{"tuple_keys": []json.RawMessage{tuple(object, "owner", user)}}
			}
			keys := []int{}
			for k := range working {
				keys = append(keys, k)
			}
			sort.Ints(keys)
			tuples := []json.RawMessage{}
			for _, k := range keys {
				tuples = append(tuples, pool[k])
			}
			want, oracleErr := o.check(tuples, raw(query))
			got, wasmErr := check(e, tx, raw(query))
			if (oracleErr != nil) != (wasmErr != nil) || (oracleErr == nil && got != want) {
				ht.Fatalf("step %d: OpenFGA=%v,%v WASM=%v,%v query=%s tuples=%s", step, want, oracleErr, got, wasmErr, raw(query), raw(tuples))
			}
		}
	}, hegel.WithTestCases(100))
}
