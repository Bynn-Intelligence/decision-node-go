package decisionnode_test

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	dn "github.com/Bynn-Intelligence/decision-node-go"
)

func TestVisionBoosterOptions(t *testing.T) {
	srv, rec := server(t, func(w http.ResponseWriter, r *http.Request, n int) {
		respond(w, 200, `{"model":"m","answers":{"q":{"type":"truth","truth":0.5}}}`, nil)
	})
	_, err := client(t, srv.URL).Decide(context.Background(), dn.Request{Questions: oneTruth(), Boosters: []any{
		dn.DepthBooster(16),
		dn.SegmentBooster(dn.SegmentOptions{Prompts: []string{"landing pad"}}),
		dn.SegmentBooster(dn.SegmentOptions{Prompts: []string{"gate", "doorway"}, MaxInstances: dn.Ptr(4), MinScore: dn.Ptr(0.1),
			PolygonPoints: dn.Ptr(8), Depth: dn.Ptr(false)}),
	}})
	if err != nil {
		t.Fatal(err)
	}
	want := `,"boosters":[{"name":"bynn_depth","options":{"grid_size":16}},{"name":"bynn_segment","options":{"prompt":"landing pad"}},` +
		`{"name":"bynn_segment","options":{"depth":false,"max_instances":4,"min_score":0.1,"polygon_points":8,"prompt":["gate","doorway"]}}]}`
	if got := string(rec.bodies[0]); !strings.HasSuffix(got, want) {
		t.Fatalf("body: %s", got)
	}
}

func TestDepthReading(t *testing.T) {
	body := `{"model":"m","answers":{},"evidence":{"bynn_depth":{"version":"bynn_depth-v1","status":"ok","latency_ms":40,"billed_calls":1,"results":[` +
		`{"image":"cam","ahead":3.21,"directions":[5.12,4.03,null,2.81,6.02,7.15,9.33],"ground":true,"grid_rows":2,"grid_cols":2,"grid":[[10.1,10.2],[2.05,3.5]],` +
		`"depth_min":1.01,"depth_max":14.98,"depth_median":4.37,"width":810,"height":1080},` +
		`{"image":"sky","ahead":null,"directions":[null,null,null,null,null,null,null],"ground":false,"grid_rows":2,"grid_cols":2,"grid":[[900,900],[900,900]],` +
		`"depth_min":900,"depth_max":900,"depth_median":900,"width":10,"height":10},` +
		`{"image":"bad","readable":false},` +
		`{"image":"old","directions":[],"ground":true,"grid_rows":2,"grid_cols":2,"grid":[],"depth_min":1,"depth_max":2,"depth_median":1,"width":1,"height":1},` +
		`{"image":"cell","ahead":1,"directions":[],"ground":true,"grid_rows":1,"grid_cols":1,"grid":[["x"]],"depth_min":1,"depth_max":2,"depth_median":1,"width":1,"height":1}]}}}`
	srv, _ := server(t, func(w http.ResponseWriter, r *http.Request, n int) { respond(w, 200, body, nil) })
	d, err := client(t, srv.URL).Decide(context.Background(), dn.Request{Questions: oneTruth()})
	if err != nil {
		t.Fatal(err)
	}
	rs := d.Evidence.Get(dn.BoosterDepth).Results
	cam, ok := rs[0].(*dn.DepthReading)
	if !ok || *cam.Ahead != 3.21 || len(cam.Directions) != 7 || cam.Directions[2] != nil || *cam.Directions[3] != 2.81 || !cam.Ground ||
		cam.Grid[1][0] != 2.05 || cam.GridRows != 2 || cam.DepthMedian != 4.37 || cam.Width != 810 {
		t.Fatalf("cam: %#v", rs[0])
	}
	if sky, ok := rs[1].(*dn.DepthReading); !ok || sky.Ahead != nil || sky.Ground {
		t.Fatalf("sky: %#v", rs[1])
	}
	for i, r := range rs[2:] {
		if _, ok := r.(*dn.UnknownReading); !ok {
			t.Fatalf("result %d: %#v", i+2, r)
		}
	}
}

