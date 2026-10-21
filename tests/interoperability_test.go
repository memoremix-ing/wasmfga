package tests

import (
	"context"
	"encoding/json"
	wasmfga "github.com/memoremix-ing/wasmfga/sdk/go"
	"os"
	"testing"
)

func TestCrossEngineTransaction(t *testing.T) {
	artifacts := []string{"../dist/wasmfga.wasm", "../dist/wasmfga-rust.wasm"}
	engines := make([]*wasmfga.Engine, 2)
	for i, path := range artifacts {
		bytes, err := os.ReadFile(path)
		if os.IsNotExist(err) {
			t.Skip("both WASM artifacts are needed")
		}
		must(t, err)
		engines[i], err = wasmfga.New(context.Background(), bytes)
		must(t, err)
		engine := engines[i]
		t.Cleanup(func() { must(t, engine.Close(context.Background())) })
	}
	model, err := os.ReadFile("fixtures/model.json")
	must(t, err)
	for direction := range 2 {
		t.Run([]string{"go_to_rust", "rust_to_go"}[direction], func(t *testing.T) {
			writer, reader := engines[direction], engines[1-direction]
			db := openDB(t)
			setup(t, writer, db, model)
			tx, err := db.Begin()
			must(t, err)
			defer tx.Rollback()
			tk := tuple("document:1", "owner", "user:alice")
			_, err = do(writer, tx, wasmfga.Request{Operation: "write", StoreID: storeID, ModelID: modelID, Writes: []json.RawMessage{tk}})
			must(t, err)
			query := raw(map[string]any{"tuple_key": map[string]string{"object": "document:1", "relation": "can_view", "user": "user:alice"}})
			got, err := check(reader, tx, query)
			must(t, err)
			if !got {
				t.Fatal("other engine could not read the uncommitted model/grant")
			}
			must(t, tx.Commit())
			tx, err = db.Begin()
			must(t, err)
			defer tx.Rollback()
			_, err = do(reader, tx, wasmfga.Request{Operation: "write", StoreID: storeID, ModelID: modelID, Deletes: []json.RawMessage{tk}})
			must(t, err)
			got, err = check(writer, tx, query)
			must(t, err)
			if got {
				t.Fatal("other engine missed the uncommitted revocation")
			}
			must(t, tx.Rollback())
			tx, err = db.Begin()
			must(t, err)
			defer tx.Rollback()
			got, err = check(writer, tx, query)
			must(t, err)
			if !got {
				t.Fatal("revocation escaped rollback")
			}
		})
	}
}
