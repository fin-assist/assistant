package generation

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/fin-assist/assistant/internal/datarevision"
	"github.com/fin-assist/assistant/internal/tooldata"
)

func TestUnitEngineKeepsEveryUsedSourceRevision(t *testing.T) {
	for _, missing := range []bool{false, true} {
		store := &engineStore{}
		at := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
		data := engineData{invoke: func(_ context.Context, name string, _ tooldata.Subject, _ any) (tooldata.Result, error) {
			value := map[string]int64{"get_data_overview": 9, "list_periods": 8, "aggregate_operations": 7, "get_analytics": 2, "find_operations": 3}[name]
			revisions := datarevision.Current{"categories": value, "uploads": value, "time_zone": value}
			if missing && name == "list_periods" {
				revisions = nil
			}
			return tooldata.Result{JSON: json.RawMessage(`{}`), Evidence: tooldata.Evidence{Source: &tooldata.Source{CalculatedAt: &at, Version: tooldata.Version{Revisions: revisions}}}}, nil
		}}
		model := &engineModel{script: func(_ context.Context, _ ModelRequest, emit func(ModelDelta) error, step int) (ModelResult, error) {
			if step == 1 {
				return ModelResult{Output: []ModelItem{engineTool("overview", "get_data_overview", `{}`), engineTool("periods", "list_periods", `{}`), engineTool("aggregate", "aggregate_operations", `{}`), engineTool("analytics", "get_analytics", `{}`), engineTool("find", "find_operations", `{}`)}}, nil
			}
			return engineFinal(emit, "Ответ")
		}}
		if err := engineForTest(t, store, model, data, engineLimits()).Run(engineContext(t), engineTask()); err != nil {
			t.Fatal(err)
		}
		source := store.outcome.Source
		if source == nil || source.Revisions.Incomplete != missing {
			t.Fatal(source)
		}
		want := []int64{2, 3, 7, 8, 9}
		if missing {
			want = []int64{2, 3, 7, 9}
		}
		if !reflect.DeepEqual(source.Revisions.Values["uploads"], want) {
			t.Fatal("latest overview replaced used snapshot", source.Revisions)
		}
		got := datarevision.Changes(source.Revisions, datarevision.Current{"categories": 9, "uploads": 9, "time_zone": 9})
		expected := []string{"categories", "uploads", "time_zone"}
		if missing {
			expected = append(expected, "unverified")
		}
		if !reflect.DeepEqual(got, expected) {
			t.Fatal(got)
		}
	}
}

func TestUnitEngineRetainsNoDataPrivateProvenance(t *testing.T) {
	store := &engineStore{}
	at := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	data := engineData{invoke: func(_ context.Context, name string, _ tooldata.Subject, _ any) (tooldata.Result, error) {
		revisions := datarevision.Current{"categories": 1, "uploads": 1, "time_zone": 1}
		result := tooldata.Result{JSON: json.RawMessage(`{}`), Evidence: tooldata.Evidence{Source: &tooldata.Source{CalculatedAt: &at, Version: tooldata.Version{Revisions: revisions}}}}
		if name == "get_analytics" {
			result.Evidence.Source = nil
			result.Evidence.NoData = true
			result.Evidence.Revisions = datarevision.Current{"categories": 3, "uploads": 3, "time_zone": 3}
		}
		return result, nil
	}}
	model := &engineModel{script: func(_ context.Context, _ ModelRequest, emit func(ModelDelta) error, step int) (ModelResult, error) {
		if step == 1 {
			return ModelResult{Output: []ModelItem{engineTool("analytics", "get_analytics", `{}`), engineTool("aggregate", "aggregate_operations", `{}`)}}, nil
		}
		return engineFinal(emit, "Нет данных в выбранном периоде")
	}}
	if err := engineForTest(t, store, model, data, engineLimits()).Run(engineContext(t), engineTask()); err != nil {
		t.Fatal(err)
	}
	if store.outcome.Source == nil || store.outcome.Charged || !reflect.DeepEqual(store.outcome.Source.Revisions.Values["uploads"], []int64{1, 3}) {
		t.Fatal(store.outcome)
	}
}

func TestUnitEngineDoesNotFabricatePureNoDataSource(t *testing.T) {
	store := &engineStore{}
	data := engineData{invoke: func(_ context.Context, name string, _ tooldata.Subject, _ any) (tooldata.Result, error) {
		return tooldata.Result{JSON: json.RawMessage(`{}`), Evidence: tooldata.Evidence{NoData: true, Revisions: datarevision.Current{"categories": 0, "uploads": 0, "time_zone": 0}}}, nil
	}}
	model := &engineModel{script: func(_ context.Context, _ ModelRequest, emit func(ModelDelta) error, step int) (ModelResult, error) {
		if step == 1 {
			return ModelResult{Output: []ModelItem{engineTool("analytics", "get_analytics", `{}`), engineTool("periods", "list_periods", `{}`)}}, nil
		}
		return engineFinal(emit, "Нет данных")
	}}
	if err := engineForTest(t, store, model, data, engineLimits()).Run(engineContext(t), engineTask()); err != nil {
		t.Fatal(err)
	}
	if store.outcome.Source != nil || store.outcome.Charged {
		t.Fatal(store.outcome)
	}
}
