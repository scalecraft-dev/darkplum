package policy

import (
	"context"
	"strings"
	"testing"
)

// Snapshot.ParamsMode is the org row's params_mode (migration 029): strict
// when set, lenient for NULL, for a database without the column (SQLSTATE
// 42703, rollout), and without the principal table at all.
func TestPgStore_LoadFromDB_ParamsMode(t *testing.T) {
	const table = `CREATE TABLE principal_routing_policies (
		principal_type TEXT NOT NULL, user_id UUID, api_key_id UUID,
		residency TEXT, allowed_models TEXT[], default_model TEXT %s)`

	cases := []struct {
		name   string
		create string
		seed   string
		want   string
	}{
		{"no table", "", "", ""},
		{"column missing (pre-029 database)", `CREATE TABLE principal_routing_policies (
			principal_type TEXT NOT NULL, user_id UUID, api_key_id UUID,
			residency TEXT, allowed_models TEXT[], default_model TEXT)`,
			`INSERT INTO principal_routing_policies (principal_type) VALUES ('org')`, ""},
		{"NULL", fmtTable(table), `INSERT INTO principal_routing_policies (principal_type) VALUES ('org')`, ""},
		{"lenient", fmtTable(table), `INSERT INTO principal_routing_policies (principal_type, params_mode) VALUES ('org', 'lenient')`, "lenient"},
		{"strict", fmtTable(table), `INSERT INTO principal_routing_policies (principal_type, params_mode) VALUES ('org', 'strict')`, "strict"},
		{"no org row", fmtTable(table), "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pool := setupPolicyDB(t)
			ctx := context.Background()
			if tc.create != "" {
				if _, err := pool.Exec(ctx, tc.create); err != nil {
					t.Fatal(err)
				}
			}
			if tc.seed != "" {
				if _, err := pool.Exec(ctx, tc.seed); err != nil {
					t.Fatal(err)
				}
			}
			snap, err := (&PgStore{pool: pool}).loadFromDB(ctx)
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			if snap.ParamsMode != tc.want {
				t.Errorf("ParamsMode = %q, want %q", snap.ParamsMode, tc.want)
			}
		})
	}
}

func fmtTable(t string) string {
	return strings.Replace(t, "%s", ", params_mode TEXT", 1)
}
