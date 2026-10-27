package datarevision

import (
	"errors"
	"reflect"
	"testing"
)

func known(value int64) Current { return Current{Categories: value, Uploads: value, TimeZone: value} }
func TestUnitRevisionExactSetsAndUncertainty(t *testing.T) {
	a, err := Merge(Baseline{}, known(0))
	if err != nil || a.Incomplete {
		t.Fatal(a, err)
	}
	if got := Changes(a, known(0)); len(got) != 0 {
		t.Fatal(got)
	}
	b, err := Merge(a, known(2))
	if err != nil {
		t.Fatal(err)
	}
	for _, now := range []int64{0, 1, 2} {
		if got := Changes(b, known(now)); !reflect.DeepEqual(got, []string{Categories, Uploads, TimeZone}) {
			t.Fatal(now, got)
		}
	}
	if !reflect.DeepEqual(a.Values[Uploads], []int64{0}) {
		t.Fatal("merge mutated older source", a)
	}
	duplicate, err := Merge(b, known(2))
	if err != nil || !reflect.DeepEqual(duplicate, b) {
		t.Fatal(duplicate, err)
	}
	missing := known(2)
	delete(missing, Uploads)
	partial, err := Merge(Baseline{}, missing)
	if err != nil || !partial.Incomplete {
		t.Fatal(partial, err)
	}
	repaired, err := Merge(partial, known(2))
	if err != nil || !repaired.Incomplete || !reflect.DeepEqual(Changes(repaired, known(2)), []string{Unverified}) {
		t.Fatal(repaired, err)
	}
	for _, current := range []Current{nil, {}, missing} {
		if got := Changes(a, current); len(got) == 0 || got[len(got)-1] != Unverified {
			t.Fatal(got)
		}
	}
	if got := Changes(Baseline{}, known(0)); !reflect.DeepEqual(got, []string{Unverified}) {
		t.Fatal(got)
	}
}
func TestUnitRevisionUnknownCodesAndValidation(t *testing.T) {
	base := known(3)
	base["future"] = 9223372036854775807
	b, err := Merge(Baseline{}, base)
	if err != nil {
		t.Fatal(err)
	}
	if len(Changes(b, base)) != 0 || !EqualKnown(base, base) {
		t.Fatal("exact opaque code not preserved")
	}
	changed := known(3)
	changed["future"] = 1
	if got := Changes(b, changed); !reflect.DeepEqual(got, []string{Unverified}) {
		t.Fatal(got)
	}
	if got := Changes(b, known(3)); !reflect.DeepEqual(got, []string{Unverified}) {
		t.Fatal(got)
	}
	if got := Changes(b, Current{Categories: 3, Uploads: 3, TimeZone: 3, "future": 9223372036854775807, "new": 0}); !reflect.DeepEqual(got, []string{Unverified}) {
		t.Fatal(got)
	}
	if EqualKnown(known(1), known(2)) || EqualKnown(known(1), nil) || EqualKnown(base, known(3)) {
		t.Fatal("incomparable revisions match")
	}
	for _, bad := range []Current{{"": 1}, {"UPPER": 1}, {"has space": 1}, {Uploads: -1}} {
		if ValidCurrent(bad) || EqualKnown(bad, bad) {
			t.Fatal(bad)
		}
		if _, err := Merge(Baseline{}, bad); !errors.Is(err, ErrInvalid) {
			t.Fatal(err)
		}
		if got := Changes(b, bad); !reflect.DeepEqual(got, []string{Unverified}) {
			t.Fatal(got)
		}
	}
	for _, bad := range []Baseline{{Values: map[string][]int64{"": {1}}}, {Values: map[string][]int64{Uploads: {}}}, {Values: map[string][]int64{Uploads: {-1}}}, {Values: map[string][]int64{Uploads: {1, 1}}}} {
		if ValidBaseline(bad) {
			t.Fatal(bad)
		}
		if _, err := Merge(bad, known(1)); !errors.Is(err, ErrInvalid) {
			t.Fatal(err)
		}
		if got := Changes(bad, known(1)); !reflect.DeepEqual(got, []string{Unverified}) {
			t.Fatal(got)
		}
	}
}

func TestUnitOpenRevisionPresenceMustMatchEveryUsedSource(t *testing.T) {
	for _, reverse := range []bool{false, true} {
		first := Current{Categories: 1, Uploads: 1, TimeZone: 1, "future": 1}
		second := Current{Categories: 1, Uploads: 1, TimeZone: 1}
		if reverse {
			first, second = second, first
		}
		b, err := Merge(Baseline{}, first)
		if err != nil {
			t.Fatal(err)
		}
		b, err = Merge(b, second)
		if err != nil {
			t.Fatal(err)
		}
		if !b.Incomplete {
			t.Fatal("missing open kind was certified", b)
		}
		got := Changes(b, Current{Categories: 1, Uploads: 1, TimeZone: 1, "future": 1})
		if len(got) != 1 || got[0] != Unverified {
			t.Fatal(got)
		}
	}
}

func TestUnitStoredRevisionDecodeRejectsLossyJSON(t *testing.T) {
	for _, raw := range []string{
		`{"values":{"categories":[null],"uploads":[null],"time_zone":[null]},"incomplete":false}`,
		`{"values":{"categories":[0],"uploads":[0],"time_zone":[0]},"incomplete":null}`,
		`{"values":{"categories":[0],"uploads":[0],"time_zone":[0]}}`,
		`{"values":{"uploads":[0.0]},"incomplete":false}`,
		`{"values":{"uploads":[false]},"incomplete":false}`,
		`{"values":{"uploads":["0"]},"incomplete":false}`,
		`{"values":{"uploads":[0,0]},"incomplete":false}`,
		`{"values":null,"incomplete":false}`,
		`{"VALUES":{"categories":[0],"uploads":[0],"time_zone":[0]},"incomplete":false}`,
		`{"values":{"categories":[0],"uploads":[0],"time_zone":[0]},"INCOMPLETE":false}`,
		`{"values":{"categories":[0],"uploads":[0],"time_zone":[0]},"incomplete":false,"unknown":0}`,
	} {
		if _, err := DecodeBaseline([]byte(raw)); !errors.Is(err, ErrInvalid) {
			t.Fatalf("invalid baseline decoded: %s %v", raw, err)
		}
	}
	b, err := DecodeBaseline([]byte(`{"values":{"categories":[0],"uploads":[9223372036854775807],"time_zone":[0]},"incomplete":false}`))
	if err != nil || b.Values[Uploads][0] != 9223372036854775807 || b.Incomplete {
		t.Fatal(b, err)
	}
}
