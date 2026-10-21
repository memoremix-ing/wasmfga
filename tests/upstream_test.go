package tests

import (
	"encoding/json"
	"fmt"
	"testing"

	wasmfga "github.com/memoremix-ing/wasmfga/sdk/go"
	parser "github.com/openfga/language/pkg/go/transformer"
	"github.com/openfga/openfga/assets"
	"google.golang.org/protobuf/encoding/protojson"
	"sigs.k8s.io/yaml"
)

// Consume the upstream data directly, including multi-stage model evolution.
// Stored tuples survive model changes, just as in the standalone OpenFGA service.
func TestUpstreamCheckCorpus(t *testing.T) {
	e := openEngine(t)
	total := 0
	for _, file := range []string{"tests/consolidated_1_1_tests.yaml", "tests/abac_tests.yaml"} {
		b, err := assets.EmbedTests.ReadFile(file)
		must(t, err)
		var corpus struct {
			Tests []struct {
				Name   string
				Stages []struct {
					Model      string
					Tuples     []json.RawMessage
					Assertions []struct {
						Tuple            json.RawMessage
						Context          json.RawMessage
						ContextualTuples []json.RawMessage `json:"contextualTuples"`
						Expectation      bool
						ErrorCode        int `json:"errorCode"`
					} `json:"checkAssertions"`
				}
			}
		}
		must(t, yaml.Unmarshal(b, &corpus))
		for _, test := range corpus.Tests {
			t.Run(test.Name, func(t *testing.T) {
				db := openDB(t)
				tx, err := db.Begin()
				must(t, err)
				defer tx.Rollback()
				_, err = do(e, tx, wasmfga.Request{Operation: "schema"})
				must(t, err)
				var accumulated []json.RawMessage
				for stageIndex, stage := range test.Stages {
					model, err := parser.TransformDSLToProto(stage.Model)
					must(t, err)
					modelBytes, err := protojson.Marshal(model)
					must(t, err)
					id := fmt.Sprintf("%026d", stageIndex+1)
					_, err = do(e, tx, wasmfga.Request{Operation: "put_model", StoreID: storeID, ModelID: id, Model: modelBytes})
					must(t, err)
					for start := 0; start < len(stage.Tuples); start += 100 {
						end := min(start+100, len(stage.Tuples))
						_, err = do(e, tx, wasmfga.Request{Operation: "write", StoreID: storeID, ModelID: id, Writes: stage.Tuples[start:end]})
						must(t, err)
					}
					accumulated = append(accumulated, stage.Tuples...)
					oracle := newOracle(t, modelBytes)
					for i, a := range stage.Assertions {
						query := raw(map[string]any{"tuple_key": a.Tuple, "context": a.Context, "contextual_tuples": map[string]any{"tuple_keys": a.ContextualTuples}})
						want, nativeErr := oracle.check(accumulated, query)
						if (nativeErr != nil) != (a.ErrorCode != 0) || (nativeErr == nil && want != a.Expectation) {
							t.Fatalf("upstream fixture/native mismatch stage=%d assertion=%d: %v,%v", stageIndex, i, want, nativeErr)
						}
						got, wasmErr := do(e, tx, wasmfga.Request{Operation: "check", StoreID: storeID, ModelID: id, Check: query})
						if (wasmErr != nil) != (nativeErr != nil) || (wasmErr == nil && (got.Allowed == nil || *got.Allowed != want)) {
							t.Fatalf("stage=%d assertion=%d WASM=%v,%v OpenFGA=%v,%v", stageIndex, i, got.Allowed, wasmErr, want, nativeErr)
						}
						total++
					}
				}
			})
		}
	}
	t.Logf("compared %d upstream Check assertions with native OpenFGA", total)
}
