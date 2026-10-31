// Package semrouter installs and removes the optional in-cluster intent
// classifier (vLLM Semantic Router) as plain Kubernetes objects, and finds and
// converts an installation that was made with helm.
//
// It is an install setting, not a command: `darkplum deploy` calls Apply when the
// install has the router on and Remove when it is off. This package decides
// nothing about settings and reads no install record; it takes a kube.Client.
//
// The objects are rendered from the chart when a release is built (helm template
// at the pinned chart version, image by digest) and shipped in the bundle as
// semantic-router/rendered.yaml, so no helm binary and no network chart download
// is needed on the operator's machine. A build that does not carry the rendered
// file reports ErrNotRendered rather than guessing.
package semrouter

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"reflect"
	"regexp"
	"sort"
	"strings"

	"sigs.k8s.io/yaml"

	"github.com/scalecraft-dev/darkplum/cli/internal/kube"
	"github.com/scalecraft-dev/darkplum/cli/internal/manifests"
)

const (
	// ChartVersion is the version of the upstream chart the rendered objects come from.
	ChartVersion = "0.3.0"
	// ReleaseName is the name the chart was (and, for a helm-era install, is) installed under.
	ReleaseName = "semantic-router"
	// Component is the value of the darkplum.ai/component label on every object, so
	// removal and pruning select exactly what this package applied.
	Component      = "semantic-router"
	ComponentLabel = "darkplum.ai/component"
	// EnvName is the gateway setting that points it at the router.
	EnvName = "SEMANTIC_ROUTER_URL"

	renderedPath = "semantic-router/rendered.yaml"
	policiesPath = "semantic-router/network-policies.yaml"
	valuesPath   = "semantic-router/values.yaml"
)

// ErrNotRendered means the bundle carries no rendered router.
var ErrNotRendered = errors.New("this build has no rendered semantic router (semantic-router/rendered.yaml); it is produced when a release is built")

// URL is where the gateway reaches the router.
func URL(namespace string) string {
	return "http://" + ReleaseName + "." + namespace + ".svc.cluster.local:8080"
}

// Objects returns the router's objects (rendered chart and network policies),
// each labelled and placed in namespace when it names none.
func Objects(k8s fs.FS, namespace string) ([]manifests.Object, error) {
	if _, err := fs.Stat(k8s, renderedPath); err != nil {
		return nil, ErrNotRendered
	}
	rendered, err := manifests.File(k8s, renderedPath)
	if err != nil {
		return nil, err
	}
	pol, err := manifests.File(k8s, policiesPath)
	if err != nil {
		return nil, err
	}
	objs := append(rendered, pol...)
	for _, o := range objs {
		md, _ := o["metadata"].(map[string]any)
		if md == nil {
			return nil, fmt.Errorf("%s has no metadata", o.Kind())
		}
		if md["namespace"] == nil || md["namespace"] == "" {
			md["namespace"] = namespace
		}
		lb, _ := md["labels"].(map[string]any)
		if lb == nil {
			lb = map[string]any{}
			md["labels"] = lb
		}
		lb[ComponentLabel] = Component
	}
	return objs, nil
}

// Decisions are the category names in the values file's decisions block: what a
// routed alias maps onto routes of the same name.
func Decisions(values []byte) []string {
	var names []string
	in := false
	for _, l := range strings.Split(string(values), "\n") {
		if strings.HasPrefix(l, "    decisions:") {
			in = true
			continue
		}
		if in {
			if m := decisionRe.FindStringSubmatch(l); m != nil {
				names = append(names, m[1])
			}
		}
	}
	return names
}

var decisionRe = regexp.MustCompile(`^      - name: ([a-z0-9_-]+)\s*$`)

// DefaultValues reads the shipped values file (what the rendered objects were made from).
func DefaultValues(k8s fs.FS) ([]byte, error) { return fs.ReadFile(k8s, valuesPath) }

