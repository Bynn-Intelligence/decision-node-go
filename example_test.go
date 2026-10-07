package decisionnode_test

import (
	"context"
	"errors"
	"fmt"
	"log"
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
