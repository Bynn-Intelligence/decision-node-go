// Package decisionnode is the Go client for the DecisionNode API.
//
// DecisionNode answers structured questions about a piece of state with
// calibrated numbers: a truth question returns the probability that a
// statement is true, a choice question picks one option with a probability
// for each, and a score question places the state on an ordered scale.
//
//	client := decisionnode.NewClient() // reads DECISIONNODE_API_KEY
//
//	d, err := client.Decide(ctx, decisionnode.Request{
//		State: "Customer: I was charged twice and nobody has replied for 3 days.",
//		Questions: decisionnode.Questions{
//			decisionnode.Q("route", decisionnode.Choice("Where should this go?", []string{"billing", "bug", "account"})),
//			decisionnode.Q("escalate", decisionnode.Truth("Escalate to a human now?")),
//		},
//	})
//	if err != nil {
//		return err
//	}
//	fmt.Println(d.Choice("route").Choice, d.Truth("escalate").Truth)
//
// Errors are typed. Match them with errors.As: every error from the API or
// the network is an *Error, and the specific types (*RateLimitError,
// *ValidationError, *RefusalError, ...) carry their own fields. A problem with
// the caller's own arguments, such as a base URL that is not https, is a
// plain error returned before anything is sent.
package decisionnode
