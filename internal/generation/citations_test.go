package generation

import (
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"
)

func citationLimits() CitationLimits {
	return CitationLimits{MaxSources: 32, MaxBlockBytes: 32 * 1024, MaxURLBytes: 2048, MaxTitleBytes: 256}
}
func citationsForTest(t *testing.T, limits CitationLimits) *CitationCollector {
	t.Helper()
	collector, err := NewCitationCollector(limits)
	if err != nil {
		t.Fatal(err)
	}
	return collector
}
func citationAnnotation(address, title string, start, end int64) map[string]any {
	return map[string]any{"type": "url_citation", "url": address, "title": title, "start_index": start, "end_index": end}
}
func citationMessage(annotations ...any) ModelItem {
	raw, _ := json.Marshal(map[string]any{"id": "m", "type": "message", "role": "assistant", "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": "Факт 🌍 с источниками. [Fake](https://unconfirmed.example)", "annotations": annotations}}})
	return ModelItem{ID: "m", Type: "message", Raw: raw}
}

func TestUnitCitationsOfficialBareHostDefaultsAndQuestionDeduplication(t *testing.T) {
	collector := citationsForTest(t, citationLimits())
	if collector.Block() != nil {
		t.Fatal("empty sources must not create a block")
	}
	// Official AI Studio websearch documentation supplies only the URL as
	// meaningful data; title and offsets have compatibility defaults.
	if err := collector.Add([]ModelItem{citationMessage(citationAnnotation("www.rbc.ru", "", 0, 0), citationAnnotation("ria.ru", "", 0, 0))}); err != nil {
		t.Fatal(err)
	}
	if err := collector.Add([]ModelItem{citationMessage(citationAnnotation("https://WWW.RBC.RU", "later title must not replace first", 0, 0), map[string]any{"type": "url_citation", "url": "https://example.org/report?q=1#section", "title": "Report"})}); err != nil {
		t.Fatal(err)
	}
	block := collector.Block()
	want := "Источники:\n- www.rbc.ru — https://www.rbc.ru\n- ria.ru — https://ria.ru\n- Report — https://example.org/report?q=1#section"
	if block == nil || block.Type != "text" || block.Version != 1 || block.Text != want || len(block.Body) != 0 {
		t.Fatalf("block %+v want %q", block, want)
	}
	if strings.Contains(block.Text, "unconfirmed.example") {
		t.Fatal("invented model text was treated as a provider citation")
	}
}

func TestUnitCitationsOnlyCompletedAssistantOutputTextAnnotations(t *testing.T) {
	collector := citationsForTest(t, citationLimits())
	function := ModelItem{ID: "f", Type: "function_call", Raw: json.RawMessage(`{"type":"function_call","annotations":[{"type":"url_citation","url":"javascript:private"}]}`)}
	search := ModelItem{ID: "s", Type: "web_search_call", Raw: json.RawMessage(`{"type":"web_search_call","action":{"url":"https://not-cited.example"}}`)}
	if err := collector.Add([]ModelItem{function, search, citationMessage(map[string]any{"type": "file_citation", "file_id": "ignored"}, map[string]any{"type": "future_annotation", "url": "javascript:ignored"})}); err != nil {
		t.Fatal(err)
	}
	if collector.Block() != nil {
		t.Fatal("non-URL annotations became sources")
	}
	for _, raw := range []string{
		`{"id":"m","type":"message","role":"user","status":"completed","content":[]}`,
		`{"id":"m","type":"message","role":"assistant","status":"in_progress","content":[]}`,
		`{"id":"foreign","type":"message","role":"assistant","status":"completed","content":[]}`,
		`{"id":"m","type":"function_call","role":"assistant","status":"completed","content":[]}`,
		`{"id":"m","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","annotations":[null]}]}`,
		`{"id":"m","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","annotations":[{}]}]}`,
		`{"id":"m","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","annotations":[{"type":42}]}]}`,
		`{"id":"m","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","annotations":[{"type":"url_citation","url":"https://example.org","url":"https://different.example"}]}]}`,
	} {
		if err := collector.Add([]ModelItem{{ID: "m", Type: "message", Raw: json.RawMessage(raw)}}); !errors.Is(err, errModelProtocol) {
			t.Fatalf("malformed message accepted: %s error %v", raw, err)
		}
	}
	other := ModelItem{ID: "m", Type: "message", Raw: json.RawMessage(`{"id":"m","type":"message","role":"assistant","status":"completed","content":[{"type":"future_content","annotations":[{"type":"url_citation","url":"https://not-output-text.example"}]}]}`)}
	if err := collector.Add([]ModelItem{other}); err != nil || collector.Block() != nil {
		t.Fatalf("non-output-text content was cited: %v", err)
	}
}

