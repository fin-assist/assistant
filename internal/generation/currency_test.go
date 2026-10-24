package generation

import "testing"

func TestUnitPresentationForeignAmountUsesISODecimalsWithoutFloat(t *testing.T) {
	for _, test := range []struct {
		code   string
		amount int64
		want   string
	}{
		{"USD", 12345, "123,45 USD"},
		{"JPY", 12345, "12345 JPY"},
		{"KWD", 12345, "12,345 KWD"},
		{"KWD", 1, "0,001 KWD"},
		{"JPY", 0, "0 JPY"},
		{"USD", 0, "0,00 USD"},
		{"CLF", 12345, "1,2345 CLF"},
		{"KWD", 9223372036854775807, "9223372036854775,807 KWD"},
		{"ZZZ", 12345, "12345 минимальных единиц ZZZ"},
		{"XAU", 12345, "12345 минимальных единиц XAU"},
		{"XXX", 0, "0 минимальных единиц XXX"},
	} {
		t.Run(test.code+"/"+test.want, func(t *testing.T) {
			got, err := presentationForeignAmount(test.amount, test.code)
			if err != nil || got != test.want {
				t.Fatalf("amount %d %s: %q %v, want %q", test.amount, test.code, got, err, test.want)
			}
		})
	}
	for _, code := range []string{"", "EURO", "usd", "US1", " Я"} {
		if _, err := presentationForeignAmount(1, code); err == nil {
			t.Fatalf("invalid currency accepted: %q", code)
		}
	}
	if _, err := presentationForeignAmount(-1, "USD"); err == nil {
		t.Fatal("negative operation amount accepted")
	}
}
