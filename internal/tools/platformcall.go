package tools

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/jackc/pgx/v5/pgxpool"

	"yuno/internal/actor"
	"yuno/internal/obs"
	"yuno/internal/store"
)

// The Chief's platform tools (Chief platform control, wave 1, DESIGN A2 to A4, A8, A9):
// platform_operations finds operations in the contract, platform_call runs one through the
// api's own handler and middleware, in-process, as the calling agent. The api's platform
// gate decides what an agent may do (A3, A5, A6, A8); these tools build the request and
// shape the answer.

const (
	PlatformOperationsName = "platform_operations"
	PlatformCallName       = "platform_call"

	// platformOperationsMax is how many operations one search returns (rule A2).
	platformOperationsMax = 20
	// platformSchemaDepth is how deep a request body's $refs are resolved (rule A2).
	platformSchemaDepth = 2
	// PlatformResultMax caps a platform_call result's text for the model (rule A8).
	PlatformResultMax = 32 << 10
	// platformOperationsBudget keeps a search's text under the MCP cap: past it the
	// body schemas are resolved less deep.
	platformOperationsBudget = 30 << 10
)

// Headers between platform_call and the api (rule A3, DESIGN B3 and B4).
const (
	HeaderIdempotencyKey      = "Idempotency-Key"
	HeaderIdempotencyReplayed = "Idempotency-Replayed"
	HeaderIfMatch             = "If-Match"
	HeaderETag                = "ETag"
	HeaderRequestID           = "X-Request-Id"
	// HeaderPlatformGate marks an answer of the platform gate rather than of the handler:
	// "denied" (the body is an Error) or "pending_approval" (the body is the pending answer).
	HeaderPlatformGate = "X-Yuno-Platform-Gate"
	GateDenied         = "denied"
	GatePending        = "pending_approval"
)

// PlatformRequest is one in-process API request of platform_call.
type PlatformRequest struct {
	Actor  actor.Actor
	Method string
	// Target is the escaped path with its query.
	Target         string
	Body           []byte
	IdempotencyKey string
	IfMatch        string
}

// PlatformResponse is the handler's answer: the status, the headers, the body (up to the
// dispatcher's cap) and the size and SHA-256 of the whole body.
type PlatformResponse struct {
	Status int
	Header http.Header
	Body   []byte
	Size   int64
	SHA256 string
}

// PlatformDispatcher sends a request through the api's handler, with the server's own
// bearer and the request's actor in the context (rule A3): implemented by the api, since
// this package cannot import it.
type PlatformDispatcher interface {
	Dispatch(ctx context.Context, req PlatformRequest) (*PlatformResponse, error)
}

var platformOperationsSchema = json.RawMessage(`{
  "type": "object",
  "properties": {` + tokenProperties + `
    "query": {"type": "string", "description": "Words to find in an operation's id, path, summary or description, such as \"pause schedule\" or \"agent files\". Leave query and tag out to list the tags."},
    "tag": {"type": "string", "description": "Only the operations of this tag, such as agents, runs or schedules."}
  },
  "additionalProperties": false
}`)

var platformCallSchema = json.RawMessage(`{
  "type": "object",
  "properties": {` + tokenProperties + `
    "operation_id": {"type": "string", "minLength": 1, "description": "The operation to run, as platform_operations names it."},
    "path_params": {"type": "object", "additionalProperties": {"type": ["string", "number"]}, "description": "A value for every {name} in the operation's path."},
    "query": {"type": "object", "description": "Query parameters by name."},
    "body": {"type": "object", "description": "The JSON request body, matching the operation's request_body."},
    "idempotency_key": {"type": "string", "minLength": 1, "maxLength": 200, "description": "Required for everything but a read: a new key for each new action, the same key when you repeat that action (after an approval, or after a failure)."},
    "if_match": {"type": "string", "description": "The etag of your latest read of the target; required to update or delete something you can read."}
  },
  "required": ["operation_id"],
  "additionalProperties": false
}`)

// NewPlatformOperations builds platform_operations (rule A2).
func NewPlatformOperations() *Tool {
	return &Tool{
		Name: PlatformOperationsName,
		Description: "Find the platform's API operations you can run with platform_call: every action of the web UI, " +
			"such as creating, changing or deleting agents, workflows, schedules, skills and projects, and reading runs. " +
			"Give query (words) or tag; with neither you get the tags. Each operation comes with its id, method, path, " +
			"risk, parameters and request body schema, at most 20 at a time.",
		InputSchema: platformOperationsSchema,
		Risk:        RiskReadOnly,
		Hits:        HitsNone,
		Timeout:     5 * time.Second,
		Chat:        true,
		Exec: func(ctx context.Context, call Call) (Result, error) {
			var in struct {
				Query string `json:"query"`
				Tag   string `json:"tag"`
			}
			if err := json.Unmarshal(call.Input, &in); err != nil {
				return Result{}, &ModelError{Message: "invalid input: " + err.Error()}
			}
			c, err := Contract()
			if err != nil {
				return Result{}, err
			}
			return JSONResult(searchOperations(c, in.Query, in.Tag)), nil
		},
	}
}

