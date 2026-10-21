//go:build !wasip1

// Package upstream binds the unmodified upstream Go Check command and local
// resolver directly to the caller's transaction. It does not hydrate snapshots.
// This is a Check-only adapter for wasmfga's schema, not OpenFGA's server storage.
package upstream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"

	openfgav1 "github.com/openfga/api/proto/openfga/v1"
	"github.com/openfga/openfga/internal/graph"
	"github.com/openfga/openfga/pkg/server/commands"
	"github.com/openfga/openfga/pkg/storage"
	"github.com/openfga/openfga/pkg/tuple"
	"github.com/openfga/openfga/pkg/typesystem"
	engine "github.com/openfga/openfga/wasmfga"
	"google.golang.org/protobuf/encoding/protojson"
)

// Prepared retains a validated immutable model and resolver, but no tuple or
// decision cache. It is bound to one caller-owned transaction, store and model.
// Do not use it after that transaction ends. Close only releases the resolver.
type Prepared struct {
	reader   *reader
	typesys  *typesystem.TypeSystem
	resolver *graph.LocalChecker
	store    string
}

func Prepare(ctx context.Context, tx engine.Transaction, store, id string) (*Prepared, error) {
	if tx == nil {
		return nil, errors.New("an active transaction binding is required")
	}
	rows, err := tx.Query(ctx, "SELECT body FROM wasmfga_models WHERE store_id = ? AND model_id = ?", []string{store, id})
	if err != nil {
		return nil, err
	}
	if len(rows) != 1 || len(rows[0]) != 1 {
		return nil, errors.New("authorization model not found or invalid result shape")
	}
	model := new(openfgav1.AuthorizationModel)
	if err := protojson.Unmarshal([]byte(rows[0][0]), model); err != nil {
		return nil, err
	}
	if model.Id != id {
		return nil, errors.New("model ID does not match request")
	}
	typesys, err := typesystem.NewAndValidate(ctx, model)
	if err != nil {
		return nil, err
	}
	return &Prepared{reader: &reader{tx: tx}, typesys: typesys, store: store,
		resolver: graph.NewLocalChecker(graph.WithResolveNodeBreadthLimit(1), graph.WithMaxResolutionDepth(25))}, nil
}

func (p *Prepared) Close() { p.resolver.Close() }

func (p *Prepared) Check(ctx context.Context, query json.RawMessage) (bool, error) {
	req := new(openfgav1.CheckRequest)
	if err := protojson.Unmarshal(query, req); err != nil {
		return false, err
	}
	return p.CheckRequest(ctx, req)
}

// CheckRequest accepts upstream's native protobuf request without JSON transport.
func (p *Prepared) CheckRequest(ctx context.Context, req *openfgav1.CheckRequest) (bool, error) {
	if req.TupleKey == nil {
		return false, errors.New("tuple_key is required")
	}
	if err := req.TupleKey.Validate(); err != nil {
		return false, err
	}
	if err := req.ContextualTuples.Validate(); err != nil {
		return false, err
	}
	if _, ok := openfgav1.ConsistencyPreference_name[int32(req.Consistency)]; !ok {
		return false, errors.New("invalid consistency preference")
	}
	res, err := commands.NewCheckCommand(p.reader, p.resolver, p.typesys).Execute(ctx, &commands.CheckCommandParams{
		StoreID: p.store, TupleKey: req.TupleKey, Context: req.Context, ContextualTuples: req.ContextualTuples,
		Consistency: openfgav1.ConsistencyPreference_HIGHER_CONSISTENCY,
	})
	if err != nil {
		return false, err
	}
	return res.Allowed, nil
}

// Check reloads and validates the model on every call. Prepare exposes the
// separate baseline with immutable-model compilation outside the timed checks.
func Check(ctx context.Context, tx engine.Transaction, store, id string, query json.RawMessage) (bool, error) {
	p, err := Prepare(ctx, tx, store, id)
	if err != nil {
		return false, err
	}
	defer p.Close()
	return p.Check(ctx, query)
}

func CheckRequest(ctx context.Context, tx engine.Transaction, store, id string, req *openfgav1.CheckRequest) (bool, error) {
	p, err := Prepare(ctx, tx, store, id)
	if err != nil {
		return false, err
	}
	defer p.Close()
	return p.CheckRequest(ctx, req)
}

type reader struct {
	tx engine.Transaction
	mu sync.Mutex
}

var _ storage.RelationshipTupleReader = (*reader)(nil)

