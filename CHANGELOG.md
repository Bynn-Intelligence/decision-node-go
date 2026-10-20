# Changelog

All notable changes to this package are listed here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions follow
[Semantic Versioning](https://semver.org/).

## [0.5.0] - Unreleased

Implements version 1.4 of the DecisionNode SDK surface: knowledge.

### Added

- Knowledge bases: collections of type `text` (`CollectionText`). `Collections().CreateWith(CollectionCreate{Name, Type, ScopeKey})` creates one with an optional scope key; `Collection` gains `ScopeKey`, `Chunks`, `Tokens`, `Version`, `StatusCounts` and `Pinned` (with `LargestScope`), each as sent and empty on a face collection. Deleting a text collection may answer `Status` "purging" with `Deleted` false, which is success too.
- Documents: `DocumentText` (Markdown unless `FormatText`), `DocumentFile`, `DocumentData`, `DocumentReader` and `DocumentUpload`; `AddDocument` and `ReplaceDocument` with `DocumentOptions` (`Title`, `Reference`, `Pinned`, `PinRank`, `Label`, `Metadata`, `Wait`), each with an Idempotency-Key, the same on every attempt. A text or Markdown file under the inline threshold that is UTF-8 with no NUL goes inline, titled by its file name's stem; every other file is uploaded with the knowledge purpose (presign with `purpose` and `filename`, the exact PUT, then `upload_id`), and a stale upload this call made is uploaded again once, under a new key. A file's type comes from its content type, its extension or its first bytes. `Client.UploadDocuments` uploads 1 to 16 documents ahead of time. A document's PUT is allowed `DefaultDocumentUploadTimeout` (120 s) unless `WithUploadTimeout` is given, and a call with `Wait` `WaitTimeout` (20 s) per attempt unless `WithTimeout` is given.
- `CollectionItem` gains a document's fields: `Title`, `Format`, `Pinned`, `PinRank`, `Source`, `File`, `Status`, `Stage`, `Progress`, `Serving`, `Error`, `Delayed`, `Warnings`, `Chunks`, `Tokens`, `ContentSHA256`, `Version`, `QueuedAt`, `ReadyAt` and `Text`. `GetItem` reads one item, with its stored text on request; `ListChunks` pages a document's chunks (`Chunk`, `ChunkPage`); `DeleteFailed` deletes every failed document and returns how many. `CheckDocument` runs a document's local checks without sending it. `ItemUpdate` gains `Pinned`, `PinRank` and `Title`. `ItemListOptions` gains `Q` (a text collection's documents whose title, file name or reference holds it, 1 to 200 characters) and `Sort` (`SortNewest`, `SortOldest`, `SortTitle`), checked locally with the API's messages; the page echoes both.
- `WaitUntilReady` and `WaitUntilCollectionReady` poll until a document is ready or failed, or until nothing is queued or processing: the first read at once, then waits from `WaitOptions.PollInterval` (1 s, at least 0.1 s) growing by half up to 10 s, or a 429's or 529's longer Retry-After, until `WaitOptions.Timeout` (300 s, a deadline from the call), then a `*TimeoutError`. A failed document is returned, not raised. Each read goes through the response hook like any call.
- `Collections().Search` returns a `*KnowledgeReading` with every passage's text.
- The `knowledge` booster: `BoosterKnowledge` and `KnowledgeBooster(collection, KnowledgeOptions)`, each option sent only when set; its reading `*KnowledgeReading` (`Collection`, `CollectionName`, `Corpus`, `Query`, `Budget`, `Scope`, `Warnings`, `Passages`, `Omitted`, `PendingItems`, `Search`, `Truncated`) with `KnowledgePassage`s. A reading or passage with a required field missing or of the wrong type is an `*UnknownReading`. A request naming knowledge keeps the default timeout even when it names `ocr`.
- The local checks of a knowledge base and its documents, with the API's entries: the type, the scope key, a title of 1 to 200 characters (and a title required for a text), a pin rank from 0 to 1,000, the format, an empty text, a reference and a label in the text route's words; a file of a type that is not a knowledge type, empty, or over 50 MB is a `*BadRequestError` with the presign's message, and one whose type cannot be told an argument error.

### Changed

- `SpecVersion` is `"1.4"`, and the conformance tests run suite 1.25.
- `Create` takes `CollectionText` as well as `CollectionFace`; any other type is refused with "Input should be 'face' or 'text'".
- A 529 whose `Details["reason"]` is `ingestion_delayed` is never retried.

## [0.4.0] - Unreleased

Implements version 1.3 of the DecisionNode SDK surface: boosters and fine-tuned models.

### Added