// searchOperations is platform_operations' answer (rule A2): the tags with their counts
// when neither query nor tag is given, else the matching operations, at most 20, with
// "more" saying how many were left out. Machine operations are never offered.
func searchOperations(c *PlatformContract, query, tag string) map[string]any {
	query, tag = strings.ToLower(strings.TrimSpace(query)), strings.TrimSpace(tag)
	offered := make([]*PlatformOperation, 0, len(c.ops))
	for _, o := range c.ops {
		if o.Risk != PlatformMachine {
			offered = append(offered, o)
		}
	}
	if query == "" && tag == "" {
		counts := map[string]int{}
		for _, o := range offered {
			for _, t := range o.Tags {
				counts[t]++
			}
		}
		type tagView struct {
			Tag        string `json:"tag"`
			Operations int    `json:"operations"`
		}
		tags := []tagView{}
		for _, t := range c.tags {
			if counts[t] > 0 {
				tags = append(tags, tagView{t, counts[t]})
			}
		}
		return map[string]any{"tags": tags}
	}
	words := strings.Fields(query)
	var found []*PlatformOperation
	for _, o := range offered {
		if tag != "" && !slices.ContainsFunc(o.Tags, func(t string) bool { return strings.EqualFold(t, tag) }) {
			continue
		}
		hay := strings.ToLower(strings.Join([]string{o.ID, o.Method, o.Path, o.Summary, o.op.Description, strings.Join(o.Tags, " ")}, " "))
		if !allWords(hay, words) {
			continue
		}
		found = append(found, o)
	}
	more := 0
	if len(found) > platformOperationsMax {
		more, found = len(found)-platformOperationsMax, found[:platformOperationsMax]
	}
	var out map[string]any
	for depth := platformSchemaDepth; depth >= 0; depth-- {
		views := make([]map[string]any, 0, len(found))
		for _, o := range found {
			views = append(views, operationView(o, depth))
		}
		out = map[string]any{"operations": views}
		if more > 0 {
			out["more"] = more
		}
		if b, err := json.Marshal(out); err == nil && len(b) <= platformOperationsBudget {
			break
		}
	}
	return out
}

func allWords(hay string, words []string) bool {
	for _, w := range words {
		if !strings.Contains(hay, w) {
			return false
		}
	}
	return true
}

// operationView is one operation as platform_operations shows it.
func operationView(o *PlatformOperation, depth int) map[string]any {
	v := map[string]any{"operation_id": o.ID, "method": o.Method, "path": o.Path, "summary": o.Summary, "risk": o.Risk}
	if o.Tool != "" {
		v["use_tool"] = o.Tool
	}
	var pathParams, queryParams []map[string]any
	for _, p := range o.params {
		if p.Value == nil {
			continue
		}
		pv := map[string]any{"name": p.Value.Name}
		if p.Value.Description != "" {
			pv["description"] = p.Value.Description
		}
		if p.Value.Schema != nil {
			pv["schema"] = schemaView(p.Value.Schema, depth, 0)
		}
		switch p.Value.In {
		case openapi3.ParameterInPath:
			pathParams = append(pathParams, pv)
		case openapi3.ParameterInQuery:
			if p.Value.Required {
				pv["required"] = true
			}
			queryParams = append(queryParams, pv)
		}
	}
	if len(pathParams) > 0 {
		v["path_params"] = pathParams
	}
	if len(queryParams) > 0 {
		v["query_params"] = queryParams
	}
	if o.HasBody() {
		if mt := o.op.RequestBody.Value.Content.Get("application/json"); mt != nil && mt.Schema != nil {
			v["request_body"] = schemaView(mt.Schema, depth, 0)
		} else {
			v["request_body"] = "not JSON: not available through platform_call"
		}
	}
	if o.Risk != PlatformRead && o.Risk != PlatformDedicated {
		needs := []string{"idempotency_key"}
		if o.GetTwin != "" && o.Method != http.MethodPost {
			needs = append(needs, "if_match")
			v["read_first"] = o.GetTwin
		}
		v["needs"] = needs
	}
	if o.Risk.Gated() {
		v["approval"] = "the first call asks a human; repeat it with the same idempotency_key once they approve"
	}
	return v
}

