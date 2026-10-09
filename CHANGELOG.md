# Changelog

All notable changes to this package are listed here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions follow
[Semantic Versioning](https://semver.org/).

## [0.4.0] - Unreleased

Implements version 1.3 of the DecisionNode SDK surface: boosters and fine-tuned models.

### Added

- Boosters: `Request.Boosters` takes up to 4 boosters, in order, each a name (`BoosterAge`, `BoosterAIGenerated`, or any string), a `Booster` (`Name`, `Images`, `Require` as a `*bool`, `Options`, sent as `{"name", "images", "require", "options"}`, each left out when nil) or a booster in its JSON wire form. The helper requests, `DecideMany` and batch requests take them as `Decide` does; sessions do not. Without boosters the request body is unchanged. Nothing about boosters is checked locally except the API's nesting limit, which counts them.
- `Decision.Evidence`, an `EvidenceList` in the request's order with `Get(name)`: `Evidence` with `Booster`, `Version`, `Status`, `LatencyMs` and `Results`, each result an `AgeReading`, an `AIGeneratedReading` (with `TopGenerator`, nil unless the picture reads as likely generated, `C2PASigner` and `C2PA`) or, for a booster this version does not know or a reading with a field missing or of the wrong type, an `UnknownReading` with its raw fields. Nil when the request named no boosters.
- `Usage.BoosterTokens` and `Usage.Boosters`, nil for a request without boosters.
- `Error.Boosters` on every error: the `x-decisionnode-boosters` header as `BoosterStatus` entries (`Name`, `Status`, `LatencyMs`) in the request's order, so a caller sees which required booster failed. `ParseBoosters` reads the same header of a decision.
- Fine-tuned models: a fine-tuned model's name is sent as given in `Model`, and `Decision.Model` carries the version that answered. `ModelInfo` gains `FineTuned` (nil for a catalogue model), `Base`, `AdapterVersion` and `Status`; `Version` stays the pinned name.

- The API's 25 MiB body limit: `MaxBodyBytes` (26,214,400 bytes) and `RequestTooLargeError`. A request body, or a batch line, over it is refused before anything is sent or uploaded (measured as it would go out, an image to upload by its upload id), and the API's 413 is the same error, never retried. Inline images and video frames count toward it; uploaded images and URLs do not, and local images at or above the inline threshold are uploaded.

- Fallback: `Request.Fallback` (and the helper requests', `DecideMany`'s and batch lines'), sent as `"fallback": true | false` only when set; `Fallback: Ptr(false)` makes a request for `decisionnode-1.0` wait for its own model (the 529, retried as any) rather than be answered by `decisionnode-1.0-flash`. `Decision.Fallback` (`ServedModel`, `RequestedModel`) from the `x-decisionnode-fallback` and `x-decisionnode-requested-model` headers, nil on an ordinary answer.

### Changed

- `SpecVersion` is `"1.3"`.
- The booster errors map to the existing types: `booster_unknown` and `booster_input_mismatch` are a `*BadRequestError`, `booster_not_enabled` a `*PermissionDeniedError`, and `booster_unavailable` an `*OverloadedError`, retried like any 529.

## [0.3.0] - Unreleased

Implements version 1.2 of the DecisionNode SDK surface: sixteen images, videos as frames, and the point and box question types.

### Added

- Point and box questions: `Point`, `PointWith`, `Box` and `BoxWith` (`Question.Image` names the image to look in, `Question.Units` is `pixels` or `normalized`), and the answers `PointAnswer` (`X`, `Y`, `Present`, `Confidence`, `Image`, `Width`, `Height`) and `BoxAnswer` (`X1`, `Y1`, `X2`, `Y2` and the same), read with `Decision.Point` and `Decision.Box`. An answer with a field missing or of the wrong type is an `UnknownAnswer`. Their rules are checked locally with the API's 422 entries: an image in the request, `image` naming one of them (required with two or more), and the units; a point or box without instructions is the API's 400.
- Videos sent as the frames the caller sampled: `Video` (`ID`, `Frames`, and the optional `FPS`, `Start` and `End`), `VideoFrame` built with `VideoFrameData` (bytes, the media type taken from the first bytes when not given) and `VideoFrameBase64` (base64 text sent as given), and `Request.Videos`, which also takes a video in its JSON wire form. The helper requests, `DecideMany`, batch requests and `CheckDecide` take videos as `Decide` does. Frames always go inline: they are never uploaded, cached, resized or re-encoded.
- The API's video rules are checked before anything is sent, with the API's own message: an id that is present and unique across images and videos, at least one frame, frame times at least 0 and strictly increasing, a JPEG, PNG or WebP media type for every frame, `FPS` above 0 and at most 4, `Start` and `End` at least 0 and `End` after `Start`. The number of frames, their sizes and their pixels are left to the server.
- `Usage.VideoFrames` and `Usage.VideoSeconds`, nil for a request without videos, and `Decision.Warnings`, the server's notes exactly as sent, empty when there are none.
- `MaxVideos` and `MaxVideoFPS`.

### Changed

- `SpecVersion` is `"1.2"`.
- Up to 16 images a request (`MaxImages`), all of a request's uploads in one presign of up to 16 files. More than 16 images, or more than 4 videos, is refused locally as the API's 422: a `*ValidationError` with a `too_long` entry on `["body", "images"]` or `["body", "videos"]`, listed after the request's other entries. `Upload` with more than 16 images returns the presign's own 400, "files: 1 to 16 files, got 17.".
- A model's image and video limits held from `Models` are also read where the API publishes them, in its `images` and `videos` objects.

## [0.2.0] - Unreleased

Implements version 1.1 of the DecisionNode SDK surface: every public call.

### Added

- Number questions: `Number` and `NumberWith` builders (`Min`, `Max` and `Step` on `Question`), `NumberAnswer` with `Number`, `Expected`, `Confidence` and `Probabilities` keyed by value as sent, `Decision.Number`, and the `Client.Number` helper.
- Images: `ImageBase64`, `ImageReader` and `ImageUpload`; `Image.MediaType`; `Client.Upload`. Local images are sent inline under `DefaultInlineThreshold` (524,288 bytes, changed with `WithInlineThreshold`) and uploaded at or above it, with one presign per request and the uploads in parallel. Uploads are cached in memory by content hash until 5 minutes before they expire, never uploaded twice at once, and uploaded again once when the server reports one stale.
- Sessions: `Client.OpenSession`, `Session.Connect`, `Send`, `Next`, `Replies`, `End` and `Close`, the events `Reply`, `FrameError` and `SessionClosed`, and `Client.Supervise`.
- The Batch API: `Client.Batches` with `Create`, `Add` (split at 10 MiB), `Finalize`, `Get`, `List`, `Results` and `Cancel`, and `Client.RunBatch`.
- `SessionLimitError`, a `RateLimitError` that is never retried.
- `ModelInfo` typed fields for the number, upload, session and batch limits.
- `Client.EndSession` ends a session by its id, sent once.
- `RawJSON` on `Reply`, `FrameError` and `SessionClosed`: the message exactly as sent.
- `UploadResult` (`ID`, `ExpiresAt`), returned by `Client.Upload`.
- `Ptr`, for the optional fields where 0 or "" differs from not given.
- Every fixed rule the API documents is checked locally, in the server's order, with the error type the server sends: question shapes and types, value types, number grids, nesting, image URLs (`image_url_refused`), session window and lifetime, frames (`Session.Send` writes nothing and uses no seq for a refused frame), batch custom ids and list paging, and, after `Models`, the limits only that list states.
- An upload refused 403 after its URL's lifetime is presigned again once.
- Public checks that run every local check of a call and send nothing: `CheckDecide`, `CheckSession`, `CheckBatchCreate`, `CheckBatchAdd` and `Session.CheckFrame`.
- `BatchService.ResultsReader` and `BatchResultsReader` read a batch's results line by line as they arrive; `Results` uses it and still returns the whole list.
- `BatchResult.RawJSON`: the result line exactly as the server sent it, without its line ending.
- `WithUploadTimeout` (default `DefaultUploadTimeout`, 60 s) bounds each storage PUT attempt apart from the API timeout.
- `ModelInfo.Version` and `ModelInfo.Alias`. Limits held from `Models` apply to a model named by an entry's name, alias or pinned version.
- A session's socket is dialled through the proxy, dialer and TLS settings of the client's HTTP transport.
- The API's fixed limits as constants: `MaxImages`, `MaxImageBytes`, `MaxImagePixels`, `MaxImageURLChars`, `MaxChoiceOptions`, `MaxScoreLevels`, `MaxNumberGridValues`, `MinPollInterval`, `DefaultPollInterval`, `DefaultBatchTimeout`.

### Changed

- `SpecVersion` is `"1.1"`.
- `SessionRequest.Window` and `TTLSeconds`, `BatchRequest.CustomID` and `BatchListOptions.Offset` and `Limit` are pointers, so an explicit 0 or "" is sent and checked rather than taken as not given.
- Every request is checked locally before anything is sent. A fault in its content (an image's format, size or pixel count, too many images or options or levels, a truth without text, a number grid the API would refuse) is the API's own `*BadRequestError` or `*ValidationError`, with no status and no request id. Image bytes of an unknown format were an argument error before; `ImageFile` no longer refuses a readable file of another format, the call does.
- The default transport keeps more idle connections, uses larger buffers, prefers HTTP/2 and resumes TLS sessions.
- Batch create, add and finalize are retried only on 429 and 529.

## [0.1.0] - Unreleased

First release. Implements version 1.0 of the DecisionNode SDK surface.

### Added

- `Client` with `Decide`, `DecideMany`, `Check`, `Classify`, `Score`, `Gate`, `Models` and `DecideRaw`, each taking a `context.Context`.
- Question builders `Truth`, `TruthWith`, `Choice` and `Score`; ordered `Questions` and `Options`; questions in their JSON wire form are accepted too.
- Typed answers `TruthAnswer`, `ChoiceAnswer` and `ScoreAnswer`, and `UnknownAnswer` for answer types added later and for an answer of a known type with missing or wrongly typed fields; such an answer never fails the decision.
- Images from a file, from bytes or from an `https` URL, with the media type taken from the first bytes (JPEG, PNG or WebP) when none is given. `ImageFile` reads the file when the `Image` is made and returns an error for an unreadable file or another format.
- The safety check result on every decision, parsed from the `x-decisionnode-safety` header.
- `Check`, `Classify`, `Score` and `Gate` take `Images` and `Model` like `Decide`; `Gate` sends fixed wording.
- `Decision.LatencyMs` and the `Usage` token counts are pointers: nil when the server did not send them, never 0.
- `Models` returns `[]ModelInfo`; `DecideMany` returns `[]DecisionResult` and an error, and refuses a concurrency below 1 before sending anything.
- `SpecVersion`, the version of the SDK specification implemented: `"1.0"`.
- Typed errors matched with `errors.As`, all wrapping `*Error`. `Error()` is the server's message exactly; the server's explanation is in `Reason`. A 403 that is not a refusal is a `*PermissionDeniedError`. A 3xx or any status without a type of its own is a bare `*Error` with the status. A 2xx whose body is not JSON is a `*ServerError`.
- Only 200 to 299 is a success. Redirects are never followed, also with a client passed to `WithHTTPClient`.
- The base URL must be `https`; `http` is accepted only for `localhost`, `127.0.0.1` and `::1`. A timeout not above 0 and negative retries are refused too. These are plain Go errors, returned by `Client.Err` and by every call before anything is sent.
- Problems with the caller's own arguments (an empty question list, concurrency, image bytes of an unknown format, a value that cannot be encoded) are plain Go errors, not `*Error`.
- An error whose response carried no server message has the empty string as its message.
- Retries on 408, 429, 5xx, timeouts and connection errors, honouring `retry-after-ms` and `Retry-After` (seconds or an HTTP date), capped at 60 s; a 10 s timeout covering each whole attempt, to the last byte of the response body.
- Configuration through options or `DECISIONNODE_API_KEY`, `DECISIONNODE_BASE_URL` and `DECISIONNODE_MODEL`. An empty option or per-call model counts as not given.
- Optional debug logging through `log/slog`.
- The README documents the `x-decisionnode-safety-body` request header and how to send it.
- The conformance test fails when `DECISIONNODE_CONFORMANCE_DIR` is unset instead of skipping.
- `DecisionNodeError`, an alias of `Error`, so the base error has the name every DecisionNode SDK uses.
- A 2xx whose body is not JSON is a `*ServerError` whose `Reason` comes from the `x-decisionnode-error` header.
- `Check`, `Classify` and `Score` return a `*ServerError` (with the status, `RequestID` and `Reason`) when their own answer is missing or malformed; `Decide` keeps returning such an answer as an `UnknownAnswer`.
- `DecideMany` checks every request before sending any; an invalid one is an argument error and nothing is sent.
- `WithHTTPClient` refuses a client whose transport is not `nil` or an `*http.Transport`, and for an `https` base URL one that sets `InsecureSkipVerify` or its own TLS dialer.
- `WithResponseHook`, called with each response received (method, path, attempt, status, and copies of the headers and the body), so a caller can see retried responses and the exact body bytes while keeping a plain `*http.Transport`.
- CI pins every action to a commit, sets `persist-credentials: false` on every checkout, and checks out conformance suite 1.3 at a pinned commit.
