package tooldata

import (
	"encoding/json"
	"time"
)

// NavigationFilter contains normalized operation navigation supplied by a peer.
// Mode is with/without; Kind is empty, expense or income. Amounts are minor units.
type NavigationFilter struct {
	From, To                                            *time.Time
	Q, CategoryID, Kind, Mode, Selection, SelectionName string
	AmountFrom, AmountTo                                *int64
}

// Bounds describes observed coverage, independently of protobuf enum names.
type Bounds struct {
	Coverage         string
	DataFrom, DataTo *time.Time
	Gaps             []Range
}

// Overview contains the calendar facts used to render aggregate charts.
type Overview struct {
	Zone   string
	Months []Month
}

// Month preserves a peer's calendar interval and actual data coverage.
type Month struct {
	Month  string
	Range  Range
	Bounds Bounds
}

// GroupKey identifies one aggregation dimension.
type GroupKey struct {
	Month    string
	Category *CategoryKey
	Merchant *MerchantKey
}

// CategoryKey identifies a category used in an aggregate.
type CategoryKey struct{ ID, Name string }

// MerchantKey preserves the peer's display name and opaque selection.
type MerchantKey struct{ Title, Selection string }

// Group contains exact aggregate values and trusted navigation.
type Group struct {
	Keys                   []GroupKey
	Amount, Count, Average int64
	Filters                *NavigationFilter
}

// Aggregate contains the financial facts used by rows and charts.
type Aggregate struct {
	Groups        []Group
	Amount, Count int64
	Range         *Range
	Zone          string
	Truncated     bool
}

// Operation contains the bounded facts needed to present one operation.
// Kind is expense, income or own_transfer; Amount is nonnegative minor units.
type Operation struct {
	Title, Kind, Currency string
	Amount                int64
	At                    time.Time
}

// Operations preserves the total match count independently of the bounded page.
type Operations struct {
	Items []Operation
	Count int64
	Range *Range
	Zone  string
}

// AnalyticsParams contains normalized calendar and transfer-mode navigation.
type AnalyticsParams struct{ Period, Key, Mode string }

// CatalogBlock preserves the versioned, transport-independent block catalog.
// Data uses the catalog JSON schema; Action is normalized operation navigation.
type CatalogBlock struct {
	ID, Type                  string
	V                         int32
	Status, Title, Alt, Empty string
	Lock                      *struct {
		Text string `json:"text"`
	}
	Data   json.RawMessage
	Action *NavigationFilter
}

// Analytics contains trusted analytics facts separately from the model payload.
type Analytics struct {
	HasData  bool
	Currency string
	Params   AnalyticsParams
	Blocks   []CatalogBlock
}
