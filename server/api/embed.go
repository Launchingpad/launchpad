// Package openapi embeds the published OpenAPI description, served at
// GET /api/v1/openapi.yaml. The web client (T6) is generated from it.
package openapi

import _ "embed"

// OpenAPI is the bytes of openapi.yaml.
//
//go:embed openapi.yaml
var OpenAPI []byte