func (r *reader) rows(ctx context.Context, store string, f storage.ReadFilter) ([]*openfgav1.Tuple, error) {
	query := "SELECT body FROM wasmfga_tuples WHERE store_id = ?"
	params := []string{store}
	for _, field := range []struct{ name, value string }{{"object", f.Object}, {"relation", f.Relation}, {"user", f.User}} {
		if field.value != "" {
			if field.name == "object" && strings.HasSuffix(field.value, ":") {
				query += " AND object >= ? AND object < ?"
				params = append(params, field.value, strings.TrimSuffix(field.value, ":")+";")
			} else {
				query += " AND " + field.name + " = ?"
				params = append(params, field.value)
			}
		}
	}
	query += " ORDER BY object, relation, user"
	// Upstream can dispatch branches concurrently; the host binding need not be
	// thread-safe. Materialize and close each SQL result before another query.
	r.mu.Lock()
	rows, err := r.tx.Query(ctx, query, params)
	r.mu.Unlock()
	if err != nil {
		return nil, err
	}
	out := make([]*openfgav1.Tuple, 0, len(rows))
	for _, row := range rows {
		if len(row) != 1 {
			return nil, errors.New("invalid tuple result shape")
		}
		tk := new(openfgav1.TupleKey)
		if err := protojson.Unmarshal([]byte(row[0]), tk); err != nil {
			return nil, err
		}
		if len(f.Conditions) > 0 && !slices.Contains(f.Conditions, tk.GetCondition().GetName()) {
			continue
		}
		out = append(out, &openfgav1.Tuple{Key: tk})
	}
	return out, nil
}

func (r *reader) Read(ctx context.Context, store string, f storage.ReadFilter, _ storage.ReadOptions) (storage.TupleIterator, error) {
	rows, err := r.rows(ctx, store, f)
	if err != nil {
		return nil, err
	}
	return storage.NewStaticTupleIterator(rows), nil
}
func (r *reader) ReadPage(context.Context, string, storage.ReadFilter, storage.ReadPageOptions) ([]*openfgav1.Tuple, string, error) {
	return nil, "", errors.New("pagination is outside this Check-only transaction adapter")
}
func (r *reader) ReadUserTuple(ctx context.Context, store string, f storage.ReadUserTupleFilter, _ storage.ReadUserTupleOptions) (*openfgav1.Tuple, error) {
	rows, err := r.rows(ctx, store, f)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, storage.ErrNotFound
	}
	return rows[0], nil
}
func (r *reader) ReadUsersetTuples(ctx context.Context, store string, f storage.ReadUsersetTuplesFilter, _ storage.ReadUsersetTuplesOptions) (storage.TupleIterator, error) {
	rows, err := r.rows(ctx, store, storage.ReadFilter{Object: f.Object, Relation: f.Relation, Conditions: f.Conditions})
	if err != nil {
		return nil, err
	}
	out := make([]*openfgav1.Tuple, 0, len(rows))
	for _, t := range rows {
		u := t.GetKey().GetUser()
		if tuple.GetUserTypeFromUser(u) != tuple.UserSet {
			continue
		}
		if len(f.AllowedUserTypeRestrictions) == 0 {
			out = append(out, t)
			continue
		}
		for _, ref := range f.AllowedUserTypeRestrictions {
			if ref.GetType() != tuple.GetType(u) {
				continue
			}
			if (ref.GetWildcard() != nil && tuple.IsTypedWildcard(u)) || (ref.GetRelation() != "" && ref.GetRelation() == tuple.GetRelation(u)) {
				out = append(out, t)
				break
			}
		}
	}
	return storage.NewStaticTupleIterator(out), nil
}
func (r *reader) ReadStartingWithUser(ctx context.Context, store string, f storage.ReadStartingWithUserFilter, _ storage.ReadStartingWithUserOptions) (storage.TupleIterator, error) {
	if f.ObjectType == "" {
		return nil, fmt.Errorf("object type is required")
	}
	rows, err := r.rows(ctx, store, storage.ReadFilter{Object: f.ObjectType + ":", Relation: f.Relation, Conditions: f.Conditions})
	if err != nil {
		return nil, err
	}
	out := make([]*openfgav1.Tuple, 0, len(rows))
	for _, t := range rows {
		_, id := tuple.SplitObject(t.GetKey().GetObject())
		if f.ObjectIDs != nil && !f.ObjectIDs.Exists(id) {
			continue
		}
		for _, u := range f.UserFilter {
			subject := u.GetObject()
			if u.GetRelation() != "" {
				subject += "#" + u.GetRelation()
			}
			if subject == t.GetKey().GetUser() {
				out = append(out, t)
				break
			}
		}
	}
	return storage.NewStaticTupleIterator(out), nil
}
