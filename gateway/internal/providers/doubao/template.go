// Package doubao ships the captured official-client /chat/completion request as
// a build-time protocol template.
//
// The template contains ONLY the wire schema the official desktop client sends:
// field names, enum values, block types and the client's own hardcoded default
// assistant bot id. It carries no cookies, tokens, session ids, phone numbers or
// any other account data, so it is safe to embed in the binary. Per-account
// values are injected at request time from the encrypted vault.
package doubao

import _ "embed"

// embeddedCompletionTemplate is the official /chat/completion body shape.
//
//go:embed templates/completion-body.json
var embeddedCompletionTemplate []byte
