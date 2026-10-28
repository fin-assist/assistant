package client

import (
	"encoding/json"
	"strings"
	"testing"

	commonv1 "github.com/fin-assist/contracts/gen/go/finassist/common/v1"
	"google.golang.org/protobuf/proto"

	"github.com/fin-assist/assistant/internal/tooldata"
)

func TestUnitFinancialModelExcludesUnneededIdentifiersFromCopy(t *testing.T) {
	c, peer, subject := financialFixture(t)
	spoken := "текущий счёт, последние цифры 4 8 2 1"
	mask := "··4821"
	op := peer.find.Items[0]
	op.Account = &commonv1.AccountRef{TypeName: "Текущий", Mask: &mask, Spoken: &spoken}
	op.FromAccount = proto.Clone(op.Account).(*commonv1.AccountRef)
	op.ToAccount = proto.Clone(op.Account).(*commonv1.AccountRef)
	before := proto.Clone(peer.find)
	got, err := c.Find(t.Context(), subject, tooldata.FindArgs{})
	if err != nil {
		t.Fatal(err)
	}
	var response map[string]any
	if json.Unmarshal(got.JSON, &response) != nil {
		t.Fatal("bad model JSON")
	}
	item := response["items"].([]any)[0].(map[string]any)
	if _, exists := item["id"]; exists {
		t.Fatal("operation UUID leaked to model")
	}
	for _, key := range []string{"account", "from_account", "to_account"} {
		account := item[key].(map[string]any)
		if _, exists := account["spoken"]; exists {
			t.Fatal("a11y duplicate leaked")
		}
		if account["type_name"] != "Текущий" || account["mask"] != mask {
			t.Fatal("account distinction lost")
		}
	}
	if !proto.Equal(before, peer.find) || !strings.Contains(string(got.JSON), op.Title) || got.Operations.Items[0].Title != op.Title {
		t.Fatal("trusted originals or financial text changed")
	}
}
func TestIntegrationFinancialModelExcludesOperationUUIDOverRealGRPC(t *testing.T) {
	c := startFinancialPeer(t, &financialPeer{})
	result, err := c.Find(t.Context(), tooldata.Subject{UserID: "user", TransferMode: "with"}, tooldata.FindArgs{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(result.JSON), "019be405-3b20-7000-8000-000000000001") || !strings.Contains(string(result.JSON), "9007199254740993") || len(result.Operations.Items) != 1 {
		t.Fatal("bad model projection across gRPC")
	}
}
