package tests

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"testing"

	wasmfga "github.com/memoremix-ing/wasmfga/sdk/go"
	openfgav1 "github.com/openfga/api/proto/openfga/v1"
	"github.com/openfga/openfga/pkg/server"
	"github.com/openfga/openfga/pkg/storage/memory"
	"google.golang.org/protobuf/encoding/protojson"
	_ "modernc.org/sqlite"
)

const storeID = "01ARZ3NDEKTSV4RRFFQ69G5FAV"
const modelID = "01ARZ3NDEKTSV4RRFFQ69G5FAW"

func must(t testing.TB, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
func raw(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}
func tuple(object, relation, user string) json.RawMessage {
	return raw(map[string]string{"object": object, "relation": relation, "user": user})
}

type testEngine interface {
	Do(context.Context, wasmfga.Transaction, wasmfga.Request) (wasmfga.Response, error)
	Close(context.Context) error
}

func openEngine(t testing.TB) testEngine {
	t.Helper()
	if name := os.Getenv("WASMFGA_NATIVE"); name != "" {
		return openNativeEngine(t, name)
	}
	artifact := os.Getenv("WASMFGA_WASM")
	if artifact == "" {
		artifact = "../dist/wasmfga.wasm"
	}
	b, err := os.ReadFile(artifact)
	must(t, err)
	e, err := wasmfga.New(context.Background(), b)
	must(t, err)
	t.Cleanup(func() { must(t, e.Close(context.Background())) })
	return e
}
func openDB(t testing.TB) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	must(t, err)
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { must(t, db.Close()) })
	return db
}
func do(e testEngine, tx *sql.Tx, r wasmfga.Request) (wasmfga.Response, error) {
	return e.Do(context.Background(), wasmfga.SQLTransaction{Tx: tx}, r)
}
func setup(t testing.TB, e testEngine, db *sql.DB, model []byte) {
	t.Helper()
	tx, err := db.Begin()
	must(t, err)
	defer tx.Rollback()
	_, err = do(e, tx, wasmfga.Request{Operation: "schema"})
	must(t, err)
	_, err = do(e, tx, wasmfga.Request{Operation: "put_model", StoreID: storeID, ModelID: modelID, Model: model})
	must(t, err)
	_, err = tx.Exec("CREATE TABLE app_audit (body TEXT NOT NULL)")
	must(t, err)
	must(t, tx.Commit())
}
func check(e testEngine, tx *sql.Tx, query json.RawMessage) (bool, error) {
	res, err := do(e, tx, wasmfga.Request{Operation: "check", StoreID: storeID, ModelID: modelID, Check: query})
	if err != nil {
		return false, err
	}
	if res.Allowed == nil {
		panic("missing allowed")
	}
	return *res.Allowed, nil
}

type oracle struct{ model *openfgav1.AuthorizationModel }

func newOracle(t testing.TB, model []byte) *oracle {
	t.Helper()
	m := new(openfgav1.AuthorizationModel)
	must(t, protojson.Unmarshal(model, m))
	m.Id = modelID
	return &oracle{model: m}
}
func (o *oracle) check(tuples []json.RawMessage, query json.RawMessage) (bool, error) {
	ctx := context.Background()
	// A separate ephemeral store prevents oracle state from leaking between checks.
	ds := memory.New(memory.WithMaxTuplesPerWrite(10000))
	defer ds.Close()
	model := o.model
	var err error
	if err = ds.WriteAuthorizationModel(ctx, storeID, model); err != nil {
		return false, err
	}
	keys := make([]*openfgav1.TupleKey, 0, len(tuples))
	for _, b := range tuples {
		tk := new(openfgav1.TupleKey)
		if err = protojson.Unmarshal(b, tk); err != nil {
			return false, err
		}
		keys = append(keys, tk)
	}
	if len(keys) > 0 {
		if err = ds.Write(ctx, storeID, nil, keys); err != nil {
			return false, err
		}
	}
	s, err := server.NewServerWithOpts(server.WithDatastore(ds))
	if err != nil {
		return false, err
	}
	defer s.Close()
	q := new(openfgav1.CheckRequest)
	if err = protojson.Unmarshal(query, q); err != nil {
		return false, err
	}
	q.StoreId = storeID
	q.AuthorizationModelId = modelID
	res, err := s.Check(ctx, q)
	if err != nil {
		return false, err
	}
	return res.Allowed, nil
}

