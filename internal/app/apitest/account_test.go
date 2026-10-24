package apitest

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	assistantv1 "github.com/fin-assist/contracts/gen/go/finassist/assistant/v1"
	deletionv1 "github.com/fin-assist/contracts/gen/go/finassist/userdeletion/v1"
	settingsv1 "github.com/fin-assist/contracts/gen/go/finassist/usersettings/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/fin-assist/assistant/internal/account"
	"github.com/fin-assist/assistant/internal/adapter/postgres"
)

const accountUser = "00000000-0000-4000-8000-000000000001"
const otherUser = "00000000-0000-4000-8000-000000000002"

func TestAPIAccountAPI(t *testing.T) {
	env := Start(t)
	c := assistantv1.NewAssistantServiceClient(env.Conn)
	settings := settingsv1.NewUserSettingsSyncServiceClient(env.Conn)
	deletions := deletionv1.NewUserDeletionServiceClient(env.Conn)
	ctx := t.Context()
	limit, err := c.GetLimit(User(ctx, accountUser), &assistantv1.GetLimitRequest{})
	if err != nil || limit.Remaining != 5 || limit.DailyMax != 5 || limit.TimeZone != "Europe/Moscow" {
		t.Fatalf("%+v %v", limit, err)
	}
	if time.Until(limit.ResetsAt.AsTime()) <= 0 || time.Until(limit.ResetsAt.AsTime()) > 24*time.Hour {
		t.Fatal(limit.ResetsAt)
	}
	var count int
	if err := env.App.DB.Primary().QueryRow(ctx, `SELECT count(*) FROM user_state`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("GetLimit wrote state: %d %v", count, err)
	}
	for _, callCtx := range []context.Context{ctx, Caller(ctx, "analytics"), Caller(ctx, "bank-bff")} {
		_, err := c.GetLimit(callCtx, &assistantv1.GetLimitRequest{})
		if status.Code(err) != codes.Unauthenticated && status.Code(err) != codes.PermissionDenied {
			t.Fatalf("unauthorized: %v", err)
		}
	}
	for _, tt := range []struct {
		version         int32
		zone            string
		expectedZone    string
		expectedVersion int32
	}{{2, "Asia/Vladivostok", "Asia/Vladivostok", 2}, {1, "UTC", "Asia/Vladivostok", 2}, {2, "Europe/Moscow", "Asia/Vladivostok", 2}, {2147483647, "UTC", "UTC", 2147483647}} {
		_, err := settings.SetUserTimezone(Caller(ctx, "user"), &settingsv1.SetUserTimezoneRequest{UserId: accountUser, Timezone: tt.zone, SettingsVersion: tt.version})
		if err != nil {
			t.Fatal(err)
		}
		var storedZone string
		var storedVersion int32
		if err := env.App.DB.Primary().QueryRow(ctx, `SELECT time_zone,settings_version FROM user_state WHERE user_id=$1`, accountUser).Scan(&storedZone, &storedVersion); err != nil || storedZone != tt.expectedZone || storedVersion != tt.expectedVersion {
			t.Fatalf("after version %d: %s v%d, want %s v%d; %v", tt.version, storedZone, storedVersion, tt.expectedZone, tt.expectedVersion, err)
		}

	}
	var zone string
	var version int32
	if err := env.App.DB.Primary().QueryRow(ctx, `SELECT time_zone,settings_version FROM user_state WHERE user_id=$1`, accountUser).Scan(&zone, &version); err != nil || zone != "UTC" || version != 2147483647 {
		t.Fatalf("%s %d %v", zone, version, err)
	}
	// Counters are scoped by the verified user, independent of the profile's timezone.
	_, err = env.App.DB.Primary().Exec(ctx, `UPDATE user_state SET limit_day=(now() AT TIME ZONE 'Europe/Moscow')::date,limit_used=3 WHERE user_id=$1`, accountUser)
	if err != nil {
		t.Fatal(err)
	}
	limit, err = c.GetLimit(User(ctx, accountUser), &assistantv1.GetLimitRequest{})
	if err != nil || limit.Remaining != 2 {
		t.Fatalf("%+v %v", limit, err)
	}
	limit, err = c.GetLimit(User(ctx, otherUser), &assistantv1.GetLimitRequest{})
	if err != nil || limit.Remaining != 5 {
		t.Fatalf("%+v %v", limit, err)
	}
	for _, bad := range []*settingsv1.SetUserTimezoneRequest{{UserId: "bad", Timezone: "UTC"}, {UserId: accountUser}, {UserId: accountUser, Timezone: "Local"}, {UserId: accountUser, Timezone: "No/Zone"}} {
		_, err := settings.SetUserTimezone(Caller(ctx, "user"), bad)
		if Reason(err) != "VALIDATION_ERROR" {
			t.Fatal(err)
		}
	}
	_, err = settings.SetUserTimezone(Caller(ctx, "bank-bff"), &settingsv1.SetUserTimezoneRequest{UserId: otherUser, Timezone: "UTC"})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatal(err)
	}
	_, err = deletions.PurgeUserData(Caller(ctx, "analytics"), &deletionv1.PurgeUserDataRequest{UserId: accountUser})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatal(err)
	}
	_, err = deletions.PurgeUserData(Caller(ctx, "user"), &deletionv1.PurgeUserDataRequest{UserId: "bad"})
	if Reason(err) != "VALIDATION_ERROR" {
		t.Fatal(err)
	}
	// Prepare every dependent table; the public deletion RPC must remove them all.
	var question, answer string
	if err := env.App.DB.Primary().QueryRow(ctx, `INSERT INTO question(user_id,text,transfer_mode) VALUES($1,'question','with') RETURNING id::text`, accountUser).Scan(&question); err != nil {
		t.Fatal(err)
	}
	if err := env.App.DB.Primary().QueryRow(ctx, `INSERT INTO answer(user_id,question_id,charge_day) VALUES($1,$2,current_date) RETURNING id::text`, accountUser, question).Scan(&answer); err != nil {
		t.Fatal(err)
	}
	if _, err := env.App.DB.Primary().Exec(ctx, `INSERT INTO answer_block(user_id,answer_id,position,type,schema_version,markdown) VALUES($1,$2,0,'text',1,'partial')`, accountUser, answer); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := deletions.PurgeUserData(Caller(ctx, "user"), &deletionv1.PurgeUserDataRequest{UserId: accountUser}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := c.GetLimit(User(ctx, accountUser), &assistantv1.GetLimitRequest{}); Reason(err) != "TOKEN_REVOKED" {
		t.Fatal(err)
	}
	if _, err := settings.SetUserTimezone(Caller(ctx, "user"), &settingsv1.SetUserTimezoneRequest{UserId: accountUser, Timezone: "UTC", SettingsVersion: 2147483647}); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"user_state", "question", "answer", "answer_block"} {
		// Table names are test constants, never user input.
		if err := env.App.DB.Primary().QueryRow(ctx, `SELECT count(*) FROM `+table+` WHERE user_id=$1`, accountUser).Scan(&count); err != nil || count != 0 {
			t.Fatalf("%s %d %v", table, count, err)
		}
	}
	// Deleting an absent user still blocks later outbox delivery.
	if _, err := deletions.PurgeUserData(Caller(ctx, "user"), &deletionv1.PurgeUserDataRequest{UserId: otherUser}); err != nil {
		t.Fatal(err)
	}
	if _, err := settings.SetUserTimezone(Caller(ctx, "user"), &settingsv1.SetUserTimezoneRequest{UserId: otherUser, Timezone: "UTC"}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.GetLimit(User(ctx, otherUser), &assistantv1.GetLimitRequest{}); Reason(err) != "TOKEN_REVOKED" {
		t.Fatal(err)
	}
}

