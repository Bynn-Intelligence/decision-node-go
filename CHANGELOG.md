# Changelog

All notable changes to this package are listed here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions follow
[Semantic Versioning](https://semver.org/).

## [0.2.0] - Unreleased

Implements version 1.1 of the DecisionNode SDK surface: every public call.

### Added

- Number questions: `Number` and `NumberWith` builders (`Min`, `Max` and `Step` on `Question`), `NumberAnswer` with `Number`, `Expected`, `Confidence` and `Probabilities` keyed by value as sent, `Decision.Number`, and the `Client.Number` helper.
- `Client.Systemone` for `POST /v1/systemone`, which writes truth questions with that path's wire type.
- Images: `ImageBase64`, `ImageReader` and `ImageUpload`; `Image.MediaType`; `Client.Upload`. Local images are sent inline under `DefaultInlineThreshold` (524,288 bytes, changed with `WithInlineThreshold`) and uploaded at or above it, with one presign per request and the uploads in parallel. Uploads are cached in memory by content hash until 5 minutes before they expire, never uploaded twice at once, and uploaded again once when the server reports one stale.
- Sessions: `Client.OpenSession`, `Session.Connect`, `Send`, `Next`, `Replies`, `End` and `Close`, the events `Reply`, `FrameError` and `SessionClosed`, and `Client.Supervise`.
- The Batch API: `Client.Batches` with `Create`, `Add` (split at 10 MiB), `Finalize`, `Get`, `List`, `Results` and `Cancel`, and `Client.RunBatch`.
- `SessionLimitError`, a `RateLimitError` that is never retried.
- `ModelInfo` typed fields for the number, upload, session and batch limits.
- The API's fixed limits as constants: `MaxImages`, `MaxImageBytes`, `MaxImagePixels`, `MaxImageURLChars`, `MaxChoiceOptions`, `MaxScoreLevels`, `MaxNumberGridValues`, `MinPollInterval`, `DefaultPollInterval`, `DefaultBatchTimeout`.

### Changed

- `SpecVersion` is `"1.1"`.
- Every request is checked locally before anything is sent. A fault in its content (an image's format, size or pixel count, too many images or options or levels, a truth without text, a number grid the API would refuse) is the API's own `*BadRequestError` or `*ValidationError`, with no status and no request id. Image bytes of an unknown format were an argument error before; `ImageFile` no longer refuses a readable file of another format, the call does.
- An answer of wire type `noul` is read into a `TruthAnswer`.
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
