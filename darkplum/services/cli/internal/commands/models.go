package commands

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	darkplum "github.com/scalecraft-dev/darkplum"
	"github.com/scalecraft-dev/darkplum/cli/internal/kube"
	"github.com/scalecraft-dev/darkplum/cli/internal/manifests"
	"github.com/scalecraft-dev/darkplum/cli/internal/models"
)

// ModelsCmd groups the self-hosted model operations. Each model is one vLLM
// Deployment and one headless Service in darkplum-inference, and one entry in
// the gateway's VLLM_UPSTREAMS.
type ModelsCmd struct {
	Add    ModelsAddCmd    `cmd:"" help:"Add a self-hosted model (one vLLM deployment, one GPU)."`
	Remove ModelsRemoveCmd `cmd:"" help:"Remove a self-hosted model added with 'models add'."`
	List   ModelsListCmd   `cmd:"" help:"List self-hosted model deployments and the gateway's registered upstreams."`
	Test   ModelsTestCmd   `cmd:"" help:"Send a test chat to a self-hosted model through a tunnel to its pod (bypasses the gateway)."`
}

// ModelsAddCmd is `darkplum models add`.
type ModelsAddCmd struct {
	Name     string `arg:"" help:"Instance name: lowercase letters, digits and dashes (max 31). The deployment is vllm-NAME."`
	HF       string `name:"hf" aliases:"model" help:"Hugging Face model ID, e.g. Qwen/Qwen2.5-7B-Instruct." required:""`
	Revision string `help:"Branch, tag or commit SHA of the model repository. Pin a commit SHA so weights cannot change under you." default:"main"`
	Served   string `help:"Name the model is served as (default: NAME)."`
	MaxLen   int    `name:"max-len" help:"Maximum context length." default:"8192"`
	Context  string `name:"context" help:"Local kind context (kind-*) to act on, for a cluster Darkplum did not install. An install is found from its settings. A loopback address can still be an ssh tunnel to a remote cluster: the flag cannot tell, so do not point it at one." default:""`
}

// ModelsRemoveCmd is `darkplum models remove`.
type ModelsRemoveCmd struct {
	Name    string `arg:"" help:"Instance name given to 'models add'."`
	Context string `name:"context" help:"Local kind context (kind-*) to act on, for a cluster Darkplum did not install. An install is found from its settings. A loopback address can still be an ssh tunnel to a remote cluster: the flag cannot tell, so do not point it at one." default:""`
}

// ModelsListCmd is `darkplum models list`.
type ModelsListCmd struct {
	Context string `name:"context" help:"Local kind context (kind-*) to act on, for a cluster Darkplum did not install. An install is found from its settings. A loopback address can still be an ssh tunnel to a remote cluster: the flag cannot tell, so do not point it at one." default:""`
}

// ModelsTestCmd is `darkplum models test`.
type ModelsTestCmd struct {
	Name string `arg:"" optional:"" help:"Instance name (default: the vLLM model deployed with the install)."`
}

// modelsEnv is what the models commands touch outside the cluster.
type modelsEnv struct {
	out  io.Writer
	kc   *kube.Client
	k8s  fs.FS
	fwd  func(service string, port int) Forwarder // tunnel to svc/<service>; models test only
	httc *http.Client
}

func newModelsEnv(g *Globals, contextFlag string) (*modelsEnv, error) {
	kc, err := installKube(os.Stdout, g.Namespace, contextFlag, g.Install)
	if err != nil {
		return nil, err
	}
	k8s, err := fs.Sub(darkplum.Tree(), "k8s")
	if err != nil {
		return nil, err
	}
	return &modelsEnv{out: os.Stdout, kc: kc, k8s: k8s, httc: &http.Client{Timeout: 30 * time.Second}}, nil
}

// Run executes `darkplum models add`.
func (c *ModelsAddCmd) Run(g *Globals) error {
	spec := models.Spec{Name: c.Name, HF: c.HF, Revision: c.Revision, Served: c.Served, MaxLen: c.MaxLen}
	if _, err := spec.Normalize(); err != nil { // before touching anything
		return err
	}
	e, err := newModelsEnv(g, c.Context)
	if err != nil {
		return err
	}
	return e.add(spec)
}

// Run executes `darkplum models remove`.
func (c *ModelsRemoveCmd) Run(g *Globals) error {
	if err := models.ValidateName(c.Name); err != nil {
		return err
	}
	e, err := newModelsEnv(g, c.Context)
	if err != nil {
		return err
	}
	return e.remove(c.Name)
}

