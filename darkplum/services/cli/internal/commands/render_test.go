package commands

import (
	"strings"
	"testing"
)

func TestYamlSingleQuoteEscapesQuotes(t *testing.T) {
	cases := map[string]string{
		"simple":      "'simple'",
		"a'b":         "'a''b'",
		"pa$$w0rd!#":  "'pa$$w0rd!#'",
		"it's a test": "'it''s a test'",
	}
	for in, want := range cases {
		if got := yamlSingleQuote(in); got != want {
			t.Errorf("yamlSingleQuote(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSecretYAMLEmbedsQuotedPassword(t *testing.T) {
	out := string(secretYAML("res", "ns", "p'wd"))
	if !strings.Contains(out, "name: res") || !strings.Contains(out, "namespace: ns") {
		t.Fatalf("secret metadata missing:\n%s", out)
	}
	// password with an embedded quote must be single-quoted and escaped.
	if !strings.Contains(out, "password: 'p''wd'") {
		t.Fatalf("password not safely quoted:\n%s", out)
	}
}

func TestJobYAMLUsesImageAndAdminArgs(t *testing.T) {
	out := string(jobYAML("res", "ns", "darkplum-api:latest", "a@b.c", "Admin Name"))
	for _, want := range []string{
		"image: darkplum-api:latest",
		"imagePullPolicy: IfNotPresent",
		`args: ["admin", "create", "--email", 'a@b.c', "--name", 'Admin Name']`,
		"name: ADMIN_PASSWORD",
		"name: darkplum-auth",
		"name: postgres-credentials",
		"restartPolicy: Never",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("job yaml missing %q:\n%s", want, out)
		}
	}
}
