package generation

import (
	"bytes"
	"encoding/json"
	"net"
	"net/url"
	"strconv"
	"strings"
	"unicode"

	"github.com/fin-assist/assistant/internal/dialogue"
)

// CitationLimits bounds the question's source list independently of provider
// response limits. The owner supplies explicit positive technical quotas.
type CitationLimits struct{ MaxSources, MaxBlockBytes, MaxURLBytes, MaxTitleBytes int }

type citationSource struct{ title, url string }

// CitationCollector collects provider-confirmed URLs across one question's model
// steps. The owner calls Add after validated completion and actual web search;
// model text, tool results and provider search actions are never scanned for URLs.
// It is question-scoped and must not be used concurrently.
type CitationCollector struct {
	limits     CitationLimits
	sources    []citationSource
	seen       map[string]bool
	blockBytes int
}

const citationHeading = "Источники:"

// NewCitationCollector creates an empty bounded collector without fetching URLs.
func NewCitationCollector(limits CitationLimits) (*CitationCollector, error) {
	if limits.MaxSources <= 0 || limits.MaxBlockBytes <= 0 || limits.MaxURLBytes <= 0 || limits.MaxTitleBytes <= 0 {
		return nil, errModelProtocol
	}
	return &CitationCollector{limits: limits, seen: map[string]bool{}, blockBytes: len(citationHeading)}, nil
}

// Add accepts only url_citation annotations in completed assistant output_text
// messages. Invalid annotations or exceeded quotas leave the collector unchanged.
func (c *CitationCollector) Add(items []ModelItem) error {
	sources := append([]citationSource(nil), c.sources...)
	seen := make(map[string]bool, len(c.seen))
	for key := range c.seen {
		seen[key] = true
	}
	blockBytes := c.blockBytes
	for _, item := range items {
		if item.Type != "message" {
			continue
		}
		if item.ID == "" || !validObject(item.Raw) {
			return errModelProtocol
		}
		var message struct {
			ID      string `json:"id"`
			Type    string `json:"type"`
			Role    string `json:"role"`
			Status  string `json:"status"`
			Content []struct {
				Type        string            `json:"type"`
				Annotations []json.RawMessage `json:"annotations"`
			} `json:"content"`
		}
		if json.Unmarshal(item.Raw, &message) != nil || message.ID != item.ID || message.Type != "message" || message.Role != "assistant" || message.Status != "completed" {
			return errModelProtocol
		}
		for _, part := range message.Content {
			if part.Type != "output_text" {
				continue
			}
			for _, raw := range part.Annotations {
				var fields map[string]json.RawMessage
				if json.Unmarshal(raw, &fields) != nil || fields == nil {
					return errModelProtocol
				}
				var kind string
				if !citationField(fields["type"], &kind) || kind == "" {
					return errModelProtocol
				}
				if kind != "url_citation" {
					continue
				}
				source, err := c.annotation(fields)
				if err != nil {
					return err
				}
				if seen[source.url] {
					continue
				}
				if len(sources) >= c.limits.MaxSources {
					return errModelProtocol
				}
				// One newline/list marker and one plain-text em-dash separator.
				overhead := len("\n- ") + len(" — ")
				remaining := c.limits.MaxBlockBytes - blockBytes
				if remaining < overhead || len(source.title) > remaining-overhead {
					return errModelProtocol
				}
				remaining -= overhead + len(source.title)
				if len(source.url) > remaining {
					return errModelProtocol
				}
				blockBytes += overhead
				blockBytes += len(source.title)
				blockBytes += len(source.url)
				sources = append(sources, source)
				seen[source.url] = true
			}
		}
	}
	c.sources, c.seen, c.blockBytes = sources, seen, blockBytes
	return nil
}

// Block returns a separate text block, or nil when no source was confirmed.
// URLs are plain text: no Markdown links or HTML are generated.
func (c *CitationCollector) Block() *dialogue.Block {
	if len(c.sources) == 0 {
		return nil
	}
	var text strings.Builder
	text.Grow(c.blockBytes)
	text.WriteString(citationHeading)
	for _, source := range c.sources {
		text.WriteString("\n- ")
		text.WriteString(source.title)
		text.WriteString(" — ")
		text.WriteString(source.url)
	}
	return &dialogue.Block{Type: "text", Version: 1, Text: text.String()}
}

