# DecisionNode Go SDK

The Go client for the [DecisionNode](https://decisionnode.com) API: ask structured questions about any state and get calibrated numbers back.

- Six question types: `truth` (how likely a statement is true), `choice` (one option of a set), `score` (a place on an ordered scale), `number` (a value on a grid you define), `point` (a spot in an image) and `box` (a rectangle in an image).
- Decisions one at a time, many at once, as a live session over a WebSocket, or as a batch at half the price.
- Up to 16 images by URL, from files or from bytes, checked locally and uploaded only when that is faster.
- Videos sent as the frames you sampled, each with its time.
- Boosters: specialist readings of your images (an age, whether a picture was AI-generated) added to a decision as evidence.
- Your own fine-tuned models, called by name.
- Typed answers and typed errors, matched with `errors.As`.
- Retries, timeouts, connection reuse and HTTP/2 built in. One dependency, for WebSockets.

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
| `dn.Number(instructions)` | Which value of 0 to 255? | `*NumberAnswer`: `Number`, `Expected`, `Confidence`, `Probabilities` by value |
| `dn.NumberWith(instructions, min, max, step)` | Which value of the grid `min`, `min+step`, ... up to `max`? (`nil` leaves a bound to its default: 0, 255, 1) | `*NumberAnswer` |
| `dn.Point(instructions)`, `dn.PointWith(instructions, image, units)` | Where in the image is it? | `*PointAnswer`: `X`, `Y`, `Present`, `Confidence`, `Image`, `Width`, `Height` |
| `dn.Box(instructions)`, `dn.BoxWith(instructions, image, units)` | Which rectangle of the image holds it? | `*BoxAnswer`: `X1`, `Y1`, `X2`, `Y2`, `Present`, `Confidence`, `Image`, `Width`, `Height` |

`options` is `dn.Options` (ordered names with descriptions), a `map[string]string` of name to description, or a `[]string` of names. `levels` is a slice of levels, 1 to 10. A number grid holds at most 4,096 values; `min` and `step` take up to 6 decimals, and a `json.Number` keeps a decimal exact. `NumberAnswer.Probabilities` is keyed by each value exactly as the server wrote it (`"7"`, `"3.5"`) and lists the values above 0.001.

```go
d, err := client.Decide(ctx, dn.Request{
	State: "Drone frame over row B4 of the north car park.",
	Questions: dn.Questions{
		dn.Q("cars", dn.NumberWith("How many cars are visible?", 0, 50, nil)),
		dn.Q("rating", dn.NumberWith("How tidy is the row, in halves?", 0, 5, 0.5)),
	},
})
fmt.Println(d.Number("cars").Number, d.Number("cars").Expected) // 7 7.0
```

A point or a box is a place in one of the request's images:

```go
d, err := client.Decide(ctx, dn.Request{
	Images: []dn.Image{dn.ImageURL("belt", "https://files.example.com/line-3/cam-2/0917.jpg")},
	Questions: dn.Questions{
		dn.Q("pick", dn.Point("Point to the yellow package closest to the robot arm.")),
		dn.Q("damage", dn.BoxWith("Draw a box around the damaged package.", "belt", "")),
		dn.Q("any_damage", dn.Truth("Is any package on the belt damaged?")),
	},
})
p, b := d.Point("pick"), d.Box("damage")
fmt.Println(p.X, p.Y, p.Present)           // 812 455 0.97
fmt.Println(b.X1, b.Y1, b.X2, b.Y2, b.Image) // 300 120 520 410 belt
```

`X` and `Y` are measured from the top-left corner of the image, `X` to the right and `Y` down; a box runs from its top-left corner (`X1`, `Y1`) to its bottom-right corner (`X2`, `Y2`). `units` is `"pixels"` (the default: whole pixels of the image as sent) or `"normalized"` (fractions of the width and the height). `Present` is the probability that what the instructions describe is in the image at all; `Confidence` the probability that the place is right if it is. `Image`, `Width` and `Height` name the image looked in and its size in pixels. The request needs an image, and `image` names the one to look in by its id when it carries two or more (`""` leaves it out); a reference by number ("Picture 2") goes in the instructions.

`State`, instructions, descriptions and levels take a string or any value that encodes to JSON: a struct, a map, a slice or a `json.RawMessage`. A question in its JSON wire form, such as a `map[string]any`, works wherever a builder does:

```go
dn.Q("escalate", map[string]any{"type": "truth", "instructions": "Escalate to a human now?"})
```

Question ids are any string, and answers come back under the same id. Read them with `d.Truth(id)`, `d.Choice(id)`, `d.Score(id)`, `d.Number(id)`, `d.Point(id)` and `d.Box(id)` (nil when the answer is of another type), or `d.Answer(id)` for the `dn.Answer` interface. An answer of a type this version does not know yet is an `*UnknownAnswer` holding the raw fields. So is an answer of a known type whose fields are missing or of the wrong type: the decision itself does not fail.

Numbers are passed through exactly as received: a probability list may read 0.8200000000000001 or sum to 0.99.

## The client

| Method | Does |
|---|---|
| `Decide(ctx, dn.Request)` | One request with any mix of questions. Returns a `*Decision`. |
| `DecideMany(ctx, []dn.Request, concurrency)` | Many requests, at most `concurrency` at a time (pass `dn.DefaultConcurrency`, 8, for the default). Returns one `DecisionResult` per request, in input order. One failure does not stop the others. Every request is checked first: a concurrency below 1 or an invalid request returns an error and nothing is sent, not even the valid requests. |
| `Check(ctx, dn.CheckRequest)` | One truth question. Returns a `*TruthAnswer`. |
| `Classify(ctx, dn.ClassifyRequest)` | One choice question. Returns a `*ChoiceAnswer`. |
| `Score(ctx, dn.ScoreRequest)` | One score question. Returns a `*ScoreAnswer`. |
| `Number(ctx, dn.NumberRequest)` | One number question, with the id `number`. Returns a `*NumberAnswer`. |
| `Gate(ctx, dn.GateRequest)` | Judges an action before it runs: `risk` (score), `touches_production` (truth) and `verdict` (allow, confirm or block). |
| `Upload(ctx, images)` | Uploads local images whatever their size and returns a `dn.UploadResult` for each (`ID`, `ExpiresAt`), to name them later with `dn.ImageUpload`. |
| `EndSession(ctx, id)` | Ends a session by its id, sent once and never retried; an already ended session is success with a nil summary. |
| `OpenSession(ctx, dn.SessionRequest)` | Opens a session. Returns a `*Session`; see [Sessions](#sessions). |
| `Supervise(ctx, body, feed, onEvent)` | Keeps a feed of frames answered across sessions, reopening as needed. |
| `Batches()` | The Batch API: `Create`, `Add`, `Finalize`, `Get`, `List`, `Results`, `Cancel`; see [Batches](#batches). |
| `RunBatch(ctx, requests, dn.RunBatchOptions)` | A batch from start to results in one call. |
| `Models(ctx)` | The models your key can use, with their published limits, as `[]dn.ModelInfo`. The session, batch, number and upload limits are also typed fields (`Sessions`, `Batch`, `NumberMaxGridValues`, ...), and your workspace's fine-tuned models are listed beside the catalogue's (see [Fine-tuned models](#fine-tuned-models)). |
| `DecideRaw(ctx, body)` | Sends a body exactly as given and returns the parsed response. For API fields this version does not know yet. |

`Check`, `Classify`, `Score`, `Number` and `Gate` take `Images`, `Videos`, `Model`, `Boosters` and `Fallback` like `Decide`. `Check`, `Classify`, `Score` and `Number` promise a typed answer, so when their own answer is missing or malformed they return a `*ServerError` with the response's status, `RequestID` and `Reason`. `Decide` returns such an answer as an `*UnknownAnswer` instead.

A `*Decision` holds `Answers`, `Model` (the resolved version), `Usage.InputTokens` and `Usage.OutputTokens` (`*int`), `RequestID`, `LatencyMs` (`*float64`, the server's time), `Safety`, `Warnings` and `Raw` (the parsed body). For a request with videos, `Usage.VideoFrames` (`*int`) is the number of frames read and `Usage.VideoSeconds` (`*float64`) the seconds of the windows read, and `Warnings` holds the server's notes exactly as sent when it read a video at a lower rate or resolution than asked (empty, never nil, when there are none). For a request with boosters, `Evidence` holds what each booster read, and `Usage.BoosterTokens` and `Usage.Boosters` (`*int`) the readings' tokens and the booster calls billed (see [Boosters](#boosters)). A value the server did not send is `nil`, never 0.

`dn.SpecVersion` is the version of the DecisionNode SDK specification this package implements, `"1.3"`.

The client is safe for concurrent use and reuses its connections (HTTP/2 when the server offers it, TLS sessions resumed): create one and share it. Cancel or bound any call with its context.

### Images

Send an image by URL when you can. It is the fastest input: nothing is uploaded, the request stays small, and the server fetches the image while it reads the request.

```go
d, err := client.Decide(ctx, dn.Request{
	State:  map[string]any{"claim": map[string]string{"total": "740.00", "currency": "SEK"}},
	Images: []dn.Image{dn.ImageURL("receipt", "https://files.example.com/claims/4411/receipt.jpg")},
	Questions: dn.Questions{
		dn.Q("total_matches", dn.Truth("Does the total on the receipt equal the total in the claim?")),
	},
})
```

| Builder | Sends |
|---|---|
| `dn.ImageURL(id, url)` | The URL, for the server to fetch. `https`, at most 2,048 characters. Recommended. |
| `dn.ImageFile(id, path)` | The file, read when the `Image` is made (an unreadable file is an error there). |
| `dn.ImageData(id, bytes, mediaType)` | The bytes. An empty `mediaType` is taken from the first bytes. |
| `dn.ImageBase64(id, text, mediaType)` | Bytes given as base64 text (a `data:` prefix is accepted). |
| `dn.ImageReader(id, r, mediaType)` | The bytes read from an `io.Reader`, read when the `Image` is made. |
| `dn.ImageUpload(id, uploadID, mediaType)` | An upload made earlier (`client.Upload`), less than an hour old. |

Every image is checked before anything is sent: at most 16 a request (URLs included; a 17th is the API's `*ValidationError` with a `too_long` entry), ids present, at most 64 characters (`dn.MaxImageIDChars`, counted as code points) and unique, local bytes JPEG, PNG or WebP by their first bytes (and matching a given media type), at most 10 MB, at most 40 megapixels read from the image header. Any other failure is the API's own `*BadRequestError` with no status, and nothing is sent. `Upload` takes up to 16 images a call. No image is resized or re-encoded.

Local images travel the faster way for their size. When a request's local images together are under `dn.DefaultInlineThreshold` (524,288 bytes; `dn.WithInlineThreshold` changes it) they go inline as base64. At or above it they are uploaded first: one `POST /v1/uploads` for all of them, then the bytes straight to storage, in parallel, then the request names them by `upload_id`. The client remembers what it uploaded, by the bytes' SHA-256, in memory, until 5 minutes before the upload expires: the same bytes in a later call, in another request of `DecideMany`, in a session frame or a batch line are named by their `upload_id` whatever their size, and are never uploaded twice at once. If the server answers that an upload the client named has expired or is gone, the client uploads it again and sends the request once more; that does not count as a retry. A request body may be at most 25 MiB (`dn.MaxBodyBytes`): inline images and video frames count toward it, uploads and URLs do not. A body over it is refused before anything is sent, with a `*dn.RequestTooLargeError`.

#### Referring to images and videos

There are two ways to say which image a question is about:

1. **By id (recommended).** Give each image a short, meaningful id and use it in the instructions, in backticks: "Does the total on `receipt` match the amount in the claim?"
2. **By number.** Images are numbered from 1 in the order of `Images`: "image 2", "the second image", "Picture 2" and "the last image" all work.

The model sees each image as `Picture 1 (receipt):`, `Picture 2 (claim_photo):`, so both ways point to the same image. Prefer ids: they stay right when images are added or reordered. Use names, not digits, as ids (`front`, not `2`). Videos work the same way, by id or by number ("video 1"), numbered separately in the order of `Videos`. The `image` of a point or box question takes the id.

```go
d, err := client.Decide(ctx, dn.Request{
	State: "Expense claim 4411: 740.00 EUR.",
	Images: []dn.Image{
		dn.ImageURL("receipt", "https://files.example.com/claims/4411/receipt.jpg"),
		dn.ImageURL("claim_photo", "https://files.example.com/claims/4411/photo.jpg"),
	},
	Questions: dn.Questions{
		dn.Q("matches", dn.Truth("Does the total on `receipt` match the amount in the claim?")),
	},
})
```

### Videos

A video travels as the frames you already hold, each with its time in seconds, and the server reads them as one video, in order and with their times. Only frames are sent: no video file, no video URL and no sound.

```go
var frames []dn.VideoFrame
for i, path := range []string{"dock-0000.jpg", "dock-0001.jpg", "dock-0002.jpg"} {
	b, err := os.ReadFile(path)
	if err != nil {
		log.Fatal(err)
	}
	frames = append(frames, dn.VideoFrameData(float64(i)*0.5, b, "")) // 0, 0.5 and 1 s
}

d, err := client.Decide(ctx, dn.Request{
	State:  "Warehouse camera 4, loading dock, shift B.",
	Videos: []any{dn.Video{ID: "dock", Frames: frames}},
	Questions: dn.Questions{
		dn.Q("fall", dn.Truth("Does a person fall in the video?")),
		dn.Q("forklifts", dn.NumberWith("How many forklifts pass the door?", 0, 50, nil)),
	},
})
fmt.Println(d.Truth("fall").Truth, d.Number("forklifts").Number, d.Warnings)
```

| Builder | Sends |
|---|---|
| `dn.VideoFrameData(t, bytes, mediaType)` | A frame `t` seconds into the video, its bytes as base64. An empty `mediaType` is taken from the first bytes (JPEG, PNG or WebP). |
| `dn.VideoFrameBase64(t, text, mediaType)` | A frame given as base64 text, sent as given, with its media type. |
| `dn.Video{ID, Frames, FPS, Start, End}` | A video: its id (unique across the request's images and videos), its frames with strictly increasing times, and optionally `FPS` (the most frames a second to read, above 0 and at most 4; the server's default is 2) and the window `[Start, End)` on the frames' own times, set with `dn.Ptr(2.0)`. |

`Request.Videos` holds up to 4 videos, each a `dn.Video` or a video in its JSON wire form (a `map[string]any` or a `json.RawMessage`), checked by the same rules. The helpers, `DecideMany` and batch requests take videos as `Decide` does; sessions do not. Frames always go inline: they are never uploaded or cached, and no frame is resized or re-encoded. The checks above run before anything is sent; how many frames a request may carry, their sizes and their pixels are the server's to judge. To turn a video file into frames, use the `dn` command line tool: `dn --video dock.mp4`.

### Boosters

A booster is a specialist model that reads the request's images before the decision. Its reading is added to what the model sees as evidence, and comes back in the response. The model still answers every question: a booster never answers one, and a request without boosters is answered exactly as before.

| Booster | Reads | Each reading |
|---|---|---|
| `age` (`dn.BoosterAge`) | A picture of a person; the largest face is read | `*dn.AgeReading`: `Image`, `Faces`, and `AgeYears`, `Low`, `High` (the range, in years) and `Confidence`, each nil when `Faces` is 0 |
| `ai_generated` (`dn.BoosterAIGenerated`) | Any picture | `*dn.AIGeneratedReading`: `Image`, `Generated` (the probability that an image generator made it), `Confidence`, `Method` (`inference`, read from the pixels, or `c2pa`, the file's Content Credentials say so), `TopGenerator` (`Name`, `Probability`; nil unless the picture reads as likely generated, and on every `c2pa` reading), `C2PASigner` (who signed the file's Content Credentials, nil unless they are trusted) and `C2PA` (`"untrusted"` when the file carries Content Credentials that are not trusted, else nil) |

```go
d, err := client.Decide(ctx, dn.Request{
	Images: []dn.Image{
		dn.ImageURL("selfie", "https://files.example.com/signup/88/selfie.jpg"),
		dn.ImageURL("listing", "https://files.example.com/listings/4411/photo.jpg"),
	},
	Boosters: []any{"age", "ai_generated"},
	Questions: dn.Questions{
		dn.Q("adult", dn.Truth("Is the person in `selfie` an adult?")),
		dn.Q("synthetic", dn.Truth("Is `listing` AI-generated?")),
	},
})
if err != nil {
	log.Fatal(err)
}
fmt.Println(d.Truth("adult").Truth, d.Truth("synthetic").Truth)

for _, r := range d.Evidence.Get("age").Results {
	if a, ok := r.(*dn.AgeReading); ok && a.Faces > 0 {
		fmt.Println(a.Image, *a.AgeYears, *a.Low, *a.High, *a.Confidence)
	}
}
for _, r := range d.Evidence.Get("ai_generated").Results {
	if g, ok := r.(*dn.AIGeneratedReading); ok {
		fmt.Println(g.Image, g.Generated, g.Method)
	}
}
fmt.Println(*d.Usage.BoosterTokens, *d.Usage.Boosters)
```

`Request.Boosters` holds up to 4 boosters, each at most once, in the order you give, usually by name (`dn.BoosterAge` and `dn.BoosterAIGenerated` name the two). Names are plain strings, so a booster added to the API later works with this version too. A booster that needs a setting is a `dn.Booster`:

```go
Boosters: []any{
	"age",
	dn.Booster{Name: "ai_generated", Images: []string{"listing"}, Require: dn.Ptr(true)},
},
```

It is sent as `{"name": "ai_generated", "images": ["listing"], "require": true}`, and a booster in that JSON wire form (a `map[string]any`) is accepted too. A `dn.Booster` with no field but `Name` is sent as the bare name.

| `dn.Booster` field | Sends |
|---|---|
| `Name` | The booster's name. |
| `Images` | The ids of the request's images the booster reads. Nil: every image of the request. |
| `Require` | `dn.Ptr(true)`: a booster that fails or does not answer in time fails the request with a 529 `*dn.OverloadedError` whose `ErrorType` is `booster_unavailable`, retried like any 529. Nil or `dn.Ptr(false)`: the decision runs without that reading, and its evidence says why. |
| `Options` | The booster's settings. Neither booster has any today. |

`d.Evidence` holds one `*dn.Evidence` per booster the request named, in the request's order, and `d.Evidence.Get(name)` finds one by name: `Booster` (its name), `Version` (the booster's version, such as `age-v3`), `Status` (`ok`; `timeout`, no answer in its time; `error`; `skipped`, none of the request's images is one it reads), `LatencyMs`, `BilledCalls` (the calls of that booster billed on this request) and `Results`, one reading per picture the booster was to read. A reading of a booster this version does not know, or one with a field missing or of the wrong type, is a `*dn.UnknownReading` whose `Raw` holds its fields; the decision does not fail. So is a picture the booster did not read (it timed out or failed: `{"image": "selfie"}`, `Status` says why) or could not decode (`{"image": "selfie", "readable": false}`); neither is billed. Fields a reading adds that this version does not know are ignored. `Usage.BoosterTokens` is the readings' share of `Usage.InputTokens`, and `Usage.Boosters` the booster calls billed. Without boosters, `Evidence` and both usage fields are nil.

Boosters read images in every form an image takes: URL, inline bytes and upload. Every question sees every reading. The helpers, `DecideMany` and batch lines take boosters as `Decide` does; sessions do not. The SDK checks nothing about boosters before sending: which boosters exist and which your workspace may use is the API's to answer, before any booster runs and before any charge. Every booster is on for a new workspace, and an owner or admin can turn one off in the console.

Booster calls are billed one per booster and image read with status `ok`, at the price per call on [Pricing](https://decisionnode.com/docs/pricing#boosters). A reading is billed every time a request uses it, one the API had already made for your workspace included; a booster that timed out or failed is free, and a request the safety check refuses pays for none. The readings' tokens are input tokens at the model's price.

### Fallback

When no worker can take a request for `decisionnode-moe` at once, the API answers it with `decisionnode-flash` rather than make it wait. `d.Model` is then the model that answered, and `d.Fallback` says so: `ServedModel` and `RequestedModel`, from the `x-decisionnode-fallback` and `x-decisionnode-requested-model` headers; on an ordinary answer it is nil. To have such a request wait for its own model instead, set `Fallback: dn.Ptr(false)` on the request (the helpers and batch lines take it too): it then gets the 529, an `*dn.OverloadedError` retried like any other. Left nil, nothing is sent and the API's default applies.

### Fine-tuned models

A model you fine-tune on your own labelled decisions, in the console, is called by its name in `Model`: `decisionnode-flash@acme/fraud` (the version deployed most recently) or `decisionnode-flash@acme/fraud:3` (one version). The name is sent as given; only your workspace's keys can call it, and a name the API does not know for your workspace is a `*dn.BadRequestError`, "Unknown model: <name>". `d.Model` carries the name pinned to the version that answered. `Models` lists your fine-tuned models beside the catalogue's, with `FineTuned` pointing to true (nil for a catalogue model), `Base` (the model it was trained from), `AdapterVersion` (its version number) and `Status`: `deployed` answers decisions, `ready` is trained and not deployed.

### Safety

On the hosted API every decision carries what the safety check found: `d.Safety.Harm`, `d.Safety.SelfHarm` and `d.Safety.Flagged`, the categories above your key's thresholds. When the check did not run, `d.Safety.Checked` is false and `d.Safety.Reason` says why. A self-hosted server sends no safety information and `d.Safety` is nil.

To also get the safety check's result in the response body, send the request header `x-decisionnode-safety-body: true`. Set it on the client with `dn.WithHeaders`, and it goes with every request:

```go
client := dn.NewClient(dn.WithHeaders(map[string]string{
	"x-decisionnode-safety-body": "true",
}))
```

The body's extra field is then in `d.Raw`.

## Sessions

A session sends the fixed part of a decision (instructions, state, questions) once, then answers a stream of frames over one WebSocket, each frame costing roughly its own tokens. Replies come back as they are ready; when frames arrive faster than they are answered the newest wins, and the reply that superseded others lists them in `Dropped`.

```go
s, err := client.OpenSession(ctx, dn.SessionRequest{
	Instructions: "You are the mission supervisor of a survey drone.",
	State:        map[string]any{"mission": "Survey the north field at 40 m"},
	Questions:    dn.Questions{dn.Q("abort", dn.Truth("Should the mission be aborted now?"))},
})
if err != nil {
	log.Fatal(err)
}
defer s.End(ctx)
if err := s.Connect(ctx); err != nil {
	log.Fatal(err)
}
s.Send(ctx, dn.Frame{Frame: map[string]any{"battery": 0.31, "wind_mps": 11.4}})
for ev := range s.Replies() {
	switch e := ev.(type) {
	case *dn.Reply:
		fmt.Println(e.Seq, e.Answers["abort"].(*dn.TruthAnswer).Truth)
	case *dn.FrameError:
		fmt.Println(e.Seq, e.ErrorType, e.Message)
	case *dn.SessionClosed:
		fmt.Println(e.Code, e.Reopen)
	}
}
```

| Call | Does |
|---|---|
| `OpenSession(ctx, dn.SessionRequest)` | `State`, `Questions`, `Instructions`, `Model`, `Window` (frames each answer sees, 1 to 64) and `TTLSeconds` (1 to 3,600), set with `dn.Ptr(16)`; nil leaves a number to the server. Returns a `*Session` with every field of the server's answer, the questions already answered on the fixed part. A key at its limit of open sessions gets a `*SessionLimitError` (also a `*RateLimitError`), never retried. |
| `s.Connect(ctx)` | Opens the socket, through the proxy, dialer and TLS settings of the client's HTTP transport. The key travels in the handshake's `Authorization` header, never in the URL. |
| `s.Send(ctx, dn.Frame)` | Sends one frame (`Frame`, `Image`, `Questions`, `Reset`, `Seq`) and returns its `seq`, counted from 1 unless given. It never waits for the reply. A frame the server would refuse for its form (no frame value or image, an empty frame, an unknown question id, a `seq` out of range, an image by URL or over the image rules) returns a `*BadRequestError` with nothing written and no `seq` used. |
| `s.Next(ctx)`, `s.Replies()` | The events: `*dn.Reply`, `*dn.FrameError` (a value, never an error: the session goes on), and last `*dn.SessionClosed`. Each carries `RawJSON`, the message exactly as the server sent it (a close, which has no JSON, as `{"code", "reason", "reopen"}`). Pings are answered for you. |
| `s.End(ctx)` | Ends the session. A session already ended is success. |
| `s.Close()` | Closes the socket from your side. |

`SessionClosed.Reopen` is true for the codes after which a new session with the same body should be opened: 1001, 1006, 1011, 4404, 4408, 4409 and 4410. After 1013 connect again to the same session. `client.Supervise(ctx, body, feed, onEvent)` does all of this for a long-lived feed: it sends each frame from the channel, hands every reply and frame error to `onEvent`, reopens on the reopen codes (at once on 4408 and 4410, else after a backoff of 1 s to 30 s with jitter), connects again on 1013, and stops on any other code, returning that `*SessionClosed`. When the feed channel is closed it waits for the outstanding replies, ends the session and returns.

## Batches

The Batch API answers many decide requests asynchronously, within 24 hours, at half the price. `RunBatch` does the whole lifecycle:

```go
batch, results, err := client.RunBatch(ctx, []dn.BatchRequest{
	{CustomID: dn.Ptr("ticket-1"), Request: dn.Request{State: "I was charged twice.", Questions: questions}},
	{CustomID: dn.Ptr("ticket-2"), Request: dn.Request{State: "The app crashes on start.", Questions: questions}},
}, dn.RunBatchOptions{PollInterval: time.Minute})
for _, r := range results {
	if r.Decision != nil {
		fmt.Println(r.CustomID, r.Decision.Choice("route").Choice) // CustomID is a string here
	} else {
		fmt.Println(r.CustomID, r.Status, r.StatusText, r.Err) // a value, never returned
	}
}
```

It checks every request locally, creates an empty draft, adds the lines in calls of at most 10 MiB, finalizes, reads the batch every `PollInterval` (at least 10 s, default 60 s; the server's `Retry-After` when that is longer) until it ends, and reads the results. Past `Timeout` (default 26 hours) it returns a `*TimeoutError`. If adding fails, the draft is cancelled. The single calls are on `client.Batches()`:

| Method | Does |
|---|---|
| `Create(ctx, requests)` | A draft with these lines (none: an empty draft). |
| `Add(ctx, batchID, requests)` | Adds lines to a draft, split into calls of at most 10 MiB, in order. |
| `Finalize(ctx, batchID)` | Queues the draft. |
| `Get(ctx, batchID)` | The `*Batch`: `State`, `Counters`, the times, `Usage`, `Error`. |
| `List(ctx, dn.BatchListOptions)` | One page of your batches, newest first. `Offset` (0 or more) and `Limit` (1 to 100) are pointers; nil leaves them to the server. |
| `Results(ctx, batchID)` | One `BatchResult` per line, in upload order: `Decision` for a 200 line, else `Err`, the error that status would give. |
| `ResultsReader(ctx, batchID)` | The same results read line by line as the file arrives, for files of any size: `for r.Next() { r.Result() }`, then `r.Err()` and `r.Close()`. The client's timeout bounds each pause in the file, not the whole download. |
| `Cancel(ctx, batchID)` | Cancels the batch. Safe to repeat. |

A request's `CustomID` is a pointer so a missing id and an empty one are told apart: every line needs one, of 1 to 512 characters, unique within the call; a fault is refused locally with the API's message ("Line 2: custom_id \"a\" is already in the batch."). Each line carries its custom id, a model (the request's, else the client's) and a state. A line's local images follow the inline threshold per line. `Create`, `Add` and `Finalize` are retried only on 429 and 529, since a repeat after a lost answer could create a second draft; a `Finalize` answered 409 reads the batch and returns it when an earlier finalize went through.

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
| 403, any other (`booster_not_enabled`, a suspended workspace) | `*PermissionDeniedError` | |
| 404 | `*NotFoundError` | |
| 413 | `*RequestTooLargeError`, never retried | |
| 422 | `*ValidationError` | `Entries` |
| 429 | `*RateLimitError` | `RetryAfter` (capped at 60 s) |
| 429, `session_limit_exceeded` | `*SessionLimitError` (also a `*RateLimitError`), never retried | `RetryAfter` |
| 529 (also `booster_unavailable`) | `*OverloadedError` | `RetryAfter` (capped at 60 s) |
| other 5xx | `*ServerError` | |
| any other status (405, 409, 408 after its retries, ...) | `*Error` with the status | |
| no response | `*ConnectionError`, `*TimeoutError` | |

A booster the API does not offer is a `*BadRequestError` with `ErrorType` `booster_unknown`, and a booster with no image to read (or naming an image the request does not have) `booster_input_mismatch`; a malformed `Boosters` list is `api_usage_error` with the reason in `Reason`. Every error of a request that named boosters carries `Boosters`: how each booster ended, from the `x-decisionnode-boosters` header, in the request's order (`Name`, `Status`, `LatencyMs`), so you can see which required booster failed. `dn.ParseBoosters(d.Header)` reads the same header of a decision.

An error whose body is not JSON (a page from a proxy or the service's edge, or no body) is classed by its status alone, as in the table: a 413 is a `*RequestTooLargeError`, a 400 a `*BadRequestError` (never a `*ContextLimitError`), a 403 a `*PermissionDeniedError` (never a `*RefusalError`), a 422 a `*ValidationError` with no entries. `ErrorType` is empty, `RequestID` and `Reason` come from their headers, nothing is read from the body, and the message is `HTTP <status> <text>`, then ` (request id <id>)` when the response has one: `HTTP 413 Content Too Large`, `HTTP 408 Request Timeout (request id req_...)`. Retries are as for any response: a 408 is retried, a 413 never.

Only 200 to 299 is a success. Redirects are never followed, even with a client of your own from `dn.WithHTTPClient`, so the API key never goes to another URL or over plain HTTP.

A client of your own must be one the SDK can inspect: its `Transport` is `nil` (Go's default transport) or an `*http.Transport`. The SDK copies the client and sets its own `CheckRedirect`. For an `https` base URL it refuses a transport of any other type (a wrapping `RoundTripper` included), an `*http.Transport` with `TLSClientConfig.InsecureSkipVerify` set, and one with its own `DialTLS` or `DialTLSContext`, since none of these can be shown to keep certificate and hostname checks on. A refused client is an argument error from `client.Err()` and from every call.

A missing API key is an `*AuthenticationError`, returned before anything is sent. A problem with your own arguments is a plain Go error, not an `*dn.Error`, and is also returned before anything is sent: an empty question list (in any request of `DecideMany`), a base URL that is not `https`, an HTTP client the SDK cannot inspect or that turns off TLS checks, a timeout not above 0, negative retries, a negative inline threshold, a concurrency below 1, a poll interval under 10 s, or a value that cannot be encoded as JSON. `dn.ImageFile` reports an unreadable file itself.

The server never sees a request the client could have refused. Every fixed rule the API documents is checked before anything is sent, in the server's order, and a fault returns what the server would answer, with no status and no request id:

| Fault | Returned as |
|---|---|
| more than 128 objects and arrays inside each other; an unknown question type | `*BadRequestError`, `ErrorType` `api_usage_error`, "Invalid request." |
| a question that is not an object or has no type; instructions, a state, an option description, a level or a truth side that is not a string, an object or an array; a choice or score without criteria, of the wrong shape, or a score with no level; a number grid that cannot be built (a bound not a number, beyond 2^53, more than 6 decimals, `step` not above 0, `min` above `max`, more than 4,096 values); a point or box in a request without images, without `image` among two or more images, or naming no image of the request, or with `units` other than `pixels` and `normalized`; a session `Window` or `TTLSeconds` out of range | `*ValidationError` with the API's entries, empty `ErrorType` |
| a choice with no option or more than 255, a score over 10 levels, a truth without instructions or criteria, a number, point or box without instructions | `*BadRequestError` with the API's message and an empty `ErrorType` |
| more than 16 images or more than 4 videos | `*ValidationError` with the API's `too_long` entry on `["body", "images"]` or `["body", "videos"]`, after the request's other entries; a local image is listed by its id and media type, without its bytes |
| an image over its limits, an empty id, an id over 64 characters ("Image 1: its id is longer than 64 characters.", images counted from 0), a repeated id, a URL that is not `https`; more than 16 images to `Upload` | `*BadRequestError`, `api_usage_error` |
| a video without an id or with an id already used by an image or a video; a video without frames; a frame time that is negative or not after the one before it; a frame that is not JPEG, PNG or WebP by its media type; `FPS` not above 0 and at most 4; `Start` or `End` negative; `End` not after `Start` | `*BadRequestError`, `api_usage_error` |
| an `https` image URL over 2,048 characters, with user information, on a port other than 443, or naming a private address | `*BadRequestError`, `image_url_refused` |
| a batch line without a valid or unique `CustomID`, an `Add` with no requests, a `List` page out of range | `*BadRequestError`, `api_usage_error` |
| a request body, or a batch line, over 25 MiB (`dn.MaxBodyBytes`, 26,214,400 bytes): a long state, many video frames, inline images | `*RequestTooLargeError`, `ErrorType` `request_too_large_error` |

To validate input without sending it, call the check that matches the call: `client.CheckDecide(req)`, `CheckSession(sessionRequest)`, `CheckBatchCreate(requests)`, `CheckBatchAdd(requests)` or `session.CheckFrame(frame)`. Each runs every local check the call would run, returns the same error, and sends and uploads nothing; nil means the call would send the request.

After a call to `Models`, the client also checks the limits only that list states, for a model named by its entry's name, alias or pinned version: a model that answers no number questions or serves no sessions, a lower `max_window`, the batch's `requests_per_batch` and `max_custom_id_chars`. In `DecideMany` and a batch, a fault in any request fails the whole call. Token counts and anything that depends on the server's state are left to the API.

## Retries and timeouts

Each attempt has 10 seconds, covering the whole attempt: connecting, sending, and reading the response to its last byte. Requests that fail with 408, 429, any 5xx, a timeout or a connection error are retried twice, waiting as long as the API asks (`retry-after-ms`, else `Retry-After` in seconds or as an HTTP date, at most 60 s) or else backing off exponentially from 0.5 s, with jitter, up to 8 s. 400, 401, 402, 403, 404 and 422 are never retried. A decision changes nothing on the server, so a retry is always safe. An image upload to storage is retried on 5xx and connection errors the same way; a storage `403` after the upload URL's 5 minutes have passed (a slow connection) is presigned again once. Batch `Create`, `Add` and `Finalize` are the exception: they are retried only on 429 and 529, which the API never applies.

## Configuration

| Option | Environment variable | Default |
|---|---|---|
| `dn.WithAPIKey(key)` | `DECISIONNODE_API_KEY` | none |
| `dn.WithBaseURL(url)` | `DECISIONNODE_BASE_URL` | `https://api.decisionnode.com` |
| `dn.WithModel(model)` | `DECISIONNODE_MODEL` | `decisionnode-moe` |
| `dn.WithTimeout(d)` | | 10 s per attempt; must be above 0 |
| `dn.WithMaxRetries(n)` | | 2; must be 0 or more |
| `dn.WithHeaders(map)` | | |
| `dn.WithInlineThreshold(n)` | | 524,288 bytes (`dn.DefaultInlineThreshold`); 0 uploads every local image |
| `dn.WithUploadTimeout(d)` | | 60 s per storage PUT attempt (`dn.DefaultUploadTimeout`), apart from the API timeout; must be above 0 |
| `dn.WithHTTPClient(c)` | | a client of its own; yours is copied and never follows redirects, and its transport must be `nil` or an `*http.Transport` that keeps TLS checks on |
| `dn.WithLogger(l)` | | off |
| `dn.WithResponseHook(f)` | | off |

An option wins over the environment, and the environment over the default. The base URL must be `https`; plain `http` is accepted only for `localhost`, `127.0.0.1` and `::1`, so the API key never travels unencrypted. `dn.NewClient` does not return an error: an invalid setting is returned by `client.Err()`, and by every call before anything is sent. An empty string counts as not given, for the options and for `Request.Model`. `Request.Model` overrides the client's model for one call. The models are `decisionnode-moe`, the main model, and `decisionnode-flash`, the fast one: each always means its current release. A decision's `Model` names the pinned id that answered, `decisionnode-<family>-<version>`; to pin a version, take its id from `Models` rather than writing one down. The earlier ids `decisionnode-latest`, `decisionnode-flash-latest`, `decisionnode-1.0` and `decisionnode-1.0-flash` are still accepted. Your own [fine-tuned models](#fine-tuned-models) take the family as their base: `decisionnode-moe@acme/fraud`.

`dn.WithLogger` takes a `*slog.Logger` and logs each attempt at debug level: method, path, status, request id and timing. State, questions and keys are never logged. The SDK sends no telemetry. Besides `Authorization`, `Accept` and `Content-Type`, the only header it sends is `User-Agent: decisionnode-go/<version>`, plus any you add with `dn.WithHeaders`. An image upload to storage carries exactly the headers the API signed for it: never the key, never your own headers, and no redirect is followed.

`dn.WithResponseHook` calls a function of yours once for every response received, retried ones included, with the method, path, attempt number, status and copies of the headers and the body exactly as sent. It runs with the attempt's context, so values you put in the call's context reach it. Use it to honour a server's `retry-after` across your own work queue, or to keep a response's bytes as received; nothing it does changes what the client returns.

## Development

```sh
DECISIONNODE_CONFORMANCE_DIR=/path/to/decision-node-conformance go test ./...
```

The conformance tests run every case of the DecisionNode conformance suite against its mock server, and need `python3` for that server. `DECISIONNODE_CONFORMANCE_DIR` must point at a checkout of the suite: when it is unset the conformance test fails, it is never skipped. To run only the unit tests, pass `-skip TestConformance`. This version passes conformance suite 1.11; CI checks the suite out at the commit named once, in `CONFORMANCE_REF` in `.github/workflows/ci.yml`, with every action pinned to a commit and no credentials left in the checkouts.

## License

MIT. See [LICENSE](LICENSE).