- Depth Vision and Segment Vision: `BoosterDepth` (`bynn_depth`) with `DepthBooster(gridSize)` and `*DepthReading` (`Ahead`, `Directions`, `Ground`, the grid and its nearest, farthest and median distances, in meters), and `BoosterSegment` (`bynn_segment`) with `SegmentBooster(SegmentOptions)` and `*SegmentReading` (the phrases read and each match's label, score, box, outline, area, centre, opening, and with depth on its distance and size in meters). A reading with a field missing or of the wrong type is an `*UnknownReading`. The README's booster table lists every booster.
- Per-key usage: `Client.Usage()` with `Key` (`GET /v1/keys/{key_id}/usage`), `List` (`GET /v1/usage`) and `All`, which follows `NextCursor`; `KeyUsage`, `UsagePage` and `UsageItem` with their fields typed and `Raw` as sent. The query's rules are checked locally with the API's messages.
- Keys for devices: `Client.Keys()` with `Create` (with an `Idempotency-Key`, yours or a new UUID, retried as Collections' `Create`), `List`, `All`, `Update` (a `Clear` field sends null), `Revoke` and `RevokeByLabels` (with `DryRun`), for a Manage device keys key; `Key`, `CreatedKey` (whose `String` hides the secret), `KeyPage`, `RevokedKey` and `KeysRevoked`. The fixed rules are checked locally with the API's own errors.
- A 429 `key_spend_cap_reached` is a `*RateLimitError` that is never retried. A publishable key's 403 `origin_not_allowed` and `model_not_allowed` are a `*PermissionDeniedError` with `Details`, and its 429 `client_rate` and `in_flight` a `*RateLimitError`, retried as any 429.
- Boosters: `Request.Boosters` takes up to 4 boosters, in order, each a name (`BoosterAge`, `BoosterAIGenerated`, or any string), a `Booster` (`Name`, `Images`, `Require` as a `*bool`, `Options`, sent as `{"name", "images", "require", "options"}`, each left out when nil, or as the bare name when only `Name` is set) or a booster in its JSON wire form. The helper requests, `DecideMany` and batch requests take them as `Decide` does; sessions do not. Without boosters the request body is unchanged. Nothing about boosters is checked locally except the API's nesting limit, which counts them.
- `Decision.Evidence`, an `EvidenceList` in the request's order with `Get(name)`: `Evidence` with `Booster`, `Version`, `Status`, `LatencyMs`, `BilledCalls` and `Results`, each result an `AgeReading`, an `AIGeneratedReading` (with `TopGenerator`, nil unless the picture reads as likely generated, `C2PASigner` and `C2PA`) or, for a booster this version does not know or a reading with a field missing or of the wrong type, an `UnknownReading` with its raw fields. Nil when the request named no boosters.
- `Usage.BoosterTokens` and `Usage.Boosters`, nil for a request without boosters.
- `Error.Boosters` on every error: the `x-decisionnode-boosters` header as `BoosterStatus` entries (`Name`, `Status`, `LatencyMs`) in the request's order, so a caller sees which required booster failed. `ParseBoosters` reads the same header of a decision.
- Fine-tuned models: a fine-tuned model's name is sent as given in `Model`, and `Decision.Model` carries the version that answered. `ModelInfo` gains `FineTuned` (nil for a catalogue model), `Base`, `AdapterVersion` and `Status`; `Version` stays the pinned name.

- The API's 25 MiB body limit: `MaxBodyBytes` (26,214,400 bytes) and `RequestTooLargeError`. A request body, or a batch line, over it is refused before anything is sent or uploaded (measured as it would go out, an image to upload by its upload id), and the API's 413 is the same error, never retried. Inline images and video frames count toward it; uploaded images and URLs do not, and local images at or above the inline threshold are uploaded.

- Fallback: `Request.Fallback` (and the helper requests', `DecideMany`'s and batch lines'), sent as `"fallback": true | false` only when set; `Fallback: Ptr(false)` makes a request for `decisionnode-moe` wait for its own model (the 529, retried as any) rather than be answered by `decisionnode-flash`. `Decision.Fallback` (`ServedModel`, `RequestedModel`) from the `x-decisionnode-fallback` and `x-decisionnode-requested-model` headers, nil on an ordinary answer.

- An error whose body is not JSON (an edge's or a proxy's page, or no body) is classed by its status alone, with an empty `ErrorType`, `RequestID` and `Reason` from their headers, nothing read from the body, and the message `HTTP <status> <text>` from a fixed table (`HTTP 413 Content Too Large`), then ` (request id <id>)` when sent. A batch result line without a body is read as a JSON null: its error keeps the message "".
- An image id over 64 characters (code points, `MaxImageIDChars`) is refused before anything is sent: "Image <n>: its id is longer than 64 characters.", after the empty-id check and before the duplicate check, on decide, the helpers, `DecideMany` (for the whole call), batch lines and `CheckDecide`; a session frame's image goes without its id and is not checked for it.

- Several places in one answer: `Points`, `PointsWith`, `Boxes` and `BoxesWith` ask a point or box question for up to `max` places, a whole number from `MinPlaces` to `MaxPlaces` (2 to 16), sent as the question's `max`. The answer is a `*PointsAnswer` or a `*BoxesAnswer` (`Count`, `Present`, `Complete`, `Points` or `Boxes`, each place with its `Confidence`, then `Image`, `Width`, `Height`), read with `Decision.Points` and `Decision.Boxes` and told by the answer's own list, so a batch result line is typed the same way; a malformed one is an `UnknownAnswer`. A `max` that is not a whole number from 2 to 16 is refused locally with a `*ValidationError` and the API's entries. Whether a model version answers several places is the server's to say: its 422 `not_supported` is a `*ValidationError`. A session's open leaves its point and box questions to the server.
- The boosters `face_detection`, `face_recognition` and `ocr`: `BoosterFaceDetection`, `BoosterFaceRecognition` and `BoosterOCR`, and `OCRBooster(prompt)` and `FaceRecognitionBooster(collection, threshold)` for their options, sent as given. Their readings: `FaceDetectionReading` (`Image`, `Faces`, `Detections` of `Box` and `Confidence`), `FaceRecognitionReading` (`Image`, `Faces`, `Matches` of `Box`, `Score`, `Collection`, `Reference`, and `Name` or `Label`, "" when not sent) and `OCRReading` (`Image`, `Text`, `Truncated`); a box (`FaceBox`) holds its corners as sent.
- Collections of faces, which `face_recognition` searches: `Client.Collections()` with `Create(name, type)`, `List`, `Get`, `Delete`, `Enroll(collection, Enrollment{Image, Reference, Label})`, `ListItems(collection, ItemListOptions)` and `DeleteItem`, returning `Collection`, `CollectionItem`, `CollectionItemPage` and `Deleted` with every field as sent and `Raw`. `Create` and `Enroll` send an `Idempotency-Key`, a new UUID for each call and the same on every attempt, and are retried like any call, a 409 `idempotency_in_progress` too after its wait; a 409 `idempotency_key_reused` is returned as the base `*Error`. A type other than face, a name not 1 to 80 characters, a reference not 1 to 64 of `A-Z a-z 0-9 _ . : -` and a label empty or over 128 characters are refused locally with a `*ValidationError` and the API's entries, each input echoed as the API echoes it (a string cut to 64 characters with "..."); the enrolled image is sent as a decision's, its URL checked by a decision's URL rules and any other fault left to the server's 422 `image_unreadable`; `ListItems`' offset and limit are checked as `Batches().List` checks them.
- `FaceRecognitionReading.CollectionName`, an own collection's display name, and on each `FaceMatch` its `CollectionName` and the item's `Metadata`, each empty when not sent; either of the wrong type makes the reading an `UnknownReading`.
- `ModelInfo`'s prices as sent: `PricePerMillionInput`, `PricePerMillionOutput`, `PricePerMillionImage`, `PricePerMillionVideo` and `BatchPriceFactor`, the share of the live prices a batch line pays (nil on a fine-tuned model).
- Collections items described and edited: `Enrollment.Metadata` and `CollectionItem.Metadata`; `UpdateItem(collection, item, ItemUpdate{Reference, Label, ClearLabel, Metadata})`, a PATCH of only the fields given with its own `Idempotency-Key`; `ItemListOptions.Reference` and `ReferencePrefix`, and the page's `TotalCapped`, `Reference` and `ReferencePrefix`. Metadata and the lookup are checked locally with the API's entries and messages.
- `Error.Details`, the body's `detail.details` when it is an object. A 529 with `Details["reason"]` `enrollment_unpriced` is never retried; a 403 `key_scope_denied` is a `*PermissionDeniedError`.
- `Usage.Cost` (`USD`, `ModelUSD`, `BoostersUSD`, `FreeTier`): what an answer cost, exactly as the API sent it, on decisions, batch result lines and session replies; nil when absent.

- A decision whose boosters name `ocr` (by name or as an object) waits `OCRTimeout`, 30 s, per attempt unless `WithTimeout` was given, and is never retried after a timeout, since the server finishes that attempt and may bill it; its other retries stay. This holds for `Decide`, the helpers and each `DecideMany` request.

### Changed

- A Depth Vision reading whose `directions` is not exactly 7 entries, or whose grid sides or picture size are not whole numbers of 0 or more, is an `*UnknownReading`, as is a Segment Vision reading whose picture size is not; `ahead` and a match's depth medians may be left out (nil). `SegmentSize` and `SegmentOpeningSize` hold `Width` and `Height` as `float64`, which the API always sends. The conformance tests run suite 1.22.
- `Batch` has no `PriceFactor`, and `ModelBatch` no `PricePerMillionInputTokens` or `PriceFactor`: the API sends none of them. The batch price is each model's `BatchPriceFactor`, and each line's charge is in its usage.
- The default model is `decisionnode-moe` (it was `decisionnode-latest`), the family id that always means the current release; `decisionnode-flash` is the fast one. A pinned id is `decisionnode-<family>-<version>`, read from the model list, and an answer names the pinned id that answered. The earlier ids stay accepted by the API.
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
