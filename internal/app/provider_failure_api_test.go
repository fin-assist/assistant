package app_test

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	commonv1 "github.com/fin-assist/contracts/gen/go/finassist/common/v1"

	"github.com/fin-assist/assistant/internal/config"
)

func TestAPIProviderTransportAndCredentialsFailuresRefundWithoutPaidRetries(t *testing.T) {
	for _, name := range []string{"http-401", "http-403", "http-429", "http-500", "http-502", "http-503", "json-response", "missing-media-type", "malformed-media-type", "encoded-response", "closed-endpoint", "missing-key", "directory-key", "empty-key", "oversized-key", "invalid-header-key"} {
		t.Run(name, func(t *testing.T) {
			model := generationAPIModel(t, func(_ int, w http.ResponseWriter, _ *http.Request) {
				code := http.StatusOK
				switch name {
				case "http-401":
					code = http.StatusUnauthorized
				case "http-403":
					code = http.StatusForbidden
				case "http-429":
					code = http.StatusTooManyRequests
				case "http-500":
					code = http.StatusInternalServerError
				case "http-502":
					code = http.StatusBadGateway
				case "http-503":
					code = http.StatusServiceUnavailable
				case "json-response":
					w.Header().Set("Content-Type", "application/json")
				case "missing-media-type":
					w.Header()["Content-Type"] = nil
				case "malformed-media-type":
					w.Header().Set("Content-Type", "text/event-stream; broken")
				case "encoded-response":
					w.Header().Set("Content-Encoding", "gzip")
				}
				w.WriteHeader(code)
				_, _ = io.WriteString(w, `{"PRIVATE":"provider detail, never user-visible"}`)
			})
			key := filepath.Join(t.TempDir(), "model-key")
			contents := "local-test-key"
			switch name {
			case "empty-key":
				contents = " \n"
			case "oversized-key":
				contents = strings.Repeat("x", 8193)
			case "invalid-header-key":
				contents = "PRIVATE\ncredential"
			}
			if name == "directory-key" {
				key = t.TempDir()
			} else if name != "missing-key" {
				if err := os.WriteFile(key, []byte(contents), 0600); err != nil {
					t.Fatal(err)
				}
			}
			env := generationAPIEnv(t, &generationAPIPeers{grant: true}, model, func(c *config.Config) { c.Generation.APIKeyFile = key })
			if name == "closed-endpoint" {
				model.server.Close()
			}
			accepted := generationAPIAsk(t, env, generationAPIUser, generationAPIKey(), "Расскажи о финансах", commonv1.TransferMode_TRANSFER_MODE_WITHOUT)
			presenterAssertFailure(t, env, generationAPIUser, accepted.AnswerId)
			want := int32(1)
			if strings.HasSuffix(name, "-key") || name == "closed-endpoint" {
				want = 0
			}
			if got := model.count.Load(); got != want {
				t.Fatalf("uncertain provider request was retried: requests=%d want=%d", got, want)
			}
			if saved := presenterHistory(t, env, generationAPIUser); strings.Contains(saved.String(), "PRIVATE") || strings.Contains(saved.String(), key) {
				t.Fatal("provider or credential details leaked into history")
			}
		})
	}
}
