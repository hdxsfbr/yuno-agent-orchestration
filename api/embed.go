// Package api holds the machine-readable HTTP contract, embedded so the running
// server publishes exactly the document the tests validate against.
package api

import _ "embed"

//go:embed openapi.yaml
var OpenAPI []byte
