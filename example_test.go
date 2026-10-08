package decisionnode_test

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"time"

	dn "github.com/Bynn-Intelligence/decision-node-go"
)

func Example() {
	client := dn.NewClient() // reads DECISIONNODE_API_KEY
	ctx := context.Background()

	d, err := client.Decide(ctx, dn.Request{
		State: map[string]any{"ticket": "I was charged twice and nobody has replied for 3 days."},
		Questions: dn.Questions{
			dn.Q("route", dn.Choice("Where should this go?", dn.Options{
				{Name: "billing", Description: "money"},
				{Name: "bug", Description: "broken"},
				{Name: "account", Description: "login"},
			})),
			dn.Q("urgency", dn.Score("How urgent is this?", []string{"routine", "today", "urgent", "critical"})),
			dn.Q("escalate", dn.Truth("Escalate to a human now?")),
		},
	})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(d.Choice("route").Choice, d.Choice("route").Probabilities)
	fmt.Println(d.Score("urgency").Score)
	fmt.Println(d.Truth("escalate").Truth)
}

func ExampleClient_Check() {
	client := dn.NewClient()
	a, err := client.Check(context.Background(), dn.CheckRequest{
		State:        "Customer: please refund the second charge.",
		Instructions: "Is the customer asking for money back?",
		IfTrue:       "a refund, a credit or a chargeback",
		IfFalse:      "anything else",
	})
	if err != nil {
		log.Fatal(err)
	}
	if a.Truth > 0.8 {
		fmt.Println("route to refunds")
	}
}

func ExampleClient_DecideMany() {
	client := dn.NewClient()
	tickets := []string{"Card declined", "App crashes on start", "Forgot my password"}

	var reqs []dn.Request
	for _, t := range tickets {
		reqs = append(reqs, dn.Request{
			State:     t,
			Questions: dn.Questions{dn.Q("route", dn.Choice("Where should this go?", []string{"billing", "bug", "account"}))},
		})
	}
	results, err := client.DecideMany(context.Background(), reqs, dn.DefaultConcurrency)
	if err != nil {
		log.Fatal(err)
	}
	for i, r := range results {
		if r.Err != nil {
			fmt.Println(tickets[i], "failed:", r.Err)
			continue
		}
		fmt.Println(tickets[i], "->", r.Decision.Choice("route").Choice)
	}
}

func ExampleClient_Gate() {
	client := dn.NewClient()
	d, err := client.Gate(context.Background(), dn.GateRequest{
		Action:  "DROP TABLE invoices;",
		Context: map[string]string{"environment": "production"},
	})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(d.Choice("verdict").Choice, d.Score("risk").Score, d.Truth("touches_production").Truth)
}

func ExampleImageFile() {
	client := dn.NewClient()
	receipt, err := dn.ImageFile("receipt", "receipt.jpg")
	if err != nil {
		log.Fatal(err)
	}
	d, err := client.Decide(context.Background(), dn.Request{
		State:  map[string]any{"claim": map[string]string{"total": "740.00", "currency": "SEK"}},
		Images: []dn.Image{receipt},
		Questions: dn.Questions{
			dn.Q("total_matches", dn.Truth("Does the total on the receipt equal the total in the claim?")),
		},
	})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(d.Truth("total_matches").Truth)
}

func ExampleError() {
	client := dn.NewClient(dn.WithMaxRetries(0))
	_, err := client.Decide(context.Background(), dn.Request{
		State:     "...",
		Questions: dn.Questions{dn.Q("q", dn.Truth("..."))},
	})

	var rate *dn.RateLimitError
	var refusal *dn.RefusalError
	var apiErr *dn.Error
	switch {
	case errors.As(err, &rate):
		time.Sleep(rate.RetryAfter)
	case errors.As(err, &refusal):
		fmt.Println("refused:", refusal.Category)
	case errors.As(err, &apiErr):
		fmt.Println(apiErr.Status, apiErr.RequestID, apiErr.Reason)
	}
}

func ExampleImageURL() {
	client := dn.NewClient()
	// A URL is the fastest image input: nothing is uploaded.
	d, err := client.Decide(context.Background(), dn.Request{
		State:  "Claim 4411: total 740.00 SEK.",
		Images: []dn.Image{dn.ImageURL("receipt", "https://files.example.com/claims/4411/receipt.jpg")},
		Questions: dn.Questions{
			dn.Q("total_matches", dn.Truth("Does the total on the receipt equal the total in the claim?")),
		},
	})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(d.Truth("total_matches").Truth)
}

