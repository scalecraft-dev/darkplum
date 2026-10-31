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
//
// Applying the objects overwrites what the bundle sets: a field someone changed
// on a live router object with kubectl edit is put back to the shipped value the
// next time Apply runs. Customise the router by changing the install, not the
// object.
package semrouter

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
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
func Apply(kc *kube.Client, tok Takeover, objs []manifests.Object, progress io.Writer) error {
	if !tok.checked {
		return errors.New("semrouter: Apply needs the result of CheckTakeover: a helm-installed router must be checked before it is overwritten")
	}
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
	// Secrets and ConfigMaps are the release's revision objects (helm's "secret"
	// and "configmap" storage drivers), sh.helm.release.v1.<name>.vN.
	Secrets    []string
	ConfigMaps []string
	// Revision and Status are those of the revision Values came from: the latest
	// one helm marks deployed, or the highest when none is.
	Revision int
	Status   string
	// Values are the values the operator installed it with (helm's "config").
	Values map[string]any
}

type helmItem struct {
	Metadata struct {
		Name   string            `json:"name"`
		Labels map[string]string `json:"labels"`
	} `json:"metadata"`
	Data map[string]string `json:"data"`
}

// DetectHelm looks for a helm release of the router by its release objects
// (owner=helm, name=semantic-router), in both of helm's storage drivers. It never
// runs helm. The values come from the revision helm marks deployed, not merely the
// highest number: a failed upgrade leaves a higher revision that never ran.
func DetectHelm(kc *kube.Client) (HelmRelease, error) {
	var rel HelmRelease
	bestDeployed, bestAny := -1, -1
	var deployedVals, anyVals map[string]any
	var anyStatus string
	for _, kind := range []string{"secret", "configmap"} {
		out, err := kc.NS(nil, "get", kind, "-l", "owner=helm,name="+ReleaseName, "-o", "json")
		if err != nil {
			return HelmRelease{}, fmt.Errorf("look for a helm release of the semantic router (%s driver): %w", kind, err)
		}
		var list struct {
			Items []helmItem `json:"items"`
		}
		if err := json.Unmarshal([]byte(out), &list); err != nil {
			return HelmRelease{}, err
		}
		for _, it := range list.Items {
			rel.Found = true
			if kind == "secret" {
				rel.Secrets = append(rel.Secrets, it.Metadata.Name)
			} else {
				rel.ConfigMaps = append(rel.ConfigMaps, it.Metadata.Name)
			}
			var v int
			fmt.Sscanf(it.Metadata.Labels["version"], "%d", &v)
			deployed := it.Metadata.Labels["status"] == "deployed"
			vals, err := decodeRelease(it.Data["release"], kind == "secret")
			if err != nil {
				return HelmRelease{}, fmt.Errorf("helm release %s %s: %w", kind, it.Metadata.Name, err)
			}
			if v > bestAny {
				bestAny, anyVals, anyStatus = v, vals, it.Metadata.Labels["status"]
			}
			if deployed && v > bestDeployed {
				bestDeployed, deployedVals = v, vals
			}
		}
	}
	sort.Strings(rel.Secrets)
	sort.Strings(rel.ConfigMaps)
	switch {
	case bestDeployed >= 0:
		rel.Revision, rel.Status, rel.Values = bestDeployed, "deployed", deployedVals
	case bestAny >= 0:
		rel.Revision, rel.Status, rel.Values = bestAny, anyStatus, anyVals
	}
	return rel, nil
}

