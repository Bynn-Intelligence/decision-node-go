// Package decisionnode is the Go client for the DecisionNode API.
//
// DecisionNode answers structured questions about a piece of state with
// calibrated numbers: a truth question returns the probability that a
// statement is true, a choice question picks one option with a probability
// for each, a score question places the state on an ordered scale, and a
// number question gives a probability for each value of a grid.
//
// Decisions are made one at a time (Decide), many at once
// (DecideMany), as a live stream of frames over a WebSocket (OpenSession,
// Supervise), or asynchronously at half the price (Batches, RunBatch).
// Images are best sent by URL (ImageURL); local images are checked before any
// request and go inline when small or are uploaded once when large. Videos
// travel as the frames the caller sampled, each with its time (Video,
// VideoFrameData). Boosters (Request.Boosters) read the images first and add
// their readings to the decision as evidence (Decision.Evidence). A
// workspace's fine-tuned models are named in Model like any other.
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
// plain error returned before anything is sent. A request the API would
// refuse for its content (an image over its limits, too many options) is
// refused locally with the API's own error type, also before anything is
// sent.
package decisionnode
