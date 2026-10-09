package decisionnode

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Error is the base of every error this package returns. Each specific error
// type below wraps an *Error, so a single errors.As check reads the common
// fields whatever the cause:
//
//	var apiErr *decisionnode.Error
//	if errors.As(err, &apiErr) {
//		log.Println(apiErr.Status, apiErr.RequestID, apiErr.Reason, apiErr)
//	}
//
// A status with no type of its own (a 3xx, 405, 409, 408 after its retries)
// is a bare *Error carrying that status.
type Error struct {
	// Status is the HTTP status, or 0 when no response was received.
	Status int
	// ErrorType is detail.error_type from the response body, when the body has one.
	ErrorType string
	// RequestID is the x-request-id response header.
	RequestID string
	// Reason is the server's explanation in words, from the
	// x-decisionnode-error response header.
	Reason string
	// Body is the parsed response body, or nil when it was not JSON.
	Body any
	// Header holds the response headers.
	Header http.Header
	// Boosters is how each booster of a request that named boosters ended,
	// from the x-decisionnode-boosters header, in the request's order: it
	// shows which required booster failed. Nil when the header is absent.
	Boosters []BoosterStatus
	// Err is the underlying cause, such as a network error, when there is one.
	Err error

	msg string
}

// Error returns the error's message: the server's message exactly as sent
// (detail.message, or detail when it is a string), empty when the server sent
// none, or the client's own description when no response was received. The
// server's explanation in words is in Reason.
func (e *Error) Error() string { return e.msg }

// Unwrap returns the underlying cause, if any.
func (e *Error) Unwrap() error { return e.Err }

// DecisionNodeError is the base error under the name every DecisionNode SDK
// exports. It is the same type as Error:
//
//	var e *decisionnode.DecisionNodeError
//	if errors.As(err, &e) { ... }
type DecisionNodeError = Error

// errorBase lets the specific error types embed *Error while keeping their own
// Error and Unwrap methods. The fields of Error are promoted as usual.
type errorBase = Error

// BadRequestError is a 400: the request is well formed but not usable.
type BadRequestError struct{ *errorBase }

func (e *BadRequestError) Unwrap() error { return e.errorBase }

// ContextLimitError is a 400 with error_type max_tokens_exceeded: the request
// is over the model's token limit. It is also a *BadRequestError.
type ContextLimitError struct{ *errorBase }

func (e *ContextLimitError) Unwrap() error { return &BadRequestError{e.errorBase} }

// AuthenticationError is a 401, or a call made without an API key.
type AuthenticationError struct{ *errorBase }

func (e *AuthenticationError) Unwrap() error { return e.errorBase }

// InsufficientCreditError is a 402: the account is out of credit.
type InsufficientCreditError struct{ *errorBase }

func (e *InsufficientCreditError) Unwrap() error { return e.errorBase }

// RefusalError is a 403 refusal: the usage policy refused the request. It is
// never retried, and the same request is refused again.
type RefusalError struct {
	*errorBase
	// Category is the refused category, "harm" or "self_harm".
	Category string
	// Safety is what the safety check found, from the x-decisionnode-safety header.
	Safety *Safety
}

func (e *RefusalError) Unwrap() error { return e.errorBase }

// PermissionDeniedError is a 403 that is not a refusal: a booster turned off
// for the workspace (ErrorType booster_not_enabled), a suspended workspace.
type PermissionDeniedError struct{ *errorBase }

func (e *PermissionDeniedError) Unwrap() error { return e.errorBase }

// NotFoundError is a 404.
type NotFoundError struct{ *errorBase }

func (e *NotFoundError) Unwrap() error { return e.errorBase }

// ValidationError is a 422: a field is missing or has the wrong type.
type ValidationError struct {
	*errorBase
	// Entries is the server's list of problems, one entry per field.
	Entries []any
}

func (e *ValidationError) Unwrap() error { return e.errorBase }

// RateLimitError is a 429.
type RateLimitError struct {
	*errorBase
	// RetryAfter is the wait the server asked for, from retry-after-ms or
	// Retry-After (seconds or an HTTP date), capped at 60 s. Zero when the
	// server sent neither.
	RetryAfter time.Duration
}

func (e *RateLimitError) Unwrap() error { return e.errorBase }

// SessionLimitError is a 429 with error_type session_limit_exceeded: the key
// already holds as many open sessions as it may. It is never retried, since
// the count frees when a session ends, not when time passes. It is also a
// *RateLimitError.
type SessionLimitError struct {
	*errorBase
	// RetryAfter is the wait the server named, capped at 60 s. Zero when the
	// server sent none.
	RetryAfter time.Duration
}

func (e *SessionLimitError) Unwrap() error {
	return &RateLimitError{errorBase: e.errorBase, RetryAfter: e.RetryAfter}
}

// OverloadedError is a 529: the service is busy, or (ErrorType
// booster_unavailable) a booster the request requires failed or did not
// answer in time. It is retried by default.
type OverloadedError struct {
	*errorBase
	// RetryAfter is the wait the server asked for, from retry-after-ms or
	// Retry-After (seconds or an HTTP date), capped at 60 s. Zero when the
	// server sent neither.
	RetryAfter time.Duration
}

func (e *OverloadedError) Unwrap() error { return e.errorBase }

// RequestTooLargeError is a 413: the request body is over MaxBodyBytes. The
// client refuses such a body itself before sending anything, with the same
// error and no status. It is never retried.
type RequestTooLargeError struct{ *errorBase }