// Apply installs the router: objects, wait for ready, point the gateway at it.
// It is idempotent. progress receives the human-readable steps.
func Apply(kc *kube.Client, objs []manifests.Object, progress io.Writer) error {
	body, err := manifests.JSONList(objs)
	if err != nil {
		return err
	}
	if _, err := kc.NS(nil, "get", "deploy", "darkplum-gateway", "-o", "name"); err != nil {
		return fmt.Errorf("the gateway is not deployed (run `darkplum deploy` first): %w", err)
	}
	fmt.Fprintf(progress, "Installing the vLLM Semantic Router (%s)...\n", ChartVersion)
	if _, err := kc.Run(body, "apply", "-f", "-"); err != nil {
		return fmt.Errorf("apply the semantic router: %w", err)
	}
	fmt.Fprintln(progress, "Waiting for it to download its classifier models (~1-2 GB from Hugging Face) and become ready: usually 1-3 minutes, up to 30 on a slow link...")
	if _, err := kc.NS(nil, "rollout", "status", "deploy/"+ReleaseName, "--timeout=1800s"); err != nil {
		return fmt.Errorf("the semantic router did not become ready: %w\n"+
			"  Check: kubectl describe pod -n %s -l app.kubernetes.io/name=%s\n"+
			"  (a Pending pod = scheduling/resources; CrashLoop = see its logs; still downloading = re-run, it is idempotent)",
			err, kc.Namespace(), ReleaseName)
	}
	fmt.Fprintln(progress, "Pointing the gateway at it...")
	if _, err := kc.NS(nil, "set", "env", "deploy/darkplum-gateway", EnvName+"="+URL(kc.Namespace())); err != nil {
		return fmt.Errorf("point the gateway at the router: %w", err)
	}
	if _, err := kc.NS(nil, "rollout", "status", "deploy/darkplum-gateway", "--timeout=180s"); err != nil {
		return fmt.Errorf("the gateway did not become ready: %w", err)
	}
	return nil
}

// defaultKinds are removed by label even when the caller has no rendered
// objects to take kinds from.
var defaultKinds = []string{"deployment", "service", "configmap", "serviceaccount", "networkpolicy", "role", "rolebinding"}

// Remove takes the router out: the gateway's setting first (so it stops calling
// it), then every labelled object. A missing gateway or setting is fine; any
// other error is not. objs, when known, adds their kinds to the removal.
func Remove(kc *kube.Client, objs []manifests.Object, progress io.Writer) error {
	if _, err := kc.NS(nil, "set", "env", "deploy/darkplum-gateway", EnvName+"-"); err != nil && !notFound(err) {
		return fmt.Errorf("unset %s: %w", EnvName, err)
	}
	kinds := map[string]bool{}
	for _, k := range defaultKinds {
		kinds[k] = true
	}
	for _, o := range objs {
		if k := strings.ToLower(o.Kind()); k != "" && o.Namespace() != "" {
			kinds[k] = true
		}
	}
	var list []string
	for k := range kinds {
		list = append(list, k)
	}
	sort.Strings(list)
	if _, err := kc.NS(nil, "delete", strings.Join(list, ","), "-l", ComponentLabel+"="+Component, "--ignore-not-found"); err != nil {
		return fmt.Errorf("remove the semantic router: %w", err)
	}
	fmt.Fprintln(progress, "Semantic router removed.")
	return nil
}

func notFound(err error) bool {
	return err != nil && strings.Contains(err.Error(), "Error from server (NotFound)")
}

// ---- helm-era installs ------------------------------------------------------------

// HelmRelease is a helm 3 release of the router found in the cluster.
type HelmRelease struct {
	Found bool
	// Secrets are the release's revision Secrets (sh.helm.release.v1.<name>.vN).
	Secrets []string
	// Values are the values the operator installed it with (helm's "config"),
	// from the latest revision.
	Values map[string]any
}