// Run executes `darkplum models list`.
func (c *ModelsListCmd) Run(g *Globals) error {
	e, err := newModelsEnv(g, c.Context)
	if err != nil {
		return err
	}
	return e.list()
}

// Run executes `darkplum models test`.
func (c *ModelsTestCmd) Run(g *Globals) error {
	if c.Name != "" {
		if err := models.ValidateName(c.Name); err != nil {
			return err
		}
	}
	// A tunnel is only built for an install's validated cluster.
	t, row, err := resolveInstall(g.Install)
	if err != nil {
		printCheckTo(os.Stdout, row)
		return reportedError{"cannot establish the install's cluster"}
	}
	fmt.Fprintln(os.Stdout, sDim("cluster "+row.detail))
	e := &modelsEnv{out: os.Stdout, kc: t.kube(g.Namespace), httc: &http.Client{Timeout: 30 * time.Second},
		fwd: func(svc string, port int) Forwarder { return t.h.PortForwardPort(inferenceNamespace, "svc/"+svc, port) }}
	return e.test(context.Background(), c.Name)
}

// ---- add ----------------------------------------------------------------------

func (e *modelsEnv) inf(stdin []byte, args ...string) (string, error) {
	return e.kc.Run(stdin, append([]string{"-n", inferenceNamespace}, args...)...)
}

// upstreams reads VLLM_UPSTREAMS from the live gateway Deployment, the only
// place it is kept.
func (e *modelsEnv) upstreams() (string, error) {
	out, err := e.kc.NS(nil, "get", "deploy", "darkplum-gateway", "-o",
		`jsonpath={.spec.template.spec.containers[0].env[?(@.name=="VLLM_UPSTREAMS")].value}`)
	if err != nil {
		return "", fmt.Errorf("read the gateway's VLLM_UPSTREAMS: %w", err)
	}
	return strings.TrimSpace(out), nil
}

// register sets VLLM_UPSTREAMS when it changed and waits for the gateway.
func (e *modelsEnv) register(current, updated, verb string) error {
	if updated == current {
		return nil
	}
	fmt.Fprintf(e.out, "%s upstream with the gateway...\n", verb)
	if _, err := e.kc.NS(nil, "set", "env", "deploy/darkplum-gateway", "VLLM_UPSTREAMS="+updated); err != nil {
		return fmt.Errorf("update the gateway's VLLM_UPSTREAMS: %w", err)
	}
	if _, err := e.kc.NS(nil, "rollout", "status", "deploy/darkplum-gateway", "--timeout=120s"); err != nil {
		return fmt.Errorf("the gateway did not become ready after the update: %w", err)
	}
	return nil
}

func (e *modelsEnv) add(spec models.Spec) error {
	spec, err := spec.Normalize()
	if err != nil {
		return err
	}
	if _, err := e.kc.NS(nil, "get", "deploy", "darkplum-gateway", "-o", "name"); err != nil {
		if kubeNotFound(err, "deployments", "darkplum-gateway") {
			return errors.New("the gateway is not deployed. Run `darkplum deploy` first.")
		}
		return fmt.Errorf("read the gateway: %w", err)
	}
	// Render before anything changes, so a template problem leaves the cluster alone.
	objs, err := models.Render(e.k8s, spec)
	if err != nil {
		return err
	}
	body, err := manifests.JSONList(objs)
	if err != nil {
		return err
	}

	svc := models.ServiceName(spec.Name)
	exists := true
	if _, err := e.inf(nil, "get", "deploy", svc, "-o", "name"); err != nil {
		if !kubeNotFound(err, "deployments", svc) {
			return fmt.Errorf("read %s: %w", svc, err)
		}
		exists = false
	}
	if !exists {
		e.warnGPU()
	}
	if spec.Revision == "main" {
		fmt.Fprintln(e.out, "NOTE: --revision main is a moving target. Pin a commit SHA (--revision <sha>) so weights cannot change under you.")
	}
	fmt.Fprintf(e.out, "Deploying %s: %s@%s served as '%s'...\n", svc, spec.HF, spec.Revision, spec.Served)

	// A Service that still has a cluster IP (from before the gateway dialled pod
	// IPs) is recreated headless: clusterIP is immutable.
	if err := e.makeHeadless(svc); err != nil {
		return err
	}
	if _, err := e.kc.Run(body, "apply", "-f", "-"); err != nil {
		return fmt.Errorf("apply %s: %w", svc, err)
	}

	cur, err := e.upstreams()
	if err != nil {
		return err
	}
	if err := e.register(cur, models.AddUpstream(cur, spec.Name), "Registering"); err != nil {
		return err
	}
	fmt.Fprintf(e.out, "\nModel download + load takes 15-25 minutes. Monitor:\n  %s\n",
		kubectlHint("logs -f -n "+inferenceNamespace+" deploy/"+svc+" -c model-download"))
	fmt.Fprintln(e.out, "Once ready, the gateway discovers it within a minute; then enable it under Admin > Models.")
	return nil
}

