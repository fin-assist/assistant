package dialogue

import (
	"encoding/json"
	"errors"
	"math"
	"regexp"
	"time"

	assistantv1 "github.com/fin-assist/contracts/gen/go/finassist/assistant/v1"
	commonv1 "github.com/fin-assist/contracts/gen/go/finassist/common/v1"
	"google.golang.org/protobuf/types/known/timestamppb"

	domain "github.com/fin-assist/assistant/internal/dialogue"
)

var errStored = errors.New("invalid saved dialogue data")

// Message converts one saved question or answer into its transport representation.
func Message(m domain.Message) (*assistantv1.Message, error) {
	created, err := timestamp(m.CreatedAt)
	if err != nil {
		return nil, err
	}
	result := &assistantv1.Message{Id: m.ID, CreatedAt: created, Currency: "RUB"}
	if m.Answer == nil {
		result.Role = assistantv1.Role_ROLE_USER
		result.Text = &m.Question
		return result, nil
	}
	a := *m.Answer
	snapshot, err := AnswerSnapshot(a)
	if err != nil {
		return nil, err
	}
	result.Role = assistantv1.Role_ROLE_ASSISTANT
	result.Status = snapshot.Status
	result.Currency = snapshot.Currency
	result.Blocks = snapshot.Blocks
	if a.Status == "complete" {
		result.Chips = snapshot.Chips
		result.Source = snapshot.Source
	}
	if a.Status == "complete" {
		result.Charged = &a.Charged
	}
	if a.Status == "failed" {
		failure, e := Error(a, 0)
		if e != nil {
			return nil, e
		}
		result.ErrorCode = failure.Code
		result.ErrorText = &failure.Text
		result.Retryable = &failure.Retryable
	}
	return result, nil
}

// AnswerSnapshot converts the complete observable state without exposing failed partial content.
func AnswerSnapshot(a domain.Answer) (*assistantv1.AnswerSnapshot, error) {
	result := &assistantv1.AnswerSnapshot{Currency: a.Currency}
	if result.Currency == "" {
		result.Currency = "RUB"
	}
	switch a.Status {
	case "generating":
		result.Status = assistantv1.AnswerStatus_ANSWER_STATUS_GENERATING
	case "complete":
		result.Status = assistantv1.AnswerStatus_ANSWER_STATUS_COMPLETE
	case "failed":
		result.Status = assistantv1.AnswerStatus_ANSWER_STATUS_FAILED
		return result, nil
	default:
		return nil, errStored
	}
	var err error
	result.Blocks, err = Blocks(a.Blocks)
	if err != nil {
		return nil, err
	}
	result.Chips, err = Chips(a.Chips)
	if err != nil {
		return nil, err
	}
	result.Source, err = Source(a.Source)
	if err != nil {
		return nil, err
	}

	return result, nil
}

type blockBody struct {
	Title   string `json:"title"`
	AltText string `json:"alt_text"`
	Data    struct {
		Kind      string `json:"kind"`
		ValueKind string `json:"value_kind"`
		Highlight *int32 `json:"highlight_index"`
		Points    []struct {
			Range    savedRange   `json:"range"`
			Value    *json.Number `json:"value"`
			Coverage string       `json:"coverage"`
			DataFrom *time.Time   `json:"data_from"`
			DataTo   *time.Time   `json:"data_to"`
		} `json:"points"`
		Rows []struct {
			Label     string          `json:"label"`
			Sublabel  *string         `json:"sublabel"`
			Value     json.RawMessage `json:"value"`
			ValueKind string          `json:"value_kind"`
			Text      *string         `json:"text"`
		} `json:"rows"`
	} `json:"data"`
}
type savedRange struct {
	From time.Time `json:"from"`
	To   time.Time `json:"to"`
}

