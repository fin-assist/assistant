package generation

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"testing"
)

func builtinRegistryForTest(t *testing.T) *ToolRegistry {
	t.Helper()
	registry, err := NewToolRegistry(BuiltinTools(engineNoData())...)
	if err != nil {
		t.Fatal(err)
	}
	return registry
}

func TestRegistryPreservesProviderSchemas(t *testing.T) {
	// Captured from the pre-refactor serialized request schemas. Exact bytes also
	// preserve escaping, ordering and the model input budget charged for schemas.
	for searches, want := range map[int]string{0: "3812b4b9b6ae3d28d1a0791292e980593301ca5549847891c046b1d2c2856767", 3: "54fd3db3add97fa0305e555e127228d185291a28606de149d2e40067ea7fa1e5"} {
		raw, err := json.Marshal(builtinRegistryForTest(t).definitions(searches))
		if err != nil {
			t.Fatal(err)
		}
		if got := fmt.Sprintf("%x", sha256.Sum256(raw)); got != want {
			t.Fatalf("schemas changed: %s want %s", got, want)
		}
	}
}
