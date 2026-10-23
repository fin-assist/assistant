package generation

import (
	"encoding/json"
	"math"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/fin-assist/assistant/internal/dialogue"
	"github.com/fin-assist/assistant/internal/domain"
	"github.com/fin-assist/assistant/internal/tooldata"
)

// Presentation chooses only a saved read result and a supported view, never values.
type presentationArgs struct {
	ResultID         string `json:"result_id"`
	View             string `json:"view"`
	Path             string `json:"path,omitempty"`
	CoverageResultID string `json:"coverage_result_id,omitempty"`
}
type presentationResult struct {
	Blocks     []dialogue.Block
	Chips      []json.RawMessage
	Incomplete bool
}

const presentationExactInteger int64 = 9007199254740991

type presentationRange struct {
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
}
type presentationBounds struct {
	Coverage string              `json:"coverage"`
	DataFrom *time.Time          `json:"data_from"`
	DataTo   *time.Time          `json:"data_to"`
	Gaps     []presentationRange `json:"gaps"`
}
type presentationFilter struct {
	From          *time.Time   `json:"from,omitempty"`
	To            *time.Time   `json:"to,omitempty"`
	Q             string       `json:"q,omitempty"`
	CategoryID    string       `json:"category_id,omitempty"`
	Kind          string       `json:"kind,omitempty"`
	AmountFrom    *json.Number `json:"amount_from_minor,omitempty"`
	AmountTo      *json.Number `json:"amount_to_minor,omitempty"`
	Mode          string       `json:"transfer_mode"`
	Selection     string       `json:"selection,omitempty"`
	SelectionName string       `json:"selection_name,omitempty"`
}
type presentationGroup struct {
	Keys []struct {
		Month    string `json:"month"`
		Category *struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"category"`
		Merchant *struct {
			Title     string `json:"title"`
			Selection string `json:"selection"`
		} `json:"merchant"`
	} `json:"keys"`
	Amount  json.Number         `json:"amount_minor"`
	Count   json.Number         `json:"operation_count"`
	Average json.Number         `json:"average_minor"`
	Filters *presentationFilter `json:"filters"`
}
type presentationAggregate struct {
	Groups    []presentationGroup `json:"groups"`
	Amount    json.Number         `json:"total_amount_minor"`
	Count     json.Number         `json:"total_operation_count"`
	Range     *presentationRange  `json:"range"`
	Zone      string              `json:"time_zone"`
	Truncated bool                `json:"truncated"`
}
type presentationRow struct {
	Label    string `json:"label"`
	Sublabel string `json:"sublabel,omitempty"`
	Value    any    `json:"value,omitempty"`
	Kind     string `json:"value_kind,omitempty"`
	Text     string `json:"text,omitempty"`
}
type presentationPoint struct {
	Range struct {
		From time.Time `json:"from"`
		To   time.Time `json:"to"`
	} `json:"range"`
	Value    *int64     `json:"value,omitempty"`
	Coverage string     `json:"coverage"`
	DataFrom *time.Time `json:"data_from,omitempty"`
	DataTo   *time.Time `json:"data_to,omitempty"`
}

func preparePresentation(task domain.GenerationTask, args presentationArgs, refs map[string]ToolResultReference) (presentationResult, error) {
	ref, ok := refs[args.ResultID]
	if !ok || args.ResultID == "" || ref.CallID != args.ResultID || (task.TransferMode != "with" && task.TransferMode != "without") {
		return presentationResult{}, errModelProtocol
	}
	switch args.View {
	case "rows":
		if args.Path != "" || args.CoverageResultID != "" {
			return presentationResult{}, errModelProtocol
		}
		switch ref.Name {
		case "aggregate_operations":
			return presentAggregateRows(task, ref)
		case "find_operations":
			return presentOperationRows(task, ref)
		}
	case "chart":
		if ref.Name != "aggregate_operations" || args.Path != "" {
			return presentationResult{}, errModelProtocol
		}
		return presentAggregateChart(task, args, ref, refs)
	case "analytics":
		if ref.Name != "get_analytics" || args.CoverageResultID != "" {
			return presentationResult{}, errModelProtocol
		}
		return presentAnalytics(task, args, ref)
	}
	return presentationResult{}, errModelProtocol
}
func presentationDecode(raw json.RawMessage, out any) error {
	if !validObject(raw) || json.Unmarshal(raw, out) != nil {
		return errModelProtocol
	}
	return nil
}

// Missing native protobuf scalar fields represent zero; optional catalog values
// use explicit pointers elsewhere and retain their absence.
func presentationInt(n json.Number) (int64, error) {
	if n == "" {
		return 0, nil
	}
	v, err := n.Int64()
	if err != nil {
		return 0, errModelProtocol
	}
	return v, nil
}
func presentationIntegerRow(label string, value int64, kind string) presentationRow {
	row := presentationRow{Label: label}
	if value > presentationExactInteger || value < -presentationExactInteger {
		row.Text = presentationDisplay(value, kind)
		return row
	}
	row.Value = value
	row.Kind = kind
	return row
}
func presentationDisplay(value int64, kind string) string {
	text := strconv.FormatInt(value, 10)
	if kind != "amount" {
		return text
	}
	sign := ""
	if strings.HasPrefix(text, "-") {
		sign = "-"
		text = text[1:]
	}
	for len(text) < 3 {
		text = "0" + text
	}
	return sign + text[:len(text)-2] + "," + text[len(text)-2:] + " ₽"
}
func presentationRows(rows []presentationRow) (dialogue.Block, error) {
	if len(rows) == 0 {
		return dialogue.Block{}, errModelProtocol
	}
	var alt []string
	for _, row := range rows {
		value := row.Text
		if value == "" {
			switch n := row.Value.(type) {
			case int64:
				value = presentationDisplay(n, row.Kind)
			case json.Number:
				value = n.String()
			case float64:
				value = strconv.FormatFloat(n, 'g', -1, 64)
			default:
				return dialogue.Block{}, errModelProtocol
			}
		}
		alt = append(alt, row.Label+": "+value)
	}
	return presentationBlock("rows", strings.Join(alt, ". "), map[string]any{"rows": rows})
}
func presentationBlock(kind, alt string, data any) (dialogue.Block, error) {
	body, err := json.Marshal(map[string]any{"type": kind, "v": 1, "alt_text": alt, "data": data})
	if err != nil {
		return dialogue.Block{}, errModelProtocol
	}
	return dialogue.Block{Type: kind, Version: 1, Body: body}, nil
}
func presentationText(text string) presentationResult {
	return presentationResult{Blocks: []dialogue.Block{{Type: "text", Version: 1, Text: text}}}
}

func presentAggregateRows(task domain.GenerationTask, ref ToolResultReference) (presentationResult, error) {
	if ref.Result.Evidence.NoData {
		out := presentationText("Нет данных за выбранный период")
		out.Incomplete = true
		return out, nil
	}
	args, ok := ref.Args.(tooldata.AggregateArgs)
	if !ok {
		return presentationResult{}, errModelProtocol
	}
	var r presentationAggregate
	if err := presentationDecode(ref.Result.JSON, &r); err != nil {
		return presentationResult{}, err
	}
	amount, err := presentationInt(r.Amount)
	if err != nil {
		return presentationResult{}, err
	}
	count, err := presentationInt(r.Count)
	if err != nil || count < 0 {
		return presentationResult{}, errModelProtocol
	}
	rows := []presentationRow{presentationIntegerRow("Сумма", amount, "amount"), presentationIntegerRow("Всего операций", count, "count")}
	out := presentationResult{Incomplete: ref.Result.Evidence.NoData}
	if len(r.Groups) == 0 {
		filter, err := presentationOriginalFilter(task, args.Filter, r.Range)
		if err != nil {
			return out, err
		}
		chip, err := presentationNativeChip(task, "Операции по выборке", filter)
		if err != nil {
			return out, err
		}
		if chip != nil {
			out.Chips = append(out.Chips, chip)
		}
	}
	for _, group := range r.Groups {
		label, err := presentationGroupLabel(group, args.GroupBy)
		if err != nil {
			return out, err
		}
		amount, err := presentationInt(group.Amount)
		if err != nil {
			return out, err
		}
		count, err := presentationInt(group.Count)
		if err != nil || count < 0 {
			return out, errModelProtocol
		}
		average, err := presentationInt(group.Average)
		if err != nil {
			return out, err
		}
		rows = append(rows, presentationIntegerRow(label, amount, "amount"), presentationIntegerRow(label+" · операций", count, "count"), presentationIntegerRow(label+" · средний чек", average, "amount"))
		if group.Filters == nil {
			return out, errModelProtocol
		}
		chip, err := presentationNativeChip(task, label, group.Filters)
		if err != nil {
			return out, err
		}
		if chip != nil {
			out.Chips = append(out.Chips, chip)
		}
	}
	if r.Truncated || ref.Result.Evidence.Truncated {
		rows = append(rows, presentationRow{Label: "Выборка", Text: "Показана только часть групп"})
	}
	block, err := presentationRows(rows)
	out.Blocks = []dialogue.Block{block}
	return out, err
}
func presentationGroupLabel(group presentationGroup, dimensions []string) (string, error) {
	if len(group.Keys) != len(dimensions) {
		return "", errModelProtocol
	}
	if len(dimensions) == 0 {
		return "Итого по выборке", nil
	}
	var names []string
	for i, key := range group.Keys {
		n := 0
		if key.Month != "" {
			n++
		}
		if key.Category != nil {
			n++
		}
		if key.Merchant != nil {
			n++
		}
		if n != 1 {
			return "", errModelProtocol
		}
		switch dimensions[i] {
		case "month":
			if !presentationMonthKey(key.Month) {
				return "", errModelProtocol
			}
			names = append(names, key.Month)
		case "category":
			if key.Category == nil || key.Category.ID == "" || key.Category.Name == "" {
				return "", errModelProtocol
			}
			names = append(names, key.Category.Name)
		case "merchant":
			if key.Merchant == nil || key.Merchant.Title == "" || key.Merchant.Selection == "" {
				return "", errModelProtocol
			}
			names = append(names, key.Merchant.Title)
		default:
			return "", errModelProtocol
		}
	}
	return strings.Join(names, " · "), nil
}
func presentOperationRows(task domain.GenerationTask, ref ToolResultReference) (presentationResult, error) {
	if ref.Result.Evidence.NoData {
		out := presentationText("Нет данных за выбранный период")
		out.Incomplete = true
		return out, nil
	}
	args, ok := ref.Args.(tooldata.FindArgs)
	if !ok {
		return presentationResult{}, errModelProtocol
	}
	var r struct {
		Items []struct {
			Title    string      `json:"title"`
			Kind     string      `json:"kind"`
			Amount   json.Number `json:"amount_minor"`
			Currency string      `json:"currency"`
			At       time.Time   `json:"occurred_at"`
		} `json:"items"`
		Count json.Number        `json:"total_count"`
		Range *presentationRange `json:"range"`
		Zone  string             `json:"time_zone"`
	}
	if err := presentationDecode(ref.Result.JSON, &r); err != nil {
		return presentationResult{}, err
	}
	count, err := presentationInt(r.Count)
	if err != nil || count < int64(len(r.Items)) {
		return presentationResult{}, errModelProtocol
	}
	zone, err := presentationZone(r.Zone)
	if err != nil {
		return presentationResult{}, err
	}
	rows := []presentationRow{presentationIntegerRow("Всего совпадений", count, "count")}
	for _, item := range r.Items {
		if item.Title == "" || item.At.IsZero() {
			return presentationResult{}, errModelProtocol
		}
		label := ""
		switch item.Kind {
		case "OPERATION_KIND_EXPENSE":
			label = "Расход: "
		case "OPERATION_KIND_INCOME":
			label = "Доход: "
		case "OPERATION_KIND_OWN_TRANSFER":
			label = "Перевод: "
		default:
			return presentationResult{}, errModelProtocol
		}
		amount, err := presentationInt(item.Amount)
		if err != nil || amount < 0 {
			return presentationResult{}, errModelProtocol
		}
		row := presentationIntegerRow(label+item.Title, amount, "amount")
		row.Sublabel = item.At.In(zone).Format("02.01.2006")
		if item.Currency != "RUB" {
			text, err := presentationForeignAmount(amount, item.Currency)
			if err != nil {
				return presentationResult{}, err
			}
			row.Value = nil
			row.Kind = ""
			row.Text = text
		}
		rows = append(rows, row)
	}
	if count > int64(len(r.Items)) {
		rows = append(rows, presentationRow{Label: "Выборка", Text: "Показана только часть операций"})
	}
	block, err := presentationRows(rows)
	if err != nil {
		return presentationResult{}, err
	}
	out := presentationResult{Blocks: []dialogue.Block{block}, Incomplete: ref.Result.Evidence.NoData}
	filter, err := presentationOriginalFilter(task, args.Filter, r.Range)
	if err != nil {
		return out, err
	}
	chip, err := presentationNativeChip(task, "Операции по выборке", filter)
	if err != nil {
		return out, err
	}
	if chip != nil {
		out.Chips = []json.RawMessage{chip}
	}
	return out, nil
}
func presentationZone(name string) (*time.Location, error) {
	if name == "" || name == "Local" {
		return nil, errModelProtocol
	}
	zone, err := time.LoadLocation(name)
	if err != nil {
		return nil, errModelProtocol
	}
	return zone, nil
}
func presentationMonthKey(key string) bool {
	at, err := time.Parse("2006-01", key)
	return err == nil && at.Year() > 0 && at.Format("2006-01") == key
}
func presentationOriginalFilter(task domain.GenerationTask, f tooldata.Filter, r *presentationRange) (*presentationFilter, error) {
	out := &presentationFilter{From: f.From, To: f.To, Q: f.Query, CategoryID: f.CategoryID, Kind: f.Kind, Selection: f.Selection, Mode: task.TransferMode}
	if f.AmountFrom != nil {
		n := json.Number(strconv.FormatInt(*f.AmountFrom, 10))
		out.AmountFrom = &n
	}
	if f.AmountTo != nil {
		n := json.Number(strconv.FormatInt(*f.AmountTo, 10))
		out.AmountTo = &n
	}
	if r != nil {
		if !r.Start.Before(r.End) {
			return nil, errModelProtocol
		}
		out.From = &r.Start
		out.To = &r.End
	}
	return out, nil
}
func presentationNativeChip(task domain.GenerationTask, label string, f *presentationFilter) (json.RawMessage, error) {
	if f == nil || label == "" {
		return nil, errModelProtocol
	}
	mode := f.Mode
	switch mode {
	case "TRANSFER_MODE_WITH":
		mode = "with"
	case "TRANSFER_MODE_WITHOUT":
		mode = "without"
	}
	if mode != task.TransferMode {
		return nil, errModelProtocol
	}
	kind := f.Kind
	switch kind {
	case "KIND_FILTER_UNSPECIFIED":
		kind = ""
	case "KIND_FILTER_EXPENSE":
		kind = "expense"
	case "KIND_FILTER_INCOME":
		kind = "income"
	}
	if kind != "" && kind != "expense" && kind != "income" {
		return nil, errModelProtocol
	}
	filters := map[string]any{"transfer_mode": task.TransferMode}
	for name, value := range map[string]string{"q": f.Q, "category_id": f.CategoryID, "kind": kind, "selection": f.Selection, "selection_name": f.SelectionName} {
		if value != "" {
			filters[name] = value
		}
	}
	if f.From != nil {
		if f.From.IsZero() {
			return nil, errModelProtocol
		}
		filters["from"] = f.From
	}
	if f.To != nil {
		if f.To.IsZero() {
			return nil, errModelProtocol
		}
		filters["to"] = f.To
	}
	if f.From != nil && f.To != nil && !f.From.Before(*f.To) {
		return nil, errModelProtocol
	}
	var lower, upper *int64
	for name, n := range map[string]*json.Number{"amount_from": f.AmountFrom, "amount_to": f.AmountTo} {
		if n != nil {
			v, err := presentationInt(*n)
			if err != nil || v < 0 {
				return nil, errModelProtocol
			}
			filters[name] = v
			if name == "amount_from" {
				lower = &v
			} else {
				upper = &v
			}
		}
	}
	if lower != nil && upper != nil && *lower > *upper {
		return nil, errModelProtocol
	}
	if len(filters) == 1 || (len(filters) == 2 && f.SelectionName != "") {
		return nil, nil
	}
	raw, err := json.Marshal(map[string]any{"label": label, "screen": "operations", "filters": filters})
	if err != nil {
		return nil, errModelProtocol
	}
	return raw, nil
}

func presentAggregateChart(task domain.GenerationTask, args presentationArgs, ref ToolResultReference, refs map[string]ToolResultReference) (presentationResult, error) {
	original, ok := ref.Args.(tooldata.AggregateArgs)
	if !ok || !slices.Equal(original.GroupBy, []string{"month"}) {
		return presentationResult{}, errModelProtocol
	}
	var r presentationAggregate
	if err := presentationDecode(ref.Result.JSON, &r); err != nil {
		return presentationResult{}, err
	}
	coverage, ok := refs[args.CoverageResultID]
	if !ok || coverage.CallID != args.CoverageResultID || coverage.Name != "get_data_overview" {
		return presentationResult{}, errModelProtocol
	}
	a, b := ref.Result.Evidence.Source, coverage.Result.Evidence.Source
	if a == nil || b == nil || a.TimeZone != b.TimeZone || a.TimeZone != r.Zone || !presentationSameVersion(a.Version, b.Version) {
		return presentationResult{}, errModelProtocol
	}
	var overview struct {
		Zone   string `json:"time_zone"`
		Months []struct {
			Month  string             `json:"month"`
			Range  presentationRange  `json:"range"`
			Bounds presentationBounds `json:"bounds"`
		} `json:"months"`
	}
	if err := presentationDecode(coverage.Result.JSON, &overview); err != nil || overview.Zone != r.Zone {
		return presentationResult{}, errModelProtocol
	}
	zone, err := presentationZone(r.Zone)
	if err != nil || r.Range == nil || !presentationFullMonthRange(*r.Range, zone) {
		return presentationResult{}, errModelProtocol
	}
	months := map[string]struct {
		rangeValue presentationRange
		bounds     presentationBounds
	}{}
	for _, month := range overview.Months {
		if _, duplicate := months[month.Month]; duplicate || !presentationMonthKey(month.Month) || !presentationFullMonthRange(month.Range, zone) || month.Range.Start.In(zone).Format("2006-01") != month.Month || !month.Range.End.Equal(month.Range.Start.In(zone).AddDate(0, 1, 0)) {
			return presentationResult{}, errModelProtocol
		}
		months[month.Month] = struct {
			rangeValue presentationRange
			bounds     presentationBounds
		}{month.Range, month.Bounds}
	}
	var points []presentationPoint
	seen := map[string]bool{}
	for _, group := range r.Groups {
		label, err := presentationGroupLabel(group, original.GroupBy)
		if err != nil || seen[label] {
			return presentationResult{}, errModelProtocol
		}
		seen[label] = true
		month, exists := months[label]
		if !exists || month.rangeValue.Start.Before(r.Range.Start) || month.rangeValue.End.After(r.Range.End) {
			return presentationResult{}, errModelProtocol
		}
		value, err := presentationInt(group.Amount)
		if err != nil {
			return presentationResult{}, err
		}
		if value > presentationExactInteger || value < -presentationExactInteger {
			return presentAggregateRows(task, ref)
		}
		point := presentationPoint{Value: &value, DataFrom: month.bounds.DataFrom, DataTo: month.bounds.DataTo}
		point.Range.From = month.rangeValue.Start
		point.Range.To = month.rangeValue.End
		switch month.bounds.Coverage {
		case "COVERAGE_COMPLETE":
			point.Coverage = "complete"
		case "COVERAGE_PARTIAL":
			point.Coverage = "partial"
		case "COVERAGE_NO_DATA":
			return presentationResult{}, errModelProtocol
		default:
			return presentationResult{}, errModelProtocol
		}
		if !presentationValidBounds(month.bounds, month.rangeValue) {
			return presentationResult{}, errModelProtocol
		}
		points = append(points, point)
	}
	sort.Slice(points, func(i, j int) bool { return points[i].Range.From.Before(points[j].Range.From) })
	if len(points) == 0 {
		return presentAggregateRows(task, ref)
	}
	title := "Суммы по месяцам"
	var alt []string
	var limitations []string
	for _, point := range points {
		key := point.Range.From.In(zone).Format("2006-01")
		text := key + ": " + presentationDisplay(*point.Value, "amount")
		if point.Coverage == "partial" {
			text += "; данные частично, с " + point.DataFrom.In(zone).Format("02.01.2006 15:04:05") + " по " + point.DataTo.In(zone).Format("02.01.2006 15:04:05")
		}
		for _, gap := range months[key].bounds.Gaps {
			gapText := "нет данных с " + gap.Start.In(zone).Format("02.01.2006 15:04:05") + " до " + gap.End.In(zone).Format("02.01.2006 15:04:05")
			text += "; " + gapText
			limitations = append(limitations, key+": "+gapText)
		}
		alt = append(alt, text)
	}
	if r.Truncated || ref.Result.Evidence.Truncated {
		alt = append(alt, "Показана только часть месяцев")
		limitations = append(limitations, "Показана только часть месяцев")
	}
	block, err := presentationBlock("chart", title+". "+strings.Join(alt, ". "), map[string]any{"kind": "bar", "value_kind": "amount", "points": points})
	if err != nil {
		return presentationResult{}, err
	}
	var envelope map[string]any
	if json.Unmarshal(block.Body, &envelope) != nil {
		return presentationResult{}, errModelProtocol
	}
	envelope["title"] = title
	block.Body, _ = json.Marshal(envelope)
	out := presentationResult{Blocks: []dialogue.Block{block}, Incomplete: ref.Result.Evidence.NoData}
	if len(limitations) > 0 {
		out.Blocks = append(out.Blocks, presentationText(strings.Join(limitations, ". ")).Blocks...)
	}
	for _, group := range r.Groups {
		label, _ := presentationGroupLabel(group, original.GroupBy)
		chip, err := presentationNativeChip(task, label, group.Filters)
		if err != nil {
			return out, err
		}
		if chip != nil {
			out.Chips = append(out.Chips, chip)
		}
	}
	return out, nil
}
func presentationSameVersion(a, b tooldata.Version) bool {
	return presentationSameTime(a.CategoriesChangedAt, b.CategoriesChangedAt) && presentationSameTime(a.UploadsChangedAt, b.UploadsChangedAt)
}
func presentationSameTime(a, b *time.Time) bool {
	return a == nil && b == nil || a != nil && b != nil && a.Equal(*b)
}
func presentationFullMonthRange(r presentationRange, zone *time.Location) bool {
	a, b := r.Start.In(zone), r.End.In(zone)
	return a.Year() > 0 && a.Year() < 10000 && b.Year() > 0 && b.Year() < 10000 && a.Before(b) && a.Day() == 1 && b.Day() == 1 && a.Hour() == 0 && a.Minute() == 0 && a.Second() == 0 && a.Nanosecond() == 0 && b.Hour() == 0 && b.Minute() == 0 && b.Second() == 0 && b.Nanosecond() == 0
}
func presentationValidBounds(b presentationBounds, r presentationRange) bool {
	if b.DataFrom == nil || b.DataTo == nil || b.DataFrom.Before(r.Start) || !b.DataFrom.Before(r.End) || b.DataTo.Before(*b.DataFrom) || !b.DataTo.Before(r.End) {
		return false
	}
	for _, gap := range b.Gaps {
		if !gap.Start.Before(gap.End) || gap.Start.Before(r.Start) || gap.End.After(r.End) {
			return false
		}
	}
	return true
}

var presentationPath = regexp.MustCompile(`^blocks\[([0-9]+)\](?:\.data\.tiles\[([0-9]+)\])?$`)

type presentationCatalogBlock struct {
	ID     string `json:"id"`
	Type   string `json:"type"`
	V      int32  `json:"v"`
	Status string `json:"status"`
	Title  string `json:"title"`
	Alt    string `json:"alt_text"`
	Empty  string `json:"empty_text"`
	Lock   *struct {
		Text string `json:"text"`
	} `json:"lock"`
	Data   json.RawMessage `json:"data"`
	Action json.RawMessage `json:"action"`
}

func presentAnalytics(task domain.GenerationTask, args presentationArgs, ref ToolResultReference) (presentationResult, error) {
	if _, ok := ref.Args.(tooldata.AnalyticsArgs); !ok {
		return presentationResult{}, errModelProtocol
	}
	matches := presentationPath.FindStringSubmatch(args.Path)
	if matches == nil {
		return presentationResult{}, errModelProtocol
	}
	index, err := strconv.Atoi(matches[1])
	if err != nil {
		return presentationResult{}, errModelProtocol
	}
	var r struct {
		HasData  bool   `json:"has_data"`
		Currency string `json:"currency"`
		Params   struct {
			Period string `json:"period"`
			Key    string `json:"period_key"`
			Mode   string `json:"transfer_mode"`
		} `json:"params"`
		Blocks []presentationCatalogBlock `json:"blocks"`
	}
	if err := presentationDecode(ref.Result.JSON, &r); err != nil || !r.HasData || index >= len(r.Blocks) || index < 0 {
		return presentationResult{}, errModelProtocol
	}
	block := r.Blocks[index]
	selectedLocked := slices.Contains(ref.Result.Evidence.LockedPaths, args.Path)
	var result presentationResult
	var selectedTile json.RawMessage
	if matches[2] != "" {
		if block.Type != "tile_group" || block.V != 1 {
			return result, errModelProtocol
		}
		var tiles struct {
			Tiles []json.RawMessage `json:"tiles"`
		}
		if json.Unmarshal(block.Data, &tiles) != nil {
			return result, errModelProtocol
		}
		tileIndex, err := strconv.Atoi(matches[2])
		if err != nil || tileIndex < 0 || tileIndex >= len(tiles.Tiles) {
			return result, errModelProtocol
		}
		selectedTile = tiles.Tiles[tileIndex]
	}
	if matches[2] != "" && (block.Status == "locked" || block.Status == "none" || block.Status == "unavailable") {
		parentLocked := slices.Contains(ref.Result.Evidence.LockedPaths, "blocks["+strconv.Itoa(index)+"]")
		if (block.Status == "locked") != parentLocked {
			return result, errModelProtocol
		}
		if parentLocked {
			result = presentationText(presentationLockText(block.Lock))
			result.Incomplete = true
		} else if block.Empty != "" {
			result = presentationText(block.Empty)
		} else {
			result = presentationText("Показатель пока недоступен")
		}
	} else if matches[2] != "" {
		var tile struct {
			Title  string `json:"title"`
			Status string `json:"status"`
			Lock   *struct {
				Text string `json:"text"`
			} `json:"lock"`
			Value   *json.Number    `json:"value"`
			Kind    string          `json:"value_kind"`
			Caption string          `json:"caption"`
			Approx  bool            `json:"approx"`
			Plus    bool            `json:"show_plus"`
			Action  json.RawMessage `json:"action"`
		}
		if json.Unmarshal(selectedTile, &tile) != nil {
			return result, errModelProtocol
		}
		if (tile.Status == "locked") != selectedLocked {
			return result, errModelProtocol
		}
		if selectedLocked {
			result = presentationText(presentationLockText(tile.Lock))
			result.Incomplete = true
		} else if tile.Status == "none" || tile.Status == "unavailable" {
			result = presentationText("Показатель пока недоступен")
		} else if tile.Status == "" || tile.Status == "ready" || tile.Status == "tentative" {
			row, err := presentationCatalogRow(tile.Title, tile.Caption, tile.Value, tile.Kind, "", r.Currency)
			if err != nil {
				return result, err
			}
			if tile.Status == "tentative" {
				row.Label += " (предварительно)"
			}
			if tile.Approx || tile.Plus {
				text := row.Text
				if text == "" {
					switch value := row.Value.(type) {
					case int64:
						text = presentationDisplay(value, row.Kind)
					case float64:
						text = strconv.FormatFloat(value, 'g', -1, 64) + " (доля)"
					}
				}
				if tile.Plus && tile.Value != nil {
					value, err := tile.Value.Float64()
					if err == nil && value > 0 {
						text = "+" + text
					}
				}
				if tile.Approx {
					text = "≈" + text
				}
				row.Text, row.Value, row.Kind = text, nil, ""
			}
			rows, err := presentationRows([]presentationRow{row})
			if err != nil {
				return result, err
			}
			result.Blocks = []dialogue.Block{rows}
			if chip := presentationActionChip(task, tile.Title, tile.Action, false); chip != nil {
				result.Chips = append(result.Chips, chip)
			}
		} else {
			return result, errModelProtocol
		}
	} else {
		if (block.Status == "locked") != selectedLocked {
			return result, errModelProtocol
		}
		if selectedLocked {
			result = presentationText(presentationLockText(block.Lock))
			result.Incomplete = true
		} else if block.Status == "none" || block.Status == "unavailable" {
			text := block.Empty
			if text == "" {
				text = "Показатель пока недоступен"
			}
			result = presentationText(text)
		} else if block.Status == "" || block.Status == "ready" || block.Status == "tentative" {
			result, err = presentCatalogBlock(block, r.Currency)
			if err != nil {
				return result, err
			}
			if chip := presentationActionChip(task, block.Title, block.Action, true); chip != nil {
				result.Chips = append(result.Chips, chip)
			}
			if block.Type == "rows" && block.V == 1 {
				var data struct {
					Rows []struct {
						Label  string          `json:"label"`
						Action json.RawMessage `json:"action"`
					} `json:"rows"`
				}
				if json.Unmarshal(block.Data, &data) != nil {
					return result, errModelProtocol
				}
				for _, row := range data.Rows {
					if chip := presentationActionChip(task, row.Label, row.Action, false); chip != nil {
						result.Chips = append(result.Chips, chip)
					}
				}
			}
		} else {
			return result, errModelProtocol
		}
	}
	chip, err := presentationAnalyticsChip(task, r.Params.Period, r.Params.Key, r.Params.Mode)
	if err != nil {
		return result, err
	}
	result.Chips = append(result.Chips, chip)
	result.Chips = presentationUniqueChips(result.Chips)
	return result, nil
}

// Native block actions use OperationsFilter protobuf names; actions inside Struct
// data retain the REST filter shape. Never infer either shape from model content.
func presentationActionChip(task domain.GenerationTask, label string, raw json.RawMessage, native bool) json.RawMessage {
	var action struct {
		Type   string          `json:"type"`
		Filter json.RawMessage `json:"filter"`
	}
	if len(raw) == 0 || json.Unmarshal(raw, &action) != nil || action.Type != "open_operations" || !validObject(action.Filter) {
		return nil
	}
	var filter presentationFilter
	if json.Unmarshal(action.Filter, &filter) != nil {
		return nil
	}
	if !native {
		var amounts struct {
			From *json.Number `json:"amount_from"`
			To   *json.Number `json:"amount_to"`
		}
		if json.Unmarshal(action.Filter, &amounts) != nil {
			return nil
		}
		filter.AmountFrom, filter.AmountTo = amounts.From, amounts.To
	}
	if label == "" {
		label = "Операции по выборке"
	}
	chip, err := presentationNativeChip(task, label, &filter)
	if err != nil {
		return nil
	}
	return chip
}

func presentationUniqueChips(chips []json.RawMessage) []json.RawMessage {
	var result []json.RawMessage
	seen := map[string]bool{}
	for _, raw := range chips {
		var identity map[string]json.RawMessage
		if json.Unmarshal(raw, &identity) != nil {
			continue
		}
		delete(identity, "label")
		key, err := json.Marshal(identity)
		if err == nil && !seen[string(key)] {
			seen[string(key)] = true
			result = append(result, raw)
		}
	}
	return result
}
func presentationLockText(lock *struct {
	Text string `json:"text"`
}) string {
	if lock == nil || lock.Text == "" {
		return "Пока недоступно"
	}
	return lock.Text
}
func presentationAnalyticsChip(task domain.GenerationTask, period, key, mode string) (json.RawMessage, error) {
	switch mode {
	case "TRANSFER_MODE_WITH":
		mode = "with"
	case "TRANSFER_MODE_WITHOUT":
		mode = "without"
	}
	if mode != task.TransferMode {
		return nil, errModelProtocol
	}
	switch period {
	case "PERIOD_TYPE_MONTH":
		period = "month"
	case "PERIOD_TYPE_QUARTER":
		period = "quarter"
	case "PERIOD_TYPE_YEAR":
		period = "year"
	default:
		return nil, errModelProtocol
	}
	valid := false
	switch period {
	case "month":
		valid = presentationMonthKey(key)
	case "quarter":
		if len(key) == 7 && key[4:6] == "-Q" && key[6] >= '1' && key[6] <= '4' {
			year, err := time.Parse("2006", key[:4])
			valid = err == nil && year.Year() > 0
		}
	case "year":
		year, err := time.Parse("2006", key)
		valid = err == nil && year.Year() > 0 && len(key) == 4
	}
	if !valid {
		return nil, errModelProtocol
	}
	raw, err := json.Marshal(map[string]any{"label": "Аналитика за " + key, "screen": "analytics", "params": map[string]string{"period": period, "date": key, "transfer_mode": task.TransferMode}})
	if err != nil {
		return nil, errModelProtocol
	}
	return raw, nil
}
func presentCatalogBlock(block presentationCatalogBlock, currency string) (presentationResult, error) {
	if block.Type == "text" && block.V == 1 {
		var text struct {
			Text string `json:"text"`
		}
		if json.Unmarshal(block.Data, &text) != nil || text.Text == "" {
			return presentationResult{}, errModelProtocol
		}
		return presentationText(text.Text), nil
	}
	if block.V != 1 || (block.Type != "chart" && block.Type != "rows") {
		if block.Alt == "" {
			return presentationResult{}, errModelProtocol
		}
		return presentationText(block.Alt), nil
	}
	if block.Type == "rows" {
		var data struct {
			Rows []struct {
				Label    string       `json:"label"`
				Sublabel string       `json:"sublabel"`
				Value    *json.Number `json:"value"`
				Kind     string       `json:"value_kind"`
				Text     string       `json:"text"`
			} `json:"rows"`
		}
		if json.Unmarshal(block.Data, &data) != nil {
			return presentationResult{}, errModelProtocol
		}
		var rows []presentationRow
		for _, item := range data.Rows {
			row, err := presentationCatalogRow(item.Label, item.Sublabel, item.Value, item.Kind, item.Text, currency)
			if err != nil {
				return presentationResult{}, err
			}
			rows = append(rows, row)
		}
		body, err := presentationRows(rows)
		return presentationResult{Blocks: []dialogue.Block{body}}, err
	}
	var chart struct {
		Kind       string            `json:"kind"`
		ValueKind  string            `json:"value_kind"`
		Points     []json.RawMessage `json:"points"`
		Summary    json.RawMessage   `json:"summary"`
		Caption    string            `json:"caption"`
		Notes      []json.RawMessage `json:"notes"`
		ShowValues *bool             `json:"show_values"`
	}
	if json.Unmarshal(block.Data, &chart) != nil {
		return presentationResult{}, errModelProtocol
	}
	if chart.Kind != "bar" || (chart.ValueKind != "amount" && chart.ValueKind != "percent") || len(chart.Points) == 0 || chart.Summary != nil || chart.Caption != "" || len(chart.Notes) > 0 || chart.ShowValues != nil || (chart.ValueKind == "amount" && currency != "RUB") {
		if block.Alt == "" {
			return presentationResult{}, errModelProtocol
		}
		return presentationText(block.Alt), nil
	}
	var previousEnd time.Time
	var header struct {
		Highlight *int32 `json:"highlight_index"`
	}
	if json.Unmarshal(block.Data, &header) != nil || header.Highlight != nil && (*header.Highlight < 0 || int(*header.Highlight) >= len(chart.Points)) {
		return presentationResult{}, errModelProtocol
	}
	for _, raw := range chart.Points {
		var point struct {
			Value *json.Number `json:"value"`
			Note  string       `json:"note"`
			Range struct {
				From time.Time `json:"from"`
				To   time.Time `json:"to"`
			} `json:"range"`
			Coverage string     `json:"coverage"`
			From     *time.Time `json:"data_from"`
			To       *time.Time `json:"data_to"`
		}
		if json.Unmarshal(raw, &point) != nil {
			return presentationResult{}, errModelProtocol
		}
		if point.Range.From.IsZero() || !point.Range.From.Before(point.Range.To) || point.Range.From.Year() < 1 || point.Range.To.Year() > 9999 || !previousEnd.IsZero() && point.Range.From.Before(previousEnd) {
			return presentationResult{}, errModelProtocol
		}
		previousEnd = point.Range.To
		switch point.Coverage {
		case "complete", "partial":
		case "no_data":
			if point.Value != nil {
				return presentationResult{}, errModelProtocol
			}
		default:
			return presentationResult{}, errModelProtocol
		}
		for _, at := range []*time.Time{point.From, point.To} {
			if at != nil && (at.Before(point.Range.From) || !at.Before(point.Range.To)) {
				return presentationResult{}, errModelProtocol
			}
		}
		if point.From != nil && point.To != nil && point.To.Before(*point.From) {
			return presentationResult{}, errModelProtocol
		}
		if point.Note != "" {
			if block.Alt == "" {
				return presentationResult{}, errModelProtocol
			}
			return presentationText(block.Alt), nil
		}
		if point.Value != nil {
			if chart.ValueKind == "amount" {
				v, err := presentationInt(*point.Value)
				if err != nil {
					return presentationResult{}, err
				}
				if v > presentationExactInteger || v < -presentationExactInteger {
					if block.Alt == "" {
						return presentationResult{}, errModelProtocol
					}
					return presentationText(block.Alt), nil
				}
			} else {
				v, err := point.Value.Float64()
				if err != nil || math.IsNaN(v) || math.IsInf(v, 0) || v < 0 || v > 1 {
					return presentationResult{}, errModelProtocol
				}
			}
		}
	}
	if block.Alt == "" {
		return presentationResult{}, errModelProtocol
	}
	raw, err := json.Marshal(map[string]any{"type": "chart", "v": 1, "title": block.Title, "alt_text": block.Alt, "data": block.Data})
	if err != nil {
		return presentationResult{}, errModelProtocol
	}
	return presentationResult{Blocks: []dialogue.Block{{Type: "chart", Version: 1, Body: raw}}}, nil
}
func presentationCatalogRow(label, sublabel string, value *json.Number, kind, text, currency string) (presentationRow, error) {
	if label == "" {
		return presentationRow{}, errModelProtocol
	}
	row := presentationRow{Label: label, Sublabel: sublabel}
	if value == nil {
		if text == "" {
			return row, errModelProtocol
		}
		row.Text = text
		return row, nil
	}
	switch kind {
	case "amount", "count":
		v, err := presentationInt(*value)
		if err != nil || kind == "count" && v < 0 {
			return row, errModelProtocol
		}
		row = presentationIntegerRow(label, v, kind)
		row.Sublabel = sublabel
		if kind == "amount" && currency != "RUB" {
			row.Value = nil
			row.Kind = ""
			row.Text = strings.TrimSuffix(presentationDisplay(v, "amount"), "₽") + currency
		}
	case "percent":
		v, err := value.Float64()
		if err != nil || math.IsInf(v, 0) || math.IsNaN(v) {
			return row, errModelProtocol
		}
		if v < 0 || v > 1 {
			row.Text = value.String()
		} else {
			row.Value = v
			row.Kind = "percent"
		}
	default:
		if text == "" {
			return row, errModelProtocol
		}
		row.Text = text
	}
	return row, nil
}