// Blocks preserves block positions, falling back to alt text for future catalog types.
func Blocks(items []domain.Block) ([]*assistantv1.Block, error) {
	result := make([]*assistantv1.Block, 0, len(items))
	for _, item := range items {
		if item.Type == "text" && item.Version == 1 {
			result = append(result, textBlock(item.Text))
			continue
		}
		var envelope struct {
			AltText string          `json:"alt_text"`
			Data    json.RawMessage `json:"data"`
		}
		if json.Unmarshal(item.Body, &envelope) != nil || envelope.AltText == "" {
			return nil, errStored
		}
		if item.Version != 1 || (item.Type != "chart" && item.Type != "rows") {
			result = append(result, textBlock(envelope.AltText))
			continue
		}
		if item.Type == "chart" {
			var shape struct {
				Kind      string `json:"kind"`
				ValueKind string `json:"value_kind"`
			}
			if json.Unmarshal(envelope.Data, &shape) != nil {
				return nil, errStored
			}
			// Future chart kinds and units may carry a different data shape.
			// Read their discriminators before touching typed numeric payloads.
			if shape.Kind != "bar" || (shape.ValueKind != "amount" && shape.ValueKind != "percent") {
				result = append(result, textBlock(envelope.AltText))
				continue
			}
		}
		var body blockBody
		if json.Unmarshal(item.Body, &body) != nil {
			return nil, errStored
		}
		block := &assistantv1.Block{AltText: &body.AltText}
		if item.Type == "chart" {

			chart := &assistantv1.ChartBlock{Kind: assistantv1.ChartKind_CHART_KIND_BAR, Title: body.Title, HighlightIndex: body.Data.Highlight, ValueKind: assistantv1.ValueKind_VALUE_KIND_AMOUNT}
			if body.Data.ValueKind == "percent" {
				chart.ValueKind = assistantv1.ValueKind_VALUE_KIND_PERCENT
			}
			if chart.HighlightIndex != nil && (*chart.HighlightIndex < 0 || int(*chart.HighlightIndex) >= len(body.Data.Points)) {
				return nil, errStored
			}
			for _, p := range body.Data.Points {
				r, err := timeRange(p.Range.From, p.Range.To)
				if err != nil {
					return nil, err
				}
				bounds := &commonv1.DataBounds{}
				switch p.Coverage {
				case "complete":
					bounds.Coverage = commonv1.Coverage_COVERAGE_COMPLETE
				case "partial":
					bounds.Coverage = commonv1.Coverage_COVERAGE_PARTIAL
				case "no_data":
					bounds.Coverage = commonv1.Coverage_COVERAGE_NO_DATA
				default:
					return nil, errStored
				}
				if p.DataFrom != nil {
					bounds.DataFrom, err = timestamp(*p.DataFrom)
					if err != nil {
						return nil, err
					}
				}
				if p.DataTo != nil {
					bounds.DataTo, err = timestamp(*p.DataTo)
					if err != nil {
						return nil, err
					}
				}
				point := &assistantv1.ChartPoint{Range: r, Bounds: bounds}
				if p.Value != nil {
					v, e := number(*p.Value, body.Data.ValueKind)
					if e != nil {
						return nil, e
					}
					point.Value = &v
				}
				chart.Points = append(chart.Points, point)
			}
			block.Content = &assistantv1.Block_Chart{Chart: chart}
		} else {
			rows := &assistantv1.RowsBlock{}
			for _, r := range body.Data.Rows {
				if r.Label == "" {
					return nil, errStored
				}
				row := &assistantv1.BlockRow{Label: r.Label, Sublabel: r.Sublabel}
				switch r.ValueKind {
				case "amount", "count":
					if r.Value == nil {
						return nil, errStored
					}
					n, e := rowNumber(r.Value)
					if e != nil {
						return nil, e
					}
					integer, e := n.Int64()
					if e != nil || (r.ValueKind == "count" && integer < 0) {
						return nil, errStored
					}
					if r.ValueKind == "amount" {
						row.Value = &assistantv1.BlockRow_AmountMinor{AmountMinor: integer}
					} else {
						row.Value = &assistantv1.BlockRow_Count{Count: integer}
					}
				case "percent":
					if r.Value == nil {
						return nil, errStored
					}
					rawNumber, e := rowNumber(r.Value)
					if e != nil {
						return nil, e
					}
					n, e := number(rawNumber, "percent")
					if e != nil {
						return nil, e
					}
					row.Value = &assistantv1.BlockRow_Percent{Percent: n}
				case "text":
					var text string
					if json.Unmarshal(r.Value, &text) != nil || string(r.Value) == "null" {
						return nil, errStored
					}
					row.Value = &assistantv1.BlockRow_Text{Text: text}
				default:
					if r.Text == nil {
						return nil, errStored
					}
					row.Value = &assistantv1.BlockRow_Text{Text: *r.Text}
				}
				rows.Rows = append(rows.Rows, row)
			}
			block.Content = &assistantv1.Block_Rows{Rows: rows}
		}
		result = append(result, block)
	}
	return result, nil
}
func textBlock(s string) *assistantv1.Block {
	return &assistantv1.Block{Content: &assistantv1.Block_Text{Text: &assistantv1.TextBlock{Text: s}}}
}
func number(n json.Number, kind string) (float64, error) {
	if kind == "amount" {
		v, e := n.Int64()
		if e != nil || v > 9007199254740991 || v < -9007199254740991 {
			return 0, errStored
		}
		return float64(v), nil
	}
	v, e := n.Float64()
	if e != nil || math.IsInf(v, 0) || math.IsNaN(v) || v < 0 || v > 1 {
		return 0, errStored
	}
	return v, nil
}
func timestamp(t time.Time) (*timestamppb.Timestamp, error) {
	p := timestamppb.New(t)
	if t.IsZero() || p.CheckValid() != nil {
		return nil, errStored
	}
	return p, nil
}
func timeRange(from, to time.Time) (*commonv1.TimeRange, error) {
	if !from.Before(to) {
		return nil, errStored
	}
	a, e := timestamp(from)
	if e != nil {
		return nil, e
	}
	b, e := timestamp(to)
	if e != nil {
		return nil, e
	}
	return &commonv1.TimeRange{Start: a, End: b}, nil
}
func transfer(s string) (commonv1.TransferMode, error) {
	switch s {
	case "with":
		return commonv1.TransferMode_TRANSFER_MODE_WITH, nil
	case "without":
		return commonv1.TransferMode_TRANSFER_MODE_WITHOUT, nil
	default:
		return 0, errStored
	}
}

