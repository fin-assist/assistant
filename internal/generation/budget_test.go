package generation

import (
	"errors"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func budgetLimits() BudgetLimits {
	return BudgetLimits{InputTokensPerStep: 24_000, InputTokensTotal: 48_000, HistoryTokens: 16_000, RequestBytesPerStep: 64 * 1024, RequestBytesTotal: 128 * 1024, MaxSteps: 8, MaxFunctions: 16}
}
func budgetForTest(t *testing.T, limits BudgetLimits) *Budget {
	t.Helper()
	b, err := NewBudget(limits)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
func budgetJSON(bytes int) []byte { return []byte(`{"a":"` + strings.Repeat("a", bytes-8) + `"}`) }

type budgetState struct{ steps, functions, tokens, bytes int }

func budgetSnapshot(b *Budget) budgetState {
	b.mu.Lock()
	defer b.mu.Unlock()
	return budgetState{b.steps, b.functions, b.inputTokens, b.requestBytes}
}

func TestUnitEstimateTokensUsesSerializedUTF8Bytes(t *testing.T) {
	for _, test := range []struct {
		raw  string
		want int
	}{{"", 0}, {"a", 1}, {"abcd", 1}, {"abcde", 2}, {"аб", 1}, {"абв", 2}, {"🌍", 1}, {"🌍a", 2}, {`{"x":"🌍"}`, 3}, {`{"x":"\ud83c\udf0d"}`, 5}} {
		if got := EstimateTokens([]byte(test.raw)); got != test.want {
			t.Fatalf("EstimateTokens(%q)=%d, want %d", test.raw, got, test.want)
		}
	}
}

func TestUnitBudgetRejectsInvalidLimits(t *testing.T) {
	base := budgetLimits()
	for name, mutate := range map[string]func(*BudgetLimits){
		"step tokens zero": func(l *BudgetLimits) { l.InputTokensPerStep = 0 }, "total tokens negative": func(l *BudgetLimits) { l.InputTokensTotal = -1 }, "history zero": func(l *BudgetLimits) { l.HistoryTokens = 0 }, "step bytes zero": func(l *BudgetLimits) { l.RequestBytesPerStep = 0 }, "total bytes zero": func(l *BudgetLimits) { l.RequestBytesTotal = 0 }, "steps zero": func(l *BudgetLimits) { l.MaxSteps = 0 }, "functions negative": func(l *BudgetLimits) { l.MaxFunctions = -1 }, "token step above total": func(l *BudgetLimits) { l.InputTokensPerStep = l.InputTokensTotal + 1 }, "byte step above total": func(l *BudgetLimits) { l.RequestBytesPerStep = l.RequestBytesTotal + 1 }, "history above step": func(l *BudgetLimits) { l.HistoryTokens = l.InputTokensPerStep + 1 },
	} {
		t.Run(name, func(t *testing.T) {
			limits := base
			mutate(&limits)
			b, err := NewBudget(limits)
			if b != nil || !errors.Is(err, ErrInvalidBudget) {
				t.Fatalf("%+v %v", b, err)
			}
		})
	}
	if _, err := NewBudget(BudgetLimits{}); !errors.Is(err, ErrInvalidBudget) {
		t.Fatal(err)
	}
}

func TestUnitBudgetByteBoundariesAndRepeatedContext(t *testing.T) {
	b := budgetForTest(t, budgetLimits())
	raw := budgetJSON(64 * 1024)
	for i := 0; i < 2; i++ {
		if err := b.ReserveRequest(raw); err != nil {
			t.Fatal(err)
		}
	}
	if state := budgetSnapshot(b); state.steps != 2 || state.bytes != 128*1024 || state.tokens != 32*1024 {
		t.Fatalf("repeated context counted incorrectly %+v", state)
	}
	if b.RemainingInputBytes() != 0 || b.RemainingInputTokens() != 15_232 {
		t.Fatalf("remaining bytes %d tokens %d", b.RemainingInputBytes(), b.RemainingInputTokens())
	}
	before := budgetSnapshot(b)
	if err := b.ReserveRequest([]byte(`{}`)); !errors.Is(err, ErrBudgetExceeded) {
		t.Fatal(err)
	}
	if got := budgetSnapshot(b); got != before {
		t.Fatalf("rejected request changed counters: %+v -> %+v", before, got)
	}
	b = budgetForTest(t, budgetLimits())
	before = budgetSnapshot(b)
	if err := b.ReserveRequest(budgetJSON(64*1024 + 1)); !errors.Is(err, ErrBudgetExceeded) {
		t.Fatal(err)
	}
	if got := budgetSnapshot(b); got != before {
		t.Fatalf("oversized request consumed counters: %+v", got)
	}
	// The exact bytes supplied to ReserveRequest are counted, not the smaller
	// equivalent value a parser might marshal back without whitespace.
	raw = []byte("   {\"a\":\"б\"}   ")
	if err := b.ReserveRequest(raw); err != nil {
		t.Fatal(err)
	}
	if state := budgetSnapshot(b); state.bytes != len(raw) || state.tokens != 4 {
		t.Fatalf("raw bytes lost: %+v", state)
	}
}

func TestUnitBudgetEstimatedTokenBoundaries(t *testing.T) {
	limits := budgetLimits()
	limits.InputTokensPerStep = 4
	limits.InputTokensTotal = 6
	limits.HistoryTokens = 4
	b := budgetForTest(t, limits)
	if b.RemainingInputTokens() != 4 || b.RemainingInputBytes() != 64*1024 {
		t.Fatal("initial next-step allowance")
	}
	if err := b.ReserveRequest(budgetJSON(17)); !errors.Is(err, ErrBudgetExceeded) {
		t.Fatal(err)
	}
	if err := b.ReserveRequest(budgetJSON(16)); err != nil {
		t.Fatal(err)
	}
	if b.RemainingInputTokens() != 2 {
		t.Fatalf("remaining token allowance %d", b.RemainingInputTokens())
	}
	before := budgetSnapshot(b)
	if err := b.ReserveRequest(budgetJSON(12)); !errors.Is(err, ErrBudgetExceeded) {
		t.Fatal(err)
	}
	if got := budgetSnapshot(b); got != before {
		t.Fatalf("denial changed counters %+v -> %+v", before, got)
	}
	if err := b.ReserveRequest(budgetJSON(8)); err != nil {
		t.Fatal(err)
	}
	if b.RemainingInputTokens() != 0 {
		t.Fatal("total estimated token budget not exhausted")
	}
	if err := b.ReserveRequest([]byte(`{}`)); !errors.Is(err, ErrBudgetExceeded) {
		t.Fatal(err)
	}
}

func TestUnitBudgetStepsAndFunctionsAreIndependent(t *testing.T) {
	limits := budgetLimits()
	limits.MaxSteps = 2
	limits.MaxFunctions = 3
	b := budgetForTest(t, limits)
	if err := b.ReserveFunctions(2); err != nil {
		t.Fatal(err)
	}
	before := budgetSnapshot(b)
	if err := b.ReserveFunctions(2); !errors.Is(err, ErrBudgetExceeded) {
		t.Fatal(err)
	}
	if err := b.ReserveFunctions(-1); !errors.Is(err, ErrInvalidBudget) {
		t.Fatal(err)
	}
	if got := budgetSnapshot(b); got != before {
		t.Fatalf("failed function batch changed counters %+v", got)
	}
	if err := b.ReserveFunctions(1); err != nil {
		t.Fatal(err)
	}
	if err := b.ReserveFunctions(0); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := b.ReserveRequest([]byte(`{}`)); err != nil {
			t.Fatal(err)
		}
	}
	if b.RemainingInputBytes() != 0 || b.RemainingInputTokens() != 0 {
		t.Fatal("step cap must prevent another request")
	}
	before = budgetSnapshot(b)
	if err := b.ReserveRequest([]byte(`{}`)); !errors.Is(err, ErrBudgetExceeded) {
		t.Fatal(err)
	}
	if got := budgetSnapshot(b); got != before {
		t.Fatalf("step denial consumed quota %+v", got)
	}
}

func TestUnitBudgetMalformedRequestsHaveNoEffect(t *testing.T) {
	b := budgetForTest(t, budgetLimits())
	if err := b.ReserveRequest([]byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	before := budgetSnapshot(b)
	for _, raw := range [][]byte{nil, {}, []byte(" "), []byte("null"), []byte("[]"), []byte("true"), []byte(`"text"`), []byte("42"), []byte(`{"bad":`), []byte(`{} {}`), []byte("{\"text\":\"\xff\"}"), []byte("\u00a0{}"), []byte("{\"text\":\"\n\"}")} {
		if err := b.ReserveRequest(raw); !errors.Is(err, ErrInvalidBudget) {
			t.Fatalf("input %q: %v", raw, err)
		}
		if got := budgetSnapshot(b); got != before {
			t.Fatalf("malformed request changed counters %+v -> %+v", before, got)
		}
	}
	if err := b.ReserveRequest([]byte("\r\n{\"text\":\"🌍\"}\t")); err != nil {
		t.Fatal(err)
	}
}

func TestUnitBudgetLargestIntDoesNotOverflow(t *testing.T) {
	maximum := int(^uint(0) >> 1)
	limits := BudgetLimits{InputTokensPerStep: maximum, InputTokensTotal: maximum, HistoryTokens: maximum, RequestBytesPerStep: maximum, RequestBytesTotal: maximum, MaxSteps: maximum, MaxFunctions: maximum}
	b := budgetForTest(t, limits)
	if b.RemainingInputBytes() != maximum || b.RemainingInputTokens() != maximum {
		t.Fatal("maximum quota overflow")
	}
	if err := b.ReserveFunctions(maximum); err != nil {
		t.Fatal(err)
	}
	before := budgetSnapshot(b)
	if err := b.ReserveFunctions(maximum); !errors.Is(err, ErrBudgetExceeded) {
		t.Fatal(err)
	}
	if got := budgetSnapshot(b); got != before {
		t.Fatal("function counters wrapped")
	}
	// Exercise arithmetic at the independent counter limits without allocating
	// an object larger than available memory; the tiny request must fit exactly.
	b.mu.Lock()
	b.steps = maximum - 1
	b.requestBytes = maximum - 2
	b.inputTokens = maximum - 1
	b.mu.Unlock()
	if err := b.ReserveRequest([]byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	if got := budgetSnapshot(b); got.steps != maximum || got.bytes != maximum || got.tokens != maximum || got.functions != maximum {
		t.Fatalf("counters overflowed %+v", got)
	}
	before = budgetSnapshot(b)
	if err := b.ReserveRequest([]byte(`{}`)); !errors.Is(err, ErrBudgetExceeded) {
		t.Fatal(err)
	}
	if got := budgetSnapshot(b); !reflect.DeepEqual(got, before) {
		t.Fatal("request counters wrapped")
	}
}

func TestUnitBudgetConcurrentReservations(t *testing.T) {
	limits := budgetLimits()
	limits.MaxSteps = 37
	limits.MaxFunctions = 43
	b := budgetForTest(t, limits)
	var requests, functions atomic.Int64
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 100; i++ {
		wg.Go(func() {
			<-start
			if err := b.ReserveRequest([]byte(`{}`)); err == nil {
				requests.Add(1)
			} else if !errors.Is(err, ErrBudgetExceeded) {
				t.Error(err)
			}
			if err := b.ReserveFunctions(1); err == nil {
				functions.Add(1)
			} else if !errors.Is(err, ErrBudgetExceeded) {
				t.Error(err)
			}
			_ = b.RemainingInputBytes()
			_ = b.RemainingInputTokens()
		})
	}
	close(start)
	wg.Wait()
	if requests.Load() != 37 || functions.Load() != 43 {
		t.Fatalf("reservation count request=%d functions=%d", requests.Load(), functions.Load())
	}
	if got := budgetSnapshot(b); got != (budgetState{steps: 37, functions: 43, tokens: 37, bytes: 74}) {
		t.Fatalf("counters %+v", got)
	}
}