func ExampleClient_Number() {
	client := dn.NewClient()
	a, err := client.Number(context.Background(), dn.NumberRequest{
		State:        "Drone frame over row B4 of the north car park.",
		Instructions: "How many cars are visible?",
		Min:          0,
		Max:          50,
	})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(a.Number, a.Expected, a.Confidence, a.Probabilities)
}

func ExampleClient_OpenSession() {
	client := dn.NewClient()
	ctx := context.Background()
	s, err := client.OpenSession(ctx, dn.SessionRequest{
		Instructions: "You are the mission supervisor of a survey drone.",
		State:        map[string]any{"mission": "Survey the north field at 40 m"},
		Questions: dn.Questions{
			dn.Q("abort", dn.Truth("Should the mission be aborted now?")),
		},
	})
	if err != nil {
		log.Fatal(err)
	}
	defer func() { _, _ = s.End(ctx) }()
	if err := s.Connect(ctx); err != nil {
		log.Fatal(err)
	}
	if _, err := s.Send(ctx, dn.Frame{Frame: map[string]any{"battery": 0.31, "wind_mps": 11.4}}); err != nil {
		log.Fatal(err)
	}
	for ev := range s.Replies() {
		switch e := ev.(type) {
		case *dn.Reply:
			fmt.Println(e.Seq, e.Answers["abort"].(*dn.TruthAnswer).Truth)
		case *dn.FrameError:
			fmt.Println("frame", e.Seq, e.ErrorType, e.Message)
		case *dn.SessionClosed:
			fmt.Println("closed", e.Code, e.Reopen)
		}
	}
}

func ExampleClient_Supervise() {
	client := dn.NewClient()
	feed := make(chan dn.Frame)
	go func() {
		defer close(feed)
		for _, battery := range []float64{0.9, 0.6, 0.3} {
			feed <- dn.Frame{Frame: map[string]any{"battery": battery}}
			time.Sleep(time.Second)
		}
	}()
	closed, err := client.Supervise(context.Background(), dn.SessionRequest{
		Questions: dn.Questions{dn.Q("abort", dn.Truth("Should the mission be aborted now?"))},
	}, feed, func(ev dn.SessionEvent) {
		if r, ok := ev.(*dn.Reply); ok {
			fmt.Println(r.Seq, r.Answers["abort"].(*dn.TruthAnswer).Truth)
		}
	})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(closed)
}

func ExampleClient_RunBatch() {
	client := dn.NewClient()
	var reqs []dn.BatchRequest
	for i, ticket := range []string{"I was charged twice.", "The app crashes on start."} {
		reqs = append(reqs, dn.BatchRequest{
			CustomID: dn.Ptr(fmt.Sprintf("ticket-%d", i+1)),
			Request: dn.Request{
				State:     ticket,
				Questions: dn.Questions{dn.Q("route", dn.Choice("Where should this go?", []string{"billing", "bug"}))},
			},
		})
	}
	batch, results, err := client.RunBatch(context.Background(), reqs, dn.RunBatchOptions{PollInterval: 30 * time.Second})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(batch.State, batch.Counters.Completed)
	for _, r := range results {
		if r.Decision != nil {
			fmt.Println(r.CustomID, r.Decision.Choice("route").Choice)
		} else {
			fmt.Println(r.CustomID, r.Status, r.StatusText, r.Err)
		}
	}
}

func ExampleVideo() {
	client := dn.NewClient()
	ctx := context.Background()

	// Frames a camera already holds, each with its time in seconds.
	var frames []dn.VideoFrame
	for i, path := range []string{"dock-0000.jpg", "dock-0001.jpg", "dock-0002.jpg"} {
		b, err := os.ReadFile(path)
		if err != nil {
			log.Fatal(err)
		}
		frames = append(frames, dn.VideoFrameData(float64(i)*0.5, b, ""))
	}

	d, err := client.Decide(ctx, dn.Request{
		State:  "Warehouse camera 4, loading dock, shift B.",
		Videos: []any{dn.Video{ID: "dock", Frames: frames}},
		Questions: dn.Questions{
			dn.Q("fall", dn.Truth("Does a person fall in the video?")),
			dn.Q("forklifts", dn.NumberWith("How many forklifts pass the door?", 0, 50, nil)),
		},
	})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(d.Truth("fall").Truth, d.Number("forklifts").Number, d.Warnings)
}