// Source converts source metadata; unknown semantic codes cannot be guessed safely.
func Source(s *domain.Source) (*assistantv1.AnswerSource, error) {
	if s == nil {
		return nil, nil
	}
	at, e := timestamp(s.CalculatedAt)
	if e != nil {
		return nil, e
	}
	mode, e := transfer(s.TransferMode)
	if e != nil {
		return nil, e
	}
	result := &assistantv1.AnswerSource{CalculatedAt: at, TransferMode: mode}
	if s.From != nil || s.To != nil {
		if s.From == nil || s.To == nil {
			return nil, errStored
		}
		result.DataRange, e = timeRange(*s.From, *s.To)
		if e != nil {
			return nil, e
		}
	}
	for _, code := range s.ChangedSince {
		switch code {
		case "categories":
			result.ChangedSince = append(result.ChangedSince, assistantv1.ChangedSince_CHANGED_SINCE_CATEGORIES)
		case "uploads":
			result.ChangedSince = append(result.ChangedSince, assistantv1.ChangedSince_CHANGED_SINCE_UPLOADS)
		default:
			return nil, errStored
		}
	}
	return result, nil
}

type savedFilter struct {
	From          *time.Time `json:"from"`
	To            *time.Time `json:"to"`
	Q             *string    `json:"q"`
	Category      *string    `json:"category_id"`
	Kind          string     `json:"kind"`
	AmountFrom    *int64     `json:"amount_from"`
	AmountTo      *int64     `json:"amount_to"`
	Transfer      string     `json:"transfer_mode"`
	Selection     *string    `json:"selection"`
	SelectionName *string    `json:"selection_name"`
}

var monthKey = regexp.MustCompile(`^[0-9]{4}-(0[1-9]|1[0-2])$`)
var quarterKey = regexp.MustCompile(`^[0-9]{4}-Q[1-4]$`)
var yearKey = regexp.MustCompile(`^[0-9]{4}$`)

