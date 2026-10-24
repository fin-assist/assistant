package apitest

import (
	"strings"
	"testing"

	assistantv1 "github.com/fin-assist/contracts/gen/go/finassist/assistant/v1"
	settingsv1 "github.com/fin-assist/contracts/gen/go/finassist/usersettings/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestAPISettingsCommitViolationRollsBackAndStaysInternal(t *testing.T) {
	env := Start(t)
	ctx := t.Context()
	// Test-only deferred FK makes the query succeed and the real COMMIT fail.
	_, err := env.App.DB.Primary().Exec(ctx, `CREATE TABLE accepted_settings_version (version integer PRIMARY KEY);
INSERT INTO accepted_settings_version VALUES (1);
ALTER TABLE user_state ADD CONSTRAINT test_settings_version_fk FOREIGN KEY(settings_version)
REFERENCES accepted_settings_version(version) DEFERRABLE INITIALLY DEFERRED`)
	if err != nil {
		t.Fatal(err)
	}
	client := settingsv1.NewUserSettingsSyncServiceClient(env.Conn)
	_, err = client.SetUserTimezone(Caller(ctx, "user"), &settingsv1.SetUserTimezoneRequest{UserId: accountUser, Timezone: "UTC", SettingsVersion: 2})
	if status.Code(err) != codes.Internal || Reason(err) != "" || len(status.Convert(err).Details()) != 0 {
		t.Fatalf("unknown commit violation must stay INTERNAL: %v", err)
	}
	for _, private := range []string{"test_settings_version_fk", "accepted_settings_version", "SQLSTATE", "23503", accountUser} {
		if strings.Contains(err.Error(), private) {
			t.Fatalf("commit error leaked database details: %v", err)
		}
	}
	var rows int
	if err := env.App.DB.Primary().QueryRow(ctx, `SELECT count(*) FROM user_state WHERE user_id=$1`, accountUser).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("failed COMMIT retained settings: %d, %v", rows, err)
	}
	_, err = client.SetUserTimezone(Caller(ctx, "user"), &settingsv1.SetUserTimezoneRequest{UserId: accountUser, Timezone: "UTC", SettingsVersion: 1})
	if err != nil {
		t.Fatal(err)
	}
	var storedZone string
	var version int
	if err := env.App.DB.Primary().QueryRow(ctx, `SELECT time_zone,settings_version FROM user_state WHERE user_id=$1`, accountUser).Scan(&storedZone, &version); err != nil || storedZone != "UTC" || version != 1 {
		t.Fatalf("valid transaction was not committed: %s v%d, %v", storedZone, version, err)
	}
	limit, err := assistantv1.NewAssistantServiceClient(env.Conn).GetLimit(User(ctx, accountUser), &assistantv1.GetLimitRequest{})
	if err != nil || limit.TimeZone != "Europe/Moscow" || limit.Remaining != 5 {
		t.Fatalf("valid transaction after failed COMMIT: %+v, %v", limit, err)
	}
}
