package generation

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/fin-assist/assistant/internal/tooldata"
)

type toolPlan struct {
	item         ModelItem
	args         any
	presentation *presentationArgs
	required     bool
	terminal     string
	run          func(context.Context, tooldata.Subject) (tooldata.Result, error)
}

type filterArguments struct {
	From       *time.Time `json:"from"`
	To         *time.Time `json:"to"`
	Query      string     `json:"q"`
	CategoryID string     `json:"category_id"`
	Selection  string     `json:"selection"`
	Kind       string     `json:"kind"`
	AmountFrom *int64     `json:"amount_from_minor"`
	AmountTo   *int64     `json:"amount_to_minor"`
}

func (a filterArguments) domain() tooldata.Filter {
	return tooldata.Filter{From: a.From, To: a.To, Query: a.Query, CategoryID: a.CategoryID, Selection: a.Selection, Kind: a.Kind, AmountFrom: a.AmountFrom, AmountTo: a.AmountTo}
}

func validFilter(f filterArguments, allTime bool) bool {
	if allTime && (f.From != nil || f.To != nil) {
		return false
	}
	for _, at := range []*time.Time{f.From, f.To} {
		if at != nil && (at.IsZero() || at.Year() < 1 || at.Year() > 9999) {
			return false
		}
	}
	if f.From != nil && f.To != nil && !f.From.Before(*f.To) {
		return false
	}
	if f.AmountFrom != nil && *f.AmountFrom < 0 || f.AmountTo != nil && *f.AmountTo < 0 || f.AmountFrom != nil && f.AmountTo != nil && *f.AmountFrom > *f.AmountTo {
		return false
	}
	return f.Kind == "" || f.Kind == "expense" || f.Kind == "income"
}
func validPeriod(period, key string) bool {
	if period == "" {
		period = "month"
	}
	if period != "month" && period != "quarter" && period != "year" {
		return false
	}
	if key == "" {
		return true
	}
	if (period == "month" || period == "quarter" || period == "year") && len(key) == 7 && key[4] == '-' {
		if at, err := time.Parse("2006-01", key); err == nil && at.Year() > 0 {
			return true
		}
	}
	if period != "month" && len(key) == 7 && key[4:6] == "-Q" && key[6] >= '1' && key[6] <= '4' {
		if at, err := time.Parse("2006", key[:4]); err == nil && at.Year() > 0 {
			return true
		}
	}
	if period == "year" && len(key) == 4 {
		if at, err := time.Parse("2006", key); err == nil && at.Year() > 0 {
			return true
		}
	}
	return false
}

func validObject(raw []byte) bool {
	if !utf8.Valid(raw) {
		return false
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return false
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	if jsonValue(d) != nil {
		return false
	}
	_, err := d.Token()
	return errors.Is(err, io.EOF)
}
func jsonValue(d *json.Decoder) error {
	token, err := d.Token()
	if err != nil {
		return err
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delimiter {
	case '{':
		keys := map[string]bool{}
		for d.More() {
			key, err := d.Token()
			if err != nil {
				return err
			}
			name, ok := key.(string)
			if !ok || keys[name] {
				return errModelProtocol
			}
			keys[name] = true
			if err := jsonValue(d); err != nil {
				return err
			}
		}
	case '[':
		for d.More() {
			if err := jsonValue(d); err != nil {
				return err
			}
		}
	default:
		return errModelProtocol
	}
	end, err := d.Token()
	if err != nil {
		return err
	}
	if delimiter == '{' && end != json.Delim('}') || delimiter == '[' && end != json.Delim(']') {
		return errModelProtocol
	}
	return nil
}

func validateOutput(items []ModelItem) ([]ModelItem, int, string, error) {
	var calls []ModelItem
	searches := 0
	var text strings.Builder
	ids := map[string]bool{}
	for _, item := range items {
		if item.ID == "" || ids[item.ID] || !validObject(item.Raw) {
			return nil, 0, "", errModelProtocol
		}
		ids[item.ID] = true
		var wire struct {
			ID        string `json:"id"`
			Type      string `json:"type"`
			CallID    string `json:"call_id"`
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
			Role      string `json:"role"`
			Content   []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		}
		if json.Unmarshal(item.Raw, &wire) != nil || wire.ID != item.ID || wire.Type != item.Type {
			return nil, 0, "", errModelProtocol
		}
		switch item.Type {
		case "function_call":
			if item.CallID == "" || item.Name == "" || wire.CallID != item.CallID || wire.Name != item.Name || wire.Arguments != item.Arguments {
				return nil, 0, "", errModelProtocol
			}
			calls = append(calls, item)
		case "message":
			if wire.Role != "assistant" {
				return nil, 0, "", errModelProtocol
			}
			for _, part := range wire.Content {
				if part.Type == "refusal" {
					return nil, 0, "", ErrRefused
				}
				if part.Type != "output_text" {
					return nil, 0, "", errModelProtocol
				}
				text.WriteString(part.Text)
			}
		case "reasoning":
		case "web_search_call":
			searches++
		default:
			return nil, 0, "", errModelProtocol
		}
	}
	return calls, searches, text.String(), nil
}