func TestAPITimezoneAndPurgeRace(t *testing.T) {
	env := Start(t)
	settings := settingsv1.NewUserSettingsSyncServiceClient(env.Conn)
	deletion := deletionv1.NewUserDeletionServiceClient(env.Conn)
	start := make(chan struct{})
	errs := make(chan error, 21)
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Go(func() {
			<-start
			_, err := settings.SetUserTimezone(Caller(t.Context(), "user"), &settingsv1.SetUserTimezoneRequest{UserId: accountUser, Timezone: "UTC", SettingsVersion: int32(i)})
			errs <- err
		})
	}
	wg.Go(func() {
		<-start
		_, err := deletion.PurgeUserData(Caller(t.Context(), "user"), &deletionv1.PurgeUserDataRequest{UserId: accountUser})
		errs <- err
	})
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	var n int
	if err := env.App.DB.Primary().QueryRow(t.Context(), `SELECT count(*) FROM user_state WHERE user_id=$1`, accountUser).Scan(&n); err != nil || n != 0 {
		t.Fatalf("resurrected %d %v", n, err)
	}
}

func TestIntegrationAccountRepositoryRollbackAndErrors(t *testing.T) {
	env := Start(t)
	repo := postgres.NewAccounts(env.App.DB)
	ctx := t.Context()
	if err := repo.SetTimezone(ctx, accountUser, "UTC", 1); err == nil {
		t.Fatal("write outside transaction")
	}
	if err := repo.Delete(ctx, accountUser); err == nil {
		t.Fatal("delete outside transaction")
	}
	uc := account.New(env.App.DB, repo, env.App.Tombstones, time.Now, 5)
	if err := uc.SetTimezone(ctx, accountUser, "UTC", 1); err != nil {
		t.Fatal(err)
	}
	// A database failure during deletion must not commit either effects or tombstone.
	if _, err := env.App.DB.Primary().Exec(ctx, `ALTER TABLE user_state RENAME TO hidden_user_state`); err != nil {
		t.Fatal(err)
	}

	assistantClient := assistantv1.NewAssistantServiceClient(env.Conn)
	settingsClient := settingsv1.NewUserSettingsSyncServiceClient(env.Conn)
	deletionClient := deletionv1.NewUserDeletionServiceClient(env.Conn)
	_, limitErr := assistantClient.GetLimit(User(ctx, accountUser), &assistantv1.GetLimitRequest{})
	_, settingsErr := settingsClient.SetUserTimezone(Caller(ctx, "user"), &settingsv1.SetUserTimezoneRequest{UserId: accountUser, Timezone: "UTC", SettingsVersion: 2})
	_, purgeErr := deletionClient.PurgeUserData(Caller(ctx, "user"), &deletionv1.PurgeUserDataRequest{UserId: accountUser})
	for _, err := range []error{limitErr, settingsErr, purgeErr} {
		if status.Code(err) != codes.Internal || strings.Contains(err.Error(), "user_state") || strings.Contains(err.Error(), "SELECT") || strings.Contains(err.Error(), "DELETE") {
			t.Fatalf("storage error was not sanitized: %v", err)
		}
	}
	if err := uc.Purge(ctx, accountUser); err == nil {
		t.Fatal("purge should fail")
	}
	var deleted bool
	if err := env.App.DB.Primary().QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM tombstone_users WHERE user_id=$1)`, accountUser).Scan(&deleted); err != nil || deleted {
		t.Fatalf("partial tombstone %v %v", deleted, err)
	}
	if _, err := uc.GetLimit(ctx, accountUser); err == nil {
		t.Fatal("read should fail")
	}
	if err := uc.SetTimezone(ctx, accountUser, "UTC", 2); err == nil {
		t.Fatal("write should fail")
	}
	if _, err := env.App.DB.Primary().Exec(ctx, `ALTER TABLE hidden_user_state RENAME TO user_state`); err != nil {
		t.Fatal(err)
	}
	if err := uc.Purge(ctx, accountUser); err != nil {
		t.Fatal(err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := uc.GetLimit(cancelled, accountUser); err == nil {
		t.Fatal("cancelled read succeeded")
	}
}
