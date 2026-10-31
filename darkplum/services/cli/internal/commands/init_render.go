package commands

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/scalecraft-dev/darkplum/cli/internal/install"
)

// tfvarsKV is one rendered variable. Value is a Go string, int, bool or []string.
type tfvarsKV struct {
	Key   string
	Value any
}

// hclString quotes s as an HCL string literal. Besides \ and ", the template
// sequences ${ and %{ must be escaped or Terraform would interpolate them, and
// control characters must not break the one-line-per-key layout the Makefile's
// aws_profile sed relies on.
func hclString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	rs := []rune(s)
	for i, r := range rs {
		switch r {
		case '\\':
			b.WriteString(`\\`)
		case '"':
			b.WriteString(`\"`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		case '$', '%':
			if i+1 < len(rs) && rs[i+1] == '{' {
				b.WriteRune(r)
				b.WriteRune(r) // $${ and %%{ are the literal forms
			} else {
				b.WriteRune(r)
			}
		default:
			if r < 0x20 || r == 0x7f {
				fmt.Fprintf(&b, `\u%04x`, r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}

func hclValue(v any) string {
	switch x := v.(type) {
	case string:
		return hclString(x)
	case int:
		return strconv.Itoa(x)
	case bool:
		return strconv.FormatBool(x)
	case []string:
		parts := make([]string, len(x))
		for i, s := range x {
			parts[i] = hclString(s)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	}
	panic(fmt.Sprintf("hclValue: unsupported type %T", v))
}

// renderTFVars writes `key = value`, one per line, in the given order. now is
// injected so golden tests are stable; a zero time omits the timestamp.
func renderTFVars(kvs []tfvarsKV, now time.Time) string {
	var b strings.Builder
	b.WriteString("# Written by `darkplum init`")
	if !now.IsZero() {
		b.WriteString(" on " + now.UTC().Format("2006-01-02"))
	}
	b.WriteString(".\n# Change a value by re-running ./bin/darkplum init; every other variable keeps its\n# default (see variables.tf). Keep one `key = \"value\"` per line: the Makefile reads\n# aws_profile from here with sed.\n")
	for _, kv := range kvs {
		fmt.Fprintf(&b, "%s = %s\n", kv.Key, hclValue(kv.Value))
	}
	return b.String()
}

// backendFileName is the partial backend configuration init writes next to
// terraform.tfvars. The Terraform code declares `backend "s3" {}` and nothing
// else; this file supplies the bucket, key and region (terraform init
// -backend-config=backend.hcl), so the code that ships in the binary never
// names a customer's bucket and nothing is ever written into it.
const backendFileName = "backend.hcl"

// backendSpec is the S3 backend written to backend.hcl.
type backendSpec struct {
	Bucket, Key, Region, KMSKeyARN string
}

func renderBackend(s backendSpec) string {
	return "# Written by `darkplum init`: Terraform state lives in a versioned, KMS-encrypted,\n" +
		"# private S3 bucket in your own account. Native S3 locking needs Terraform >= 1.10.\n" +
		"# Passed to terraform init as -backend-config=backend.hcl (the AWS profile is added separately).\n" +
		"# WARNING: deleting the KMS key alias/darkplum-tfstate makes this state unrecoverable.\n" +
		backendHCLBody(install.Backend{Bucket: s.Bucket, Key: s.Key, Region: s.Region, KMSKeyID: s.KMSKeyARN, Encrypt: true, UseLockfile: true})
}

// writeFileAtomic writes data to path with mode 0600 via temp file + rename, so
// a crash never leaves a half-written file. An existing file is copied to
// path+".bak" first.
func writeFileAtomic(path string, data []byte) error {
	if old, err := os.ReadFile(path); err == nil {
		if err := os.WriteFile(path+".bak", old, 0o600); err != nil {
			return fmt.Errorf("back up %s: %w", path, err)
		}
		// WriteFile keeps the mode of a .bak that already existed.
		if err := os.Chmod(path+".bak", 0o600); err != nil {
			return fmt.Errorf("back up %s: %w", path, err)
		}
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".darkplum-init-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
