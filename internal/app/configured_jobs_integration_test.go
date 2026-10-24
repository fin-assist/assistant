package app_test

import (
	"testing"
	"time"
)

// Exercise configured model/peer assembly and its registered recovery job through
// PostgreSQL state. No assistant RPC is called, so this is component integration.
func TestIntegrationConfiguredGenerationAssemblyRecoversStaleAttempts(t *testing.T) {
	peers := &generationAPIPeers{grant: true}
	model := generationAPIModel(t, nil)
	env := generationAPIEnv(t, peers, model)
	_, answer := generationAPISeedAnswer(t, env, "generating", "", nil)
	recoveryExec(t, env, `UPDATE answer SET created_at=$3 WHERE user_id=$1 AND id=$2`, generationAPIUser, answer, generationAPINow.Add(-4*time.Minute))
	cancel, done := recoveryJob(t, env.App, "stale-generations")
	defer stopRecoveryJob(t, cancel, done)
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		var state, code string
		var charged bool
		var used int
		err := env.App.DB.Reader(t.Context()).QueryRow(t.Context(), `SELECT a.status,coalesce(a.error_code,''),a.charged,u.limit_used FROM answer a JOIN user_state u USING(user_id) WHERE a.user_id=$1 AND a.id=$2`, generationAPIUser, answer).Scan(&state, &code, &charged, &used)
		if err != nil {
			t.Fatal(err)
		}
		if state == "failed" {
			if code != "MODEL_ERROR" || charged || used != 0 {
				t.Fatalf("recovered state=%s code=%s charged=%v used=%d", state, code, charged, used)
			}
			break
		}
		select {
		case <-deadline.C:
			t.Fatal("configured assembly recovery job did not finalize expired attempt")
		case <-tick.C:
		}
	}
	if model.count.Load() != 0 || peers.count("consent") != 0 {
		t.Fatalf("recovery resent saved question to dependencies: model=%d consent=%d", model.count.Load(), peers.count("consent"))
	}
}