func (c *CitationCollector) annotation(fields map[string]json.RawMessage) (citationSource, error) {
	var address, title string
	var start, end int64
	if !citationField(fields["url"], &address) {
		return citationSource{}, errModelProtocol
	}
	if raw, present := fields["title"]; present && !citationField(raw, &title) {
		return citationSource{}, errModelProtocol
	}
	if raw, present := fields["start_index"]; present && !citationField(raw, &start) {
		return citationSource{}, errModelProtocol
	}
	if raw, present := fields["end_index"]; present && !citationField(raw, &end) {
		return citationSource{}, errModelProtocol
	}
	// AI Studio documents all fields except URL as compatibility defaults (0,
	// 0, ""). Other offsets have no documented Unicode unit. Treat them only
	// as optional ordered integers, never as rune/byte/UTF-16 text positions;
	// sources are always rendered as an unanchored list, without text slicing.
	// https://aistudio.yandex.ru/en/docs//ai-studio/concepts/agents/tools/websearch

	if start < 0 || end < start {
		return citationSource{}, errModelProtocol
	}
	normalized, host, ok := citationURL(address, c.limits.MaxURLBytes)
	if !ok {
		return citationSource{}, errModelProtocol
	}
	title = citationTitle(title)
	if title == "" {
		title = host
	}
	if len(title) > c.limits.MaxTitleBytes {
		return citationSource{}, errModelProtocol
	}
	return citationSource{title: title, url: normalized}, nil
}

// Read canonical wire names explicitly; encoding/json struct fields would also
// accept case aliases such as URL, which are not provider citation fields.
func citationField(raw json.RawMessage, target any) bool {
	return len(raw) > 0 && !bytes.Equal(bytes.TrimSpace(raw), []byte("null")) && json.Unmarshal(raw, target) == nil
}

func citationURL(raw string, maxBytes int) (string, string, bool) {
	if raw == "" || len(raw) > maxBytes {
		return "", "", false
	}
	for _, r := range raw {
		if unicode.IsControl(r) || unicode.IsSpace(r) || unicode.Is(unicode.Cf, r) || r == '<' || r == '>' || r == '"' || r == '`' || r == '\\' {
			return "", "", false
		}
	}
	decoded, err := url.PathUnescape(raw)
	if err != nil {
		return "", "", false
	}
	for _, r := range decoded {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return "", "", false
		}
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", "", false
	}
	if u.Scheme == "" {
		// AI Studio's documented examples return bare domains. Add a secure
		// scheme only to an ordinary hostname/path, never //host or /path.
		if u.Host != "" || strings.HasPrefix(raw, "/") {
			return "", "", false
		}
		u, err = url.Parse("https://" + raw)
		if err != nil {
			return "", "", false
		}
	}
	if (!strings.EqualFold(u.Scheme, "http") && !strings.EqualFold(u.Scheme, "https")) || u.User != nil || u.Opaque != "" || !citationHostname(u.Hostname()) {
		return "", "", false
	}
	if port := u.Port(); port != "" {
		number, err := strconv.Atoi(port)
		if err != nil || number < 1 || number > 65535 {
			return "", "", false
		}
	}
	u.Scheme = strings.ToLower(u.Scheme)
	u.Host = strings.ToLower(u.Host)
	address := u.String()
	if len(address) > maxBytes {
		return "", "", false
	}
	return address, strings.ToLower(u.Hostname()), true
}

func citationHostname(host string) bool {
	if net.ParseIP(host) != nil {
		return true
	}
	if host == "" || len(host) > 253 {
		return false
	}
	host = strings.TrimSuffix(host, ".")
	for _, label := range strings.Split(host, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, r := range label {
			if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-') {
				return false
			}
		}
	}
	return true
}

func citationTitle(title string) string {
	var clean strings.Builder
	for _, r := range title {
		if unicode.IsSpace(r) {
			clean.WriteByte(' ')
			continue
		}
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			continue
		}
		if strings.ContainsRune("<>[]()*_`\\", r) {
			clean.WriteByte(' ')
			continue
		}
		clean.WriteRune(r)
	}
	return strings.Join(strings.Fields(clean.String()), " ")
}