// schemaView renders a schema as plain JSON Schema with its $refs resolved to depth
// levels; a deeper $ref stays {"$ref": "<name>"}.
func schemaView(ref *openapi3.SchemaRef, depth, level int) any {
	if ref == nil {
		return nil
	}
	if ref.Ref != "" {
		if level >= depth || ref.Value == nil {
			return map[string]any{"$ref": strings.TrimPrefix(ref.Ref, "#/components/schemas/")}
		}
		level++
	}
	s := ref.Value
	if s == nil {
		return map[string]any{}
	}
	v := map[string]any{}
	if s.Type != nil && len(*s.Type) > 0 {
		if len(*s.Type) == 1 {
			v["type"] = (*s.Type)[0]
		} else {
			v["type"] = []string(*s.Type)
		}
	}
	if s.Description != "" {
		v["description"] = s.Description
	}
	if s.Format != "" {
		v["format"] = s.Format
	}
	if len(s.Enum) > 0 {
		v["enum"] = s.Enum
	}
	if s.Default != nil {
		v["default"] = s.Default
	}
	if s.Nullable {
		v["nullable"] = true
	}
	if s.Min != nil {
		v["minimum"] = *s.Min
	}
	if s.Max != nil {
		v["maximum"] = *s.Max
	}
	if s.MinLength > 0 {
		v["minLength"] = s.MinLength
	}
	if s.MaxLength != nil {
		v["maxLength"] = *s.MaxLength
	}
	if len(s.Required) > 0 {
		v["required"] = s.Required
	}
	if len(s.Properties) > 0 {
		props := map[string]any{}
		for name, p := range s.Properties {
			props[name] = schemaView(p, depth, level)
		}
		v["properties"] = props
	}
	if s.Items != nil {
		v["items"] = schemaView(s.Items, depth, level)
	}
	if ap := s.AdditionalProperties; ap.Schema != nil {
		v["additionalProperties"] = schemaView(ap.Schema, depth, level)
	} else if ap.Has != nil {
		v["additionalProperties"] = *ap.Has
	}
	for name, list := range map[string]openapi3.SchemaRefs{"allOf": s.AllOf, "oneOf": s.OneOf, "anyOf": s.AnyOf} {
		if len(list) == 0 {
			continue
		}
		out := make([]any, 0, len(list))
		for _, m := range list {
			out = append(out, schemaView(m, depth, level))
		}
		v[name] = out
	}
	return v
}

// PlatformCallDeps are what platform_call needs: the contract is the embedded one.
type PlatformCallDeps struct {
	Pool       *pgxpool.Pool
	Dispatcher PlatformDispatcher
}

type platformCallInput struct {
	OperationID    string                     `json:"operation_id"`
	PathParams     map[string]json.RawMessage `json:"path_params"`
	Query          map[string]json.RawMessage `json:"query"`
	Body           json.RawMessage            `json:"body"`
	IdempotencyKey string                     `json:"idempotency_key"`
	IfMatch        string                     `json:"if_match"`
}

// NewPlatformCall builds platform_call (rules A3, A4, A8, A9).
func NewPlatformCall(d PlatformCallDeps) *Tool {
	return &Tool{
		Name: PlatformCallName,
		Description: "Run one platform API operation, the way the web UI does, as yourself on behalf of the person. " +
			"Find it with platform_operations first. Give operation_id, path_params, query and body as the operation " +
			"describes them. Everything but a read needs idempotency_key: a new one for each new action, the same one " +
			"when you repeat that action. To update or delete something, read it first and pass the etag of that read " +
			"as if_match. A destructive or privileged action answers pending_approval with a code: give the person the " +
			"code, and once they approve, repeat the same call with the same idempotency_key. Never put a token or " +
			"other secret in a body. Returns the HTTP status, the response body (an error body too) and its etag.",
		InputSchema: platformCallSchema,
		Risk:        RiskComputational,
		Hits:        HitsLocalService,
		Timeout:     60 * time.Second,
		// Never repeated by the platform: the agent repeats a call, with its key.
		MaxAttempts: 1,
		Chat:        true,
		Exec: func(ctx context.Context, call Call) (Result, error) {
			return execPlatformCall(ctx, d, call)
		},
	}
}

