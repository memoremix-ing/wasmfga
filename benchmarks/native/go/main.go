// Native benchmark host: no WASM runtime or RPC in measured calls.
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"runtime"
	"sort"
	"time"

	openfgav1 "github.com/openfga/api/proto/openfga/v1"
	"github.com/openfga/openfga/pkg/server"
	"github.com/openfga/openfga/pkg/storage/memory"
	"github.com/openfga/openfga/pkg/typesystem"
	engine "github.com/openfga/openfga/wasmfga"
	"github.com/openfga/openfga/wasmfga/upstream"
	"google.golang.org/protobuf/encoding/protojson"
	_ "modernc.org/sqlite"
)

const store = "01ARZ3NDEKTSV4RRFFQ69G5FAV"
const id = "01ARZ3NDEKTSV4RRFFQ69G5FAW"

type fixture struct {
	Name          string   `json:"name"`
	ModelBody     string   `json:"modelBody"`
	CheckBody     string   `json:"checkBody"`
	TupleBodies   []string `json:"tupleBodies"`
	HydratedInput string   `json:"hydratedInput"`
	Allowed       bool     `json:"allowed"`
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}
func write(path string, value any) {
	b, err := json.Marshal(value)
	must(err)
	must(os.WriteFile(path, b, 0644))
}
func oracle(fixtures []fixture, output string) {
	ctx := context.Background()
	for _, w := range fixtures {
		m := new(openfgav1.AuthorizationModel)
		must(protojson.Unmarshal([]byte(w.ModelBody), m))
		_, err := typesystem.NewAndValidate(ctx, m)
		must(err)
		ds := memory.New(memory.WithMaxTuplesPerWrite(10000))
		must(ds.WriteAuthorizationModel(ctx, store, m))
		keys := []*openfgav1.TupleKey{}
		for _, body := range w.TupleBodies {
			tk := new(openfgav1.TupleKey)
			must(protojson.Unmarshal([]byte(body), tk))
			keys = append(keys, tk)
		}
		if len(keys) > 0 {
			must(ds.Write(ctx, store, nil, keys))
		}
		s, err := server.NewServerWithOpts(server.WithDatastore(ds))
		must(err)
		q := new(openfgav1.CheckRequest)
		must(protojson.Unmarshal([]byte(w.CheckBody), q))
		q.StoreId = store
		q.AuthorizationModelId = id
		res, err := s.Check(ctx, q)
		must(err)
		if res.Allowed != w.Allowed {
			panic("fixture disagrees with native OpenFGA: " + w.Name)
		}
		s.Close()
		ds.Close()
	}
	write(output, map[string]any{"passed": true, "workloads": len(fixtures), "oracle": "unmodified native OpenFGA Server.Check"})
}