func TestUnitCitationsUnsafeAndMalformedAnnotationsFailWithoutLeaking(t *testing.T) {
	for _, address := range []string{"javascript:alert(1)", "data:text/plain,private", "ftp://example.org", "file:///tmp/private", "//example.org", "/relative", "example.org@trusted.example", "https://user:PRIVATE@example.org", "https://@example.org", "https://example.org/\nPRIVATE", "example.org/white space", "https://example.org/%0APRIVATE", "https://exa\tmple.org", "https://", "https:///path", "https:example.org", "https://[broken", "https://-bad.example", "bad_.example", "https://example.org:65536", "https://example.org:0", "https://example.org/<script>", "https://example.org/\"PRIVATE", "https://example.org/\\PRIVATE", "https://example.org/\u202ePRIVATE"} {
		t.Run(address, func(t *testing.T) {
			collector := citationsForTest(t, citationLimits())
			err := collector.Add([]ModelItem{citationMessage(citationAnnotation(address, "PRIVATE source", 0, 0))})
			if !errors.Is(err, errModelProtocol) || strings.Contains(err.Error(), "PRIVATE") || collector.Block() != nil {
				t.Fatalf("unsafe annotation error=%v block=%+v", err, collector.Block())
			}
		})
	}
	for name, annotation := range map[string]map[string]any{
		"missing URL": {"type": "url_citation"}, "URL number": {"type": "url_citation", "url": 1}, "URL null": {"type": "url_citation", "url": nil}, "title number": {"type": "url_citation", "url": "https://example.org", "title": 1}, "title null": {"type": "url_citation", "url": "https://example.org", "title": nil}, "start fraction": {"type": "url_citation", "url": "https://example.org", "start_index": 1.5}, "start text": {"type": "url_citation", "url": "https://example.org", "start_index": "1"}, "start null": {"type": "url_citation", "url": "https://example.org", "start_index": nil}, "negative start": citationAnnotation("https://example.org", "title", -1, 0), "negative end": citationAnnotation("https://example.org", "title", 0, -1), "reverse range": citationAnnotation("https://example.org", "title", 2, 1), "only start with missing end": {"type": "url_citation", "url": "https://example.org", "start_index": 1},
	} {
		t.Run(name, func(t *testing.T) {
			collector := citationsForTest(t, citationLimits())
			if err := collector.Add([]ModelItem{citationMessage(annotation)}); !errors.Is(err, errModelProtocol) {
				t.Fatal(err)
			}
		})
	}
}

func TestUnitCitationOffsetsAreUnanchoredMetadataAndTitleIsPlainOneLine(t *testing.T) {
	collector := citationsForTest(t, citationLimits())
	for _, annotation := range []map[string]any{
		citationAnnotation("HTTPS://EXAMPLE.ORG/one", "  <b>**Source**</b>\n[link](x)\t`code`\u202e\x00", 0, 0),
		citationAnnotation("https://example.org/two", "Кириллица 🌍", 1, math.MaxInt64),
		{"type": "url_citation", "url": "https://example.org/three", "end_index": 2},
	} {
		if err := collector.Add([]ModelItem{citationMessage(annotation)}); err != nil {
			t.Fatal(err)
		}
	}
	block := collector.Block()
	if !strings.Contains(block.Text, "b Source /b link x code — https://example.org/one") || !strings.Contains(block.Text, "Кириллица 🌍") || strings.ContainsAny(block.Text, "<>[]*`\t\u202e\x00") {
		t.Fatalf("unsafe title formatting %q", block.Text)
	}
	if strings.Count(block.Text, "\n") != 3 {
		t.Fatalf("title injected extra lines: %q", block.Text)
	}
}

