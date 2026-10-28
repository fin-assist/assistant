package client

import (
	"fmt"
	"math"

	commonv1 "github.com/fin-assist/contracts/gen/go/finassist/common/v1"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/fin-assist/assistant/internal/tooldata"
)

func financialStatus(status string) bool {
	switch status {
	case "", "ready", "tentative", "locked", "none", "unavailable":
		return true
	}
	return false
}

func financialBlock(block *commonv1.Block, path string) ([]string, error) {
	// common.Block.id is optional, including the analytics section heading.
	// Validate its primitive and financial evidence without inventing an ID.
	if block == nil || block.Type == "" || block.V < 1 || !financialStatus(block.Status) {
		return nil, tooldata.ErrInvalidResponse
	}
	var locked []string
	if block.Status == "locked" {
		if block.Lock == nil || block.Lock.Text == "" {
			return nil, tooldata.ErrInvalidResponse
		}
		locked = append(locked, path)
	}
	if block.Data != nil && !financialNumbers(block.Data, "") {
		return nil, tooldata.ErrInvalidResponse
	}
	if block.Type != "tile_group" {
		return locked, nil
	}
	// Version 1 explicitly carries metric states inside tiles. A newer schema
	// needs a reader update before its states can be treated as trusted facts.
	if block.V != 1 || block.Data == nil {
		return nil, tooldata.ErrInvalidResponse
	}
	value, ok := block.Data.Fields["tiles"]
	if !ok {
		return nil, tooldata.ErrInvalidResponse
	}
	tiles, ok := value.Kind.(*structpb.Value_ListValue)
	if !ok {
		return nil, tooldata.ErrInvalidResponse
	}
	for i, value := range tiles.ListValue.Values {
		tile, ok := value.Kind.(*structpb.Value_StructValue)
		if !ok || tile.StructValue == nil {
			return nil, tooldata.ErrInvalidResponse
		}
		fields := tile.StructValue.Fields
		id, ok := financialString(fields["id"])
		if !ok || id == "" {
			return nil, tooldata.ErrInvalidResponse
		}
		status := ""
		if value, exists := fields["status"]; exists {
			status, ok = financialString(value)
			if !ok {
				return nil, tooldata.ErrInvalidResponse
			}
		}
		if !financialStatus(status) {
			return nil, tooldata.ErrInvalidResponse
		}
		if status == "locked" {
			lock, exists := fields["lock"]
			if !exists {
				return nil, tooldata.ErrInvalidResponse
			}
			if _, ok := lock.Kind.(*structpb.Value_StructValue); !ok {
				return nil, tooldata.ErrInvalidResponse
			}
			locked = append(locked, fmt.Sprintf("%s.data.tiles[%d]", path, i))
		}
	}
	return locked, nil
}

func financialString(value *structpb.Value) (string, bool) {
	if value == nil {
		return "", false
	}
	text, ok := value.Kind.(*structpb.Value_StringValue)
	if !ok {
		return "", false
	}
	return text.StringValue, true
}

// Struct represents numbers as doubles. Preserve the catalog's explicit exact
// integer ceiling instead of passing rounded monetary evidence to the model.
func financialNumbers(object *structpb.Struct, inheritedKind string) bool {
	kind := inheritedKind
	if value, exists := object.Fields["value_kind"]; exists {
		var ok bool
		kind, ok = financialString(value)
		if !ok {
			return false
		}
	}
	for name, value := range object.Fields {
		if value == nil {
			return false
		}
		switch data := value.Kind.(type) {
		case *structpb.Value_NumberValue:
			if math.IsInf(data.NumberValue, 0) || math.IsNaN(data.NumberValue) {
				return false
			}
			if name == "value" {
				switch kind {
				case "amount", "count":
					if math.Trunc(data.NumberValue) != data.NumberValue || math.Abs(data.NumberValue) > 9007199254740991 {
						return false
					}
				case "percent":
				default:
					return false
				}
			}
		case *structpb.Value_StructValue:
			if data.StructValue == nil || !financialNumbers(data.StructValue, kind) {
				return false
			}
		case *structpb.Value_ListValue:
			if data.ListValue == nil {
				return false
			}
			for _, item := range data.ListValue.Values {
				if item == nil {
					return false
				}
				if nested, ok := item.Kind.(*structpb.Value_StructValue); ok && (nested.StructValue == nil || !financialNumbers(nested.StructValue, kind)) {
					return false
				}
			}
		}
	}
	return true
}