// decodeRelease reads the user-supplied values out of a helm release object's
// data.release: helm's base64 of gzip of JSON. A Secret's data adds Kubernetes'
// own base64 layer on top (secret true); a ConfigMap's does not.
func decodeRelease(stored string, secret bool) (map[string]any, error) {
	inner := []byte(stored)
	if secret {
		var err error
		if inner, err = base64.StdEncoding.DecodeString(stored); err != nil {
			return nil, fmt.Errorf("not base64: %w", err)
		}
	}
	zipped, err := base64.StdEncoding.DecodeString(string(inner))
	if err != nil {
		return nil, fmt.Errorf("not a helm release: %w", err)
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

// shippedValues are the canonical hashes (sha256 of the values parsed and
// re-marshalled as JSON, so comments and layout do not matter) of every
// values.yaml this repository has shipped for the router, taken from its git
// history (k8s/semantic-router/values.yaml at 8907e6d, bd10508, 18f4974). A
// router installed from any of them is the default router, not a customised one.
var shippedValues = map[string]string{
	"c4bd55fe454af82b019e7f5b535ea2419b9394b09c587c5ec85ebd550c5d96d3": "8907e6d",
	"d7a4b3ee150662c6ae7d2b2427a9336a90285ed5a7cb9a4c58e66e0b1088a892": "bd10508",
	"6f758286453167a97ce106ad7110f3102cf43b0e7f8c1b2a776eab7cc82c93c2": "18f4974",
}

func canonicalHash(v map[string]any) string {
	b, _ := json.Marshal(v)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// Differences lists the value paths where the release's values differ from the
// shipped defaults, both normalised through JSON. Empty means the release is
// the default router: its values equal the values.yaml this build ships, or any
// values.yaml an earlier release of this repository shipped.
func Differences(rel HelmRelease, defaults []byte) ([]string, error) {
	var want map[string]any
	if err := yaml.Unmarshal(defaults, &want); err != nil {
		return nil, fmt.Errorf("the shipped values: %w", err)
	}
	// Round-trip so numbers compare the way helm stored them.
	raw, _ := json.Marshal(want)
	_ = json.Unmarshal(raw, &want)
	if _, ok := shippedValues[canonicalHash(rel.Values)]; ok || canonicalHash(rel.Values) == canonicalHash(want) {
		return nil, nil
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
	return "the helm-installed semantic router has values that differ from every values file this project shipped (" + strings.Join(e.Keys, ", ") +
		"); taking it over would replace them with the default decisions"
}

// Takeover is the proof that a helm-installed router (if there is one) was
// checked. Only CheckTakeover makes a usable one; Apply and FinishTakeover refuse
// the zero value, so no caller can overwrite a customised helm router by
// forgetting to look.
type Takeover struct {
	checked bool
	rel     HelmRelease
}

// Found reports whether there is a helm release to take over.
func (t Takeover) Found() bool { return t.rel.Found }

// CheckTakeover decides whether a helm release can be taken over: no release is
// fine, a default one is fine, a customised one is refused unless convert says the
// operator accepts losing the customisation.
func CheckTakeover(rel HelmRelease, defaults []byte, convert bool) (Takeover, error) {
	if rel.Found {
		diffs, err := Differences(rel, defaults)
		if err != nil {
			return Takeover{}, err
		}
		if len(diffs) > 0 && !convert {
			return Takeover{}, &ErrCustomised{Keys: diffs}
		}
	}
	return Takeover{checked: true, rel: rel}, nil
}

// FinishTakeover deletes the helm release objects (both storage drivers), once
// Apply has put the router's objects in place, so a later `helm upgrade` cannot
// fight the CLI. The workload objects are not touched.
func FinishTakeover(kc *kube.Client, tok Takeover) error {
	if !tok.checked {
		return errors.New("semrouter: FinishTakeover needs the result of CheckTakeover")
	}
	for _, n := range tok.rel.Secrets {
		if _, err := kc.NS(nil, "delete", "secret", n, "--ignore-not-found"); err != nil {
			return fmt.Errorf("delete helm release secret %s: %w", n, err)
		}
	}
	for _, n := range tok.rel.ConfigMaps {
		if _, err := kc.NS(nil, "delete", "configmap", n, "--ignore-not-found"); err != nil {
			return fmt.Errorf("delete helm release configmap %s: %w", n, err)
		}
	}
	return nil
}