func execPlatformCall(ctx context.Context, d PlatformCallDeps, call Call) (Result, error) {
	var in platformCallInput
	if err := json.Unmarshal(call.Input, &in); err != nil {
		return Result{}, &ModelError{Message: "invalid input: " + err.Error()}
	}
	c, err := Contract()
	if err != nil {
		return Result{}, err
	}
	op, ok := c.Operation(strings.TrimSpace(in.OperationID))
	if !ok {
		return Result{}, &ModelError{Message: "unknown operation"}
	}
	target, err := platformTarget(op, in.PathParams, in.Query)
	if err != nil {
		return Result{}, err
	}
	var body []byte
	if raw := bytes.TrimSpace(in.Body); len(raw) > 0 && string(raw) != "null" {
		if !op.HasBody() {
			return Result{}, &ModelError{Message: "body: " + op.ID + " takes no body"}
		}
		if body, err = canonicalBody(raw); err != nil {
			return Result{}, &ModelError{Message: "body: not valid JSON"}
		}
	}
	if d.Dispatcher == nil || d.Pool == nil {
		return Result{}, errors.New("platform_call: not configured")
	}
	who, err := callActor(ctx, d.Pool, call)
	if err != nil {
		return Result{}, err
	}
	resp, err := d.Dispatcher.Dispatch(ctx, PlatformRequest{Actor: who, Method: op.Method, Target: target, Body: body,
		IdempotencyKey: strings.TrimSpace(in.IdempotencyKey), IfMatch: strings.TrimSpace(in.IfMatch)})
	if err != nil {
		return Result{}, err
	}
	return platformResult(resp), nil
}

// platformTarget is the request's escaped path and query: every {name} of the path from
// path_params (each value one escaped segment, which a {name...} route unescapes again)
// and the query from query.
func platformTarget(op *PlatformOperation, pathParams, query map[string]json.RawMessage) (string, error) {
	want := op.PathParams()
	for name := range pathParams {
		if !slices.Contains(want, name) {
			return "", &ModelError{Message: "path_params: " + name + " is not a parameter of " + op.ID}
		}
	}
	path := op.Path
	for _, name := range want {
		v, ok := scalarText(pathParams[name])
		if !ok || strings.TrimSpace(v) == "" {
			return "", &ModelError{Message: "path_params: " + name + " is required"}
		}
		path = strings.Replace(path, "{"+name+"}", url.PathEscape(v), 1)
	}
	if len(query) == 0 {
		return path, nil
	}
	q := url.Values{}
	names := make([]string, 0, len(query))
	for name := range query {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		raw := query[name]
		var list []json.RawMessage
		if json.Unmarshal(raw, &list) != nil {
			list = []json.RawMessage{raw}
		}
		for _, item := range list {
			v, ok := scalarText(item)
			if !ok {
				return "", &ModelError{Message: "query: " + name + " must be a string, number or boolean"}
			}
			q.Add(name, v)
		}
	}
	return path + "?" + q.Encode(), nil
}

// scalarText is a JSON string, number or boolean as text.
func scalarText(raw json.RawMessage) (string, bool) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return "", false
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s, true
	}
	var n json.Number
	if json.Unmarshal(raw, &n) == nil {
		return n.String(), true
	}
	var b bool
	if json.Unmarshal(raw, &b) == nil {
		return fmt.Sprint(b), true
	}
	return "", false
}

// canonicalBody is the body as the request sends it: re-encoded with sorted keys, so a
// repeat of the same body hashes the same (rule A6), and without the redaction
// placeholders a read leaves under secret keys, which only mean "unchanged" (the
// placeholder is never a value to store).
func canonicalBody(raw json.RawMessage) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil || dec.More() {
		return nil, errors.New("invalid JSON")
	}
	return json.Marshal(dropRedacted(v))
}

func dropRedacted(v any) any {
	switch x := v.(type) {
	case map[string]any:
		for k, val := range x {
			if s, ok := val.(string); ok && s == obs.Redacted && obs.IsSecretKey(k) {
				delete(x, k)
				continue
			}
			x[k] = dropRedacted(val)
		}
	case []any:
		for i := range x {
			x[i] = dropRedacted(x[i])
		}
	}
	return v
}

// callActor is the actor of a platform_call (rule A4): the calling agent, via the
// Chief's tools, on behalf of the person of the call's conversation ("" without one).
func callActor(ctx context.Context, pool *pgxpool.Pool, call Call) (actor.Actor, error) {
	who := actor.Actor{Kind: actor.Agent, ID: call.AgentID, Via: actor.ViaChief}
	a, err := store.GetAgentForRun(ctx, pool, call.AgentID)
	switch {
	case err == nil:
		who.Name = a.Name
	case !errors.Is(err, store.ErrNotFound):
		return who, err
	}
	if call.ConversationID == "" {
		return who, nil
	}
	conv, err := store.GetConversation(ctx, pool, call.ConversationID)
	switch {
	case err == nil:
		who.OnBehalfOf = conv.Channel + ":" + conv.ExternalChatID
	case !errors.Is(err, store.ErrNotFound):
		return who, err
	}
	return who, nil
}