// DetectHelm looks for a helm release of the router by its release Secrets
// (owner=helm, name=semantic-router). It never runs helm.
func DetectHelm(kc *kube.Client) (HelmRelease, error) {
	out, err := kc.NS(nil, "get", "secret", "-l", "owner=helm,name="+ReleaseName, "-o", "json")
	if err != nil {
		return HelmRelease{}, fmt.Errorf("look for a helm release of the semantic router: %w", err)
	}
	var list struct {
		Items []struct {
			Metadata struct {
				Name   string            `json:"name"`
				Labels map[string]string `json:"labels"`
			} `json:"metadata"`
			Data map[string]string `json:"data"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(out), &list); err != nil {
		return HelmRelease{}, err
	}
	var rel HelmRelease
	best := -1
	for _, it := range list.Items {
		rel.Found = true
		rel.Secrets = append(rel.Secrets, it.Metadata.Name)
		var v int
		fmt.Sscanf(it.Metadata.Labels["version"], "%d", &v)
		if v > best {
			vals, err := decodeRelease(it.Data["release"])
			if err != nil {
				return HelmRelease{}, fmt.Errorf("helm release secret %s: %w", it.Metadata.Name, err)
			}
			best, rel.Values = v, vals
		}
	}
	sort.Strings(rel.Secrets)
	return rel, nil
}

// decodeRelease reads the user-supplied values out of a helm release Secret's
// data.release: Kubernetes base64, then helm's base64 of gzip of JSON.
func decodeRelease(k8sB64 string) (map[string]any, error) {
	inner, err := base64.StdEncoding.DecodeString(k8sB64)
	if err != nil {
		return nil, fmt.Errorf("not base64: %w", err)
	}
	zipped, err := base64.StdEncoding.DecodeString(string(inner))
	if err != nil {
		// Some helm versions store the gzip stream without a second base64 layer.
		zipped = inner
	}
	zr, err := gzip.NewReader(bytes.NewReader(zipped))
	if err != nil {
		return nil, fmt.Errorf("not a helm release: %w", err)
	}
	raw, err := io.ReadAll(io.LimitReader(zr, 64<<20))
	if err != nil {
		return nil, err
	}
	var rel struct {
		Config map[string]any `json:"config"`
	}
	if err := json.Unmarshal(raw, &rel); err != nil {
		return nil, err
	}
	if rel.Config == nil {
		rel.Config = map[string]any{}
	}
	return rel.Config, nil
}

// Differences lists the value paths where the release's values differ from the
// shipped defaults, both normalised through JSON. Empty means equal: the
// release can be taken over without losing anything.
func Differences(rel HelmRelease, defaults []byte) ([]string, error) {
	var want map[string]any
	if err := yaml.Unmarshal(defaults, &want); err != nil {
		return nil, fmt.Errorf("the shipped values: %w", err)
	}
	var diffs []string
	diffMaps("", rel.Values, want, &diffs)
	sort.Strings(diffs)
	return diffs, nil
}

func diffMaps(prefix string, a, b any, out *[]string) {
	am, aok := a.(map[string]any)
	bm, bok := b.(map[string]any)
	if aok && bok {
		keys := map[string]bool{}
		for k := range am {
			keys[k] = true
		}
		for k := range bm {
			keys[k] = true
		}
		for k := range keys {
			diffMaps(join(prefix, k), am[k], bm[k], out)
		}
		return
	}
	if !reflect.DeepEqual(a, b) {
		*out = append(*out, prefix)
	}
}

func join(p, k string) string {
	if p == "" {
		return k
	}
	return p + "." + k
}

// ErrCustomised is returned by CheckTakeover for a release with changed values.
type ErrCustomised struct{ Keys []string }

func (e *ErrCustomised) Error() string {
	return "the helm-installed semantic router has values that differ from the shipped defaults (" + strings.Join(e.Keys, ", ") +
		"); taking it over would replace them with the default decisions"
}

// CheckTakeover refuses to convert a customised release unless convert is set.
func CheckTakeover(rel HelmRelease, defaults []byte, convert bool) error {
	if !rel.Found {
		return nil
	}
	diffs, err := Differences(rel, defaults)
	if err != nil {
		return err
	}
	if len(diffs) > 0 && !convert {
		return &ErrCustomised{Keys: diffs}
	}
	return nil
}

// FinishTakeover deletes the helm release Secrets, once Apply has put the
// router's objects in place, so a later `helm upgrade` cannot fight the CLI. The
// workload objects are not touched.
func FinishTakeover(kc *kube.Client, rel HelmRelease) error {
	for _, s := range rel.Secrets {
		if _, err := kc.NS(nil, "delete", "secret", s, "--ignore-not-found"); err != nil {
			return fmt.Errorf("delete helm release secret %s: %w", s, err)
		}
	}
	return nil
}
