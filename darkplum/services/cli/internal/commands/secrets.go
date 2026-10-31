package commands

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/scalecraft-dev/darkplum/cli/internal/kube"
)

// The Secrets the platform reads. `darkplum deploy` and `darkplum sso configure`
// both write darkplum-auth through ensureAuthSecret, so the two cannot disagree
// about which keys it holds (sso used to create it without SERVICE_KEY_SECRET).
//
// The rules, each the answer to a way a live install gets damaged:
//   - A Secret is created only when kubectl says it is NotFound. Any other error
//     (auth, wrong cluster, network) aborts: guessing "missing" there would
//     replace a live signing key and log everyone out, or change the pseudonym
//     every vendor has already seen.
//   - On an existing Secret only MISSING keys are added (a merge patch of just
//     those keys). A present key is never overwritten, with one exception: the
//     OIDC_* values `sso configure` was explicitly asked to set.
//   - Values travel on stdin. They are never in argv, never printed, and a Secret
//     that already exists is read only to learn which keys it has.
const (
	authSecretName     = "darkplum-auth"
	userHMACSecretName = "darkplum-user-hmac"
	postgresSecretName = "postgres-credentials"

	keySessionSigning = "SESSION_SIGNING_KEY"
	keyServiceKey     = "SERVICE_KEY_SECRET"
	keyUserHMAC       = "user-hmac-key"
	keyUserHMACID     = "user-hmac-key-id"
	// oidcUnset is what a fresh install stores for OIDC: the api reads it as "SSO
	// off" (and so does `darkplum status`).
	oidcUnset = "unconfigured"
)

var oidcKeys = []string{"OIDC_ISSUER", "OIDC_CLIENT_ID", "OIDC_CLIENT_SECRET", "OIDC_REDIRECT_URL"}

// secretKey is one key a Secret should hold. value is called only when the key
// has to be written, so a random value is never generated for a key that exists.
type secretKey struct {
	name  string
	value func() (string, error)
	// overwrite writes the key even when it is present: only for a value the
	// operator just supplied.
	overwrite bool
}

func fixed(v string) func() (string, error) { return func() (string, error) { return v, nil } }

// randomKey is 48 random bytes, base64.
func randomKey() (string, error) {
	b := make([]byte, 48)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(b), nil
}

// secretOutcome says what ensureSecret did, never with a value.
type secretOutcome struct {
	Created     bool
	Added       []string // keys added to a Secret that already existed
	Overwritten []string // keys replaced because the caller asked for it
	// StrippedAnnotation is set when a last-applied-configuration annotation (a
	// clear-text copy of the values) was removed from an existing Secret.
	StrippedAnnotation bool
}

// kubeNotFound reports a kubectl "NotFound" that names this very object. A bare
// NotFound is not enough: "the server could not find the requested resource" is
// also NotFound, and means the API group is missing, not the Secret.
func kubeNotFound(err error, resource, name string) bool {
	if err == nil {
		return false
	}
	m := err.Error()
	return strings.Contains(m, "(NotFound)") && strings.Contains(m, resource) && strings.Contains(m, `"`+name+`"`)
}

// ensureSecret makes sure the named Secret in ns holds keys, by the rules above.
//
// pair says the keys belong together (a key and the id that names it): a Secret
// holding some of them and not the others is refused, not repaired, because the
// missing half cannot be invented without changing what the survivor means.
func ensureSecret(kc *kube.Client, ns, name string, keys []secretKey, pair bool) (secretOutcome, error) {
	var res secretOutcome
	out, err := kc.Run(nil, "-n", ns, "get", "secret", name, "-o", "json")
	switch {
	case err == nil:
	case kubeNotFound(err, "secrets", name):
		data := map[string]string{}
		for _, k := range keys {
			v, err := k.value()
			if err != nil {
				return res, err
			}
			data[k.name] = v
		}
		body, _ := json.Marshal(map[string]any{
			"apiVersion": "v1", "kind": "Secret", "type": "Opaque",
			"metadata":   map[string]any{"name": name, "namespace": ns, "labels": map[string]string{"app.kubernetes.io/part-of": "darkplum"}},
			"stringData": data,
		})
		// create, not apply: apply would copy the values into a last-applied annotation.
		if _, err := kc.Run(body, "create", "-f", "-"); err != nil {
			return res, fmt.Errorf("create secret %s: %w", name, err)
		}
		res.Created = true
		return res, nil
	default:
		return res, fmt.Errorf("could not read secret %s (refusing to proceed, nothing was changed): %w", name, err)
	}

	have, err := secretKeysPresent(out)
	if err != nil {
		return res, fmt.Errorf("secret %s: %w", name, err)
	}
	if pair {
		var have2, missing []string
		for _, k := range keys {
			if have[k.name] {
				have2 = append(have2, k.name)
			} else {
				missing = append(missing, k.name)
			}
		}
		if len(have2) > 0 && len(missing) > 0 {
			return res, fmt.Errorf("secret %s has %s but not %s; refusing to guess the other half (regenerating the key would change every pseudonym a vendor has seen). Nothing was changed",
				name, strings.Join(have2, ", "), strings.Join(missing, ", "))
		}
	}
	// A Secret written by an older flow with client-side apply carries its own
	// values, in clear text, in the last-applied-configuration annotation.
	if hasLastApplied(out) {
		if err := stripLastApplied(kc, ns, name); err != nil {
			return secretOutcome{}, err
		}
		res.StrippedAnnotation = true
	}
	patch := map[string]string{}
	for _, k := range keys {
		switch {
		case !have[k.name]:
			res.Added = append(res.Added, k.name)
		case k.overwrite:
			res.Overwritten = append(res.Overwritten, k.name)
		default:
			continue
		}
		v, err := k.value()
		if err != nil {
			return secretOutcome{}, err
		}
		patch[k.name] = v
	}
	if len(patch) == 0 {
		return res, nil
	}
	body, _ := json.Marshal(map[string]any{"stringData": patch})
	if _, err := kc.Run(body, "-n", ns, "patch", "secret", name, "--type", "merge", "--patch-file", "/dev/stdin"); err != nil {
		return secretOutcome{}, fmt.Errorf("patch secret %s: %w", name, err)
	}
	sort.Strings(res.Added)
	sort.Strings(res.Overwritten)
	return res, nil
}