// Suite 1.21's shape rules: always 7 directions, the grid's sides and the
// picture's whole numbers of 0 or more, ahead and a depth's medians possibly
// left out, a size's width and height numbers.
func TestVisionReadingShapes(t *testing.T) {
	depth := func(change string) string {
		r := `{"image":"cam","ahead":1,"directions":[1,2,3,4,5,6,7],"ground":true,"grid_rows":1,"grid_cols":1,"grid":[[1]],` +
			`"depth_min":1,"depth_max":2,"depth_median":1,"width":10,"height":10}`
		if change != "" {
			from, to, _ := strings.Cut(change, "=>")
			r = strings.Replace(r, from, to, 1)
		}
		return r
	}
	pad := `{"label":"pad","score":0.3,"box":[0.1,0.1,0.2,0.2],"polygon":[[0.1,0.1]],"area":0.01,"centroid":[0.15,0.15],"opening":null,"depth":%s,"size_m":%s}`
	segment := func(width, instance string) string {
		return `{"image":"cam","width":` + width + `,"height":10,"prompts":["pad"],"instances":[` + instance + `],"depth_source":"bynn_depth","assumed_hfov_deg":65}`
	}
	body := `{"model":"m","answers":{},"evidence":{"bynn_depth":{"status":"ok","results":[` +
		depth(`"ahead":1,=>`) + `,` + depth(`[1,2,3,4,5,6,7]=>[1,2,3,4,5,6]`) + `,` + depth(`"grid_rows":1=>"grid_rows":-1`) + `,` +
		depth(`"width":10=>"width":10.5`) + `,` + depth(`"height":10=>"height":-10`) + `]},"bynn_segment":{"status":"ok","results":[` +
		segment("10", fmt.Sprintf(pad, `{"median":4.2}`, `{"width":1,"height":2,"opening":null}`)) + `,` +
		segment("-1", fmt.Sprintf(pad, "null", "null")) + `,` +
		segment("10", fmt.Sprintf(pad, "null", `{"width":null,"height":2,"opening":null}`)) + `]}}}`
	srv, _ := server(t, func(w http.ResponseWriter, r *http.Request, n int) { respond(w, 200, body, nil) })
	d, err := client(t, srv.URL).Decide(context.Background(), dn.Request{Questions: oneTruth()})
	if err != nil {
		t.Fatal(err)
	}
	ds := d.Evidence.Get(dn.BoosterDepth).Results
	if r, ok := ds[0].(*dn.DepthReading); !ok || r.Ahead != nil {
		t.Fatalf("ahead left out: %#v", ds[0])
	}
	for i, r := range ds[1:] {
		if _, ok := r.(*dn.UnknownReading); !ok {
			t.Fatalf("depth %d: %#v", i+1, r)
		}
	}
	ss := d.Evidence.Get(dn.BoosterSegment).Results
	if r, ok := ss[0].(*dn.SegmentReading); !ok || *r.Instances[0].Depth.Median != 4.2 || r.Instances[0].Depth.OpeningMedian != nil || r.Instances[0].SizeM.Height != 2 {
		t.Fatalf("medians left out: %#v", ss[0])
	}
	for i, r := range ss[1:] {
		if _, ok := r.(*dn.UnknownReading); !ok {
			t.Fatalf("segment %d: %#v", i+1, r)
		}
	}
}

func TestSegmentReading(t *testing.T) {
	gate := `{"label":"gate","score":0.91,"box":[0.31,0.22,0.58,0.81],"polygon":[[0.31,0.22],[0.58,0.22],[0.58,0.81]],"area":0.12,"centroid":[0.45,0.47],` +
		`"opening":{"polygon":[[0.4,0.3],[0.5,0.3],[0.5,0.5]],"centroid":[0.44,0.38],"width":0.1,"height":0.2,"offset":[-0.06,-0.12],"area":0.02},` +
		`"depth":{"median":4.2,"opening_median":11.3,"opening_rim_median":null},"size_m":{"width":1.6,"height":2.1,"opening":{"width":1.4,"height":1.4}}}`
	pad := `{"label":"landing pad","score":0.3,"box":[0.1,0.1,0.2,0.2],"polygon":[[0.1,0.1],[0.2,0.1],[0.2,0.2]],"area":0.01,"centroid":[0.15,0.15],` +
		`"opening":null,"depth":null,"size_m":null}`
	body := `{"model":"m","answers":{},"evidence":{"bynn_segment":{"version":"bynn_segment-v1","status":"ok","latency_ms":60,"billed_calls":3,"results":[` +
		`{"image":"cam","width":1280,"height":720,"prompts":["gate","landing pad"],"instances":[` + gate + `,` + pad + `],"depth_source":"bynn_depth","assumed_hfov_deg":65},` +
		`{"image":"empty","width":10,"height":10,"prompts":["gate"],"instances":[],"depth_source":null,"assumed_hfov_deg":65},` +
		`{"image":"bad","readable":false},` +
		`{"image":"box","width":10,"height":10,"prompts":["gate"],"instances":[` + strings.Replace(pad, `"box":[0.1,0.1,0.2,0.2]`, `"box":[0.1,0.1]`, 1) + `],"depth_source":null,"assumed_hfov_deg":65}]}}}`
	srv, _ := server(t, func(w http.ResponseWriter, r *http.Request, n int) { respond(w, 200, body, nil) })
	d, err := client(t, srv.URL).Decide(context.Background(), dn.Request{Questions: oneTruth()})
	if err != nil {
		t.Fatal(err)
	}
	rs := d.Evidence.Get(dn.BoosterSegment).Results
	cam, ok := rs[0].(*dn.SegmentReading)
	if !ok || cam.Width != 1280 || len(cam.Prompts) != 2 || cam.DepthSource != "bynn_depth" || *cam.AssumedHFOVDeg != 65 || len(cam.Instances) != 2 {
		t.Fatalf("cam: %#v", rs[0])
	}
	g, p := cam.Instances[0], cam.Instances[1]
	if g.Label != "gate" || g.Box.X2 != 0.58 || len(g.Polygon) != 3 || g.Centroid.Y != 0.47 || g.Opening == nil || g.Opening.Offset.X != -0.06 ||
		*g.Depth.OpeningMedian != 11.3 || g.Depth.OpeningRimMedian != nil || g.SizeM.Opening.Width != 1.4 {
		t.Fatalf("gate: %+v", g)
	}
	if p.Opening != nil || p.Depth != nil || p.SizeM != nil {
		t.Fatalf("pad: %+v", p)
	}
	if e, ok := rs[1].(*dn.SegmentReading); !ok || len(e.Instances) != 0 || e.DepthSource != "" {
		t.Fatalf("empty: %#v", rs[1])
	}
	for i, r := range rs[2:] {
		if _, ok := r.(*dn.UnknownReading); !ok {
			t.Fatalf("result %d: %#v", i+2, r)
		}
	}
}
