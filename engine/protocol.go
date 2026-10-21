// Package engine bridges host-owned SQL transactions to the unmodified OpenFGA evaluator.
package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	openfgav1 "github.com/openfga/api/proto/openfga/v1"
	"github.com/openfga/openfga/internal/graph"
	"github.com/openfga/openfga/internal/validation"
	"github.com/openfga/openfga/pkg/server/commands"
	"github.com/openfga/openfga/pkg/storage/memory"
	"github.com/openfga/openfga/pkg/typesystem"
	"google.golang.org/protobuf/encoding/protojson"
)

const MaxTuples = 10000
const MaxInputBytes = 16 << 20

// Results contains one row set for each query statement returned by the first call.
// Only text columns cross this ABI; protobuf values use canonical protobuf JSON.
type Request struct {
	Operation string            `json:"operation"`
	Dialect   string            `json:"dialect,omitempty"`
	StoreID   string            `json:"store_id,omitempty"`
	ModelID   string            `json:"model_id,omitempty"`
	Model     json.RawMessage   `json:"model,omitempty"`
	Check     json.RawMessage   `json:"check,omitempty"`
	Writes    []json.RawMessage `json:"writes,omitempty"`
	Deletes   []json.RawMessage `json:"deletes,omitempty"`
	Results   *[][][]string     `json:"results,omitempty"`
}

type Statement struct {
	Kind            string   `json:"kind"`
	SQL             string   `json:"sql"`
	Params          []string `json:"params"`
	ExpectedChanges *int     `json:"expected_changes,omitempty"`
}

type Response struct {
	NeedsResults bool        `json:"needs_results,omitempty"`
	Statements   []Statement `json:"statements,omitempty"`
	Allowed      *bool       `json:"allowed,omitempty"`
	Error        string      `json:"error,omitempty"`
}

func Run(input []byte) []byte {
	res := func() Response {
		if len(input) > MaxInputBytes {
			return Response{Error: "input exceeds 16 MiB"}
		}
		var req Request
		if err := json.Unmarshal(input, &req); err != nil {
			return Response{Error: err.Error()}
		}
		res, err := Process(context.Background(), req)
		if err != nil {
			return Response{Error: err.Error()}
		}
		return res
	}()
	out, _ := json.Marshal(res)
	return out
}

func statement(kind, sql string, params ...string) Statement {
	if params == nil {
		params = []string{}
	}
	return Statement{Kind: kind, SQL: sql, Params: params}
}

