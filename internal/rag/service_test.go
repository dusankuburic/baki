package rag

import (
	"context"
	"strings"
	"testing"
	"unicode/utf8"
)

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

func TestChunkText_ExactSize(t *testing.T) {
	in := strings.Repeat("x", 10)
	chunks := chunkText(in, 10)
	if len(chunks) != 1 || chunks[0] != in {
		t.Errorf("exact size: want 1 chunk, got %d (%q)", len(chunks), chunks)
	}
}

func TestChunkText_RespectsParagraphBoundaries(t *testing.T) {
	in := "First paragraph.\n\nSecond paragraph.\n\nThird paragraph."
	chunks := chunkText(in, 1000)
	if len(chunks) != 1 {
		t.Errorf("all paragraphs fit: want 1 chunk, got %d", len(chunks))
	}
}

func TestChunkText_SplitsLongParagraphsAtSentences(t *testing.T) {
	sentences := make([]string, 20)
	for i := range sentences {
		sentences[i] = "This is sentence number " + string(rune('0'+i)) + "."
	}
	in := strings.Join(sentences, " ")
	chunks := chunkText(in, 100)
	if len(chunks) < 2 {
		t.Errorf("long input should split into multiple chunks, got %d", len(chunks))
	}
}

func TestChunkText_SplitsByRunesNotBytes(t *testing.T) {
	in := strings.Repeat("☃", 25)
	size := 10
	chunks := chunkText(in, size)
	if len(chunks) < 2 {
		t.Errorf("want multiple chunks for 25 snowmen with size 10, got %d", len(chunks))
	}
	// Overlap (15% of size) + space delimiter can push a chunk slightly past
	// the target size. Allow size + overlap + 1 (delimiter).
	maxAllowed := size + size*15/100 + 1
	for i, c := range chunks {
		if n := len([]rune(c)); n > maxAllowed {
			t.Errorf("chunk %d exceeds max allowed (%d): %d", i, maxAllowed, n)
		}
	}
}

func TestChunkText_PacksMultiByteParagraphsByRunes(t *testing.T) {
	para := strings.Repeat("ж", 20)
	in := para + "\n\n" + para + "\n\n" + strings.Repeat("ж", 60)
	chunks := chunkText(in, 50)
	if len(chunks) == 0 {
		t.Fatal("expected chunks")
	}
	// Two 20-rune paragraphs fit a 50-rune chunk together (20+2+20); counting
	// their 40-byte UTF-8 length instead would wrongly split them.
	if first := chunks[0]; first != para+"\n\n"+para {
		t.Errorf("want first chunk to pack both short paragraphs, got %q", first)
	}
	for i, c := range chunks {
		if n := utf8.RuneCountInString(c); n > 50 {
			t.Errorf("chunk %d exceeds size in runes: %d", i, n)
		}
	}
}

func TestChunkText_NoContentLoss(t *testing.T) {
	in := "First paragraph with some content.\n\nSecond paragraph with more content.\n\nThird one here."
	chunks := chunkText(in, 50)
	allContent := strings.Join(chunks, " ")
	for _, word := range []string{"First", "paragraph", "Second", "Third"} {
		if !strings.Contains(allContent, word) {
			t.Errorf("content lost: %q not found in chunks", word)
		}
	}
}

func TestAddDocument_EmptyContent(t *testing.T) {
	svc := NewKnowledgeService(nil, nil, nil)
	err := svc.AddDocument(context.Background(), "scope", "org", "f.txt", "   \n\t ")
	if err == nil || !strings.Contains(err.Error(), "empty") {
		t.Errorf("want 'document is empty' error, got %v", err)
	}
}

func TestAddDocument_NoProvider(t *testing.T) {
	svc := NewKnowledgeService(nil, nil, nil)
	err := svc.AddDocument(context.Background(), "scope", "org", "f.txt", "real content")
	if err == nil || !strings.Contains(err.Error(), "embedding provider") {
		t.Errorf("want embedding-provider error, got %v", err)
	}
}

func TestSearch_NoProvider(t *testing.T) {
	svc := NewKnowledgeService(nil, nil, nil)
	out, err := svc.Search(context.Background(), "scope", "org", "query")
	if err == nil {
		t.Errorf("want error when no provider configured, got nil (out=%q)", out)
	}
}