func (e *RequestTooLargeError) Unwrap() error { return e.errorBase }

// ServerError is any other 5xx.
type ServerError struct{ *errorBase }

func (e *ServerError) Unwrap() error { return e.errorBase }

// ConnectionError means no response was received: the connection failed or
// was dropped.
type ConnectionError struct{ *errorBase }

func (e *ConnectionError) Unwrap() error { return e.errorBase }

// TimeoutError means an attempt took longer than the client's timeout.
type TimeoutError struct{ *errorBase }

func (e *TimeoutError) Unwrap() error { return e.errorBase }

// Timeout reports true, so the error satisfies the net.Error convention.
func (e *TimeoutError) Timeout() bool { return true }

// newError builds the typed error for an HTTP response that is not a success.
func newError(status int, header http.Header, body []byte) error {
	base := &Error{
		Status:    status,
		RequestID: header.Get("x-request-id"),
		Reason:    header.Get("x-decisionnode-error"),
		Header:    header,
		Boosters:  ParseBoosters(header),
	}
	var parsed any
	if len(body) > 0 && json.Unmarshal(body, &parsed) == nil {
		base.Body = parsed
	}
	var entries []any
	var category string
	if obj, ok := parsed.(map[string]any); ok {
		switch detail := obj["detail"].(type) {
		case map[string]any:
			base.ErrorType, _ = detail["error_type"].(string)
			base.msg, _ = detail["message"].(string)
			category, _ = detail["category"].(string)
		case string:
			base.msg = detail
		case []any:
			entries = detail
		}
	}

	switch {
	case status == 400 && base.ErrorType == "max_tokens_exceeded":
		return &ContextLimitError{base}
	case status == 400:
		return &BadRequestError{base}
	case status == 401:
		return &AuthenticationError{base}
	case status == 402:
		return &InsufficientCreditError{base}
	case status == 403 && base.ErrorType == "refusal_error":
		return &RefusalError{errorBase: base, Category: category, Safety: parseSafety(header.Values("x-decisionnode-safety"))}
	case status == 403:
		return &PermissionDeniedError{base}
	case status == 404:
		return &NotFoundError{base}
	case status == 413:
		return &RequestTooLargeError{base}
	case status == 422:
		return &ValidationError{errorBase: base, Entries: entries}
	case status == 429 && base.ErrorType == "session_limit_exceeded":
		wait, _ := retryAfter(header)
		return &SessionLimitError{errorBase: base, RetryAfter: wait}
	case status == 429:
		wait, _ := retryAfter(header)
		return &RateLimitError{errorBase: base, RetryAfter: wait}
	case status == 529:
		wait, _ := retryAfter(header)
		return &OverloadedError{errorBase: base, RetryAfter: wait}
	case status >= 500 && status <= 599:
		return &ServerError{base}
	default:
		return base
	}
}

// maxRetryAfter caps the wait a server may ask for.
const maxRetryAfter = 60 * time.Second

// retryAfter reads the wait a server asked for: retry-after-ms first, then
// Retry-After in seconds or as an HTTP date, capped at 60 s.
func retryAfter(header http.Header) (time.Duration, bool) {
	d, ok := retryAfterHeader(header)
	if ok && d > maxRetryAfter {
		d = maxRetryAfter
	}
	return d, ok
}

func retryAfterHeader(header http.Header) (time.Duration, bool) {
	if v := strings.TrimSpace(header.Get("retry-after-ms")); v != "" {
		if ms, err := strconv.ParseFloat(v, 64); err == nil && ms >= 0 {
			if ms > float64(maxRetryAfter/time.Millisecond) {
				return maxRetryAfter, true
			}
			return time.Duration(ms * float64(time.Millisecond)), true
		}
	}
	if v := strings.TrimSpace(header.Get("retry-after")); v != "" {
		if s, err := strconv.ParseFloat(v, 64); err == nil && s >= 0 {
			if s > maxRetryAfter.Seconds() {
				return maxRetryAfter, true
			}
			return time.Duration(s * float64(time.Second)), true
		}
		if t, err := http.ParseTime(v); err == nil {
			d := time.Until(t)
			if d < 0 {
				d = 0
			}
			return d, true
		}
	}
	return 0, false
}

// localBadRequest is the error for a request whose content breaks a fixed
// rule of the API, found before anything is sent: the class, error_type and
// message the API itself would answer with, but no status and no request id.
func localBadRequest(errorType, message, reason string) error {
	return &BadRequestError{&Error{ErrorType: errorType, Reason: reason, msg: message}}
}

// bodyTooLarge is the local form of the API's 413 for a body of n bytes.
func bodyTooLarge(n int) error {
	return &RequestTooLargeError{&Error{
		ErrorType: "request_too_large_error",
		Reason:    fmt.Sprintf("the body is %d bytes; the limit is %d", n, MaxBodyBytes),
		msg:       "The request body is over 26,214,400 bytes (25 MiB). Send large images as uploads (POST /v1/uploads) and name them by upload_id.",
	}}
}

// checkBodySize refuses an encoded body over MaxBodyBytes.
func checkBodySize(n int) error {
	if n > MaxBodyBytes {
		return bodyTooLarge(n)
	}
	return nil
}

// localValidation is the local form of a 422: entries in the API's shape, the
// reason in words, no status and no request id. Its message is the reason, so
// the error says what is wrong.
func localValidation(reason string, entries ...any) error {
	return &ValidationError{errorBase: &Error{Reason: reason, msg: reason}, Entries: entries}
}
