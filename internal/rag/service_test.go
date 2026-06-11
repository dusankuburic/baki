package rag

import (
	"context"
	"strings"
	"testing"
)

// TestChunkText_ReassemblyInvariant is the most important property: chunking
// must never lose or reorder content — concatenating the chunks must reproduce
// the original text exactly, for inputs of every length relative to size.
func TestChunkText_ReassemblyInvariant(t *testing.T) {
	cases := []string{
		"",
		"a",
		"exactly-ten",                    // arbitrary
		strings.Repeat("x", 10),          // exact multiple of size=10
		strings.Repeat("y", 25),          // non-multiple
		"héllo wörld — unicode ☃ test 🚀", // multi-byte runes
	}
	const size = 10
	for _, in := range cases {
		chunks := chunkText(in, size)
		if got := strings.Join(chunks, ""); got != in {
			t.Errorf("reassembly mismatch:\n in=%q\n got=%q", in, got)
		}
	}
}

func TestChunkText_Empty(t *testing.T) {
	if chunks := chunkText("", 10); len(chunks) != 0 {
		t.Errorf("empty input: want 0 chunks, got %d (%q)", len(chunks), chunks)
	}
}

func TestChunkText_ShorterThanSize(t *testing.T) {
	chunks := chunkText("hi", 10)
	if len(chunks) != 1 || chunks[0] != "hi" {
		t.Errorf("short input: want [\"hi\"], got %q", chunks)
	}
}

func TestChunkText_ExactMultiple(t *testing.T) {
	chunks := chunkText(strings.Repeat("x", 30), 10)
	if len(chunks) != 3 {
		t.Fatalf("exact multiple: want 3 chunks, got %d", len(chunks))
	}
	for i, c := range chunks {
		if len([]rune(c)) != 10 {
			t.Errorf("chunk %d: want 10 runes, got %d", i, len([]rune(c)))
		}
	}
}

func TestChunkText_NonMultipleRemainder(t *testing.T) {
	chunks := chunkText(strings.Repeat("x", 25), 10)
	if len(chunks) != 3 {
		t.Fatalf("non-multiple: want 3 chunks, got %d", len(chunks))
	}
	if got := len([]rune(chunks[2])); got != 5 {
		t.Errorf("final chunk: want 5-rune remainder, got %d", got)
	}
}

// TestChunkText_SplitsByRunesNotBytes guards against corrupting multi-byte
// characters: every chunk must be at most `size` runes and remain valid text
// (no rune is split across a chunk boundary).
func TestChunkText_SplitsByRunesNotBytes(t *testing.T) {
	in := strings.Repeat("☃", 25) // each snowman is 3 bytes, 1 rune
	chunks := chunkText(in, 10)
	if len(chunks) != 3 {
		t.Fatalf("want 3 chunks, got %d", len(chunks))
	}
	for i, c := range chunks {
		if n := len([]rune(c)); n > 10 {
			t.Errorf("chunk %d exceeds size in runes: %d", i, n)
		}
		if !strings.HasPrefix(strings.Repeat("☃", 10), c) && i < 2 {
			t.Errorf("chunk %d corrupted a multi-byte rune: %q", i, c)
		}
	}
}

// TestAddDocument_EmptyContent verifies the cheap up-front guard rejects empty
// documents before any (network-bound) embedding work is attempted.
func TestAddDocument_EmptyContent(t *testing.T) {
	svc := NewKnowledgeService(nil, nil, nil)
	err := svc.AddDocument(context.Background(), "scope", "org", "f.txt", "   \n\t ")
	if err == nil || !strings.Contains(err.Error(), "empty") {
		t.Errorf("want 'document is empty' error, got %v", err)
	}
}

// TestAddDocument_NoProvider verifies a missing embedding provider surfaces a
// clean error (not a panic) when content is otherwise valid.
func TestAddDocument_NoProvider(t *testing.T) {
	svc := NewKnowledgeService(nil, nil, nil)
	err := svc.AddDocument(context.Background(), "scope", "org", "f.txt", "real content")
	if err == nil || !strings.Contains(err.Error(), "embedding provider") {
		t.Errorf("want embedding-provider error, got %v", err)
	}
}

// TestSearch_NoProvider verifies Search fails gracefully (error, no panic) when
// no embedding provider is configured.
func TestSearch_NoProvider(t *testing.T) {
	svc := NewKnowledgeService(nil, nil, nil)
	out, err := svc.Search(context.Background(), "scope", "org", "query")
	if err == nil {
		t.Errorf("want error when no provider configured, got nil (out=%q)", out)
	}
}
