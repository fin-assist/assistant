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

func TestUnitRegistryPreservesProviderSchemas(t *testing.T) {
	// Captured from the serialized request schemas (FIN-304 descriptions). Exact bytes also
	// preserve escaping, ordering and the model input budget charged for schemas.
	for searches, want := range map[int]string{0: "9d5d736de3e55855c0b0ff7904aab5503331e6a4dcb7062c91f2b4ab6e99b445", 3: "fd8231f6d43ee9e83e6b91416b8de73100761f649bc97dbd35b056e18cfb77b2"} {
		raw, err := json.Marshal(builtinRegistryForTest(t).definitions(searches))
		if err != nil {
			t.Fatal(err)
		}
		if got := fmt.Sprintf("%x", sha256.Sum256(raw)); got != want {
			t.Fatalf("schemas changed: %s want %s", got, want)
		}
	}
}