func main() {
	file := flag.String("fixtures", "dist/native/fixtures.json", "")
	output := flag.String("output", "", "")
	name := flag.String("engine", "go", "")
	mode := flag.String("mode", "sqlite", "")
	gate := flag.Bool("gate", false, "")
	isOracle := flag.Bool("oracle", false, "")
	scale := flag.Float64("scale", 1, "")
	flag.Parse()
	if *output == "" {
		panic("output required")
	}
	bytes, err := os.ReadFile(*file)
	must(err)
	var fixtures []fixture
	must(json.Unmarshal(bytes, &fixtures))
	if *isOracle {
		oracle(fixtures, *output)
		return
	}
	if (*name != "go" && *name != "upstream" && *name != "upstream-cached") || (*mode != "json" && *mode != "sqlite") || (*mode == "json" && *name != "go") || *scale <= 0 {
		panic("invalid engine/mode/scale")
	}
	ctx := context.Background()
	measurements := []map[string]any{}
	var sqliteVersion string
	for _, w := range fixtures {
		db, err := sql.Open("sqlite", ":memory:")
		must(err)
		db.SetMaxOpenConns(1)
		must(db.QueryRow("SELECT sqlite_version()").Scan(&sqliteVersion))
		tx, err := db.Begin()
		must(err)
		binding := engine.SQLTransaction{Tx: tx}
		_, err = engine.Do(ctx, binding, engine.Request{Operation: "schema"})
		must(err)
		_, err = tx.Exec("INSERT INTO wasmfga_models VALUES (?, ?, ?)", store, id, w.ModelBody)
		must(err)
		insert, err := tx.Prepare("INSERT INTO wasmfga_tuples VALUES (?, ?, ?, ?, ?)")
		must(err)
		for _, body := range w.TupleBodies {
			var t struct{ Object, Relation, User string }
			must(json.Unmarshal([]byte(body), &t))
			_, err = insert.Exec(store, t.Object, t.Relation, t.User, body)
			must(err)
		}
		must(insert.Close())
		var prepared *upstream.Prepared
		prepStart := time.Now()
		if *name == "upstream-cached" {
			prepared, err = upstream.Prepare(ctx, binding, store, id)
			must(err)
		}
		prepareUs := float64(time.Since(prepStart).Nanoseconds()) / 1000
		request := engine.Request{Operation: "check", StoreID: store, ModelID: id, Check: json.RawMessage(w.CheckBody)}
		typedQuery := new(openfgav1.CheckRequest)
		must(protojson.Unmarshal([]byte(w.CheckBody), typedQuery))
		input := []byte(w.HydratedInput)
		run := func() bool {
			if *mode == "json" {
				res, err := engine.Decode(engine.Run(input))
				must(err)
				if res.Allowed == nil {
					panic("missing decision")
				}
				return *res.Allowed
			}
			if *name == "upstream-cached" {
				v, err := prepared.CheckRequest(ctx, typedQuery)
				must(err)
				return v
			}
			if *name == "upstream" {
				v, err := upstream.CheckRequest(ctx, binding, store, id, typedQuery)
				must(err)
				return v
			}
			res, err := engine.Do(ctx, binding, request)
			must(err)
			if res.Allowed == nil {
				panic("missing decision")
			}
			return *res.Allowed
		}
		verify := func(got bool) {
			if got != w.Allowed {
				panic("incorrect decision: " + w.Name)
			}
		}
		firstStart := time.Now()
		first := run()
		firstUs := float64(time.Since(firstStart).Nanoseconds()) / 1000
		verify(first)
		iterations, warmup := 5000, 500
		if len(w.TupleBodies) >= 10000 {
			iterations, warmup = 50, 10
		} else if len(w.TupleBodies) >= 1000 {
			iterations, warmup = 500, 100
		}
		iterations = max(10, int(float64(iterations)*(*scale)))
		warmup = max(10, int(float64(warmup)*(*scale)))
		if *gate {
			iterations, warmup = 1, 0
		}
		for i := 0; i < warmup; i++ {
			verify(run())
		}
		durations := make([]float64, iterations)
		for i := range iterations {
			start := time.Now()
			got := run()
			durations[i] = float64(time.Since(start).Nanoseconds()) / 1000
			verify(got)
		}
		sort.Float64s(durations)
		var sum float64
		for _, v := range durations {
			sum += v
		}
		measurements = append(measurements, map[string]any{"workload": w.Name, "tuples": len(w.TupleBodies), "snapshotInputBytes": len(input), "iterations": iterations, "warmup": warmup, "firstCheckUs": firstUs, "prepareUs": prepareUs, "medianUs": durations[len(durations)/2], "p95Us": durations[min(len(durations)-1, len(durations)*95/100)], "meanUs": sum / float64(len(durations))})
		if prepared != nil {
			prepared.Close()
		}
		must(tx.Rollback())
		must(db.Close())
	}
	write(*output, map[string]any{"engine": *name, "mode": *mode, "measurements": measurements, "sqliteVersion": sqliteVersion, "goVersion": runtime.Version(), "gomaxprocs": runtime.GOMAXPROCS(0), "driver": "modernc.org/sqlite"})
	fmt.Printf("%s %s: %d workloads verified\n", *name, *mode, len(measurements))
}
