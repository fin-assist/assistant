package dialogue

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/fin-assist/assistant/internal/datarevision"
)

func TestUnitSourceRevisionComparisonIgnoresAllWallClocks(t *testing.T) {
	calculated := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	base := datarevision.Baseline{Values: map[string][]int64{"categories": {7}, "uploads": {11}, "time_zone": {3}}}
	source := &Source{CalculatedAt: calculated, Revisions: base}
	for _, stamp := range []time.Time{calculated.Add(-time.Hour), calculated, calculated.Add(time.Hour)} {
		source.CalculatedAt = stamp
		current := Version{Revisions: datarevision.Current{"categories": 8, "uploads": 12, "time_zone": 4}}
		got := enrich(source, current)
		if !reflect.DeepEqual(got.ChangedSince, []string{"categories", "uploads", "time_zone"}) {
			t.Fatal(stamp, got)
		}
		current.Revisions = datarevision.Current{"categories": 7, "uploads": 11, "time_zone": 3}
		if got := enrich(source, current); len(got.ChangedSince) != 0 {
			t.Fatal("timestamps claimed a change", stamp, got)
		}
	}
	for _, source := range []*Source{{CalculatedAt: calculated}, {CalculatedAt: calculated, Revisions: datarevision.Baseline{Values: map[string][]int64{"uploads": {11}}}}, {CalculatedAt: calculated, Revisions: datarevision.Baseline{Values: base.Values, Incomplete: true}}} {
		current := Version{Revisions: datarevision.Current{"categories": 7, "uploads": 11, "time_zone": 3}}
		if got := enrich(source, current); !reflect.DeepEqual(got.ChangedSince, []string{"unverified"}) {
			t.Fatal(got)
		}
	}
	if source.ChangedSince != nil {
		t.Fatal("enrichment mutated source")
	}
}

func TestUnitSourceVersionOutageIsExplicitAndParentCancellationWins(t *testing.T) {
	source := &Source{Revisions: datarevision.Baseline{Values: map[string][]int64{"categories": {1}, "uploads": {1}, "time_zone": {1}}}}
	repo := &saved{page: Page{Messages: []Message{{Answer: &Answer{Source: source}}}}, answers: []Answer{{Status: "complete", Source: source}}}
	v := &versions{err: ErrVersionUnavailable}
	s := New(repo, v, &notices{ch: make(chan struct{})}, allowance{})
	page, err := s.List(t.Context(), "u", "", 20)
	if err != nil || !reflect.DeepEqual(page.Messages[0].Answer.Source.ChangedSince, []string{datarevision.Unverified}) || source.Revisions.Incomplete {
		t.Fatal(page, err)
	}
	if err := s.Watch(t.Context(), "u", "a", func(e Event) error {
		if e.Kind == "snapshot" && !reflect.DeepEqual(e.Answer.Source.ChangedSince, []string{datarevision.Unverified}) {
			t.Fatal(e)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	v.err = errors.New("auth refusal")
	if _, err := s.List(t.Context(), "u", "", 20); err == nil {
		t.Fatal("unclassified refusal ignored")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	v.err = ErrVersionUnavailable
	if _, err := s.List(ctx, "u", "", 20); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
func TestUnitWatchRechecksUnchangedSourceBeforeEmission(t *testing.T) {
	base := datarevision.Baseline{Values: map[string][]int64{"categories": {5}, "uploads": {5}, "time_zone": {5}}}
	a := Answer{Status: "generating", Source: &Source{Revisions: base}}
	b := a
	b.Status = "complete"
	v := &versions{version: Version{Revisions: datarevision.Current{"categories": 5, "uploads": 5, "time_zone": 5}}}
	n := &notices{ch: make(chan struct{}, 1)}
	var kinds []string
	err := New(&saved{answers: []Answer{a, b}}, v, n, allowance{}).Watch(t.Context(), "u", "a", func(e Event) error {
		kinds = append(kinds, e.Kind)
		if e.Kind == "snapshot" {
			if len(e.Answer.Source.ChangedSince) != 0 {
				t.Fatal(e)
			}
			v.version.Revisions["uploads"] = 6
			n.ch <- struct{}{}
		}
		if e.Kind == "source" && !reflect.DeepEqual(e.Source.ChangedSince, []string{datarevision.Uploads}) {
			t.Fatal(e)
		}
		return nil
	})
	if err != nil || v.calls != 2 || !reflect.DeepEqual(kinds, []string{"snapshot", "source", "done"}) {
		t.Fatal(kinds, err, v.calls)
	}
}