// Chips converts stored REST navigation parameters to typed gRPC values.
func Chips(items []json.RawMessage) ([]*assistantv1.Chip, error) {
	result := make([]*assistantv1.Chip, 0, len(items))
	for _, raw := range items {
		var c struct {
			Label   string       `json:"label"`
			Screen  string       `json:"screen"`
			Filters *savedFilter `json:"filters"`
			Params  *struct {
				Period   string `json:"period"`
				Date     string `json:"date"`
				Transfer string `json:"transfer_mode"`
			} `json:"params"`
		}
		if json.Unmarshal(raw, &c) != nil || c.Label == "" {
			return nil, errStored
		}
		chip := &assistantv1.Chip{Label: c.Label}
		switch c.Screen {
		case "operations":
			if c.Filters == nil || c.Params != nil {
				return nil, errStored
			}
			f := c.Filters
			hasText := func(s *string) bool { return s != nil && *s != "" }
			if f.From == nil && f.To == nil && !hasText(f.Q) && !hasText(f.Category) && f.Kind == "" && f.AmountFrom == nil && f.AmountTo == nil && !hasText(f.Selection) {
				return nil, errStored
			}
			mode, e := transfer(f.Transfer)
			if e != nil {
				return nil, e
			}
			out := &commonv1.OperationsFilter{Q: f.Q, CategoryId: f.Category, AmountFromMinor: f.AmountFrom, AmountToMinor: f.AmountTo, TransferMode: mode, Selection: f.Selection, SelectionName: f.SelectionName}
			if f.From != nil {
				out.From, e = timestamp(*f.From)
				if e != nil {
					return nil, e
				}
			}
			if f.To != nil {
				out.To, e = timestamp(*f.To)
				if e != nil {
					return nil, e
				}
			}
			if f.From != nil && f.To != nil && !f.From.Before(*f.To) {
				return nil, errStored
			}
			if (f.AmountFrom != nil && *f.AmountFrom < 0) || (f.AmountTo != nil && *f.AmountTo < 0) || (f.AmountFrom != nil && f.AmountTo != nil && *f.AmountFrom > *f.AmountTo) {
				return nil, errStored
			}
			switch f.Kind {
			case "":
			case "expense":
				out.Kind = commonv1.KindFilter_KIND_FILTER_EXPENSE
			case "income":
				out.Kind = commonv1.KindFilter_KIND_FILTER_INCOME
			default:
				return nil, errStored
			}
			chip.Screen = assistantv1.ChipScreen_CHIP_SCREEN_OPERATIONS
			chip.Filters = out
		case "analytics":
			if c.Params == nil || c.Filters != nil {
				return nil, errStored
			}
			p := c.Params
			mode, e := transfer(p.Transfer)
			if e != nil {
				return nil, e
			}
			out := &commonv1.AnalyticsParams{PeriodKey: p.Date, TransferMode: mode}
			switch p.Period {
			case "month":
				if !monthKey.MatchString(p.Date) {
					return nil, errStored
				}
				out.Period = commonv1.PeriodType_PERIOD_TYPE_MONTH
			case "quarter":
				if !quarterKey.MatchString(p.Date) {
					return nil, errStored
				}
				out.Period = commonv1.PeriodType_PERIOD_TYPE_QUARTER
			case "year":
				if !yearKey.MatchString(p.Date) {
					return nil, errStored
				}
				out.Period = commonv1.PeriodType_PERIOD_TYPE_YEAR
			default:
				return nil, errStored
			}
			chip.Screen = assistantv1.ChipScreen_CHIP_SCREEN_ANALYTICS
			chip.Params = out
		default:
			return nil, errStored
		}
		result = append(result, chip)
	}
	return result, nil
}

// Error supplies server-owned display and retry policy, including legacy saved answers.
func Error(a domain.Answer, remaining int) (*assistantv1.AnswerError, error) {
	if a.Status != "failed" || remaining < 0 || remaining > math.MaxInt32 {
		return nil, errStored
	}
	result := &assistantv1.AnswerError{RemainingLimit: int32(remaining)}
	switch a.ErrorCode {
	case "MODEL_ERROR":
		result.Code = assistantv1.AnswerErrorCode_ANSWER_ERROR_CODE_MODEL_ERROR
		result.Text = "Не получилось ответить. Вопрос не потрачен"
		result.Retryable = true
	case "CANNOT_ANSWER":
		result.Code = assistantv1.AnswerErrorCode_ANSWER_ERROR_CODE_CANNOT_ANSWER
		result.Text = "Не получилось ответить — недостаточно данных, переформулируйте вопрос. Вопрос не потрачен"
	case "OFF_TOPIC":
		result.Code = assistantv1.AnswerErrorCode_ANSWER_ERROR_CODE_OFF_TOPIC
		result.Text = "Вопрос не по теме — помощник отвечает только на вопросы о ваших финансах. С другими вопросами помогут универсальные ассистенты: Алиса AI, GigaChat, ChatGPT, Claude, DeepSeek"
	default:
		return nil, errStored
	}
	if a.Retryable != nil {
		result.Retryable = *a.Retryable
	}
	return result, nil
}

func rowNumber(raw json.RawMessage) (json.Number, error) {
	if len(raw) == 0 || raw[0] == '"' || string(raw) == "null" {
		return "", errStored
	}
	var n json.Number
	if json.Unmarshal(raw, &n) != nil {
		return "", errStored
	}
	return n, nil
}
