package runner

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/scalecraft-dev/darkplum/cli/internal/bundle"
)

// TerraformDirs are the places a Terraform run reads and writes, all outside the
// bundle: the bundle is verified read-only and nothing Terraform produces may
// land in it.
type TerraformDirs struct {
	Workspace   string // the install's workspace: the process runs here
	Data        string // TF_DATA_DIR, per bundle key
	PluginCache string // TF_PLUGIN_CACHE_DIR, shared by installs
	BackendHCL  string // -backend-config file (absolute)
	TFVars      string // -var-file (absolute), for plan/apply
}

// Terraform runs terraform against a verified bundle. v can only come from
// bundle.Verify, so Terraform never runs from a tree that was not just checked
// against the hash shipped in this binary. The root is passed as -chdir (the
// bundle), the process runs in the workspace, and TF_DATA_DIR points into the
// workspace, so nothing terraform writes (data dir, saved plans, crash logs)
// lands in the bundle. The caller cannot pass its own -chdir.
func Terraform(v bundle.Verified, d TerraformDirs, env []string, args ...string) (stdout []byte, stderr string, err error) {
	if !v.Valid() {
		return nil, "", errors.New("runner: terraform needs a verified bundle")
	}
	for _, a := range args {
		if strings.HasPrefix(a, "-chdir") {
			return nil, "", fmt.Errorf("runner: %q is set by the runner", a)
		}
	}
	if d.Workspace == "" || d.Data == "" || !filepath.IsAbs(d.Workspace) || !filepath.IsAbs(d.Data) {
		return nil, "", errors.New("runner: terraform needs absolute workspace and data directories")
	}
	if within(d.Data, v.Dir()) || within(d.Workspace, v.Dir()) {
		return nil, "", errors.New("runner: terraform's data directory and working directory must be outside the bundle")
	}
	for _, dir := range []string{d.Data, d.PluginCache} {
		if dir != "" {
			if err := os.MkdirAll(dir, 0o700); err != nil {
				return nil, "", err
			}
		}
	}
	cmd := exec.Command("terraform", append([]string{"-chdir=" + v.TerraformDir()}, args...)...)
	cmd.Dir = d.Workspace
	cmd.Env = append(os.Environ(), "TF_DATA_DIR="+d.Data, "TF_IN_AUTOMATION=1", "TF_INPUT=0")
	if d.PluginCache != "" {
		cmd.Env = append(cmd.Env, "TF_PLUGIN_CACHE_DIR="+d.PluginCache)
	}
	cmd.Env = append(cmd.Env, env...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err = cmd.Run()
	return out.Bytes(), errb.String(), err
}

// TerraformInit initialises the backend from the install's backend.hcl with the
// lock file read-only: the providers are exactly the ones the shipped lock names,
// for this platform, or init fails.
func TerraformInit(v bundle.Verified, d TerraformDirs, profile string, env []string) (stderr string, err error) {
	if d.BackendHCL == "" || !filepath.IsAbs(d.BackendHCL) {
		return "", errors.New("runner: terraform init needs the install's absolute backend.hcl")
	}
	args := []string{"init", "-input=false", "-lockfile=readonly", "-backend-config=" + d.BackendHCL}
	if profile != "" {
		args = append(args, "-backend-config=profile="+profile)
	}
	_, stderr, err = Terraform(v, d, env, args...)
	return stderr, err
}

func within(p, dir string) bool {
	rel, err := filepath.Rel(dir, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
