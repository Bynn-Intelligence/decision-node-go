# DecisionNode Go SDK

The Go client for the [DecisionNode](https://decisionnode.com) API: ask structured questions about any state and get calibrated numbers back.

- Three question types: `truth` (how likely a statement is true), `choice` (one option of a set) and `score` (a place on an ordered scale).
- Typed answers and typed errors, matched with `errors.As`.
- Retries, timeouts and concurrency built in. Standard library only.

## Install

```sh
go get github.com/Bynn-Intelligence/decision-node-go
```

Requires Go 1.22 or later.

## Set your API key

```sh
export DECISIONNODE_API_KEY="dn_..."
```

## Quickstart

```go
package main

import (
	"context"
	"fmt"
	"log"

	dn "github.com/Bynn-Intelligence/decision-node-go"
)

func main() {
	client := dn.NewClient() // reads DECISIONNODE_API_KEY

	d, err := client.Decide(context.Background(), dn.Request{
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

	fmt.Println(d.Choice("route").Choice)          // billing
	fmt.Println(d.Choice("route").Probabilities)   // map[account:0.05 billing:0.87 bug:0.08]
	fmt.Println(d.Score("urgency").Score)          // 2.31
	fmt.Println(d.Truth("escalate").Truth)         // 0.94
}
```

There are no thresholds in the API: what 0.94 means is your code's decision.

## Questions

| Builder | Asks | Answer |
|---|---|---|
| `dn.Truth(instructions)` | Is this true? | `*TruthAnswer`: `Truth` (0 to 1) |
| `dn.TruthWith(instructions, ifTrue, ifFalse)` | The same, with what a yes and a no mean (either may be `nil`) | `*TruthAnswer` |
| `dn.Choice(instructions, options)` | Which option fits? | `*ChoiceAnswer`: `Choice`, `Confidence`, `Probabilities` by option name |
| `dn.Score(instructions, levels)` | Where on this scale, low to high? | `*ScoreAnswer`: `Score`, `Confidence`, `Probabilities` and `Legend` by level index |

`options` is `dn.Options` (ordered names with descriptions), a `map[string]string` of name to description, or a `[]string` of names. `levels` is a slice of levels, 1 to 10.

`State`, instructions, descriptions and levels take a string or any value that encodes to JSON: a struct, a map, a slice or a `json.RawMessage`. A question in its JSON wire form, such as a `map[string]any`, works wherever a builder does:

```go
dn.Q("escalate", map[string]any{"type": "truth", "instructions": "Escalate to a human now?"})
```

Question ids are any string, and answers come back under the same id. Read them with `d.Truth(id)`, `d.Choice(id)` and `d.Score(id)` (nil when the answer is of another type), or `d.Answer(id)` for the `dn.Answer` interface. An answer of a type this version does not know yet is an `*UnknownAnswer` holding the raw fields. So is an answer of a known type whose fields are missing or of the wrong type: the decision itself does not fail.

Numbers are passed through exactly as received: a probability list may read 0.8200000000000001 or sum to 0.99.

## The client

| Method | Does |
|---|---|
| `Decide(ctx, dn.Request)` | One request with any mix of questions. Returns a `*Decision`. |
| `DecideMany(ctx, []dn.Request, concurrency)` | Many requests, at most `concurrency` at a time (pass `dn.DefaultConcurrency`, 8, for the default). Returns one `DecisionResult` per request, in input order. One failure does not stop the others. Every request is checked first: a concurrency below 1 or an invalid request returns an error and nothing is sent, not even the valid requests. |
| `Check(ctx, dn.CheckRequest)` | One truth question. Returns a `*TruthAnswer`. |
| `Classify(ctx, dn.ClassifyRequest)` | One choice question. Returns a `*ChoiceAnswer`. |
| `Score(ctx, dn.ScoreRequest)` | One score question. Returns a `*ScoreAnswer`. |
| `Gate(ctx, dn.GateRequest)` | Judges an action before it runs: `risk` (score), `touches_production` (truth) and `verdict` (allow, confirm or block). |
| `Models(ctx)` | The models your key can use, with their published limits, as `[]dn.ModelInfo`. |
| `DecideRaw(ctx, body)` | Sends a body exactly as given and returns the parsed response. For API fields this version does not know yet. |

`Check`, `Classify`, `Score` and `Gate` take `Images` and `Model` like `Decide`. `Check`, `Classify` and `Score` promise a typed answer, so when their own answer is missing or malformed they return a `*ServerError` with the response's status, `RequestID` and `Reason`. `Decide` returns such an answer as an `*UnknownAnswer` instead.

A `*Decision` holds `Answers`, `Model` (the resolved version), `Usage.InputTokens` and `Usage.OutputTokens` (`*int`), `RequestID`, `LatencyMs` (`*float64`, the server's time), `Safety` and `Raw` (the parsed body). A value the server did not send is `nil`, never 0.

`dn.SpecVersion` is the version of the DecisionNode SDK specification this package implements, `"1.0"`.

The client is safe for concurrent use and reuses its connections: create one and share it. Cancel or bound any call with its context.

### Images

```go
receipt, err := dn.ImageFile("receipt", "receipt.jpg")
if err != nil {
	log.Fatal(err)
}
d, err := client.Decide(ctx, dn.Request{
	State:  map[string]any{"claim": map[string]string{"total": "740.00", "currency": "SEK"}},
	Images: []dn.Image{receipt},
	Questions: dn.Questions{
		dn.Q("total_matches", dn.Truth("Does the total on the receipt equal the total in the claim?")),
	},
})
```

`dn.ImageFile(id, path)` reads a JPEG, PNG or WebP file right away and returns an error when the file cannot be read or is in another format. `dn.ImageData(id, bytes, mediaType)` sends bytes; with an empty `mediaType` the type is taken from the first bytes (JPEG, PNG or WebP), and bytes in any other format make the call fail before anything is sent. `dn.ImageURL(id, url)` sends an `https` URL for the API to fetch. Every question sees every image. Up to 4 images a request, 10 MB each.

### Safety

On the hosted API every decision carries what the safety check found: `d.Safety.Harm`, `d.Safety.SelfHarm` and `d.Safety.Flagged`, the categories above your key's thresholds. When the check did not run, `d.Safety.Checked` is false and `d.Safety.Reason` says why. A self-hosted server sends no safety information and `d.Safety` is nil.

To also get the safety check's result in the response body, send the request header `x-decisionnode-safety-body: true`. Set it on the client with `dn.WithHeaders`, and it goes with every request:

```go
client := dn.NewClient(dn.WithHeaders(map[string]string{
	"x-decisionnode-safety-body": "true",
}))
```

The body's extra field is then in `d.Raw`.

## Errors

Every error from the API or the network is an `*dn.Error` (also exported as `dn.DecisionNodeError`, the name every DecisionNode SDK uses for its base error) with `Status`, `ErrorType`, `RequestID` and `Reason` (the server's explanation in words, from the `x-decisionnode-error` header). `err.Error()` is the server's message exactly as sent, empty when it sent none, or the client's own description when no response arrived. Match the specific types with `errors.As`:

```go
var rate *dn.RateLimitError
var refusal *dn.RefusalError
var apiErr *dn.Error
switch {
case errors.As(err, &rate):
	time.Sleep(rate.RetryAfter)
case errors.As(err, &refusal):
	log.Println("refused:", refusal.Category)
case errors.As(err, &apiErr):
	log.Println(apiErr.Status, apiErr.RequestID, apiErr.Reason)
}
```

| Status | Type | Extra fields |
|---|---|---|
| 200 to 299 with a body that is not JSON | `*ServerError` with `Reason` from `x-decisionnode-error` | |
| 200 to 299 where `Check`, `Classify` or `Score` gets no valid answer of its type | `*ServerError` with `Reason` from `x-decisionnode-error` | |
| 3xx | `*Error` with the status | |
| 400 | `*BadRequestError` | |
| 400, `max_tokens_exceeded` | `*ContextLimitError` (also a `*BadRequestError`) | |
| 401 | `*AuthenticationError` | |
| 402 | `*InsufficientCreditError` | |
| 403, refusal | `*RefusalError` | `Category` (`harm` or `self_harm`), `Safety` |
| 403, any other | `*PermissionDeniedError` | |
| 404 | `*NotFoundError` | |
| 422 | `*ValidationError` | `Entries` |
| 429 | `*RateLimitError` | `RetryAfter` (capped at 60 s) |
| 529 | `*OverloadedError` | `RetryAfter` (capped at 60 s) |
| other 5xx | `*ServerError` | |
| any other status (405, 409, 408 after its retries, ...) | `*Error` with the status | |
| no response | `*ConnectionError`, `*TimeoutError` | |

Only 200 to 299 is a success. Redirects are never followed, even with a client of your own from `dn.WithHTTPClient`, so the API key never goes to another URL or over plain HTTP.

A client of your own must be one the SDK can inspect: its `Transport` is `nil` (Go's default transport) or an `*http.Transport`. The SDK copies the client and sets its own `CheckRedirect`. For an `https` base URL it refuses a transport of any other type (a wrapping `RoundTripper` included), an `*http.Transport` with `TLSClientConfig.InsecureSkipVerify` set, and one with its own `DialTLS` or `DialTLSContext`, since none of these can be shown to keep certificate and hostname checks on. A refused client is an argument error from `client.Err()` and from every call.

A missing API key is an `*AuthenticationError`, returned before anything is sent. A problem with your own arguments is a plain Go error, not an `*dn.Error`, and is also returned before anything is sent: an empty question list (in any request of `DecideMany`), a base URL that is not `https`, an HTTP client the SDK cannot inspect or that turns off TLS checks, a timeout not above 0, negative retries, a concurrency below 1, image bytes in an unknown format, or a value that cannot be encoded as JSON. `dn.ImageFile` reports an unreadable file itself. Everything else is checked by the API, since limits differ by model and are listed by `Models`.

## Retries and timeouts

Each attempt has 10 seconds, covering the whole attempt: connecting, sending, and reading the response to its last byte. Requests that fail with 408, 429, any 5xx, a timeout or a connection error are retried twice, waiting as long as the API asks (`retry-after-ms`, else `Retry-After` in seconds or as an HTTP date, at most 60 s) or else backing off exponentially from 0.5 s, with jitter, up to 8 s. 400, 401, 402, 403, 404 and 422 are never retried. A decision changes nothing on the server, so a retry is always safe.

## Configuration

| Option | Environment variable | Default |
|---|---|---|
| `dn.WithAPIKey(key)` | `DECISIONNODE_API_KEY` | none |
| `dn.WithBaseURL(url)` | `DECISIONNODE_BASE_URL` | `https://api.decisionnode.com` |
| `dn.WithModel(model)` | `DECISIONNODE_MODEL` | `decisionnode-latest` |
| `dn.WithTimeout(d)` | | 10 s per attempt; must be above 0 |
| `dn.WithMaxRetries(n)` | | 2; must be 0 or more |
| `dn.WithHeaders(map)` | | |
| `dn.WithHTTPClient(c)` | | a client of its own; yours is copied and never follows redirects, and its transport must be `nil` or an `*http.Transport` that keeps TLS checks on |
| `dn.WithLogger(l)` | | off |
| `dn.WithResponseHook(f)` | | off |

An option wins over the environment, and the environment over the default. The base URL must be `https`; plain `http` is accepted only for `localhost`, `127.0.0.1` and `::1`, so the API key never travels unencrypted. `dn.NewClient` does not return an error: an invalid setting is returned by `client.Err()`, and by every call before anything is sent. An empty string counts as not given, for the options and for `Request.Model`. `Request.Model` overrides the client's model for one call. The models are `decisionnode-latest` and `decisionnode-flash-latest`, which move with releases, and the pinned `decisionnode-1.0` and `decisionnode-1.0-flash`.

`dn.WithLogger` takes a `*slog.Logger` and logs each attempt at debug level: method, path, status, request id and timing. State, questions and keys are never logged. The SDK sends no telemetry. Besides `Authorization`, `Accept` and `Content-Type`, the only header it sends is `User-Agent: decisionnode-go/<version>`, plus any you add with `dn.WithHeaders`.

`dn.WithResponseHook` calls a function of yours once for every response received, retried ones included, with the method, path, attempt number, status and copies of the headers and the body exactly as sent. It runs with the attempt's context, so values you put in the call's context reach it. Use it to honour a server's `retry-after` across your own work queue, or to keep a response's bytes as received; nothing it does changes what the client returns.

## Development

```sh
DECISIONNODE_CONFORMANCE_DIR=/path/to/decision-node-conformance go test ./...
```

The conformance tests run every case of the DecisionNode conformance suite against its mock server, and need `python3` for that server. `DECISIONNODE_CONFORMANCE_DIR` must point at a checkout of the suite: when it is unset the conformance test fails, it is never skipped. To run only the unit tests, pass `-skip TestConformance`. This version passes conformance suite 1.3; CI checks the suite out at the commit named once, in `CONFORMANCE_REF` in `.github/workflows/ci.yml`, with every action pinned to a commit and no credentials left in the checkouts.

## License

MIT. See [LICENSE](LICENSE).
