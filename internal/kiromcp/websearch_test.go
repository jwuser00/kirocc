package kiromcp

import (
	"strings"
	"testing"
)

// capturedResultJSON is a trimmed real web_search response captured 2026-08-05.
// publishedDate arrives as epoch milliseconds or null, and entries carry extra
// fields (id, maxVerbatimWordLimit, publicDomain) that must not break parsing.
const capturedResultJSON = `{"results":[` +
	`{"title":"Go Release Dashboard","url":"https://dev.golang.org/release","snippet":"Go1.25.","publishedDate":null,"id":"1","domain":"golang.org","maxVerbatimWordLimit":30,"publicDomain":true},` +
	`{"title":"The Go Programming Language","url":"https://go.dev/doc/go1.26","snippet":"The latest Go release, version 1.26, arrives in February 2026.","publishedDate":1775520000000,"id":"2","domain":"go.dev","maxVerbatimWordLimit":30,"publicDomain":true}` +
	`],"totalResults":2,"query":"latest Go release","error":null}`

func TestParseSearchResults_RealCapturedShape(t *testing.T) {
	results, ok := ParseSearchResults(capturedResultJSON)
	if !ok {
		t.Fatal("ParseSearchResults failed on a captured live response")
	}
	if len(results) != 2 {
		t.Fatalf("results = %d, want 2", len(results))
	}
	if results[0].PublishedDate != "" {
		t.Errorf("null publishedDate = %q, want empty", results[0].PublishedDate)
	}
	// 1775520000000 ms = 2026-04-07 UTC.
	if results[1].PublishedDate != "2026-04-07" {
		t.Errorf("epoch publishedDate = %q, want 2026-04-07", results[1].PublishedDate)
	}
	if results[1].URL != "https://go.dev/doc/go1.26" {
		t.Errorf("url = %q", results[1].URL)
	}
}

func TestParseSearchResults_StringDateAccepted(t *testing.T) {
	results, ok := ParseSearchResults(`{"results":[{"title":"t","publishedDate":"2026-08-01"}]}`)
	if !ok || results[0].PublishedDate != "2026-08-01" {
		t.Errorf("results = %+v, ok = %v", results, ok)
	}
}

func TestParseSearchResults_NotTheExpectedShape(t *testing.T) {
	for _, text := range []string{"", "plain text failure", `{"error":"boom"}`, `[]`} {
		if _, ok := ParseSearchResults(text); ok {
			t.Errorf("ParseSearchResults(%q) ok = true, want false", text)
		}
	}
}

func TestMarshalSearchResults_RoundTripsWithContent(t *testing.T) {
	results, ok := ParseSearchResults(capturedResultJSON)
	if !ok {
		t.Fatal("parse")
	}
	results[1].Content = "fetched page text"
	out := MarshalSearchResults(results)
	if !strings.Contains(out, `"content":"fetched page text"`) {
		t.Errorf("marshaled output missing content: %s", out)
	}
	if !strings.Contains(out, `"publishedDate":"2026-04-07"`) {
		t.Errorf("marshaled output missing normalized date: %s", out)
	}
	if strings.Contains(out, "1775520000000") {
		t.Errorf("raw epoch leaked into model-facing JSON: %s", out)
	}
}

func TestEncodeDecodeResultContent(t *testing.T) {
	for _, text := range []string{"plain", "한국어 콘텐츠\nwith newlines"} {
		enc := EncodeResultContent(text)
		dec, ok := DecodeResultContent(enc)
		if !ok || dec != text {
			t.Errorf("round trip failed: %q -> %q (%v)", text, dec, ok)
		}
	}
	if _, ok := DecodeResultContent("%%%not-base64%%%"); ok {
		t.Error("garbage decoded as ok")
	}
	if _, ok := DecodeResultContent(""); ok {
		t.Error("empty decoded as ok")
	}
	// Random binary that happens to be valid base64 but not UTF-8 text.
	if dec, ok := DecodeResultContent("AAEC/w=="); ok {
		t.Errorf("non-UTF8 payload decoded as %q", dec)
	}
}
