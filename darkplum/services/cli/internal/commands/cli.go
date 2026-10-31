package commands

// CLI is the kong grammar for the darkplum operator CLI.
type CLI struct {
	Namespace string `help:"Control-plane namespace." default:"darkplum-system"`
	Install   string `help:"Install (workspace) to act on. Needed only when there is more than one; never taken from the environment." default:""`

	Admin      AdminCmd         `cmd:"" help:"Manage admin accounts."`
	Sso        SSOCmd           `cmd:"" name:"sso" help:"Configure SSO / OIDC."`
	Status     StatusCmd        `cmd:"" help:"Show control-plane and inference health."`
	Init       InitCmd          `cmd:"" help:"Prompt for install settings and write terraform.tfvars (run before make deploy)."`
	Deploy     DeployCmd        `cmd:"" help:"Create or update the install: infrastructure, verified images, the platform. Safe to re-run; it is also how you upgrade."`
	Destroy    DestroyCmd       `cmd:"" help:"Delete the install's cloud resources (asks you to type the cluster name)."`
	Configure  ConfigureCmd     `cmd:"" help:"First run after make deploy: create the first admin, enable models, send a test chat, open the web UI."`
	Kubeconfig KubeconfigCmd    `cmd:"" help:"Validate the install's cluster and print export KUBECONFIG=<its private kubeconfig> (never touches ~/.kube)."`
	Doctor     DoctorCmd        `cmd:"" help:"Read-only environment and health diagnostics."`
	VerifyRel  VerifyReleaseCmd `cmd:"" name:"verify-release" hidden:"" help:"Verify, anonymously, that this build's images were signed by the release workflow (run by CI right after signing)."`
	Version    VersionCmd       `cmd:"" help:"Show this build: version, channel, the images it deploys (by digest) and the third-party images it pins."`
}

// Globals carries values shared across commands, bound into each Run method.
type Globals struct {
	Namespace string
	Install   string
}
