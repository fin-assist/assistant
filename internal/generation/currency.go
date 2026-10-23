package generation

import (
	"strconv"
	"strings"

	"golang.org/x/text/currency"
)

// presentationForeignAmount uses currency metadata only for the decimal scale.
// Decimal insertion stays on integer strings, including values above 2^53.
func presentationForeignAmount(amount int64, code string) (string, error) {
	if amount < 0 || len(code) != 3 {
		return "", errModelProtocol
	}
	for _, letter := range code {
		if letter < 'A' || letter > 'Z' {
			return "", errModelProtocol
		}
	}
	scale, known := presentationCurrencyScale(code)
	// Unknown codes and special X units may have no ISO minor-unit scale.
	// Preserve their exact raw units instead of inventing two decimal places.
	if !known {
		return strconv.FormatInt(amount, 10) + " минимальных единиц " + code, nil
	}
	text := strconv.FormatInt(amount, 10)
	if scale > 0 {
		if len(text) <= scale {
			text = strings.Repeat("0", scale-len(text)+1) + text
		}
		text = text[:len(text)-scale] + "," + text[len(text)-scale:]
	}
	return text + " " + code, nil
}

func presentationCurrencyScale(code string) (int, bool) {
	unit, err := currency.ParseISO(code)
	if err != nil || strings.HasPrefix(code, "X") {
		return 0, false
	}
	scale, _ := currency.Standard.Rounding(unit)
	return scale, true
}
