package aistudio

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

var errPrivacy = errors.New("model input filtering failed")

// These deliberately narrow rules recognize requisites, not arbitrary long numbers.
var (
	bareCardPattern = regexp.MustCompile(`[0-9]{13,19}`)
	emailPattern    = regexp.MustCompile(`[\p{L}\p{N}.!#$%&'*+/=?^_` + "`" + `{|}~-]+@[\p{L}\p{N}](?:[\p{L}\p{N}-]*[\p{L}\p{N}])?(?:\.[\p{L}\p{N}](?:[\p{L}\p{N}-]*[\p{L}\p{N}])?)+`)
	phonePattern    = regexp.MustCompile(`(?:\+7|[78])[ \t\x{00a0}\x{202f}(-]*[0-9]{3}[ \t\x{00a0}\x{202f})-]*[0-9]{3}[ \t\x{00a0}\x{202f}-]*[0-9]{2}[ \t\x{00a0}\x{202f}-]*[0-9]{2}`)
	accountPattern  = regexp.MustCompile(`[0-9]{20}|[0-9]{5}[ .-][0-9]{3}[ .-][0-9][ .-][0-9]{4}[ .-][0-9]{7}`)
	cardPattern     = regexp.MustCompile(`[0-9](?:[ \t\x{00a0}\x{202f}-]?[0-9]){12,18}`)
	phoneLabel      = regexp.MustCompile(`(?i)(?:^|[^\p{L}\p{N}_])(?:телефон|тел\.?|phone|mobile)\s*[:№#=]?\s*$`)
	accountLabel    = regexp.MustCompile(`(?i)(?:^|[^\p{L}\p{N}_])(?:сч[её]т|сч[её]та|р/с|л/с|account|iban)\s*[:№#=]?\s*$`)
	cardLabel       = regexp.MustCompile(`(?i)(?:^|[^\p{L}\p{N}_])(?:карт(?:а|ы|е|у|ой|ою)|card|pan)\s*[:№#=]?\s*$`)
)

type privacyReplacement struct{ token, value string }
type outboundRedactor struct {
	key          []byte
	scope        string
	replacements []privacyReplacement
}

func (f *outboundRedactor) token(kind, original, normalized string) string {
	mac := hmac.New(sha256.New, f.key)
	_, _ = mac.Write([]byte(f.scope + "\x00" + kind + "\x00" + normalized))
	token := "[" + kind + "_" + hex.EncodeToString(mac.Sum(nil)[:12]) + "]"
	f.replacements = append(f.replacements, privacyReplacement{token: token, value: original})
	return token
}