func TestUnitCitationsLimitsAreExactAndFailedAddIsAtomic(t *testing.T) {
	for _, limits := range []CitationLimits{{}, {MaxSources: 1, MaxBlockBytes: 1, MaxURLBytes: 1}, {MaxSources: -1, MaxBlockBytes: 1, MaxURLBytes: 1, MaxTitleBytes: 1}} {
		if _, err := NewCitationCollector(limits); err == nil {
			t.Fatal("invalid limits accepted")
		}
	}
	collector := citationsForTest(t, citationLimits())
	first := citationMessage(citationAnnotation("https://example.org/one", "First", 0, 0))
	if err := collector.Add([]ModelItem{first}); err != nil {
		t.Fatal(err)
	}
	before := collector.Block().Text
	if err := collector.Add([]ModelItem{citationMessage(citationAnnotation("https://example.org/two", "Second", 0, 0), citationAnnotation("javascript:PRIVATE", "Third", 0, 0))}); err == nil || collector.Block().Text != before {
		t.Fatal("failed batch partially changed sources")
	}
	full := citationsForTest(t, citationLimits())
	if err := full.Add([]ModelItem{first}); err != nil {
		t.Fatal(err)
	}
	size := len(full.Block().Text)
	for _, delta := range []int{-1, 0} {
		limits := citationLimits()
		limits.MaxBlockBytes = size + delta
		c := citationsForTest(t, limits)
		err := c.Add([]ModelItem{first})
		if (err == nil) != (delta == 0) {
			t.Fatalf("exact block cap delta=%d error=%v", delta, err)
		}
	}
	limits := citationLimits()
	limits.MaxSources = 1
	c := citationsForTest(t, limits)
	if err := c.Add([]ModelItem{first, first}); err != nil {
		t.Fatal("duplicate used a source slot", err)
	}
	if err := c.Add([]ModelItem{citationMessage(citationAnnotation("https://example.org/two", "Second", 0, 0))}); err == nil || c.Block().Text != before {
		t.Fatal("source count cap or rollback failed")
	}
	for _, test := range []struct {
		name  string
		limit int
		value string
		ok    bool
	}{
		{"exact title", 256, strings.Repeat("я", 128), true}, {"large title", 256, strings.Repeat("я", 129), false},
	} {
		t.Run(test.name, func(t *testing.T) {
			limits := citationLimits()
			limits.MaxTitleBytes = test.limit
			c := citationsForTest(t, limits)
			err := c.Add([]ModelItem{citationMessage(citationAnnotation("https://example.org", test.value, 0, 0))})
			if (err == nil) != test.ok {
				t.Fatal(err)
			}
		})
	}
	for _, size := range []int{len("https://example.org"), len("https://example.org") - 1} {
		limits := citationLimits()
		limits.MaxURLBytes = size
		c := citationsForTest(t, limits)
		err := c.Add([]ModelItem{citationMessage(citationAnnotation("https://example.org", "Title", 0, 0))})
		if (err == nil) != (size == len("https://example.org")) {
			t.Fatalf("URL cap %d err %v", size, err)
		}
	}
	limits = citationLimits()
	limits.MaxURLBytes = len("example.org")
	c = citationsForTest(t, limits)
	if err := c.Add([]ModelItem{citationMessage(citationAnnotation("example.org", "", 0, 0))}); err == nil {
		t.Fatal("normalization exceeded URL bound")
	}
	limits = citationLimits()
	limits.MaxTitleBytes = 1
	c = citationsForTest(t, limits)
	if err := c.Add([]ModelItem{citationMessage(citationAnnotation("example.org", "", 0, 0))}); err == nil {
		t.Fatal("fallback hostname exceeded title bound")
	}
}

func TestUnitCitationsSafeURLNormalizationAndEmptyTitleFallback(t *testing.T) {
	for raw, want := range map[string]string{"example.org/report?q=a%20b#part": "https://example.org/report?q=a%20b#part", "http://example.org:8080/path": "http://example.org:8080/path", "https://[::1]/path": "https://[::1]/path", "https://127.0.0.1/path": "https://127.0.0.1/path", "HTTPS://EXAMPLE.ORG/Path": "https://example.org/Path", "https://xn--e1afmkfd.xn--p1ai/path": "https://xn--e1afmkfd.xn--p1ai/path"} {
		t.Run(raw, func(t *testing.T) {
			c := citationsForTest(t, citationLimits())
			if err := c.Add([]ModelItem{citationMessage(citationAnnotation(raw, "\n\t***\u202e", 0, 0))}); err != nil {
				t.Fatal(err)
			}
			if !strings.HasSuffix(c.Block().Text, " — "+want) {
				t.Fatalf("normalized URL %q want %q", c.Block().Text, want)
			}
		})
	}
}

func TestUnitCitationsCanonicalAnnotationFieldsDoNotAcceptJSONCaseAliases(t *testing.T) {
	c := citationsForTest(t, citationLimits())
	if err := c.Add([]ModelItem{citationMessage(map[string]any{"type": "url_citation", "URL": "https://not-canonical.example"})}); err == nil {
		t.Fatal("case alias became a confirmed provider URL")
	}
	if err := c.Add([]ModelItem{citationMessage(map[string]any{"type": "url_citation", "url": "https://provider.example", "URL": "https://not-canonical.example", "TITLE": "not-canonical title"})}); err != nil {
		t.Fatal(err)
	}
	if c.Block().Text != "Источники:\n- provider.example — https://provider.example" {
		t.Fatalf("case alias changed confirmed source %q", c.Block().Text)
	}
}