const lastAppliedAnnotation = "kubectl.kubernetes.io/last-applied-configuration"

func hasLastApplied(secretJSON string) bool {
	var s struct {
		Metadata struct {
			Annotations map[string]string `json:"annotations"`
		} `json:"metadata"`
	}
	if json.Unmarshal([]byte(secretJSON), &s) != nil {
		return false
	}
	_, ok := s.Metadata.Annotations[lastAppliedAnnotation]
	return ok
}

// stripLastApplied removes the annotation and nothing else: a merge patch of one
// metadata key, so the Secret's values are not touched.
func stripLastApplied(kc *kube.Client, ns, name string) error {
	body, _ := json.Marshal(map[string]any{"metadata": map[string]any{"annotations": map[string]any{lastAppliedAnnotation: nil}}})
	if _, err := kc.Run(body, "-n", ns, "patch", "secret", name, "--type", "merge", "--patch-file", "/dev/stdin"); err != nil {
		return fmt.Errorf("remove the clear-text last-applied annotation from secret %s: %w", name, err)
	}
	return nil
}

// secretKeysPresent reads the names of the keys with a non-empty value from a
// Secret's JSON. The values themselves are not kept.
func secretKeysPresent(secretJSON string) (map[string]bool, error) {
	var s struct {
		Data map[string]string `json:"data"`
	}
	if err := json.Unmarshal([]byte(secretJSON), &s); err != nil {
		return nil, errors.New("unexpected answer reading the secret")
	}
	have := map[string]bool{}
	for k, v := range s.Data {
		if strings.TrimSpace(v) != "" {
			have[k] = true
		}
	}
	return have, nil
}

// ensureAuthSecret makes darkplum-auth hold the signing key, the service-key
// secret and the OIDC settings. oidc nil is a deploy: missing OIDC keys get the
// "unconfigured" placeholder and installed values are kept. oidc set (all four
// keys) is `sso configure`: those four are written even if present.
func ensureAuthSecret(kc *kube.Client, ns string, oidc map[string]string) (secretOutcome, error) {
	if oidc != nil {
		for _, k := range oidcKeys {
			if strings.TrimSpace(oidc[k]) == "" {
				return secretOutcome{}, fmt.Errorf("%s is required to configure SSO", k)
			}
		}
	}
	keys := make([]secretKey, 0, len(oidcKeys)+2)
	for _, k := range oidcKeys {
		if oidc != nil {
			keys = append(keys, secretKey{name: k, value: fixed(oidc[k]), overwrite: true})
		} else {
			keys = append(keys, secretKey{name: k, value: fixed(oidcUnset)})
		}
	}
	keys = append(keys,
		secretKey{name: keySessionSigning, value: randomKey},
		secretKey{name: keyServiceKey, value: randomKey})
	return ensureSecret(kc, ns, authSecretName, keys, false)
}

// ensureUserHMAC makes darkplum-user-hmac hold the per-install key the gateway
// uses to pseudonymise the OpenAI `user` field. Regenerating it would change every
// pseudonym a vendor has already seen, so an installed value is never replaced.
func ensureUserHMAC(kc *kube.Client, ns string) (secretOutcome, error) {
	return ensureSecret(kc, ns, userHMACSecretName, []secretKey{
		{name: keyUserHMAC, value: randomKey},
		{name: keyUserHMACID, value: fixed("k1")},
	}, true)
}

// applyPostgresSecret writes the database URL from Terraform's output. Unlike the
// others it is always replaced: Terraform owns the value. Server-side apply keeps
// the URL out of a last-applied-configuration annotation.
func applyPostgresSecret(kc *kube.Client, ns, databaseURL string) error {
	if strings.TrimSpace(databaseURL) == "" {
		return errors.New("the database_url output is empty")
	}
	body, _ := json.Marshal(map[string]any{
		"apiVersion": "v1", "kind": "Secret", "type": "Opaque",
		"metadata":   map[string]any{"name": postgresSecretName, "namespace": ns, "labels": map[string]string{"app.kubernetes.io/part-of": "darkplum"}},
		"stringData": map[string]string{"DATABASE_URL": databaseURL},
	})
	if _, err := kc.Run(body, "apply", "--server-side", "--force-conflicts", "--field-manager=darkplum-deploy", "-f", "-"); err != nil {
		return fmt.Errorf("write secret %s: %w", postgresSecretName, err)
	}
	// An install made by the Makefile flow has the old URL in clear text in this
	// annotation; server-side apply does not remove it.
	return stripLastApplied(kc, ns, postgresSecretName)
}