// warnGPU warns, and only warns, when no GPU is free for a new model.
func (e *modelsEnv) warnGPU() {
	nodes, err := e.kc.Run(nil, "get", "nodes", "-l", "darkplum.ai/node-type=gpu-inference", "-o", "json")
	if err == nil {
		var pods string
		if pods, err = e.inf(nil, "get", "pods", "-o", "json"); err == nil {
			var total, used int
			if total, used, err = models.GPUs([]byte(nodes), []byte(pods)); err == nil {
				if total-used < 1 {
					fmt.Fprintf(e.out, "WARNING: no free GPU (%d of %d allocated). The model will stay Pending until the GPU\n"+
						"         node group grows: re-run `darkplum init` (GPU mode), then `darkplum deploy`.\n", used, total)
				}
				return
			}
		}
	}
	fmt.Fprintln(e.out, sDim("note: could not check for a free GPU: "+firstLine(err.Error())))
}

func (e *modelsEnv) makeHeadless(svc string) error {
	ip, err := e.inf(nil, "get", "svc", svc, "-o", "jsonpath={.spec.clusterIP}")
	if err != nil {
		if kubeNotFound(err, "services", svc) {
			return nil
		}
		return fmt.Errorf("read Service %s: %w", svc, err)
	}
	if ip = strings.TrimSpace(ip); ip != "" && ip != "None" {
		fmt.Fprintf(e.out, "Recreating Service %s as headless (was ClusterIP %s)...\n", svc, ip)
		if _, err := e.inf(nil, "delete", "svc", svc); err != nil {
			return fmt.Errorf("recreate Service %s: %w", svc, err)
		}
	}
	return nil
}

// ---- remove -------------------------------------------------------------------

func (e *modelsEnv) remove(name string) error {
	cur, err := e.upstreams()
	if err != nil {
		return err
	}
	if err := e.register(cur, models.RemoveUpstream(cur, name), "Unregistering"); err != nil {
		return err
	}
	if out, err := e.inf(nil, "delete", "deploy,svc", "-l", "darkplum.ai/model-instance="+name, "--ignore-not-found"); err != nil {
		return fmt.Errorf("delete %s: %w", models.ServiceName(name), err)
	} else if strings.TrimSpace(out) != "" {
		fmt.Fprint(e.out, out)
	}
	fmt.Fprintf(e.out, "Removed %s. Aliases pointing at its model now refuse requests until retargeted.\n", models.ServiceName(name))
	return nil
}

// ---- list ---------------------------------------------------------------------

