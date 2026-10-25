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
	for searches, want := range map[int]string{0: "7e691c5cc474af27c8789a5422a35ab895d82b43919f13080767c4e3a4743a16", 3: "b09f7d31f4a51a1a9c05f63fd1e56b15008eb1e05aaa9d0032d0e2772bbd899e"} {
		raw, err := json.Marshal(builtinRegistryForTest(t).definitions(searches))
		if err != nil {
			t.Fatal(err)
		}
		if got := fmt.Sprintf("%x", sha256.Sum256(raw)); got != want {
			t.Fatalf("schemas changed: %s want %s", got, want)
		}
	}
}
