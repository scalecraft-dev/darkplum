package commands

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/scalecraft-dev/darkplum/cli/internal/iamcheck"
	"github.com/scalecraft-dev/darkplum/cli/internal/install"
	"github.com/scalecraft-dev/darkplum/cli/internal/runner"
)

// VerifyCmd is `darkplum verify`: Layer 1 evidence. IAM permissions are the
// enforcement; this proves, with IAM's own policy simulator, that the gateway's
// role can invoke only the Bedrock models the install allows. It changes nothing
// and its output is plain text meant to be pasted into an audit ticket.
type VerifyCmd struct {
	JSON bool `name:"json" help:"Print the evidence as JSON."`
}

// verifyEnv is what verify touches outside itself, so tests can replace it.
type verifyEnv struct {
	out     io.Writer
	in      install.Install
	rec     install.Record
	aws     runner.AWSFunc
	outputs func() (map[string]json.RawMessage, error)
}

// Run executes `darkplum verify`.
func (c *VerifyCmd) Run(g *Globals) error {
	in, rec, err := openInstall(g.Install)
	if err != nil {
		printCheckTo(os.Stderr, noInstallRow(err))
		return reportedError{"no install to verify"}
	}
	e := &verifyEnv{out: os.Stdout, in: in, rec: rec,
		aws: func(args ...string) (string, error) {
			return runner.AWS(runner.ProfileArgs(rec.Profile, append([]string{"--region", rec.Region}, args...)...)...)
		},
		outputs: func() (map[string]json.RawMessage, error) { return terraformOutputs(in, rec) }}
	return e.run(context.Background(), c.JSON)
}

func (e *verifyEnv) run(ctx context.Context, asJSON bool) error {
	// 1. The credentials in use belong to the account this install is pinned to:
	// a simulation run as the wrong account proves nothing about this install.
	acct, err := e.aws("sts", "get-caller-identity", "--query", "Account", "--output", "text")
	if err != nil {
		hint := ""
		if e.rec.Profile != "" {
			hint = " Run `aws sso login --profile " + e.rec.Profile + "` and retry."
		}
		return fmt.Errorf("the AWS session is not usable: %s.%s", firstLine(err.Error()), hint)
	}
	acct = strings.TrimSpace(acct)
	if !contains(e.rec.AccountIDs, acct) {
		return fmt.Errorf("the AWS credentials are for account %s, but this install is pinned to %s. Nothing was checked.",
			acct, strings.Join(e.rec.AccountIDs, ", "))
	}

	raw, err := e.outputs()
	if err != nil {
		return fmt.Errorf("read the infrastructure outputs: %w", err)
	}
	o, err := iamcheck.FromTerraform(raw)
	if err != nil {
		return err
	}
	if o.GatewayRoleARN != "" {
		if ra, ok := iamcheck.RoleAccount(o.GatewayRoleARN); ok && !contains(e.rec.AccountIDs, ra) {
			return fmt.Errorf("the gateway role %s is in account %s, which this install is not pinned to. Nothing was checked.", o.GatewayRoleARN, ra)
		}
	}

	rep, err := iamcheck.Run(ctx, e.aws, o)
	var se *iamcheck.SimulationError
	if err != nil && !errors.As(err, &se) {
		return err
	}
	if asJSON {
		b, _ := json.MarshalIndent(map[string]any{"install": e.rec.Name, "cluster": e.rec.ClusterName, "caller_account": acct,
			"passed": err == nil && rep.Passed(), "report": rep}, "", "  ")
		fmt.Fprintln(e.out, string(b))
	} else {
		fmt.Fprintf(e.out, "darkplum verify: install %s, cluster %s, caller account %s\n", e.rec.Name, e.rec.ClusterName, acct)
		rep.WriteText(e.out)
	}
	switch {
	case se != nil:
		hint := ""
		if e.rec.Profile != "" {
			hint = " If the session expired: `aws sso login --profile " + e.rec.Profile + "`. The caller needs iam:SimulatePrincipalPolicy on the gateway role."
		}
		if !asJSON {
			// reportedError means "already shown", so the way forward is shown here.
			fmt.Fprintln(e.out, "The simulation could not run: "+se.First+"."+hint)
		}
		return reportedError{"the simulation could not run: " + se.First}
	case !rep.Passed():
		return reportedError{"Layer 1 allowlist does not behave as configured"}
	}
	return nil
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
