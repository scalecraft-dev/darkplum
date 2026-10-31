package commands

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/scalecraft-dev/darkplum/cli/internal/install"
)

// `darkplum init --adopt` takes over an install that the Makefile flow created:
// a Terraform root (terraform/aws) holding backend.tf and terraform.tfvars, whose
// state lives in S3. The state is NOT moved: the new workspace records where it
// already is. The two files are copied into ~/.darkplum/installs/<name>/ and the
// originals are renamed to *.adopted, so a stale checkout can no longer run
// Terraform against the same state by accident, and so undoing the adoption is
// two `mv` commands.
//
// Only S3-backed installs are adopted. An install on a local state file would
// need that file moved or copied, and the safe way to do that (hash it, count
// resources, keep the original) is not built; it is refused with instructions.

var (
	// bucketNameRe is the bucket `darkplum init` creates:
	// darkplum-tfstate-<account>-<region>-<suffix>.
	bucketNameRe = regexp.MustCompile(`^darkplum-tfstate-([0-9]{12})-([a-z0-9-]+?)-[a-z0-9]{6}$`)
	backendS3Re  = regexp.MustCompile(`(?m)^\s*backend\s+"s3"\s*\{`)
	backendAnyRe = regexp.MustCompile(`(?m)^\s*backend\s+"([a-z0-9]+)"\s*\{`)
)

const adoptedSuffix = ".adopted"

func (c *InitCmd) runAdopt(w io.Writer) error {
	home, err := install.DefaultHome()
	if err != nil {
		return err
	}
	return c.adopt(w, home, time.Now)
}

func (c *InitCmd) adopt(w io.Writer, home install.Home, now func() time.Time) error {
	dir := c.Dir
	if dir == "" {
		dir = filepath.Join("terraform", "aws")
	}
	fmt.Fprintf(w, "%s\n\n", sHeader("▸ init --adopt"))
	refuse := func(name, detail, hint string) error {
		printCheckTo(w, checkResult{name: name, status: checkFail, detail: detail, hint: hint})
		return reportedError{"nothing was adopted"}
	}

	tfvarsPath := filepath.Join(dir, "terraform.tfvars")
	backendPath := filepath.Join(dir, "backend.tf")
	tfvarsBytes, err := os.ReadFile(tfvarsPath)
	if err != nil {
		return refuse("install settings", "no terraform.tfvars in "+dir,
			"run this from the darkplum/ directory of the Makefile-era install, or pass --dir <its terraform/aws directory>")
	}
	backendBytes, err := os.ReadFile(backendPath)
	if err != nil {
		return refuse("state backend", "no backend.tf in "+dir+": this install keeps its state in a local file",
			"adopting a local-state install is not supported yet. Move its state to S3 first (re-run the old `./bin/darkplum init --backend s3` and `terraform init -migrate-state`), then adopt")
	}
	if m := backendAnyRe.FindSubmatch(backendBytes); m == nil || string(m[1]) != "s3" {
		kind := "none"
		if m != nil {
			kind = string(m[1])
		}
		return refuse("state backend", "backend.tf declares backend "+kind+", not s3",
			"only installs whose state is in an S3 bucket can be adopted")
	}
	if !backendS3Re.Match(backendBytes) {
		return refuse("state backend", "backend.tf has no s3 backend block", "restore the backend.tf `darkplum init` wrote")
	}

	be := parseTFVars(string(backendBytes))
	first := func(m map[string]string, k string) string {
		if s := tfStrings(m[k]); len(s) > 0 {
			return s[0]
		}
		return ""
	}
	tf, err := readTFVars(tfvarsPath)
	if err != nil {
		return refuse("install settings", firstLine(err.Error()), "")
	}
	rec := install.Record{
		ClusterName: "darkplum",
		Region:      tf.Region,
		Profile:     tf.Profile,
		AccountIDs:  tf.AccountIDs,
		Created:     now().UTC(),
		AdoptedFrom: absOrSame(dir),
		Backend: install.Backend{
			Type: "s3", Bucket: first(be, "bucket"), Key: first(be, "key"), Region: first(be, "region"),
			KMSKeyID: first(be, "kms_key_id"), Encrypt: strings.TrimSpace(be["encrypt"]) == "true",
			UseLockfile: strings.TrimSpace(be["use_lockfile"]) == "true",
		},
	}
	if cn := first(tf.Raw, "cluster_name"); cn != "" {
		rec.ClusterName = cn
	}
	rec.Name = c.Install
	if rec.Name == "" {
		rec.Name = rec.ClusterName
	}
	rec.Schema = 1
	if err := rec.Validate(); err != nil {
		hint := "re-run `./bin/darkplum init` in the old checkout so the install is pinned to one AWS account and region"
		return refuse("install record", firstLine(err.Error()), hint)
	}

	// The settings and the backend must describe the same install.
	if rec.Backend.Region != rec.Region {
		return refuse("state backend", fmt.Sprintf("the state bucket is in %s, the install in %s", rec.Backend.Region, rec.Region),
			"adopt only installs as `darkplum init` created them")
	}
	if m := bucketNameRe.FindStringSubmatch(rec.Backend.Bucket); m != nil {
		pinned := false
		for _, a := range rec.AccountIDs {
			pinned = pinned || a == m[1]
		}
		if !pinned || m[2] != rec.Region {
			return refuse("state backend", fmt.Sprintf("bucket %s belongs to account %s in %s, not to this install (%s in %s)",
				rec.Backend.Bucket, m[1], m[2], strings.Join(rec.AccountIDs, ","), rec.Region),
				"a state bucket from another account or region would adopt someone else's state; check backend.tf")
		}
	} else {
		printCheckTo(w, checkResult{name: "state bucket", status: checkWarn,
			detail: rec.Backend.Bucket + " is not a bucket `darkplum init` created", hint: "it is used as written; confirm it is this install's"})
	}
	if bc, err := readBackendCache(dir); err == nil && bc != nil {
		if bc.Bucket != rec.Backend.Bucket || bc.Key != rec.Backend.Key {
			return refuse("state backend", fmt.Sprintf("Terraform last initialised this directory against s3://%s/%s, but backend.tf says s3://%s/%s",
				bc.Bucket, bc.Key, rec.Backend.Bucket, rec.Backend.Key),
				"the checkout is half-way between two states; resolve it with the old flow first, then adopt")
		}
	}

	in, err := install.Open(home, rec.Name)
	if err != nil {
		return refuse("install name", firstLine(err.Error()), "choose another with --install")
	}
	if _, err := in.Load(); err == nil {
		return refuse("workspace", "an install named "+rec.Name+" already exists in "+string(home),
			"it was adopted or created before; use `--install NAME` for a different name, or remove "+in.Dir()+" if it is a leftover")
	} else if !errors.Is(err, install.ErrNoInstall) {
		return refuse("workspace", firstLine(err.Error()), "remove "+in.Dir()+" if it is a leftover")
	}

	bucketURL := "s3://" + rec.Backend.Bucket + "/" + rec.Backend.Key
	printCheckTo(w, checkResult{name: "install", status: checkPass,
		detail: fmt.Sprintf("%s · %s · account %s", rec.Name, rec.Region, strings.Join(rec.AccountIDs, ","))})
	printCheckTo(w, checkResult{name: "state", status: checkPass, detail: bucketURL + " (stays where it is; nothing is moved)"})
	if c.DryRun {
		fmt.Fprintf(w, "\n%s\n", sDim("dry run: would create "+in.Dir()+" and rename "+backendPath+" and "+tfvarsPath+" to *"+adoptedSuffix))
		return nil
	}

	// 1. build the workspace completely, 2. read it back, 3. only then rename the originals.
	if err := in.Save(rec); err != nil {
		return refuse("workspace", firstLine(err.Error()), "")
	}
	cleanup := func() { _ = os.RemoveAll(in.Dir()) }
	if err := in.WriteFile("terraform.tfvars", tfvarsBytes); err != nil {
		cleanup()
		return refuse("workspace", firstLine(err.Error()), "")
	}
	if err := in.WriteFile("backend.hcl", []byte(renderBackendHCL(rec.Backend))); err != nil {
		cleanup()
		return refuse("workspace", firstLine(err.Error()), "")
	}
	back, err := os.ReadFile(in.TFVars())
	got, lerr := in.Load()
	if err != nil || lerr != nil || sha256.Sum256(back) != sha256.Sum256(tfvarsBytes) || got.Backend != rec.Backend || got.Name != rec.Name {
		cleanup()
		return refuse("workspace", "the new workspace did not read back as written", "nothing was changed in "+dir)
	}

	stamp := now().UTC().Format("20060102T150405Z")
	moved, err := renameAside(stamp, backendPath, tfvarsPath)
	if err != nil {
		cleanup() // the originals were restored by renameAside
		return refuse("original files", firstLine(err.Error()), "nothing was changed in "+dir+" and no workspace was kept")
	}
	printCheckTo(w, checkResult{name: "workspace", status: checkPass, detail: in.Dir()})
	for _, m := range moved {
		printCheckTo(w, checkResult{name: "renamed", status: checkPass, detail: m[0] + " -> " + filepath.Base(m[1])})
	}
	fmt.Fprintf(w, "\n%s\n", sDim("to undo: mv "+shellQuote(moved[0][1])+" "+shellQuote(moved[0][0])+" && mv "+shellQuote(moved[1][1])+" "+shellQuote(moved[1][0])+" && rm -rf "+shellQuote(in.Dir())))
	fmt.Fprintf(w, "%s\n", sSuccess("✓ adopted "+rec.Name))
	return nil
}

