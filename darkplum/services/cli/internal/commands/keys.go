package commands

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"
	"time"
	"unicode"

	"golang.org/x/term"

	darkplum "github.com/scalecraft-dev/darkplum"
	"github.com/scalecraft-dev/darkplum/cli/internal/kube"
	"github.com/scalecraft-dev/darkplum/cli/internal/vendor"
)

// KeysCmd groups vendor API key operations.
//
// A key lives in one place: the darkplum-provider-keys Secret (inference keys) or
// darkplum-billing-keys (billing keys) in the control-plane namespace. It is
// read from a hidden prompt or stdin, or from the vendor's usual environment
// variable for scripts, and is sent to the vendor in an HTTP header and to the
// cluster on kubectl's stdin. It is never an argument of any process.
type KeysCmd struct {
	Set    KeysSetCmd    `cmd:"" help:"Install or rotate a vendor API key (anthropic, openai) or the Hugging Face token (huggingface)."`
	Remove KeysRemoveCmd `cmd:"" help:"Remove a vendor API key and its catalog filter."`
	List   KeysListCmd   `cmd:"" help:"Show which keys are installed and their catalog filters (never the keys)."`
}

// KeysSetCmd is `darkplum keys set`.
type KeysSetCmd struct {
	Vendor      string `arg:"" enum:"anthropic,openai,huggingface" help:"anthropic, openai or huggingface."`
	Models      string `help:"Catalog filter: comma-separated globs of model ids that may ever be enabled, e.g. 'claude-*'. Required the first time; kept after that."`
	Workspace   string `help:"Anthropic workspace id (wrkspc_...) for a key created at organisation level. Kept from the last run when omitted."`
	NoWorkspace bool   `name:"no-workspace" help:"Clear the stored Anthropic workspace id."`
	AllowAll    bool   `name:"allow-all" help:"Allow a filter that admits every model the key can reach, including ones not yet released."`
	SkipVerify  bool   `name:"skip-verify" help:"Do not check the key against the vendor first."`
	Billing     bool   `help:"Store the vendor's billing (admin) key used for cost reconciliation, not an inference key."`
	KeyStdin    bool   `name:"key-stdin" help:"Read the key from stdin instead of prompting. ANTHROPIC_API_KEY, OPENAI_API_KEY, ANTHROPIC_ADMIN_KEY, OPENAI_ADMIN_KEY and HF_TOKEN are also read when set."`
	Context     string `name:"context" help:"Local kind context (kind-*) to act on, for a cluster Darkplum did not install. An install is found from its settings. A loopback address can still be an ssh tunnel to a remote cluster: the flag cannot tell, so do not point it at one." default:""`
}

// KeysRemoveCmd is `darkplum keys remove`.
type KeysRemoveCmd struct {
	Vendor  string `arg:"" enum:"anthropic,openai" help:"anthropic or openai."`
	Billing bool   `help:"Remove the billing key instead of the inference key."`
	Context string `name:"context" help:"Local kind context (kind-*) to act on, for a cluster Darkplum did not install. An install is found from its settings. A loopback address can still be an ssh tunnel to a remote cluster: the flag cannot tell, so do not point it at one." default:""`
}

// KeysListCmd is `darkplum keys list`.
type KeysListCmd struct {
	Context string `name:"context" help:"Local kind context (kind-*) to act on, for a cluster Darkplum did not install. An install is found from its settings. A loopback address can still be an ssh tunnel to a remote cluster: the flag cannot tell, so do not point it at one." default:""`
}

// keysEnv is everything a keys command touches outside the cluster, so tests
// can replace it.
type keysEnv struct {
	out     io.Writer
	kc      *kube.Client
	getenv  func(string) string
	readKey func(fromStdin bool, label string) (string, error)
	check   *vendor.Checker
	now     func() time.Time
	// interactive is true at a terminal. A key found in the environment there is
	// shown (last 4 characters) and confirmed before use: an exported variable is
	// easy to forget, and the key it holds may belong to something else.
	interactive bool
	confirm     func(label string) (bool, error)
	k8s         fs.FS // the bundle's k8s/ tree (the inference namespace manifest)
}

