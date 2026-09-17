package commands

// CLI is the kong grammar for the darkplum operator CLI.
type CLI struct {
	Namespace string `help:"Control-plane namespace." default:"darkplum-system"`

	Admin  AdminCmd  `cmd:"" help:"Manage admin accounts."`
	Sso    SSOCmd    `cmd:"" name:"sso" help:"Configure SSO / OIDC."`
	Status StatusCmd `cmd:"" help:"Show control-plane and inference health."`
	Doctor DoctorCmd `cmd:"" help:"Read-only environment and health diagnostics."`
}

// Globals carries values shared across commands, bound into each Run method.
type Globals struct {
	Namespace string
}