// renameAside renames each file to <file>.adopted (with the stamp when that name
// is taken). On any failure the ones already renamed are put back.
func renameAside(stamp string, files ...string) ([][2]string, error) {
	var moved [][2]string
	undo := func() {
		for i := len(moved) - 1; i >= 0; i-- {
			_ = os.Rename(moved[i][1], moved[i][0])
		}
	}
	for _, f := range files {
		to := f + adoptedSuffix
		if _, err := os.Lstat(to); err == nil {
			to = f + adoptedSuffix + "-" + stamp
		}
		if err := os.Rename(f, to); err != nil {
			undo()
			return nil, fmt.Errorf("cannot rename %s: %w", f, err)
		}
		moved = append(moved, [2]string{f, to})
	}
	return moved, nil
}

// renderBackendHCL is the -backend-config file for `terraform init`: partial
// configuration, so the bundle's Terraform carries no bucket names.
func renderBackendHCL(b install.Backend) string {
	return "# Written by `darkplum init --adopt`. Passed to terraform init as -backend-config.\n" + backendHCLBody(b)
}

func backendHCLBody(b install.Backend) string {
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "bucket       = %s\n", hclString(b.Bucket))
	fmt.Fprintf(&buf, "key          = %s\n", hclString(b.Key))
	fmt.Fprintf(&buf, "region       = %s\n", hclString(b.Region))
	fmt.Fprintf(&buf, "encrypt      = %t\n", b.Encrypt)
	if b.KMSKeyID != "" {
		fmt.Fprintf(&buf, "kms_key_id   = %s\n", hclString(b.KMSKeyID))
	}
	fmt.Fprintf(&buf, "use_lockfile = %t\n", b.UseLockfile)
	return buf.String()
}

func absOrSame(p string) string {
	if a, err := filepath.Abs(p); err == nil {
		return a
	}
	return p
}
