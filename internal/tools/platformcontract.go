package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/getkin/kin-openapi/openapi3"

	"yuno/api"
)

// The platform contract (Chief platform control, wave 1, DESIGN 5.32 rule 1): every operation of
// api/openapi.yaml with its operationId (the Go handler's name) and its risk class
// (x-yuno-risk). platform_operations searches it, platform_call builds its requests from
// it, and the api's platform gate finds the operation of a request in it.

// PlatformRisk is an operation's x-yuno-risk.
type PlatformRisk string

const (
	// PlatformRead changes nothing: no gate.
	PlatformRead PlatformRisk = "read"
	// PlatformRoutine changes something an operator does every day: an agent needs an
	// idempotency key, and If-Match on an update or delete with a GET twin (rule A3).
	PlatformRoutine PlatformRisk = "routine"
	// PlatformDestructive and PlatformPrivileged run for an agent only with a human
	// approval of that exact call (rule A6).
	PlatformDestructive PlatformRisk = "destructive"
	PlatformPrivileged  PlatformRisk = "privileged"
	// PlatformDedicated is refused to agents, who use the purpose-built tool that carries
	// the person's evidence (x-yuno-tool).
	PlatformDedicated PlatformRisk = "dedicated"
	// PlatformMachine is never offered to agents.
	PlatformMachine PlatformRisk = "machine"
)

// PlatformRisks are the valid classes.
var PlatformRisks = []PlatformRisk{PlatformRead, PlatformRoutine, PlatformDestructive, PlatformPrivileged, PlatformDedicated, PlatformMachine}

// Gated reports whether an agent's call of this class waits for a human approval.
func (r PlatformRisk) Gated() bool { return r == PlatformDestructive || r == PlatformPrivileged }

// PlatformOperation is one operation of the contract.
type PlatformOperation struct {
	ID      string
	Method  string // upper case
	Path    string // the contract's path, e.g. /api/agents/{id}
	Summary string
	Tags    []string
	Risk    PlatformRisk
	// Tool is the purpose-built tool of a dedicated operation (x-yuno-tool).
	Tool string
	// GetTwin is the operationId of the GET at the same path, "" when there is none: an
	// update or delete with one needs If-Match from an agent (rule A3).
	GetTwin string

	op         *openapi3.Operation
	pathParams []string
	params     openapi3.Parameters
}

// PathParams are the names of the path's parameters, in order.
func (o *PlatformOperation) PathParams() []string { return o.pathParams }

// HasBody reports whether the operation takes a request body.
func (o *PlatformOperation) HasBody() bool {
	return o.op.RequestBody != nil && o.op.RequestBody.Value != nil
}

// PlatformContract is the indexed contract.
type PlatformContract struct {
	ops     []*PlatformOperation
	byID    map[string]*PlatformOperation
	byRoute map[string]*PlatformOperation // "GET /api/agents/{id}"
	tags    []string
}

var loadPlatformContract = sync.OnceValues(func() (*PlatformContract, error) {
	return LoadPlatformContract(api.OpenAPI)
})

// Contract is the api's embedded contract, loaded once.
func Contract() (*PlatformContract, error) { return loadPlatformContract() }

// LoadPlatformContract indexes an OpenAPI document. Every operation must have an
// operationId and a valid x-yuno-risk, and a dedicated one its x-yuno-tool.
func LoadPlatformContract(raw []byte) (*PlatformContract, error) {
	doc, err := openapi3.NewLoader().LoadFromData(raw)
	if err != nil {
		return nil, fmt.Errorf("load contract: %w", err)
	}
	if err := doc.Validate(context.Background()); err != nil {
		return nil, fmt.Errorf("contract: %w", err)
	}
	c := &PlatformContract{byID: map[string]*PlatformOperation{}, byRoute: map[string]*PlatformOperation{}}
	for _, t := range doc.Tags {
		c.tags = append(c.tags, t.Name)
	}
	items := doc.Paths.Map()
	paths := make([]string, 0, len(items))
	for p := range items {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, path := range paths {
		item := items[path]
		methods := make([]string, 0, 4)
		for m := range item.Operations() {
			methods = append(methods, m)
		}
		sort.Strings(methods)
		for _, m := range methods {
			op := item.Operations()[m]
			o := &PlatformOperation{ID: op.OperationID, Method: strings.ToUpper(m), Path: path, Summary: op.Summary,
				Tags: op.Tags, op: op, pathParams: pathParamNames(path)}
			o.Risk = PlatformRisk(extString(op.Extensions, "x-yuno-risk"))
			o.Tool = extString(op.Extensions, "x-yuno-tool")
			switch {
			case o.ID == "":
				return nil, fmt.Errorf("contract: %s %s has no operationId", o.Method, path)
			case !slices.Contains(PlatformRisks, o.Risk):
				return nil, fmt.Errorf("contract: %s has no valid x-yuno-risk (%q)", o.ID, o.Risk)
			case o.Risk == PlatformDedicated && o.Tool == "":
				return nil, fmt.Errorf("contract: %s is dedicated and names no x-yuno-tool", o.ID)
			case c.byID[o.ID] != nil:
				return nil, fmt.Errorf("contract: operationId %s is used twice", o.ID)
			}
			o.params = append(append(openapi3.Parameters{}, item.Parameters...), op.Parameters...)
			c.ops = append(c.ops, o)
			c.byID[o.ID] = o
			c.byRoute[o.Method+" "+path] = o
		}
	}
	for _, o := range c.ops {
		if get := c.byRoute["GET "+o.Path]; get != nil && o.Method != "GET" {
			o.GetTwin = get.ID
		}
	}
	return c, nil
}

// Operations are every operation, by path then method.
func (c *PlatformContract) Operations() []*PlatformOperation { return c.ops }

// Operation is the operation with this operationId.
func (c *PlatformContract) Operation(id string) (*PlatformOperation, bool) {
	o, ok := c.byID[id]
	return o, ok
}

// Route is the operation at the method and contract path.
func (c *PlatformContract) Route(method, path string) (*PlatformOperation, bool) {
	o, ok := c.byRoute[strings.ToUpper(method)+" "+path]
	return o, ok
}

// ForPattern is the operation of a ServeMux pattern ("PUT /api/skills/{id}/files/{path...}"):
// the pattern's method and its path with every {name...} written {name}. A pattern with
// no method names no operation.
func (c *PlatformContract) ForPattern(pattern string) (*PlatformOperation, bool) {
	method, path, ok := strings.Cut(pattern, " ")
	if !ok {
		return nil, false
	}
	return c.Route(method, strings.ReplaceAll(path, "...}", "}"))
}

func extString(ext map[string]any, key string) string {
	switch v := ext[key].(type) {
	case string:
		return v
	case json.RawMessage:
		var s string
		_ = json.Unmarshal(v, &s)
		return s
	}
	return ""
}

func pathParamNames(path string) []string {
	var out []string
	for _, seg := range strings.Split(path, "/") {
		if strings.HasPrefix(seg, "{") && strings.HasSuffix(seg, "}") {
			out = append(out, strings.TrimSuffix(strings.TrimPrefix(seg, "{"), "}"))
		}
	}
	return out
}
