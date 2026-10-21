//go:build conformance_audit

package tests

// These oracle comparisons expose known Rust gaps. They are opt-in while the
// gaps are open; they must move into the default suite as each gap is fixed.

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	wasmfga "github.com/memoremix-ing/wasmfga/sdk/go"
	openfgav1 "github.com/openfga/api/proto/openfga/v1"
	"github.com/openfga/openfga/pkg/typesystem"
	"google.golang.org/protobuf/encoding/protojson"
)

func auditModel(t *testing.T) map[string]any {
	t.Helper()
	b, err := os.ReadFile("fixtures/model.json")
	must(t, err)
	var model map[string]any
	must(t, json.Unmarshal(b, &model))
	return model
}

func auditValidate(t *testing.T, body []byte) {
	t.Helper()
	model := new(openfgav1.AuthorizationModel)
	must(t, protojson.Unmarshal(body, model))
	_, err := typesystem.NewAndValidate(context.Background(), model)
	must(t, err)
}

func TestConformanceAuditCELModels(t *testing.T) {
	e := openEngine(t)
	for _, c := range []struct{ name, expression string }{
		{"string_concatenation", "'a' + 'b' == 'ab'"},
		{"int_conversion", "int('1') == 1"},
		{"duration_arithmetic", "duration('1s') + duration('1s') == duration('2s')"},
		{"list_macro", "[1, 2].exists(x, x == 1)"},
	} {
		t.Run(c.name, func(t *testing.T) {
			model := auditModel(t)
			model["conditions"].(map[string]any)["granted"].(map[string]any)["expression"] = c.expression
			body := raw(model)
			auditValidate(t, body)
			_, err := e.Do(context.Background(), &modelSink{}, wasmfga.Request{Operation: "put_model", StoreID: storeID, ModelID: modelID, Model: body})
			if err != nil {
				t.Fatalf("OpenFGA accepts %q; guest rejects: %v", c.expression, err)
			}
		})
	}
}

func TestConformanceAuditNumericString(t *testing.T) {
	body := raw(auditModel(t))
	auditValidate(t, body)
	stored := []json.RawMessage{raw(map[string]any{"object": "document:1", "relation": "conditional", "user": "user:alice", "condition": map[string]any{"name": "granted", "context": map[string]any{"required": 1}}})}
	query := raw(map[string]any{"tuple_key": map[string]any{"object": "document:1", "relation": "conditional", "user": "user:alice"}, "context": map[string]any{"level": "1e0"}})
	auditCheck(t, body, stored, query, true)
}

func TestConformanceAuditContextualDuplicates(t *testing.T) {
	model := auditModel(t)
	doc := model["type_definitions"].([]any)[3].(map[string]any)
	metadata := doc["metadata"].(map[string]any)["relations"].(map[string]any)
	metadata["blocked"] = map[string]any{"directly_related_user_types": []any{map[string]any{"type": "group", "relation": "member", "condition": "granted"}}}
	body := raw(model)
	auditValidate(t, body)
	stored := []json.RawMessage{tuple("document:1", "owner", "user:alice"), tuple("group:1", "member", "user:alice")}
	conditional := func(required int) any {
		return map[string]any{"object": "document:1", "relation": "blocked", "user": "group:1#member", "condition": map[string]any{"name": "granted", "context": map[string]any{"required": required}}}
	}
	query := raw(map[string]any{"tuple_key": map[string]any{"object": "document:1", "relation": "can_view", "user": "user:alice"}, "context": map[string]any{"level": 1}, "contextual_tuples": map[string]any{"tuple_keys": []any{conditional(2), conditional(0)}}})
	auditCheck(t, body, stored, query, false)
}

func auditCheck(t *testing.T, body []byte, stored []json.RawMessage, query json.RawMessage, expected bool) {
	t.Helper()
	want, oracleErr := newOracle(t, body).check(stored, query)
	must(t, oracleErr)
	if want != expected {
		t.Fatalf("unexpected oracle result: %v", want)
	}
	e := openEngine(t)
	db := openDB(t)
	setup(t, e, db, body)
	tx, err := db.Begin()
	must(t, err)
	defer tx.Rollback()
	_, err = do(e, tx, wasmfga.Request{Operation: "write", StoreID: storeID, ModelID: modelID, Writes: stored})
	must(t, err)
	got, guestErr := check(e, tx, query)
	if guestErr != nil || got != want {
		t.Fatalf("OpenFGA=%v,nil; guest=%v,%v", want, got, guestErr)
	}
}