func newKeysEnv(g *Globals, contextFlag string) (*keysEnv, error) {
	// Resolve the cluster BEFORE asking for a key, so a wrong cluster fails first.
	kc, err := installKube(os.Stdout, g.Namespace, contextFlag, g.Install)
	if err != nil {
		return nil, err
	}
	k8s, err := fs.Sub(darkplum.Tree(), "k8s")
	if err != nil {
		return nil, err
	}
	return &keysEnv{out: os.Stdout, kc: kc, getenv: os.Getenv, readKey: readPassword,
		check: vendor.NewChecker(), now: time.Now, k8s: k8s,
		interactive: term.IsTerminal(int(os.Stdin.Fd())), confirm: confirmYN}, nil
}

// Run executes `darkplum keys set`.
func (c *KeysSetCmd) Run(g *Globals) error {
	if err := c.validate(); err != nil {
		return err
	}
	env, err := newKeysEnv(g, c.Context)
	if err != nil {
		return err
	}
	return env.set(context.Background(), c)
}

// Run executes `darkplum keys remove`.
func (c *KeysRemoveCmd) Run(g *Globals) error {
	env, err := newKeysEnv(g, c.Context)
	if err != nil {
		return err
	}
	return env.remove(c)
}

// Run executes `darkplum keys list`.
func (c *KeysListCmd) Run(g *Globals) error {
	env, err := newKeysEnv(g, c.Context)
	if err != nil {
		return err
	}
	return env.list()
}

// validate rejects flag combinations that make no sense, before anything runs.
func (c *KeysSetCmd) validate() error {
	hf := c.Vendor == "huggingface"
	switch {
	case hf && (c.Models != "" || c.Workspace != "" || c.NoWorkspace || c.AllowAll || c.SkipVerify || c.Billing):
		return errors.New("keys set huggingface takes only --key-stdin and --context")
	case c.Billing && (c.Models != "" || c.Workspace != "" || c.NoWorkspace || c.AllowAll || c.SkipVerify):
		return errors.New("--billing stores the admin key only: --models, --workspace, --no-workspace, --allow-all and --skip-verify do not apply (the key is always checked)")
	case c.Workspace != "" && c.NoWorkspace:
		return errors.New("--workspace and --no-workspace contradict each other")
	case (c.Workspace != "" || c.NoWorkspace) && c.Vendor != "anthropic":
		return errors.New("--workspace is an Anthropic setting")
	case c.Workspace != "" && !vendor.ValidWorkspace(c.Workspace):
		return errors.New("--workspace must be an Anthropic workspace id such as wrkspc_01AbC... Nothing was changed.")
	}
	return nil
}

func (e *keysEnv) set(ctx context.Context, c *KeysSetCmd) error {
	if c.Vendor == "huggingface" {
		return e.setHuggingFace(c)
	}
	info, _ := vendor.Get(vendor.Name(c.Vendor))
	if c.Billing {
		return e.setBilling(ctx, info, c)
	}
	return e.setInference(ctx, info, c)
}

// ---- reading the cluster ----------------------------------------------------

