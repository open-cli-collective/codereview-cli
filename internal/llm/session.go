package llm

import "errors"

// ErrMissingProviderSession marks a failure to resume or fork a provider
// conversation that no longer exists. Concrete adapters wrap their CLI errors
// with this sentinel so the retry loop can tell "the conversation is gone" apart
// from a genuine provider failure, mirroring the ErrTransient taxonomy.
//
// A missing conversation is always recoverable by starting a new one: session
// reuse saves prompt cost and preserves prior context, but it is not part of a
// review's correctness. Failing the reviewer instead turns a dangling session ID
// into a round that inspected nothing, which reads as a clean pass.
var ErrMissingProviderSession = errors.New("llm: provider session no longer exists")
