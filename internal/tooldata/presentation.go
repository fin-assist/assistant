package tooldata

import (
	"encoding/json"
	"time"
)

// NavigationFilter contains normalized operation navigation supplied by a peer.
// Mode is with/without; Kind is empty, expense or income. Amounts are minor units.
type NavigationFilter struct {
	From          *time.Time `json:"from,omitempty"`
	To            *time.Time `json:"to,omitempty"`
	Q             string     `json:"q,omitempty"`
	CategoryID    string     `json:"category_id,omitempty"`
	Kind          string     `json:"kind,omitempty"`
	Mode          string     `json:"transfer_mode"`
	Selection     string     `json:"selection,omitempty"`
	SelectionName string     `json:"selection_name,omitempty"`
	AmountFrom    *int64     `json:"amount_from_minor,omitempty"`
	AmountTo      *int64     `json:"amount_to_minor,omitempty"`
}

// Bounds describes observed coverage, independently of protobuf enum names.
type Bounds struct {
	Coverage string     `json:"coverage"`
	DataFrom *time.Time `json:"data_from"`
	DataTo   *time.Time `json:"data_to"`
	Gaps     []Range    `json:"gaps"`
}

// Overview contains the calendar facts used to render aggregate charts.
type Overview struct {
	Zone   string  `json:"time_zone"`
	Months []Month `json:"months"`
}

// Month preserves a peer's calendar interval and actual data coverage.
type Month struct {
	Month  string `json:"month"`
	Range  Range  `json:"range"`
	Bounds Bounds `json:"bounds"`
}

// GroupKey identifies one aggregation dimension.
type GroupKey struct {
	Month    string       `json:"month"`
	Category *CategoryKey `json:"category"`
	Merchant *MerchantKey `json:"merchant"`
}

// CategoryKey identifies a category used in an aggregate.
type CategoryKey struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// MerchantKey preserves the peer's display name and opaque selection.
type MerchantKey struct {
	Title     string `json:"title"`
	Selection string `json:"selection"`
}

// Group contains exact aggregate values and trusted navigation.
type Group struct {
	Keys    []GroupKey        `json:"keys"`
	Amount  int64             `json:"amount_minor"`
	Count   int64             `json:"operation_count"`
	Average int64             `json:"average_minor"`
	Filters *NavigationFilter `json:"filters"`
}

// Aggregate contains the financial facts used by rows and charts.
type Aggregate struct {
	Groups    []Group `json:"groups"`
	Amount    int64   `json:"total_amount_minor"`
	Count     int64   `json:"total_operation_count"`
	Range     *Range  `json:"range"`
	Zone      string  `json:"time_zone"`
	Truncated bool    `json:"truncated"`
}

// Operation contains the bounded facts needed to present one operation.
// Kind is expense, income or own_transfer; Amount is nonnegative minor units.
type Operation struct {
	Title    string    `json:"title"`
	Kind     string    `json:"kind"`
	Currency string    `json:"currency"`
	Amount   int64     `json:"amount_minor"`
	At       time.Time `json:"occurred_at"`
}

// Operations preserves the total match count independently of the bounded page.
type Operations struct {
	Items []Operation `json:"items"`
	Count int64       `json:"total_count"`
	Range *Range      `json:"range"`
	Zone  string      `json:"time_zone"`
}

// AnalyticsParams contains normalized calendar and transfer-mode navigation.
type AnalyticsParams struct {
	Period string `json:"period"`
	Key    string `json:"period_key"`
	Mode   string `json:"transfer_mode"`
}

// CatalogBlock preserves the versioned, transport-independent block catalog.
// Data uses the catalog JSON schema; Action is normalized operation navigation.
type CatalogBlock struct {
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
	Data   json.RawMessage   `json:"data"`
	Action *NavigationFilter `json:"action"`
}

// Analytics contains trusted analytics facts separately from the model payload.
type Analytics struct {
	HasData  bool            `json:"has_data"`
	Currency string          `json:"currency"`
	Params   AnalyticsParams `json:"params"`
	Blocks   []CatalogBlock  `json:"blocks"`
}
