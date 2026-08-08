package render

import "fmt"

// Kind classifies a render failure so the HTTP layer can pick a status code
// without string-matching on error text.
type Kind int

const (
	// KindInvalid is a caller mistake: bad JSON, out-of-range option, unknown
	// page size. Maps to 400.
	KindInvalid Kind = iota
	// KindTooLarge is a request body over the configured limit. Maps to 413.
	KindTooLarge
	// KindTimeout is a render that exceeded its deadline. Maps to 504.
	KindTimeout
	// KindUnavailable is no usable browser after a retry. Maps to 503.
	KindUnavailable
	// KindInternal is anything else. Maps to 500, and its message must not
	// reach the client.
	KindInternal
)

func (k Kind) String() string {
	switch k {
	case KindInvalid:
		return "invalid_request"
	case KindTooLarge:
		return "payload_too_large"
	case KindTimeout:
		return "render_timeout"
	case KindUnavailable:
		return "browser_unavailable"
	default:
		return "internal_error"
	}
}

// Error carries a Kind alongside the underlying cause.
type Error struct {
	Kind Kind
	Msg  string
	Err  error
}

func (e *Error) Error() string {
	if e.Err != nil {
		return e.Msg + ": " + e.Err.Error()
	}
	return e.Msg
}

func (e *Error) Unwrap() error { return e.Err }

func invalidf(format string, args ...any) *Error {
	return &Error{Kind: KindInvalid, Msg: fmt.Sprintf(format, args...)}
}

func internalf(err error, format string, args ...any) *Error {
	return &Error{Kind: KindInternal, Msg: fmt.Sprintf(format, args...), Err: err}
}
