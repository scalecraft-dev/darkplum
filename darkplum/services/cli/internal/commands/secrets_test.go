package commands

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

func secretJSON(data map[string]string) string {
	enc := map[string]string{}
	for k, v := range data {
		enc[k] = b64(v)
	}
	b, _ := json.Marshal(map[string]any{"data": enc})
	return string(b)
}

func stringData(t *testing.T, c kcall) map[string]string {
	t.Helper()
	var m struct {
		StringData map[string]string `json:"stringData"`
	}
	if err := json.Unmarshal(c.stdin, &m); err != nil {
		t.Fatalf("stdin is not JSON: %v", err)
	}
	return m.StringData
}

func TestAuthSecretCreatedOnlyOnConfirmedNotFound(t *testing.T) {
	k := newScriptKube(t).on("get secret darkplum-auth", "", secretNotFound("darkplum-auth")).on("create -f -", "", nil)
	res, err := ensureAuthSecret(k.client("darkplum-system"), "darkplum-system", nil)
	if err != nil || !res.Created {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	create := k.matching("create -f -")
	if len(create) != 1 {
		t.Fatalf("create calls = %d", len(create))
	}
	sd := stringData(t, create[0])
	for _, key := range append(append([]string{}, oidcKeys...), "SESSION_SIGNING_KEY", "SERVICE_KEY_SECRET") {
		if sd[key] == "" {
			t.Errorf("created secret lacks %s", key)
		}
	}
	if sd["OIDC_ISSUER"] != "unconfigured" {
		t.Errorf("OIDC_ISSUER = %q, want the unconfigured placeholder", sd["OIDC_ISSUER"])
	}
	if sd["SESSION_SIGNING_KEY"] == sd["SERVICE_KEY_SECRET"] {
		t.Error("the two keys must be generated independently")
	}
	k.assertClean(sd["SESSION_SIGNING_KEY"], sd["SERVICE_KEY_SECRET"])
}

func TestAuthSecretAnyOtherReadErrorAborts(t *testing.T) {
	for name, err := range map[string]error{
		"forbidden":      errors.New(`kubectl: Error from server (Forbidden): secrets "darkplum-auth" is forbidden`),
		"unreachable":    errors.New("kubectl: Unable to connect to the server: dial tcp: i/o timeout"),
		"expired token":  errors.New("kubectl: error: You must be logged in to the server (Unauthorized)"),
		"wrong NotFound": errors.New("kubectl: Error from server (NotFound): the server could not find the requested resource"),
		"namespace":      errors.New(`kubectl: Error from server (NotFound): namespaces "darkplum-system" not found`),
	} {
		t.Run(name, func(t *testing.T) {
			k := newScriptKube(t).on("get secret", "", err)
			if _, e := ensureAuthSecret(k.client("darkplum-system"), "darkplum-system", nil); e == nil {
				t.Fatal("a read error that is not this secret's NotFound must abort")
			}
			if n := len(k.matching("create")) + len(k.matching("patch")) + len(k.matching("apply")); n != 0 {
				t.Errorf("%d write calls after a failed read", n)
			}
		})
	}
}

func TestAuthSecretAddsOnlyMissingKeysAndKeepsOIDC(t *testing.T) {
	// An older install: SERVICE_KEY_SECRET was never written, OIDC is configured.
	existing := secretJSON(map[string]string{
		"OIDC_ISSUER": "https://idp.example", "OIDC_CLIENT_ID": "cid", "OIDC_CLIENT_SECRET": "sec", "OIDC_REDIRECT_URL": "https://x/cb",
		"SESSION_SIGNING_KEY": "keep-me", "SECURE_COOKIES": "true",
	})
	k := newScriptKube(t).on("get secret darkplum-auth", existing, nil).on("patch secret", "", nil)
	res, err := ensureAuthSecret(k.client("darkplum-system"), "darkplum-system", nil)
	if err != nil || res.Created || len(res.Added) != 1 || res.Added[0] != "SERVICE_KEY_SECRET" {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	p := k.matching("patch secret")
	if len(p) != 1 {
		t.Fatalf("patch calls = %d", len(p))
	}
	sd := stringData(t, p[0])
	if len(sd) != 1 || sd["SERVICE_KEY_SECRET"] == "" {
		t.Errorf("patch = %v, want only the missing SERVICE_KEY_SECRET", sd)
	}
	for _, key := range append(oidcKeys, "SESSION_SIGNING_KEY") {
		if _, has := sd[key]; has {
			t.Errorf("deploy overwrote %s", key)
		}
	}
	k.assertClean(sd["SERVICE_KEY_SECRET"], "keep-me", "https://idp.example")
	if len(k.matching("create")) != 0 {
		t.Error("an existing secret must never be re-created")
	}
}

func TestAuthSecretNoOpWhenComplete(t *testing.T) {
	all := map[string]string{"SESSION_SIGNING_KEY": "a", "SERVICE_KEY_SECRET": "b"}
	for _, k := range oidcKeys {
		all[k] = "unconfigured"
	}
	k := newScriptKube(t).on("get secret darkplum-auth", secretJSON(all), nil)
	res, err := ensureAuthSecret(k.client("darkplum-system"), "darkplum-system", nil)
	if err != nil || res.Created || len(res.Added) != 0 {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	if len(k.calls) != 1 {
		t.Errorf("calls = %d, want just the read", len(k.calls))
	}
}

func TestAuthSecretEmptyValueCountsAsMissing(t *testing.T) {
	all := map[string]string{"SESSION_SIGNING_KEY": "", "SERVICE_KEY_SECRET": "b"}
	for _, k := range oidcKeys {
		all[k] = "x"
	}
	k := newScriptKube(t).on("get secret darkplum-auth", secretJSON(all), nil).on("patch secret", "", nil)
	if _, err := ensureAuthSecret(k.client("darkplum-system"), "darkplum-system", nil); err != nil {
		t.Fatal(err)
	}
	if sd := stringData(t, k.matching("patch secret")[0]); sd["SESSION_SIGNING_KEY"] == "" || len(sd) != 1 {
		t.Errorf("patch = %v", sd)
	}
}

func TestAuthSecretSSOOverwritesOIDCOnly(t *testing.T) {
	existing := secretJSON(map[string]string{
		"OIDC_ISSUER": "old", "OIDC_CLIENT_ID": "old", "OIDC_CLIENT_SECRET": "old", "OIDC_REDIRECT_URL": "old",
		"SESSION_SIGNING_KEY": "keep-me", "SERVICE_KEY_SECRET": "keep-me-too",
	})
	k := newScriptKube(t).on("get secret darkplum-auth", existing, nil).on("patch secret", "", nil)
	oidc := map[string]string{"OIDC_ISSUER": "https://new", "OIDC_CLIENT_ID": "cid", "OIDC_CLIENT_SECRET": "s3cret-value", "OIDC_REDIRECT_URL": "https://x/cb"}
	res, err := ensureAuthSecret(k.client("darkplum-system"), "darkplum-system", oidc)
	if err != nil || len(res.Overwritten) != 4 {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	sd := stringData(t, k.matching("patch secret")[0])
	if sd["OIDC_CLIENT_SECRET"] != "s3cret-value" || sd["OIDC_ISSUER"] != "https://new" {
		t.Errorf("patch = %v", sd)
	}
	if _, has := sd["SESSION_SIGNING_KEY"]; has {
		t.Error("sso configure rotated the signing key")
	}
	k.assertClean("s3cret-value")
}

func TestAuthSecretSSORequiresAllFour(t *testing.T) {
	k := newScriptKube(t)
	if _, err := ensureAuthSecret(k.client("darkplum-system"), "darkplum-system", map[string]string{"OIDC_ISSUER": "x"}); err == nil {
		t.Fatal("partial OIDC settings must be refused")
	}
	if len(k.calls) != 0 {
		t.Error("no kubectl call before validation")
	}
}

func TestUserHMACRules(t *testing.T) {
	t.Run("not found creates both keys", func(t *testing.T) {
		k := newScriptKube(t).on("get secret darkplum-user-hmac", "", secretNotFound("darkplum-user-hmac")).on("create -f -", "", nil)
		res, err := ensureUserHMAC(k.client("darkplum-system"), "darkplum-system")
		if err != nil || !res.Created {
			t.Fatalf("%+v %v", res, err)
		}
		sd := stringData(t, k.matching("create -f -")[0])
		if sd["user-hmac-key"] == "" || sd["user-hmac-key-id"] != "k1" {
			t.Errorf("created = %v", sd)
		}
		k.assertClean(sd["user-hmac-key"])
	})
	t.Run("other error aborts: no key is generated", func(t *testing.T) {
		k := newScriptKube(t).on("get secret", "", errors.New("kubectl: error: You must be logged in to the server (Unauthorized)"))
		if _, err := ensureUserHMAC(k.client("darkplum-system"), "darkplum-system"); err == nil {
			t.Fatal("must abort")
		}
		if len(k.calls) != 1 {
			t.Errorf("calls = %d", len(k.calls))
		}
	})
	t.Run("a key without its id is refused, not repaired", func(t *testing.T) {
		k := newScriptKube(t).on("get secret darkplum-user-hmac", secretJSON(map[string]string{"user-hmac-key": "live"}), nil).on("patch secret", "", nil)
		if _, err := ensureUserHMAC(k.client("darkplum-system"), "darkplum-system"); err == nil {
			t.Fatal("guessed the id of a live key")
		}
		if len(k.matching("patch")) != 0 {
			t.Error("patched a half-present pair")
		}
	})
	t.Run("an id without its key is refused", func(t *testing.T) {
		k := newScriptKube(t).on("get secret darkplum-user-hmac", secretJSON(map[string]string{"user-hmac-key-id": "k7"}), nil)
		if _, err := ensureUserHMAC(k.client("darkplum-system"), "darkplum-system"); err == nil {
			t.Fatal("generated a new key under an existing id")
		}
		if len(k.calls) != 1 {
			t.Errorf("calls = %d, want just the read", len(k.calls))
		}
	})
	t.Run("a complete pair is left alone", func(t *testing.T) {
		k := newScriptKube(t).on("get secret darkplum-user-hmac", secretJSON(map[string]string{"user-hmac-key": "live", "user-hmac-key-id": "k3"}), nil)
		if _, err := ensureUserHMAC(k.client("darkplum-system"), "darkplum-system"); err != nil || len(k.calls) != 1 {
			t.Fatalf("err=%v calls=%d", err, len(k.calls))
		}
	})
}

func TestPostgresSecretViaStdinServerSide(t *testing.T) {
	const url = "postgres://u:p%40ss@db.example:5432/darkplum"
	k := newScriptKube(t).on("apply --server-side", "", nil).on("patch secret", "", nil)
	if err := applyPostgresSecret(k.client("darkplum-system"), "darkplum-system", url); err != nil {
		t.Fatal(err)
	}
	c := k.matching("apply")[0]
	if !strings.Contains(c.joined(), "--force-conflicts") || stringData(t, c)["DATABASE_URL"] != url {
		t.Errorf("call = %v / %s", c.args, c.stdin)
	}
	k.assertClean(url, "p%40ss")
	if applyPostgresSecret(k.client("darkplum-system"), "darkplum-system", "") == nil {
		t.Error("an empty URL must be refused")
	}
}

func TestSSOApplyUsesSharedSecretAndRestarts(t *testing.T) {
	k := newScriptKube(t).
		on("get secret darkplum-auth", "", secretNotFound("darkplum-auth")).
		on("create -f -", "", nil).
		on("rollout restart", "", nil).on("rollout status", "", nil).on("logs", "OIDC authentication enabled", nil)
	oidc := map[string]string{"OIDC_ISSUER": "https://i", "OIDC_CLIENT_ID": "c", "OIDC_CLIENT_SECRET": "topsecret", "OIDC_REDIRECT_URL": "https://r"}
	var out strings.Builder
	if err := applySSO(&out, k.client("darkplum-system"), "darkplum-system", oidc); err != nil {
		t.Fatal(err)
	}
	sd := stringData(t, k.matching("create -f -")[0])
	// The regression: sso configure used to create darkplum-auth without SERVICE_KEY_SECRET.
	if sd["SERVICE_KEY_SECRET"] == "" || sd["SESSION_SIGNING_KEY"] == "" || sd["OIDC_CLIENT_SECRET"] != "topsecret" {
		t.Errorf("created = %v", sd)
	}
	k.assertClean("topsecret", sd["SESSION_SIGNING_KEY"])
}

func TestSSOApplyAbortsOnUnreadableSecret(t *testing.T) {
	k := newScriptKube(t).on("get secret", "", errors.New("kubectl: Unable to connect to the server"))
	oidc := map[string]string{"OIDC_ISSUER": "https://i", "OIDC_CLIENT_ID": "c", "OIDC_CLIENT_SECRET": "x", "OIDC_REDIRECT_URL": "https://r"}
	if err := applySSO(&strings.Builder{}, k.client("darkplum-system"), "darkplum-system", oidc); err == nil {
		t.Fatal("must abort")
	}
	if len(k.matching("rollout")) != 0 {
		t.Error("must not restart the api after a failed secret step")
	}
}

func secretWithLastApplied(data map[string]string) string {
	enc := map[string]string{}
	for k, v := range data {
		enc[k] = b64(v)
	}
	b, _ := json.Marshal(map[string]any{"data": enc, "metadata": map[string]any{"annotations": map[string]string{
		"kubectl.kubernetes.io/last-applied-configuration": `{"data":{"SESSION_SIGNING_KEY":"clear-text-copy"}}`}}})
	return string(b)
}

func TestClearTextLastAppliedAnnotationIsRemovedWithoutTouchingValues(t *testing.T) {
	all := map[string]string{"SESSION_SIGNING_KEY": "a", "SERVICE_KEY_SECRET": "b"}
	for _, k := range oidcKeys {
		all[k] = "unconfigured"
	}
	for name, run := range map[string]func(k *scriptKube) error{
		"auth": func(k *scriptKube) error {
			k.on("get secret darkplum-auth", secretWithLastApplied(all), nil)
			_, err := ensureAuthSecret(k.client("darkplum-system"), "darkplum-system", nil)
			return err
		},
		"user-hmac": func(k *scriptKube) error {
			k.on("get secret darkplum-user-hmac", secretWithLastApplied(map[string]string{"user-hmac-key": "x", "user-hmac-key-id": "k1"}), nil)
			_, err := ensureUserHMAC(k.client("darkplum-system"), "darkplum-system")
			return err
		},
		"postgres": func(k *scriptKube) error {
			k.on("apply --server-side", "", nil)
			return applyPostgresSecret(k.client("darkplum-system"), "darkplum-system", "postgres://u:p@h/d")
		},
	} {
		t.Run(name, func(t *testing.T) {
			k := newScriptKube(t).on("patch secret", "", nil)
			if err := run(k); err != nil {
				t.Fatal(err)
			}
			p := k.matching("patch secret")
			if len(p) != 1 || !strings.Contains(string(p[0].stdin), `"kubectl.kubernetes.io/last-applied-configuration":null`) {
				t.Fatalf("patches = %v", p)
			}
			if strings.Contains(string(p[0].stdin), "stringData") || strings.Contains(string(p[0].stdin), "\"data\"") {
				t.Errorf("the strip patch touched values: %s", p[0].stdin)
			}
			k.assertClean("clear-text-copy")
		})
	}
	// A Secret without the annotation is not patched at all.
	k := newScriptKube(t).on("get secret darkplum-user-hmac", secretJSON(map[string]string{"user-hmac-key": "x", "user-hmac-key-id": "k1"}), nil)
	if _, err := ensureUserHMAC(k.client("darkplum-system"), "darkplum-system"); err != nil || len(k.matching("patch")) != 0 {
		t.Errorf("err=%v patches=%d", err, len(k.matching("patch")))
	}
}
