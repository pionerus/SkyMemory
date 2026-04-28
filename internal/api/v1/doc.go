// Package v1 defines the wire contract between cmd/studio and cmd/server.
//
// All structs in this package are versioned. Once shipped, fields must not be
// renamed, retyped, or removed within v1 — only added (with omitempty). A
// breaking change requires a new package v2.
//
// JSON encoding uses lowercase_snake_case for field names to match the Postgres
// schema. Times are RFC 3339 UTC. Money is always cents (int) plus an explicit
// currency code.
package v1
