package ai

import (
	"bytes"
	"testing"
)

// FuzzParseResponsesSSE fuzzes the Azure Responses / Foundry agent event parser under the shared SSE contract
// (checkSSE in stream_fuzz_test.go), seeded from the same inline fixtures — the Responses tests' bodies and their
// bare JSON events framed into streams.
func FuzzParseResponsesSSE(f *testing.F) {
	for _, s := range sseSeeds(f) {
		f.Add(s)
	}
	parse := readerParse(func(r *bytes.Reader, on func(Chunk)) error { return parseResponsesSSE(r, on) })
	f.Fuzz(func(t *testing.T, data []byte) {
		checkSSE(t, parse, data, sseContract{})
	})
}
