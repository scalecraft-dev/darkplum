package handler

import (
	"context"
	"net/http"
	"testing"

	"github.com/scalecraft-dev/darkplum/api/internal/activity"
)

// params_mode (migration 029): GET carries it, PUT validates it and audits a
// change with the previous and new value, an omitted value is left alone, and
// a user or key policy can never carry it.
func TestRoutingPolicy_ParamsMode(t *testing.T) {
	pool := setupTestDB(t)
	ctx := context.Background()
	adminID := newPolicyTestUser(t, ctx, pool)
	targetID := newPolicyTestUser(t, ctx, pool)
	gw := newFakePolicyGateway(t, pool)
	ph := NewPolicyHandler(pool, activity.NewRecorder(pool), gw.URL, "testkey")
	rh := NewRoutingHandler(pool, activity.NewRecorder(pool), gw.URL, "testkey")
	r := newPolicyTestRouter(ph, rh, adminID)

	get := func() routingPolicyResponse {
		t.Helper()
		resp := doJSON(t, r, http.MethodGet, "/api/v1/routing-policy", nil)
		if resp.Code != http.StatusOK {
			t.Fatalf("get: %d %s", resp.Code, resp.Body.String())
		}
		return decodeJSON[routingPolicyResponse](t, resp.Body.Bytes())
	}
	put := func(body map[string]any) *httptestResp {
		resp := doJSON(t, r, http.MethodPut, "/api/v1/routing-policy", body)
		return &httptestResp{resp.Code, resp.Body.String()}
	}
	events := func() []map[string]string {
		t.Helper()
		rows, err := pool.Query(ctx, `SELECT detail->>'previous', detail->>'new' FROM activity_events WHERE event_type = $1 ORDER BY created_at, id`,
			string(activity.RoutingPolicyParamsModeChanged))
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []map[string]string
		for rows.Next() {
			var p, n string
			if err := rows.Scan(&p, &n); err != nil {
				t.Fatal(err)
			}
			out = append(out, map[string]string{"previous": p, "new": n})
		}
		return out
	}

	if got := get().ParamsMode; got != "lenient" {
		t.Fatalf("default params_mode = %q, want lenient (NULL reads as lenient)", got)
	}
	residency := residencyCloudAccountOnly

	// An invalid value is a 400 and changes nothing.
	if r := put(map[string]any{"residency": residency, "params_mode": "bogus"}); r.code != http.StatusBadRequest {
		t.Fatalf("bogus params_mode: %d %s", r.code, r.body)
	}
	if len(events()) != 0 || get().ParamsMode != "lenient" {
		t.Fatal("a refused PUT changed state")
	}

	// lenient -> strict is audited with both values.
	if r := put(map[string]any{"residency": residency, "params_mode": "strict"}); r.code != http.StatusOK {
		t.Fatalf("strict: %d %s", r.code, r.body)
	}
	if got := get().ParamsMode; got != "strict" {
		t.Fatalf("params_mode = %q, want strict", got)
	}
	if ev := events(); len(ev) != 1 || ev[0]["previous"] != "lenient" || ev[0]["new"] != "strict" {
		t.Fatalf("events = %v, want one lenient->strict", ev)
	}

	// Omitted keeps the value (an older client); an unchanged value is not an event.
	if r := put(map[string]any{"residency": residency}); r.code != http.StatusOK {
		t.Fatalf("omitted: %d %s", r.code, r.body)
	}
	if r := put(map[string]any{"residency": residency, "params_mode": "strict"}); r.code != http.StatusOK {
		t.Fatalf("same: %d %s", r.code, r.body)
	}
	if got := get().ParamsMode; got != "strict" || len(events()) != 1 {
		t.Fatalf("params_mode %q, events %v; want strict and still one event", got, events())
	}

	if r := put(map[string]any{"residency": residency, "params_mode": "lenient"}); r.code != http.StatusOK {
		t.Fatalf("lenient: %d %s", r.code, r.body)
	}
	if ev := events(); len(ev) != 2 || ev[1]["previous"] != "strict" || ev[1]["new"] != "lenient" {
		t.Fatalf("events = %v, want strict->lenient second", ev)
	}

	// Cross-principal denial: neither a user policy nor a key policy may set it.
	for _, path := range []string{
		"/api/v1/users/" + targetID + "/routing-policy",
		"/api/v1/api-keys/" + newTestAPIKey(t, ctx, pool, targetID, "pm-key") + "/routing-policy",
	} {
		resp := doJSON(t, r, http.MethodPut, path, map[string]any{"params_mode": "strict"})
		if resp.Code != http.StatusBadRequest {
			t.Errorf("PUT %s with params_mode: %d %s, want 400", path, resp.Code, resp.Body.String())
		}
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM principal_routing_policies WHERE principal_type <> 'org' AND params_mode IS NOT NULL`).Scan(&n); err != nil || n != 0 {
		t.Errorf("a non-org row carries params_mode (%d, %v)", n, err)
	}
}

type httptestResp struct {
	code int
	body string
}
