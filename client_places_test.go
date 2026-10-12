package decisionnode_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	dn "github.com/Bynn-Intelligence/decision-node-go"
)

const placesBody = `{"model":"m","answers":{` +
	`"cars":{"type":"box","count":2,"present":0.99,"complete":0.94,"boxes":[` +
	`{"x1":112,"y1":400,"x2":388,"y2":590,"confidence":0.91},{"x1":402,"y1":395,"x2":660,"y2":598,"confidence":0.90}],` +
	`"image":"lot","width":1920,"height":1080},` +
	`"people":{"type":"point","count":0,"present":0.03,"complete":0.99,"points":[],"image":"lot","width":1920,"height":1080},` +
	`"nearest":{"type":"box","x1":402,"y1":395,"x2":660,"y2":598,"present":0.98,"confidence":0.9,"image":"lot","width":1920,"height":1080},` +
	`"bad":{"type":"box","count":1.5,"present":0.9,"complete":0.9,"boxes":[],"image":"lot","width":1920,"height":1080},` +
	`"crossed":{"type":"point","count":1,"present":0.9,"complete":0.9,"boxes":[{"x1":1,"y1":2,"x2":3,"y2":4,"confidence":0.5}],"image":"lot","width":1920,"height":1080}},` +
	`"usage":{"input_tokens":410,"output_tokens":0}}`

func TestPlaces(t *testing.T) {
	srv, rec := server(t, func(w http.ResponseWriter, r *http.Request, n int) {
		respond(w, 200, placesBody, nil)
	})
	c := client(t, srv.URL)
	lot := dn.ImageURL("lot", "https://files.example.com/lot/0917.jpg")
	d, err := c.Decide(context.Background(), dn.Request{
		Images: []dn.Image{lot},
		Questions: dn.Questions{
			dn.Q("cars", dn.Boxes("Every car in the lot.", 8)),
			dn.Q("people", dn.PointsWith("Each person.", "lot", "normalized", 6)),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	body := string(rec.bodies[0])
	for _, want := range []string{`"cars":{"type":"box","instructions":"Every car in the lot.","max":8}`,
		`"people":{"type":"point","instructions":"Each person.","max":6,"image":"lot","units":"normalized"}`} {
		if !strings.Contains(body, want) {
			t.Fatalf("body %s lacks %s", body, want)
		}
	}
	cars := d.Boxes("cars")
	if cars == nil || cars.Count != 2 || cars.Complete != 0.94 || len(cars.Boxes) != 2 || cars.Boxes[1].X1 != 402 || cars.Boxes[1].Confidence != 0.90 {
		t.Fatalf("cars: %#v", d.Answers["cars"])
	}
	people := d.Points("people")
	if people == nil || people.Count != 0 || people.Points == nil || len(people.Points) != 0 {
		t.Fatalf("people: %#v", d.Answers["people"])
	}
	if d.Box("nearest") == nil {
		t.Fatalf("nearest: %#v", d.Answers["nearest"])
	}
	for _, id := range []string{"bad", "crossed"} {
		if a, ok := d.Answers[id].(*dn.UnknownAnswer); !ok || a.Raw()["image"] != "lot" {
			t.Fatalf("%s: %#v", id, d.Answers[id])
		}
	}
}

func TestPlacesMaxCheckedLocally(t *testing.T) {
	srv, rec := server(t, func(w http.ResponseWriter, r *http.Request, n int) {
		respond(w, 200, placesBody, nil)
	})
	c := client(t, srv.URL)
	lot := dn.ImageURL("lot", "https://files.example.com/lot/0917.jpg")
	odd := dn.Box("Every car.")
	odd.Max = 2.5
	for _, tc := range []struct {
		q    dn.Question
		kind string
	}{
		{dn.Boxes("Every car.", 1), "greater_than_equal"},
		{dn.Points("Each person.", 17), "less_than_equal"},
		{odd, "int_type"},
	} {
		_, err := c.Decide(context.Background(), dn.Request{Images: []dn.Image{lot}, Questions: dn.Questions{dn.Q("q", tc.q)}})
		var ve *dn.ValidationError
		if !errors.As(err, &ve) || len(ve.Entries) != 1 {
			t.Fatalf("%s: %v", tc.kind, err)
		}
		e, _ := ve.Entries[0].(map[string]any)
		if e["type"] != tc.kind || !strings.Contains(ve.Reason, "max") {
			t.Fatalf("%s: %#v (%s)", tc.kind, e, ve.Reason)
		}
	}
	if len(rec.bodies) != 0 {
		t.Fatalf("sent %d requests", len(rec.bodies))
	}
}
