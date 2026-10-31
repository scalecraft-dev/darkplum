package commands

import (
	"fmt"

	"github.com/scalecraft-dev/darkplum/cli/internal/install"
)

const routerPitch = "Install the semantic router? An optional classifier that runs in your cluster (one pod, about 2 GiB of memory, a 1-2 GB model download from Hugging Face on first start) so routed aliases can send each request to the model that suits its topic. Aliases that do not use it are unaffected. It reads the text of the prompts routed through it, and its pod may reach any public address on port 443 (needed for the Hugging Face download at startup; private ranges stay blocked)"

// chooseRouter takes the semantic router setting from --semantic-router, or asks.
// The default is off: the router was always opt-in, and a run that does not ask
// (--yes without the flag) leaves an existing install's setting alone.
func (c *InitCmd) chooseRouter(env *initEnv, s *settings) error {
	switch c.SemanticRouter {
	case "on":
		s.router, s.routerSet = true, true
		return nil
	case "off":
		s.router, s.routerSet = false, true
		return nil
	}
	if c.Yes || !env.interactive {
		return nil
	}
	on, err := env.prompt.Confirm(routerPitch, s.router)
	if err != nil {
		return err
	}
	s.router, s.routerSet = on, true
	return nil
}

// changeRouter flips the setting on an existing install. It writes the install
// record only (the router is not Terraform's); deploy applies it.
func (c *InitCmd) changeRouter(env *initEnv, dir string, rec install.Record) error {
	w := env.out
	in, err := install.OpenDir(dir, c.ClusterName)
	if err != nil {
		return err
	}
	if rec.Name == "" {
		if rec, err = in.Load(); err != nil {
			return env.fail("install settings", firstLine(err.Error()), "choose Start over to answer the questions again", "cannot read the install record")
		}
	}
	on := rec.SemanticRouter
	switch c.SemanticRouter {
	case "on":
		on = true
	case "off":
		on = false
	default:
		if on, err = env.prompt.Confirm(routerPitch, rec.SemanticRouter); err != nil {
			return err
		}
	}
	if on == rec.SemanticRouter {
		fmt.Fprintln(w, sDim("nothing changed"))
		return nil
	}
	if c.DryRun {
		fmt.Fprintf(w, "%s\n  semantic router: %v -> %v\n%s\n", sHeader("settings (dry run — not saved)"), rec.SemanticRouter, on, sSuccess("✓ dry run complete — nothing was changed"))
		return nil
	}
	rec.SemanticRouter = on
	if err := in.Save(rec); err != nil {
		return reportedErr("could not save the install record: %v", err)
	}
	word := "off; deploy removes it"
	if on {
		word = "on; deploy installs it"
	}
	fmt.Fprintf(w, "\n%s\n", sSuccess("✓ semantic router "+word+"; every other setting is unchanged"))
	fmt.Fprintf(w, "  Next: %s to apply it\n", sCmd("darkplum deploy"))
	return nil
}
