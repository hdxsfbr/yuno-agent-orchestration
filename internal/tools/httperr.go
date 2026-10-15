package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"syscall"
)

// Transient classification for tools that call HTTP services (DESIGN 5.5 rule 4):
// timeouts, refused or reset connections and 5xx answers may be retried; anything
// else, including a body that is not JSON, is permanent.

// transportFailure turns a failed request into an error result.
func transportFailure(what string, err error) Result {
	return Result{IsError: true, Transient: isTransientNetErr(err), Text: fmt.Sprintf("%s: %s", what, describeNetErr(err))}
}

func isTransientNetErr(err error) bool {
	var ne net.Error
	switch {
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &ne) && ne.Timeout():
		return true
	case errors.Is(err, syscall.ECONNREFUSED), errors.Is(err, syscall.ECONNRESET), errors.Is(err, io.ErrUnexpectedEOF):
		return true
	}
	return false
}

func describeNetErr(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "timed out"
	case errors.Is(err, syscall.ECONNREFUSED):
		return "connection refused"
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return "timed out"
	}
	return err.Error()
}

// statusFailure is the result for a non-2xx answer: 5xx is transient, the rest
// permanent. The body's first line helps the model or the operator see why.
func statusFailure(what string, resp *http.Response, body []byte) Result {
	msg := strings.TrimSpace(string(body))
	if i := strings.IndexByte(msg, '\n'); i >= 0 {
		msg = msg[:i]
	}
	if len(msg) > 300 {
		msg = msg[:300]
	}
	text := fmt.Sprintf("%s: HTTP %d", what, resp.StatusCode)
	if msg != "" {
		text += ": " + msg
	}
	return Result{IsError: true, Transient: resp.StatusCode >= 500, Text: text}
}

// decodeJSONBody decodes a JSON answer; a body that is not JSON is a permanent error.
func decodeJSONBody(what string, body []byte, v any) *Result {
	if err := json.Unmarshal(body, v); err != nil {
		return &Result{IsError: true, Text: what + ": the response is not JSON"}
	}
	return nil
}

// readBody reads at most limit bytes of a response body.
func readBody(resp *http.Response, limit int64) ([]byte, error) {
	return io.ReadAll(io.LimitReader(resp.Body, limit))
}
