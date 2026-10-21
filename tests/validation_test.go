package tests

import (
	"context"
	"encoding/json"
	"errors"
	wasmfga "github.com/memoremix-ing/wasmfga/sdk/go"
	openfgav1 "github.com/openfga/api/proto/openfga/v1"
	"github.com/openfga/openfga/pkg/typesystem"
	"google.golang.org/protobuf/encoding/protojson"
	"os"
	"testing"
)

type modelSink struct{ executions int }

func (s *modelSink) Query(context.Context, string, []string) ([][]string, error) {
	return nil, errors.New("model validation must not query")
}
func (s *modelSink) Execute(context.Context, string, []string) (int64, error) {
	s.executions++
	return 1, nil
}

func TestModelValidationMutations(t *testing.T) {
	original, err := os.ReadFile("fixtures/model.json")
	must(t, err)
	e := openEngine(t)
	doc := func(m map[string]any) map[string]any { return m["type_definitions"].([]any)[3].(map[string]any) }
	relation := func(m map[string]any, name string, v any) { doc(m)["relations"].(map[string]any)[name] = v }
	metadata := func(m map[string]any, name string, v any) {
		doc(m)["metadata"].(map[string]any)["relations"].(map[string]any)[name] = v
	}
	expression := func(m map[string]any, value string) {
		m["conditions"].(map[string]any)["granted"].(map[string]any)["expression"] = value
	}
	cases := []struct {
		name   string
		change func(map[string]any)
	}{
		{"valid", func(m map[string]any) {}},
		{"schema", func(m map[string]any) { m["schema_version"] = "9.9" }},
		{"duplicate_type", func(m map[string]any) { ts := m["type_definitions"].([]any); m["type_definitions"] = append(ts, ts[0]) }},
		{"unknown_type", func(m map[string]any) { m["type_definitions"] = m["type_definitions"].([]any)[1:] }},
		{"computed_cycle", func(m map[string]any) {
			relation(m, "owner", map[string]any{"union": map[string]any{"child": []any{map[string]any{"this": map[string]any{}}, map[string]any{"computedUserset": map[string]any{"relation": "owner"}}}}})
		}},
		{"undefined_computed", func(m map[string]any) {
			relation(m, "owner", map[string]any{"computedUserset": map[string]any{"relation": "missing"}})
		}},
		{"missing_direct_types", func(m map[string]any) { metadata(m, "owner", map[string]any{}) }},
		{"wildcard_tupleset", func(m map[string]any) {
			metadata(m, "parent", map[string]any{"directly_related_user_types": []any{map[string]any{"type": "folder", "wildcard": map[string]any{}}}})
		}},
		{"undeclared_cel_variable", func(m map[string]any) { expression(m, "unknown == 1") }},
		{"nonboolean_cel", func(m map[string]any) { expression(m, "level") }},
		{"cel_type_error", func(m map[string]any) { expression(m, "level == 'bad'") }},
		{"empty_union", func(m map[string]any) {
			relation(m, "viewer", map[string]any{"union": map[string]any{"child": []any{}}})
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var m map[string]any
			must(t, json.Unmarshal(original, &m))
			c.change(m)
			body := raw(m)
			native := new(openfgav1.AuthorizationModel)
			must(t, protojson.Unmarshal(body, native))
			native.Id = modelID
			_, nativeErr := typesystem.NewAndValidate(context.Background(), native)
			sink := &modelSink{}
			_, wasmErr := e.Do(context.Background(), sink, wasmfga.Request{Operation: "put_model", StoreID: storeID, ModelID: modelID, Model: body})
			if (nativeErr != nil) != (wasmErr != nil) {
				t.Fatalf("OpenFGA=%v WASM=%v", nativeErr, wasmErr)
			}
			if nativeErr != nil && sink.executions != 0 {
				t.Fatal("invalid model emitted SQL")
			}
		})
	}
}