// platformResult shapes the api's answer for the model (rules A8 and A9): the gate's
// refusal as a refused call, its pending answer as is, and anything else, an error body
// too, as data: {status, request_id, etag?, idempotent_replayed?, body}, the body redacted
// and the whole within 32 KiB; a binary body is {content_type, bytes, sha256}.
func platformResult(resp *PlatformResponse) Result {
	switch resp.Header.Get(HeaderPlatformGate) {
	case GateDenied:
		var e struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.Unmarshal(resp.Body, &e)
		msg := e.Error.Message
		if msg == "" {
			msg = "refused"
		}
		return Result{IsError: true, Blocked: true, Text: msg}
	case GatePending:
		return Result{Output: obs.RedactJSON(resp.Body)}
	}
	out := map[string]any{"status": resp.Status, "request_id": resp.Header.Get(HeaderRequestID)}
	if etag := resp.Header.Get(HeaderETag); etag != "" {
		out["etag"] = etag
	}
	if strings.EqualFold(resp.Header.Get(HeaderIdempotencyReplayed), "true") {
		out["idempotent_replayed"] = true
	}
	ctype := resp.Header.Get("Content-Type")
	mt, _, _ := mime.ParseMediaType(ctype)
	full := resp.Size <= int64(len(resp.Body))
	switch {
	case resp.Size == 0:
		out["body"] = nil
	case !textual(mt):
		out["body"] = map[string]any{"content_type": ctype, "bytes": resp.Size, "sha256": resp.SHA256}
	case full && (mt == "application/json" || strings.HasSuffix(mt, "+json")):
		out["body"] = json.RawMessage(obs.RedactJSON(resp.Body))
	default:
		// Text, or JSON longer than the dispatcher keeps, which reads as text.
		out["body"] = obs.Redact(strings.ToValidUTF8(string(resp.Body), ""))
	}
	b, err := json.Marshal(out)
	if err != nil {
		return Result{IsError: true, Text: "could not encode the result"}
	}
	if len(b) <= PlatformResultMax {
		return Result{Output: b}
	}
	// Too long: the body as text, trimmed with the truncation marker until the whole fits.
	text, ok := out["body"].(string)
	if !ok {
		text = string(obs.RedactJSON(resp.Body))
	}
	budget := PlatformResultMax
	for {
		out["body"] = obs.Truncate(text, budget)
		b, _ = json.Marshal(out)
		if len(b) <= PlatformResultMax || budget < 1024 {
			return Result{Output: b}
		}
		budget -= len(b) - PlatformResultMax + 16
	}
}

// textual reports whether a media type is text the model can read.
func textual(mt string) bool {
	return strings.HasPrefix(mt, "text/") || mt == "application/json" || strings.HasSuffix(mt, "+json") ||
		mt == "application/yaml" || mt == "application/x-yaml" || mt == "application/xml" || strings.HasSuffix(mt, "+xml")
}

// BodySHA256 is the hex SHA-256 of a request body, a platform approval's body_sha256.
func BodySHA256(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

// SecretInBody reports whether a request body carries a secret (rule A8): a non-empty
// string or a number under a secret key (obs.IsSecretKey), or a credential-shaped string
// anywhere. null, "" and booleans under a secret key are no secret, and an object under one
// is walked like any other (a JSON schema may name a property token).
func SecretInBody(body []byte) bool {
	if len(bytes.TrimSpace(body)) == 0 {
		return false
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var v any
	if dec.Decode(&v) != nil {
		return credentialShaped(string(body))
	}
	return secretIn(v, false)
}

func secretIn(v any, underSecretKey bool) bool {
	switch x := v.(type) {
	case map[string]any:
		for k, val := range x {
			if secretIn(val, obs.IsSecretKey(k)) {
				return true
			}
		}
	case []any:
		for _, val := range x {
			if secretIn(val, underSecretKey) {
				return true
			}
		}
	case string:
		return underSecretKey && strings.TrimSpace(x) != "" || credentialShaped(x)
	case json.Number:
		return underSecretKey
	}
	return false
}

func credentialShaped(s string) bool {
	s = strings.ReplaceAll(s, "\x00", "")
	return obs.Redact(s) != s
}
