package status

import _ "embed"

// Schema is the JSON Schema (draft 2020-12) of status.json, Schema 1. It is
// the contract between the PowerShell producer, the Go producer and every
// reader; see KNOWN-DIFFERENCES.md for where the PowerShell code departs from
// the intent.
//
//go:embed status.schema.json
var Schema []byte