// configData reads a ConfigMap's data. A ConfigMap that does not exist is
// (nil, false, nil); any other failure is an error, never "empty".
func (e *keysEnv) configData(name string) (map[string]string, bool, error) {
	out, err := e.kc.NS(nil, "get", "configmap", name, "-o", "json")
	if kubeNotFound(err, "configmaps", name) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("read ConfigMap %s: %w", name, err)
	}
	var cm struct {
		Data map[string]string `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &cm); err != nil {
		return nil, false, fmt.Errorf("read ConfigMap %s: %w", name, err)
	}
	return cm.Data, true, nil
}

// secretKeys lists the names (never the values) in a Secret's data, in the
// namespace of the control plane unless ns is set.
func (e *keysEnv) secretKeys(name, ns string) (map[string]bool, bool, error) {
	args := []string{"get", "secret", name, "-o", `go-template={{range $k, $v := .data}}{{$k}}{{"\n"}}{{end}}`}
	var out string
	var err error
	if ns != "" {
		out, err = e.kc.Run(nil, append([]string{"-n", ns}, args...)...)
	} else {
		out, err = e.kc.NS(nil, args...)
	}
	if kubeNotFound(err, "secrets", name) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("read Secret %s: %w", name, err)
	}
	keys := map[string]bool{}
	for _, l := range strings.Split(out, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			keys[l] = true
		}
	}
	return keys, true, nil
}

func (e *keysEnv) ensure(kind, name string) error {
	if _, err := e.kc.NS(nil, "get", kind, name); err == nil {
		return nil
	} else if !kubeNotFound(err, kind+"s", name) {
		return fmt.Errorf("read %s %s: %w", kind, name, err)
	}
	if kind == "secret" {
		_, err := e.kc.NS(nil, "create", "secret", "generic", name)
		return err
	}
	_, err := e.kc.NS(nil, "create", kind, name)
	return err
}

func (e *keysEnv) mergePatch(kind, name string, body any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	_, err = e.kc.NS(b, "patch", kind, name, "--type", "merge", "--patch-file", "/dev/stdin")
	return err
}

// removeKeys removes data keys with one JSON patch; the keys are known to exist.
func (e *keysEnv) removeKeys(kind, name string, keys []string) error {
	if len(keys) == 0 {
		return nil
	}
	ops := make([]map[string]string, 0, len(keys))
	for _, k := range keys {
		ops = append(ops, map[string]string{"op": "remove", "path": "/data/" + k})
	}
	b, _ := json.Marshal(ops)
	_, err := e.kc.NS(b, "patch", kind, name, "--type", "json", "--patch-file", "/dev/stdin")
	return err
}

func (e *keysEnv) restartGateway() error {
	if _, err := e.kc.NS(nil, "rollout", "restart", "deploy/darkplum-gateway"); err != nil {
		return fmt.Errorf("restart the gateway: %w", err)
	}
	if _, err := e.kc.NS(nil, "rollout", "status", "deploy/darkplum-gateway", "--timeout=180s"); err != nil {
		return fmt.Errorf("the gateway did not become ready after the restart: %w", err)
	}
	return nil
}

// ---- reading the key --------------------------------------------------------

// key is read from stdin (--key-stdin), else the vendor's environment variable
// (for scripts; read here, so it is no flag and never in argv), else a hidden prompt.
func (e *keysEnv) key(stdin bool, envName, label string) (string, error) {
	var k string
	var err error
	fromEnv := !stdin && e.getenv(envName) != ""
	if fromEnv && e.interactive {
		v := strings.TrimSpace(e.getenv(envName))
		tail := v
		if len(tail) > 4 {
			tail = tail[len(tail)-4:]
		}
		fmt.Fprintf(e.out, "Found %s in your environment (ends in %q).\n", envName, tail)
		ok, cerr := e.confirm("Use that key?")
		if cerr != nil {
			return "", cerr
		}
		fromEnv = ok
	}
	switch {
	case stdin:
		k, err = e.readKey(true, label)
	case fromEnv:
		k = e.getenv(envName)
	default:
		k, err = e.readKey(false, label)
	}
	if err != nil {
		return "", err
	}
	k = strings.TrimSpace(k)
	if k == "" {
		return "", fmt.Errorf("no key given: pipe it with --key-stdin, set %s, or type it at the prompt", envName)
	}
	return k, nil
}

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

// ---- inference keys ---------------------------------------------------------

func (e *keysEnv) setInference(ctx context.Context, info vendor.Info, c *KeysSetCmd) error {
	key, err := e.key(c.KeyStdin, info.EnvKey, info.Display+" API key")
	if err != nil {
		return err
	}
	if !vendor.ValidKey(key) {
		return fmt.Errorf("the key contains characters no %s key has. Nothing was changed.", info.Display)
	}

	cm, _, err := e.configData(vendor.ProviderConfigMap)
	if err != nil {
		return err
	}
	oldFilter := cm[info.FilterKey]
	filter := strings.ReplaceAll(c.Models, " ", "")
	if filter == "" {
		filter = oldFilter
	}
	if filter == "" {
		return fmt.Errorf("--models is required the first time, e.g. --models '%s'.\n"+
			"It is the catalog filter: only %s models matching these globs can ever be enabled.", info.Example, info.Display)
	}
	patterns := vendor.Patterns(filter)
	if err := vendor.CheckPatterns(patterns, c.AllowAll); err != nil {
		var bw *vendor.BareWildcardError
		if errors.As(err, &bw) {
			return fmt.Errorf("--models=%q admits every model this key can reach, including ones %s has not released yet.\n"+
				"Nothing below the gateway bounds a vendor key, so name a family, e.g. --models '%s'.\n"+
				"(--allow-all overrides. Nothing was changed.)", bw.Pattern, info.Display, info.Example)
		}
		return fmt.Errorf("%w. Nothing was changed.", err)
	}

	// The stored workspace is kept unless --workspace replaces it or
	// --no-workspace clears it, so rotating a key never silently drops it.
	oldWS := cm[vendor.AnthropicWorkspaceID]
	workspace := oldWS
	switch {
	case c.NoWorkspace:
		workspace = ""
	case c.Workspace != "":
		workspace = c.Workspace
	}
	if info.Name != vendor.Anthropic {
		workspace, oldWS = "", ""
	}

	if !c.SkipVerify {
		fmt.Fprintf(e.out, "Checking the key against %s...\n", info.Display)
		ids, err := e.check.Models(ctx, info.Name, key, workspace)
		switch {
		case errors.Is(err, vendor.ErrWorkspaceNeeded):
			return fmt.Errorf("this key was created at organisation level, so %s needs a workspace id with it:\n"+
				"       darkplum keys set %s --workspace wrkspc_...   (Anthropic console > Workspaces), or use a key created inside a workspace.\n"+
				"       Nothing was changed.", info.Display, info.Name)
		case err != nil:
			var un *vendor.UnreachableError
			var rej *vendor.RejectedError
			switch {
			case errors.As(err, &un):
				return fmt.Errorf("could not reach %s. Nothing was changed. (--skip-verify skips this check.)", info.Display)
			case errors.As(err, &rej):
				return fmt.Errorf("%s rejected this key (HTTP %d). Nothing was changed.", info.Display, rej.Code)
			}
			return fmt.Errorf("checking the key: %w. Nothing was changed.", err)
		}
		matched := vendor.Match(ids, patterns)
		fmt.Fprintf(e.out, "  This key can reach %d %s models. The filter '%s' admits %d of them:\n", len(ids), info.Display, filter, len(matched))
		for _, id := range matched {
			fmt.Fprintf(e.out, "    %s\n", id)
		}
		fmt.Fprintf(e.out, "  A glob also admits matching models %s releases later; they arrive disabled, like any new model.\n", info.Display)
		fmt.Fprintln(e.out, "  The gateway drops ids that are plainly not chat models (embeddings, audio, image, moderation) after this filter.")
		if len(matched) == 0 {
			return errors.New("the filter matches nothing this key can reach. Nothing was changed.")
		}
	}

	// Writes. Order matters for failure: the key first, then the filter.
	have, _, err := e.secretKeys(vendor.ProviderKeysSecret, "")
	if err != nil {
		return err
	}
	hadKey := have[info.KeyFile]
	if err := e.ensure("secret", vendor.ProviderKeysSecret); err != nil {
		return fmt.Errorf("create %s: %w", vendor.ProviderKeysSecret, err)
	}
	if err := e.mergePatch("secret", vendor.ProviderKeysSecret, map[string]any{"data": map[string]string{info.KeyFile: b64(key)}}); err != nil {
		return fmt.Errorf("store the key: %w", err)
	}
	if err := e.ensure("configmap", vendor.ProviderConfigMap); err != nil {
		return fmt.Errorf("create %s: %w", vendor.ProviderConfigMap, err)
	}
	if filter != oldFilter {
		stamp := vendor.NextStamp(e.now(), cm[info.SetAtKey()])
		if err := e.mergePatch("configmap", vendor.ProviderConfigMap, map[string]any{"data": map[string]string{
			info.FilterKey: filter, info.SetAtKey(): stamp}}); err != nil {
			return fmt.Errorf("store the catalog filter: %w", err)
		}
	}
	restart := false
	if info.Name == vendor.Anthropic && workspace != oldWS {
		// The stamp is recomputed from the CURRENT one, re-read, so it outranks the
		// write just made above.
		cur, _, err := e.configData(vendor.ProviderConfigMap)
		if err != nil {
			return err
		}
		stamp := vendor.NextStamp(e.now(), cur[info.SetAtKey()])
		if err := e.mergePatch("configmap", vendor.ProviderConfigMap, map[string]any{"data": map[string]string{
			vendor.AnthropicWorkspaceID: workspace, info.SetAtKey(): stamp}}); err != nil {
			return fmt.Errorf("store the workspace id: %w", err)
		}
		restart = true
	}

	if !hadKey || filter != oldFilter || restart {
		fmt.Fprintf(e.out, "Restarting the gateway to load the %s settings...\n", info.Display)
		if err := e.restartGateway(); err != nil {
			return err
		}
	} else {
		fmt.Fprintln(e.out, "Key rotated in place. The gateway picks it up within about 90 seconds; no restart, no dropped requests.")
	}
	fmt.Fprintf(e.out, "\n%s key stored in %s/%s. It is org-wide: darkplum cannot see or verify what it is\n"+
		"scoped to, and has no spend cap of its own. Use %s.\n\n", info.Display, e.kc.Namespace(), vendor.ProviderKeysSecret, info.Console)
	fmt.Fprintln(e.out, "Nothing can answer yet. New models start disabled, and the organisation's routing policy keeps prompts")
	fmt.Fprintln(e.out, "inside your cloud account until an administrator allows otherwise:")
	fmt.Fprintln(e.out, "  Open Admin > Models and choose \"Review and enable\".")
	return nil
}

func (e *keysEnv) remove(c *KeysRemoveCmd) error {
	info, _ := vendor.Get(vendor.Name(c.Vendor))
	if c.Billing {
		return e.removeBilling(info)
	}
	secret, haveSecret, err := e.secretKeys(vendor.ProviderKeysSecret, "")
	if err != nil {
		return err
	}
	cm, haveCM, err := e.configData(vendor.ProviderConfigMap)
	if err != nil {
		return err
	}
	removed := false
	if haveSecret && secret[info.KeyFile] {
		if err := e.removeKeys("secret", vendor.ProviderKeysSecret, []string{info.KeyFile}); err != nil {
			return fmt.Errorf("remove the key: %w", err)
		}
		removed = true
	}
	if haveCM {
		var present []string
		names := []string{info.FilterKey, info.SetAtKey()}
		if info.Name == vendor.Anthropic {
			names = append(names, vendor.AnthropicWorkspaceID)
		}
		for _, k := range names {
			if _, ok := cm[k]; ok {
				present = append(present, k)
			}
		}
		if err := e.removeKeys("configmap", vendor.ProviderConfigMap, present); err != nil {
			return fmt.Errorf("remove the catalog filter: %w", err)
		}
		removed = removed || len(present) > 0
	}
	if !removed {
		fmt.Fprintf(e.out, "No %s key or filter is installed; nothing to remove.\n", info.Display)
		return nil
	}
	if err := e.restartGateway(); err != nil {
		return err
	}
	fmt.Fprintf(e.out, "%s key and filter removed. Its models keep their settings, show as unreachable, and refuse requests.\n", info.Display)
	fmt.Fprintf(e.out, "Revoke the key at %s as well: removing it here does not invalidate it.\n", info.Display)
	return nil
}

// ---- billing keys -----------------------------------------------------------

func (e *keysEnv) setBilling(ctx context.Context, info vendor.Info, c *KeysSetCmd) error {
	key, err := e.key(c.KeyStdin, info.EnvBilling, info.Display+" billing (admin) key")
	if err != nil {
		return err
	}
	if !vendor.ValidKey(key) {
		return fmt.Errorf("the key contains characters no %s key has. Nothing was changed.", info.Display)
	}
	fmt.Fprintf(e.out, "Checking that the key can read %s usage (admin scope)...\n", info.Display)
	if err := e.check.Billing(ctx, info.Name, key); err != nil {
		var rej *vendor.RejectedError
		if errors.As(err, &rej) {
			return fmt.Errorf("%s answered HTTP %d to a usage query with this key. It needs %s. Nothing was changed.", info.Display, rej.Code, info.BillingHint)
		}
		return fmt.Errorf("could not reach %s. Nothing was changed.", info.Display)
	}
	if err := e.ensure("secret", vendor.BillingKeysSecret); err != nil {
		return fmt.Errorf("create %s: %w", vendor.BillingKeysSecret, err)
	}
	if err := e.mergePatch("secret", vendor.BillingKeysSecret, map[string]any{"data": map[string]string{info.BillingFile: b64(key)}}); err != nil {
		return fmt.Errorf("store the billing key: %w", err)
	}
	fmt.Fprintf(e.out, "\n%s billing key stored in %s/%s. The api picks it up within about two minutes and\n"+
		"pulls %s's usage and cost reports once a day (and now); see Admin > Activity > Spend.\n"+
		"This key can read your organisation's workspaces and keys: keep it separate from the inference key, and revoke it at\n"+
		"%s if you remove it here.\n", info.Display, e.kc.Namespace(), vendor.BillingKeysSecret, info.Display, info.Display)
	return nil
}

func (e *keysEnv) removeBilling(info vendor.Info) error {
	have, ok, err := e.secretKeys(vendor.BillingKeysSecret, "")
	if err != nil {
		return err
	}
	if !ok || !have[info.BillingFile] {
		fmt.Fprintf(e.out, "No %s billing key is installed; nothing to remove.\n", info.Display)
		return nil
	}
	if err := e.removeKeys("secret", vendor.BillingKeysSecret, []string{info.BillingFile}); err != nil {
		return fmt.Errorf("remove the billing key: %w", err)
	}
	fmt.Fprintf(e.out, "%s billing key removed. Estimated spend keeps working; vendor-reported figures stop updating. Revoke the key at %s too.\n",
		info.Display, info.Display)
	return nil
}

// ---- Hugging Face -----------------------------------------------------------

const hfSecret = "huggingface-token"

func (e *keysEnv) setHuggingFace(c *KeysSetCmd) error {
	tok, err := e.key(c.KeyStdin, "HF_TOKEN", "Hugging Face token")
	if err != nil {
		return err
	}
	if strings.IndexFunc(tok, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) >= 0 {
		return errors.New("the token contains whitespace or control characters no Hugging Face token has. Nothing was changed.")
	}
	if _, err := e.kc.Run(nil, "get", "namespace", inferenceNamespace); err != nil {
		if !kubeNotFound(err, "namespaces", inferenceNamespace) {
			return fmt.Errorf("read namespace %s: %w", inferenceNamespace, err)
		}
		ns, err := fs.ReadFile(e.k8s, "base/namespaces/"+inferenceNamespace+".yaml")
		if err != nil {
			return fmt.Errorf("this build carries no %s namespace manifest: %w", inferenceNamespace, err)
		}
		if _, err := e.kc.Run(ns, "apply", "-f", "-"); err != nil {
			return fmt.Errorf("create namespace %s: %w", inferenceNamespace, err)
		}
	}
	// Server-side apply: client-side apply would write the token into the
	// last-applied-configuration annotation, in clear text, on the Secret itself.
	secret, _ := json.Marshal(map[string]any{"apiVersion": "v1", "kind": "Secret", "type": "Opaque",
		"metadata":   map[string]string{"name": hfSecret, "namespace": inferenceNamespace},
		"stringData": map[string]string{"token": tok}})
	if _, err := e.kc.Run(secret, "apply", "--server-side", "--force-conflicts", "--field-manager=darkplum-cli", "-f", "-"); err != nil {
		return fmt.Errorf("store the token: %w", err)
	}
	// A token stored by the Makefile flow (client-side apply) is also readable in
	// this annotation; a rotation must not leave the old one there.
	if _, err := e.kc.Run(nil, "-n", inferenceNamespace, "annotate", "secret", hfSecret, "kubectl.kubernetes.io/last-applied-configuration-"); err != nil {
		return fmt.Errorf("the token is stored, but the old copy in the Secret's last-applied annotation could not be removed: %w", err)
	}
	fmt.Fprintf(e.out, "Hugging Face token stored in %s/%s. New model downloads use it; restart an existing\n"+
		"deployment to pick it up:\n  %s\n", inferenceNamespace, hfSecret,
		kubectlHint("rollout restart -n "+inferenceNamespace+" deploy/<vllm-deployment>"))
	return nil
}

// ---- list -------------------------------------------------------------------

func (e *keysEnv) list() error {
	prov, _, err := e.secretKeys(vendor.ProviderKeysSecret, "")
	if err != nil {
		return err
	}
	bill, _, err := e.secretKeys(vendor.BillingKeysSecret, "")
	if err != nil {
		return err
	}
	cm, _, err := e.configData(vendor.ProviderConfigMap)
	if err != nil {
		return err
	}
	hf, _, err := e.secretKeys(hfSecret, inferenceNamespace)
	if err != nil {
		return err
	}
	yes := func(b bool) string {
		if b {
			return sSuccess("installed")
		}
		return sDim("not installed")
	}
	for _, n := range []vendor.Name{vendor.Anthropic, vendor.OpenAI} {
		info, _ := vendor.Get(n)
		fmt.Fprintf(e.out, "%s\n", sHeader("▸ "+info.Display))
		fmt.Fprintf(e.out, "  key      %s\n", yes(prov[info.KeyFile]))
		filter := cm[info.FilterKey]
		if filter == "" {
			filter = sDim("(none)")
		}
		fmt.Fprintf(e.out, "  filter   %s\n", filter)
		if at := cm[info.SetAtKey()]; at != "" {
			fmt.Fprintf(e.out, "  set at   %s\n", at)
		}
		if n == vendor.Anthropic {
			if ws := cm[vendor.AnthropicWorkspaceID]; ws != "" {
				fmt.Fprintf(e.out, "  workspace %s\n", ws)
			}
		}
		fmt.Fprintf(e.out, "  billing  %s\n", yes(bill[info.BillingFile]))
	}
	fmt.Fprintf(e.out, "%s\n  token    %s\n", sHeader("▸ Hugging Face"), yes(hf["token"]))
	return nil
}

// confirmYN asks a yes/no question on the terminal; the default is no.
func confirmYN(label string) (bool, error) {
	ans := strings.ToLower(promptLine(label + " [y/N]"))
	return ans == "y" || ans == "yes", nil
}