func (e *modelsEnv) list() error {
	out, err := e.inf(nil, "get", "deploy", "-l", "app.kubernetes.io/component=inference-backend", "-o", "json")
	if err != nil {
		return fmt.Errorf("list model deployments: %w", err)
	}
	var dl struct {
		Items []struct {
			Metadata struct{ Name string } `json:"metadata"`
			Spec     struct {
				Replicas *int `json:"replicas"`
				Template struct {
					Spec struct {
						InitContainers []struct {
							Env []struct{ Name, Value string } `json:"env"`
						} `json:"initContainers"`
					} `json:"spec"`
				} `json:"template"`
			} `json:"spec"`
			Status struct {
				ReadyReplicas int `json:"readyReplicas"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(out), &dl); err != nil {
		return fmt.Errorf("list model deployments: %w", err)
	}
	fmt.Fprintln(e.out, sHeader("▸ Self-hosted model deployments"))
	tw := tabwriter.NewWriter(e.out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tREADY\tMODEL")
	for _, it := range dl.Items {
		model := "?"
		// The model is the init container's MODEL_NAME, found by name: its position
		// in the env list is not stable.
		for _, ic := range it.Spec.Template.Spec.InitContainers {
			for _, ev := range ic.Env {
				if ev.Name == "MODEL_NAME" {
					model = ev.Value
				}
			}
		}
		want := 1
		if it.Spec.Replicas != nil {
			want = *it.Spec.Replicas
		}
		fmt.Fprintf(tw, "%s\t%d/%d\t%s\n", it.Metadata.Name, it.Status.ReadyReplicas, want, model)
	}
	tw.Flush()
	if _, err := e.inf(nil, "get", "deploy", "vllm-inference", "-o", "name"); err == nil {
		fmt.Fprintln(e.out, sDim("(vllm-inference is the default deployment from `darkplum deploy`)"))
	} else if !kubeNotFound(err, "deployments", "vllm-inference") {
		return fmt.Errorf("read vllm-inference: %w", err)
	}
	cur, err := e.upstreams()
	if err != nil {
		return err
	}
	fmt.Fprintf(e.out, "\n%s\n", sHeader("▸ Gateway VLLM_UPSTREAMS"))
	entries := models.Entries(cur)
	if len(entries) == 0 {
		fmt.Fprintln(e.out, "(none beyond the default upstream)")
	}
	sort.Strings(entries)
	for _, u := range entries {
		fmt.Fprintln(e.out, u)
	}
	return nil
}

// ---- test ---------------------------------------------------------------------

// test sends one chat to a model through a tunnel to its pod. The tunnel skips
// the gateway and its NetworkPolicy, so this proves the model serves, not that
// the gateway path works (`darkplum configure` tests that).
func (e *modelsEnv) test(ctx context.Context, name string) error {
	dep, svc := "vllm-inference", "vllm-inference"
	if name != "" {
		dep, svc = models.ServiceName(name), models.ServiceName(name)
	}
	out, err := e.inf(nil, "get", "deploy", dep, "-o", "json")
	if err != nil {
		if !kubeNotFound(err, "deployments", dep) {
			return fmt.Errorf("read %s: %w", dep, err)
		}
		if name == "" {
			fmt.Fprintln(e.out, "This is a Bedrock-only install: there is no self-hosted vLLM to test.")
			fmt.Fprintln(e.out, "Run `darkplum configure` to send a test chat through the gateway.")
			return nil
		}
		return fmt.Errorf("%s does not exist. See `darkplum models list`.", dep)
	}
	var d struct {
		Spec struct {
			Template struct {
				Spec struct {
					Containers []struct {
						Name string   `json:"name"`
						Args []string `json:"args"`
					} `json:"containers"`
				} `json:"spec"`
			} `json:"template"`
		} `json:"spec"`
	}
	if err := json.Unmarshal([]byte(out), &d); err != nil {
		return fmt.Errorf("read %s: %w", dep, err)
	}
	served := ""
	for _, c := range d.Spec.Template.Spec.Containers {
		if c.Name == "vllm" {
			served = models.ServedName(c.Args)
		}
	}
	if served == "" {
		return fmt.Errorf("%s has no vllm container with --served-model-name", dep)
	}

	fmt.Fprintf(e.out, "Opening a tunnel to %s...\n", svc)
	f := e.fwd(svc, 8000)
	defer f.Close() // on every path
	base, err := f.Open(ctx)
	if err != nil {
		return fmt.Errorf("tunnel to %s: %w", svc, err)
	}
	fmt.Fprintf(e.out, "Sending test request to model: %s\n", served)
	body, _ := json.Marshal(map[string]any{"model": served, "max_tokens": 50,
		"messages": []map[string]string{{"role": "user", "content": "Say hello in one sentence."}}})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := e.httc.Do(req)
	if err != nil {
		return fmt.Errorf("FAIL: %w\nvLLM may still be loading. Check: darkplum status", err)
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	fmt.Fprintln(e.out)
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(e.out, "FAIL: HTTP %d\nBody: %s\n\nvLLM may still be loading. Check: darkplum status\n", resp.StatusCode, rb)
		return reportedError{fmt.Sprintf("HTTP %d from %s", resp.StatusCode, svc)}
	}
	var cr struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	_ = json.Unmarshal(rb, &cr)
	if len(cr.Choices) == 0 || strings.TrimSpace(cr.Choices[0].Message.Content) == "" {
		fmt.Fprintf(e.out, "FAIL: HTTP 200 but empty completion.\nBody: %s\n", rb)
		return reportedError{"empty completion from " + svc}
	}
	fmt.Fprintf(e.out, "SUCCESS: HTTP 200\nResponse: %s\n", strings.TrimSpace(cr.Choices[0].Message.Content))
	fmt.Fprintln(e.out, sDim("This tested the model through a tunnel to its pod. The gateway path is tested by `darkplum configure`."))
	return nil
}