func Process(ctx context.Context, r Request) (Response, error) {
	if r.Dialect != "" && r.Dialect != "sqlite" && r.Dialect != "turso" {
		return Response{}, errors.New("unsupported dialect (sqlite/turso only)")
	}
	if r.Operation == "schema" {
		return Response{Statements: []Statement{
			statement("execute", `CREATE TABLE IF NOT EXISTS wasmfga_models (store_id TEXT NOT NULL, model_id TEXT NOT NULL, body TEXT NOT NULL, PRIMARY KEY (store_id, model_id))`),
			statement("execute", `CREATE TABLE IF NOT EXISTS wasmfga_tuples (store_id TEXT NOT NULL, object TEXT NOT NULL, relation TEXT NOT NULL, user TEXT NOT NULL, body TEXT NOT NULL, PRIMARY KEY (store_id, object, relation, user))`),
		}}, nil
	}
	if r.StoreID == "" || len(r.StoreID) > 256 || r.ModelID == "" || len(r.ModelID) > 256 {
		return Response{}, errors.New("store_id and model_id must contain 1..256 bytes")
	}
	if r.Operation == "put_model" {
		model := new(openfgav1.AuthorizationModel)
		if err := protojson.Unmarshal(r.Model, model); err != nil {
			return Response{}, err
		}
		model.Id = r.ModelID
		if _, err := typesystem.NewAndValidate(ctx, model); err != nil {
			return Response{}, err
		}
		body, err := protojson.Marshal(model)
		if err != nil {
			return Response{}, err
		}
		return Response{Statements: []Statement{statement("execute", `INSERT INTO wasmfga_models (store_id, model_id, body) VALUES (?, ?, ?)`, r.StoreID, r.ModelID, string(body))}}, nil
	}
	if r.Operation != "check" && r.Operation != "write" {
		return Response{}, errors.New("unknown operation")
	}
	if r.Results == nil {
		queries := []Statement{statement("query", `SELECT body FROM wasmfga_models WHERE store_id = ? AND model_id = ?`, r.StoreID, r.ModelID)}
		if r.Operation == "check" {
			queries = append(queries, statement("query", `SELECT body FROM wasmfga_tuples WHERE store_id = ? ORDER BY object, relation, user LIMIT 10001`, r.StoreID))
		}
		return Response{NeedsResults: true, Statements: queries}, nil
	}
	results := *r.Results
	want := 1
	if r.Operation == "check" {
		want = 2
	}
	if len(results) != want {
		return Response{}, errors.New("incorrect number of SQL result sets")
	}
	if len(results[0]) != 1 || len(results[0][0]) != 1 {
		return Response{}, errors.New("authorization model not found or invalid result shape")
	}
	model := new(openfgav1.AuthorizationModel)
	if err := protojson.Unmarshal([]byte(results[0][0][0]), model); err != nil {
		return Response{}, err
	}
	if model.Id != r.ModelID {
		return Response{}, errors.New("model ID does not match request")
	}
	typesys, err := typesystem.NewAndValidate(ctx, model)
	if err != nil {
		return Response{}, err
	}
	if r.Operation == "write" {
		return planWrite(r, typesys)
	}
	if len(results[1]) > MaxTuples {
		return Response{}, errors.New("store snapshot exceeds 10000 tuples")
	}
	var tuples []*openfgav1.TupleKey
	for _, row := range results[1] {
		if len(row) != 1 {
			return Response{}, errors.New("invalid tuple result shape")
		}
		tk := new(openfgav1.TupleKey)
		if err := protojson.Unmarshal([]byte(row[0]), tk); err != nil {
			return Response{}, err
		}
		tuples = append(tuples, tk)
	}
	req := new(openfgav1.CheckRequest)
	if err := protojson.Unmarshal(r.Check, req); err != nil {
		return Response{}, err
	}
	if req.TupleKey == nil {
		return Response{}, errors.New("tuple_key is required")
	}
	if err := req.TupleKey.Validate(); err != nil {
		return Response{}, err
	}
	if err := req.ContextualTuples.Validate(); err != nil {
		return Response{}, err
	}
	if _, ok := openfgav1.ConsistencyPreference_name[int32(req.Consistency)]; !ok {
		return Response{}, errors.New("invalid consistency preference")
	}
	ds := memory.New(memory.WithMaxTuplesPerWrite(MaxTuples))
	defer ds.Close()
	if len(tuples) > 0 {
		if err := ds.Write(ctx, r.StoreID, nil, tuples); err != nil {
			return Response{}, err
		}
	}
	resolver := graph.NewLocalChecker(graph.WithResolveNodeBreadthLimit(1), graph.WithMaxResolutionDepth(25))
	defer resolver.Close()
	res, err := commands.NewCheckCommand(ds, resolver, typesys).Execute(ctx, &commands.CheckCommandParams{
		StoreID: r.StoreID, TupleKey: req.TupleKey, Context: req.Context, ContextualTuples: req.ContextualTuples,
		Consistency: openfgav1.ConsistencyPreference_HIGHER_CONSISTENCY,
	})
	if err != nil {
		return Response{}, err
	}
	return Response{Allowed: &res.Allowed}, nil
}

func planWrite(r Request, typesys *typesystem.TypeSystem) (Response, error) {
	if len(r.Writes)+len(r.Deletes) == 0 || len(r.Writes)+len(r.Deletes) > 100 {
		return Response{}, errors.New("write must contain 1..100 tuple changes")
	}
	res := Response{}
	seen := map[string]bool{}
	for _, group := range []struct {
		del   bool
		items []json.RawMessage
	}{{true, r.Deletes}, {false, r.Writes}} {
		for _, raw := range group.items {
			tk := new(openfgav1.TupleKey)
			if err := protojson.Unmarshal(raw, tk); err != nil {
				return Response{}, err
			}
			if err := tk.Validate(); err != nil {
				return Response{}, err
			}
			key := strings.Join([]string{tk.Object, tk.Relation, tk.User}, "\x00")
			if seen[key] {
				return Response{}, errors.New("duplicate tuple in write request")
			}
			seen[key] = true
			var s Statement
			if group.del {
				if tk.Condition != nil {
					return Response{}, errors.New("delete must not include a condition")
				}
				s = statement("execute", `DELETE FROM wasmfga_tuples WHERE store_id = ? AND object = ? AND relation = ? AND user = ?`, r.StoreID, tk.Object, tk.Relation, tk.User)
			} else {
				if err := validation.ValidateTupleForWrite(typesys, tk); err != nil {
					return Response{}, err
				}
				body, err := protojson.Marshal(tk)
				if err != nil {
					return Response{}, err
				}
				s = statement("execute", `INSERT INTO wasmfga_tuples (store_id, object, relation, user, body) VALUES (?, ?, ?, ?, ?)`, r.StoreID, tk.Object, tk.Relation, tk.User, string(body))
			}
			one := 1
			s.ExpectedChanges = &one
			res.Statements = append(res.Statements, s)
		}
	}
	return res, nil
}

// Decode is useful to native protocol implementers and tests.
func Decode(data []byte) (Response, error) {
	var r Response
	if err := json.Unmarshal(data, &r); err != nil {
		return r, err
	}
	if r.Error != "" {
		return r, fmt.Errorf("wasmfga: %s", r.Error)
	}
	return r, nil
}