func digits(value string) string {
	var b strings.Builder
	for _, r := range value {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}
func digitBoundary(text string, start, end int) bool {
	return (start == 0 || text[start-1] < '0' || text[start-1] > '9') && (end == len(text) || text[end] < '0' || text[end] > '9')
}
func labelBefore(pattern *regexp.Regexp, text string, start int) bool {
	return pattern.MatchString(text[max(0, start-64):start])
}
func luhn(value string) bool {
	sum, double := 0, false
	for i := len(value) - 1; i >= 0; i-- {
		n := int(value[i] - '0')
		if double {
			n *= 2
			if n > 9 {
				n -= 9
			}
		}
		sum += n
		double = !double
	}
	return sum%10 == 0 && strings.Trim(value, "0") != ""
}
func groupedCard(value string) bool {
	groups := strings.FieldsFunc(value, func(r rune) bool { return r == '-' || unicode.IsSpace(r) })
	if len(groups) == 3 && len(groups[0]) == 4 && len(groups[1]) == 6 && len(groups[2]) == 5 {
		return true
	}
	if len(groups) < 4 || len(groups) > 5 {
		return false
	}
	for _, group := range groups[:len(groups)-1] {
		if len(group) != 4 {
			return false
		}
	}
	return len(groups[len(groups)-1]) >= 1 && len(groups[len(groups)-1]) <= 4
}

type privacySpan struct {
	start, end       int
	kind, normalized string
}

func (f *outboundRedactor) text(text string) (string, error) {
	if !utf8.ValidString(text) {
		return "", errPrivacy
	}
	var spans []privacySpan
	add := func(pattern *regexp.Regexp, kind string, accept func(string, int, int) (string, bool)) {
		for _, span := range pattern.FindAllStringIndex(text, -1) {
			start, end := span[0], span[1]
			overlap := false
			for _, prior := range spans {
				if start < prior.end && end > prior.start {
					overlap = true
					break
				}
			}
			if overlap {
				continue
			}
			normalized, ok := accept(text[start:end], start, end)
			if ok {
				spans = append(spans, privacySpan{start, end, kind, normalized})
			}
		}
	}
	add(emailPattern, "email", func(v string, _, _ int) (string, bool) { return v, true })
	add(accountPattern, "account", func(v string, start, end int) (string, bool) {
		return digits(v), digitBoundary(text, start, end) && (len(v) != 20 || labelBefore(accountLabel, text, start))
	})
	add(phonePattern, "phone", func(v string, start, end int) (string, bool) {
		n := digits(v)
		n = "7" + n[1:]
		return n, digitBoundary(text, start, end) && (strings.HasPrefix(v, "+7") || len(v) != 11 || labelBefore(phoneLabel, text, start))
	})
	add(bareCardPattern, "card", func(v string, start, end int) (string, bool) {
		return v, digitBoundary(text, start, end) && labelBefore(cardLabel, text, start)
	})
	add(cardPattern, "card", func(v string, start, end int) (string, bool) {
		n := digits(v)
		return n, digitBoundary(text, start, end) && groupedCard(v) && luhn(n)
	})
	sort.Slice(spans, func(i, j int) bool { return spans[i].start < spans[j].start })
	if len(spans) == 0 {
		return text, nil
	}
	var b strings.Builder
	at := 0
	for _, span := range spans {
		b.WriteString(text[at:span.start])
		b.WriteString(f.token(span.kind, text[span.start:span.end], span.normalized))
		at = span.end
	}
	b.WriteString(text[at:])
	return b.String(), nil
}

// value preserves numeric tokens, including quoted protobuf int64 values. Only
// arguments/output contain JSON encoded inside a string in the Responses protocol.
func (f *outboundRedactor) value(value any, depth int) (any, error) {
	if depth > 64 {
		return nil, errPrivacy
	}
	switch v := value.(type) {
	case map[string]any:
		result := make(map[string]any, len(v))
		for key, item := range v {
			if key == "encrypted_content" {
				content, text := item.(string)
				if item != nil && (!text || content != "") {
					return nil, errPrivacy
				}
				continue
			}
			filteredKey, err := f.text(key)
			if err != nil {
				return nil, err
			}
			if _, duplicate := result[filteredKey]; duplicate {
				return nil, errPrivacy
			}
			if text, ok := item.(string); ok && (key == "arguments" || key == "output") && (strings.HasPrefix(strings.TrimSpace(text), "{") || strings.HasPrefix(strings.TrimSpace(text), "[")) {
				raw, err := f.json([]byte(text), depth+1)
				if err != nil {
					return nil, err
				}
				result[filteredKey] = string(raw)
			} else {
				filtered, err := f.value(item, depth+1)
				if err != nil {
					return nil, err
				}
				result[filteredKey] = filtered
			}
		}
		return result, nil
	case []any:
		result := make([]any, len(v))
		for i, item := range v {
			filtered, err := f.value(item, depth+1)
			if err != nil {
				return nil, err
			}
			result[i] = filtered
		}
		return result, nil
	case string:
		return f.text(v)
	default:
		return value, nil
	}
}
func (f *outboundRedactor) json(raw []byte, depth int) ([]byte, error) {
	var value any
	if decodeJSON(raw, &value) != nil {
		return nil, errPrivacy
	}
	filtered, err := f.value(value, depth)
	if err != nil {
		return nil, err
	}
	if reflect.DeepEqual(value, filtered) {
		return append([]byte(nil), raw...), nil
	}
	result, err := json.Marshal(filtered)
	if err != nil {
		return nil, errPrivacy
	}
	return result, nil
}
func (f *outboundRedactor) request(request Request) (Request, error) {
	result := request
	var err error
	result.Instructions, err = f.text(request.Instructions)
	if err != nil {
		return Request{}, err
	}
	result.Input = make([]json.RawMessage, 0, len(request.Input))
	for _, raw := range request.Input {
		var item map[string]json.RawMessage
		if decodeJSON(raw, &item) != nil || item == nil {
			return Request{}, errPrivacy
		}
		// Visible reasoning is part of manual context; it passes the same filter.
		// Nonempty encrypted_content fails closed instead of silently dropping context.
		filtered, err := f.json(raw, 0)
		if err != nil {
			return Request{}, err
		}
		result.Input = append(result.Input, filtered)
	}
	result.Tools = make([]json.RawMessage, len(request.Tools))
	for i, raw := range request.Tools {
		result.Tools[i], err = f.json(raw, 0)
		if err != nil {
			return Request{}, err
		}
	}
	if len(result.Input) == 0 {
		return Request{}, errPrivacy
	}
	return result, nil
}

// Restore only the declared local text-search field, never provider search,
// navigation, amounts or identifiers. The next HTTP copy is always filtered again.
func restoreSearchArguments(name, arguments string, replacements []privacyReplacement) (string, error) {
	if name != "find_operations" && name != "aggregate_operations" {
		return arguments, nil
	}
	var args map[string]json.RawMessage
	if decodeJSON([]byte(arguments), &args) != nil {
		return "", errPrivacy
	}
	raw, ok := args["filter"]
	if !ok {
		return arguments, nil
	}
	var filter map[string]json.RawMessage
	if decodeJSON(raw, &filter) != nil {
		return "", errPrivacy
	}
	raw, ok = filter["q"]
	if !ok {
		return arguments, nil
	}
	var query string
	if json.Unmarshal(raw, &query) != nil {
		return "", errPrivacy
	}
	originalQuery := query
	for _, r := range replacements {
		query = strings.ReplaceAll(query, r.token, r.value)
	}
	if query == originalQuery {
		return arguments, nil
	}
	filter["q"], _ = json.Marshal(query)
	args["filter"], _ = json.Marshal(filter)
	result, _ := json.Marshal(args)
	return string(result), nil
}
