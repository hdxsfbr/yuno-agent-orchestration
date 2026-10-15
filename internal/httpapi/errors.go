package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"yuno/internal/obs"
	"yuno/internal/store"
)

var errEngineUnavailable = errors.New("the run engine is not available")

// errorBody is the one error shape every endpoint returns (DESIGN section 6).
type errorBody struct {
	Error errorDetail `json:"error"`
}

type errorDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Details any    `json:"details,omitempty"`
}

// fieldError is one entry of a 422's details: the JSON path of the offending field
// (e.g. "tool_allowlist[2]", "interaction_rules.exec") and what is wrong with it.
type fieldError struct {
	Path    string `json:"path"`
	Message string `json:"message"`
}

// fieldErrors collects validation failures in request order.
type fieldErrors []fieldError

func (fe *fieldErrors) add(path, format string, args ...any) {
	*fe = append(*fe, fieldError{Path: path, Message: fmt.Sprintf(format, args...)})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

func writeError(w http.ResponseWriter, status int, code, message string, details any) {
	writeJSON(w, status, errorBody{Error: errorDetail{Code: code, Message: message, Details: details}})
}

// writeValidation answers 422 with path-qualified details.
func writeValidation(w http.ResponseWriter, message string, fe fieldErrors) {
	writeError(w, http.StatusUnprocessableEntity, "validation_failed", message, fe)
}

// decodeJSON reads a bounded JSON body into v and rejects unknown fields.
func decodeJSON(r *http.Request, v any, maxBytes int64) error {
	r.Body = http.MaxBytesReader(nil, r.Body, maxBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if dec.More() {
		return errors.New("trailing data after the JSON body")
	}
	return nil
}

// decodeBody decodes a request body and answers 422 itself when it cannot. It
// reports whether the handler should continue.
func decodeBody(w http.ResponseWriter, r *http.Request, v any, maxBytes int64) bool {
	err := decodeJSON(r, v, maxBytes)
	if err == nil {
		return true
	}
	writeValidation(w, "invalid request body", fieldErrors{bodyFieldError(err)})
	return false
}

// bodyFieldError turns a decoding error into a path-qualified detail.
func bodyFieldError(err error) fieldError {
	var typeErr *json.UnmarshalTypeError
	var syntaxErr *json.SyntaxError
	var tooBig *http.MaxBytesError
	switch {
	case errors.As(err, &typeErr):
		return fieldError{Path: typeErr.Field, Message: "must be " + jsonTypeName(typeErr.Type.Kind().String())}
	case errors.As(err, &syntaxErr):
		return fieldError{Path: "", Message: "malformed JSON: " + syntaxErr.Error()}
	case errors.As(err, &tooBig):
		return fieldError{Path: "", Message: fmt.Sprintf("body exceeds %d bytes", tooBig.Limit)}
	case errors.Is(err, io.EOF):
		return fieldError{Path: "", Message: "body is required"}
	}
	msg := err.Error()
	if name, ok := strings.CutPrefix(msg, "json: unknown field "); ok {
		return fieldError{Path: strings.Trim(name, `"`), Message: "unknown field"}
	}
	return fieldError{Path: "", Message: strings.TrimPrefix(msg, "json: ")}
}

func jsonTypeName(kind string) string {
	switch kind {
	case "string":
		return "a string"
	case "bool":
		return "a boolean"
	case "slice", "array":
		return "an array"
	case "map", "struct":
		return "an object"
	case "int", "int8", "int16", "int32", "int64", "uint", "uint8", "uint16", "uint32", "uint64":
		return "an integer"
	case "float32", "float64":
		return "a number"
	}
	return "a " + kind
}

// storeError answers 404 for store.ErrNotFound and 500 for anything else.
func (s *Server) storeError(w http.ResponseWriter, r *http.Request, what string, err error) {
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", what+" not found", nil)
		return
	}
	s.internalError(w, r, what, err)
}

// internalError logs the cause and answers 500 without it.
func (s *Server) internalError(w http.ResponseWriter, r *http.Request, op string, err error) {
	s.log.Error("request failed", "request_id", obs.RequestID(r.Context()), "op", op, "error", err)
	writeError(w, http.StatusInternalServerError, "internal", "internal error", nil)
}