func TestBorrowedTransaction(t *testing.T) {
	e := openEngine(t)
	db := openDB(t)
	model, err := os.ReadFile("fixtures/model.json")
	must(t, err)
	setup(t, e, db, model)
	q := raw(map[string]any{"tuple_key": map[string]string{"object": "document:1", "relation": "can_view", "user": "user:alice"}})
	for _, commit := range []bool{false, true} {
		tx, err := db.Begin()
		must(t, err)
		_, err = do(e, tx, wasmfga.Request{Operation: "write", StoreID: storeID, ModelID: modelID, Writes: []json.RawMessage{tuple("document:1", "owner", "user:alice")}})
		must(t, err)
		allowed, err := check(e, tx, q)
		must(t, err)
		if !allowed {
			t.Fatal("cannot read uncommitted grant")
		}
		_, err = tx.Exec("INSERT INTO app_audit VALUES (?)", "authorized operation")
		must(t, err)
		if commit {
			must(t, tx.Commit())
		} else {
			must(t, tx.Rollback())
		}
		tx, err = db.Begin()
		must(t, err)
		allowed, err = check(e, tx, q)
		must(t, err)
		if allowed != commit {
			t.Fatalf("allowed=%v, committed=%v", allowed, commit)
		}
		var n int
		must(t, tx.QueryRow("SELECT count(*) FROM app_audit").Scan(&n))
		if n != map[bool]int{false: 0, true: 1}[commit] {
			t.Fatal("application write was not atomic")
		}
		must(t, tx.Rollback())
	}
}

type corpusCase struct {
	Name    string            `json:"name"`
	Tuples  []json.RawMessage `json:"tuples"`
	Check   json.RawMessage   `json:"check"`
	Allowed bool              `json:"allowed"`
	Error   bool              `json:"error,omitempty"`
}

func TestConformanceCorpus(t *testing.T) {
	e := openEngine(t)
	model, err := os.ReadFile("fixtures/model.json")
	must(t, err)
	o := newOracle(t, model)
	b, err := os.ReadFile("fixtures/cases.json")
	must(t, err)
	var cases []corpusCase
	must(t, json.Unmarshal(b, &cases))
	for _, c := range cases {
		t.Run(c.Name, func(t *testing.T) {
			want, oracleErr := o.check(c.Tuples, c.Check)
			if (oracleErr != nil) != c.Error || (oracleErr == nil && want != c.Allowed) {
				t.Fatalf("fixture disagrees with OpenFGA: %v %v", want, oracleErr)
			}
			db := openDB(t)
			setup(t, e, db, model)
			tx, err := db.Begin()
			must(t, err)
			defer tx.Rollback()
			if len(c.Tuples) > 0 {
				_, err = do(e, tx, wasmfga.Request{Operation: "write", StoreID: storeID, ModelID: modelID, Writes: c.Tuples})
				must(t, err)
			}
			got, err := check(e, tx, c.Check)
			if (err != nil) != (oracleErr != nil) || (err == nil && got != want) {
				t.Fatalf("WASM=%v,%v; OpenFGA=%v,%v", got, err, want, oracleErr)
			}
		})
	}
}

func TestFailuresAndIsolation(t *testing.T) {
	e := openEngine(t)
	db := openDB(t)
	model, err := os.ReadFile("fixtures/model.json")
	must(t, err)
	setup(t, e, db, model)
	tx, err := db.Begin()
	must(t, err)
	_, err = do(e, tx, wasmfga.Request{Operation: "put_model", StoreID: "other-tenant", ModelID: modelID, Model: model})
	must(t, err)
	grant := tuple("document:1", "owner", "user:alice")
	_, err = do(e, tx, wasmfga.Request{Operation: "write", StoreID: storeID, ModelID: modelID, Writes: []json.RawMessage{grant}})
	must(t, err)
	must(t, tx.Commit())
	q := raw(map[string]any{"tuple_key": map[string]string{"object": "document:1", "relation": "can_view", "user": "user:alice"}})
	tx, err = db.Begin()
	must(t, err)
	res, err := do(e, tx, wasmfga.Request{Operation: "check", StoreID: "other-tenant", ModelID: modelID, Check: q})
	must(t, err)
	if res.Allowed == nil || *res.Allowed {
		t.Fatal("grant leaked across stores")
	}
	_, err = do(e, tx, wasmfga.Request{Operation: "put_model", StoreID: storeID, ModelID: modelID, Model: model})
	if err == nil {
		t.Fatal("model ID was mutable")
	}
	must(t, tx.Rollback())

	// The first insert succeeds, the second collides. The host still owns the
	// active transaction, and rolling it back removes the tentative first insert.
	tx, err = db.Begin()
	must(t, err)
	_, err = do(e, tx, wasmfga.Request{Operation: "write", StoreID: storeID, ModelID: modelID, Writes: []json.RawMessage{tuple("document:2", "owner", "user:alice"), grant}})
	if err == nil {
		t.Fatal("duplicate insert accepted")
	}
	var n int
	must(t, tx.QueryRow("SELECT count(*) FROM wasmfga_tuples WHERE object = 'document:2'").Scan(&n))
	if n != 1 {
		t.Fatal("failed operation unexpectedly took transaction ownership")
	}
	must(t, tx.Rollback())
	tx, err = db.Begin()
	must(t, err)
	defer tx.Rollback()
	q = raw(map[string]any{"tuple_key": map[string]string{"object": "document:2", "relation": "can_view", "user": "user:alice"}})
	allowed, err := check(e, tx, q)
	must(t, err)
	if allowed {
		t.Fatal("failed batch escaped rollback")
	}
	_, err = do(e, tx, wasmfga.Request{Operation: "write", StoreID: storeID, ModelID: modelID, Deletes: []json.RawMessage{tuple("document:missing", "owner", "user:alice")}})
	if err == nil {
		t.Fatal("missing delete accepted")
	}
}
