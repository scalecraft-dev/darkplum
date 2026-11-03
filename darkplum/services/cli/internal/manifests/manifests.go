// Package manifests renders the Kubernetes objects of a Darkplum install from the
// verified bundle, in Go, with no shell, sed or awk between the files and the
// cluster.
//
//   - Each component directory is built with kustomize (in process, Reorder
//     legacy: byte-identical to `kubectl kustomize`, see the spike), then patched
//     as structured data: images, environment values and CIDRs are set on the
//     field they belong to, and a rule that matches nothing, or more than once, is
//     an error rather than a silent no-op.
//   - After patching, assertions hold the whole render to the invariants that the
//     old flow only hoped for: every Darkplum container runs exactly the
//     expected digest in the customer's registry, nothing third-party is a mutable
//     tag, no PLACEHOLDER_ survived, and the vendor-key objects carry no `data`
//     (re-applying them must never be able to wipe an installed key).
//   - Phases are ordered so network policies and identities exist before any
//     workload starts (the old flow applied policies after the pods were up).
package manifests

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"

	"sigs.k8s.io/kustomize/api/krusty"
	"sigs.k8s.io/kustomize/kyaml/filesys"
	"sigs.k8s.io/yaml"
)

// Object is one Kubernetes object as unstructured data.
type Object map[string]any

func (o Object) Kind() string { return str(o["kind"]) }
func (o Object) Name() string { return str(dig(o, "metadata", "name")) }
func (o Object) Namespace() string {
	return str(dig(o, "metadata", "namespace"))
}

// Key is kind/namespace/name, for messages.
func (o Object) Key() string { return o.Kind() + "/" + o.Namespace() + "/" + o.Name() }

// Kustomize builds one directory of root (the bundle's k8s/ tree) in process. The
// order is kustomize's legacy resource order, the one `kubectl kustomize` uses.
func Kustomize(root fs.FS, dir string) ([]Object, error) {
	mem := filesys.MakeFsInMemory()
	err := fs.WalkDir(root, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		dst := path.Join("/k8s", p)
		if d.IsDir() {
			return mem.MkdirAll(dst)
		}
		b, err := fs.ReadFile(root, p)
		if err != nil {
			return err
		}
		return mem.WriteFile(dst, b)
	})
	if err != nil {
		return nil, err
	}
	opts := krusty.MakeDefaultOptions()
	opts.Reorder = krusty.ReorderOptionLegacy
	m, err := krusty.MakeKustomizer(opts).Run(mem, path.Join("/k8s", dir))
	if err != nil {
		return nil, fmt.Errorf("kustomize %s: %w", dir, err)
	}
	var out []Object
	for _, r := range m.Resources() {
		mp, err := r.Map()
		if err != nil {
			return nil, fmt.Errorf("kustomize %s: %w", dir, err)
		}
		out = append(out, Object(mp))
	}
	return out, nil
}

// File reads the objects of one YAML file (documents separated by ---).
func File(root fs.FS, p string) ([]Object, error) {
	b, err := fs.ReadFile(root, p)
	if err != nil {
		return nil, err
	}
	var out []Object
	for _, doc := range strings.Split(string(b), "\n---") {
		if strings.TrimSpace(stripComments(doc)) == "" {
			continue
		}
		var o Object
		if err := yaml.Unmarshal([]byte(doc), &o); err != nil {
			return nil, fmt.Errorf("%s: %w", p, err)
		}
		if o.Kind() == "" {
			return nil, fmt.Errorf("%s: a document without a kind", p)
		}
		out = append(out, o)
	}
	return out, nil
}

func stripComments(s string) string {
	var b strings.Builder
	for _, l := range strings.Split(s, "\n") {
		if t := strings.TrimSpace(l); t != "" && !strings.HasPrefix(t, "#") {
			b.WriteString(l + "\n")
		}
	}
	return b.String()
}

// JSONList renders objects as a v1 List, which `kubectl apply -f -` takes on stdin.
func JSONList(objs []Object) ([]byte, error) {
	items := make([]any, len(objs))
	for i, o := range objs {
		items[i] = o
	}
	return json.Marshal(map[string]any{"apiVersion": "v1", "kind": "List", "items": items})
}

// ---- structured patching -------------------------------------------------

// SetContainerImage sets the image of the one container (or init container) whose
// current image is exactly from. Zero or several matches are errors.
func SetContainerImage(o Object, from, to string) error {
	n := 0
	for _, c := range containers(o) {
		if str(c["image"]) == from {
			c["image"] = to
			n++
		}
	}
	if n != 1 {
		return fmt.Errorf("%s: expected exactly one container using image %q, found %d", o.Key(), from, n)
	}
	return nil
}

// SetEnv sets the value of the one env entry named name in the one container that
// declares it. Zero or several are errors, so a renamed variable cannot turn a
// setting into a silent no-op.
func SetEnv(o Object, name, value string) error {
	n := 0
	for _, c := range containers(o) {
		env, _ := c["env"].([]any)
		for _, e := range env {
			if m, ok := e.(map[string]any); ok && str(m["name"]) == name {
				if _, has := m["valueFrom"]; has {
					return fmt.Errorf("%s: env %s is a reference, not a value", o.Key(), name)
				}
				m["value"] = value
				n++
			}
		}
	}
	if n != 1 {
		return fmt.Errorf("%s: expected exactly one env entry %s, found %d", o.Key(), name, n)
	}
	return nil
}

// EnvValue reads env name from the one container that declares it.
func EnvValue(o Object, name string) (string, bool) {
	for _, c := range containers(o) {
		env, _ := c["env"].([]any)
		for _, e := range env {
			if m, ok := e.(map[string]any); ok && str(m["name"]) == name {
				return str(m["value"]), true
			}
		}
	}
	return "", false
}

// SetCIDR replaces every ipBlock.cidr in a NetworkPolicy that equals placeholder
// with cidr; there must be exactly one.
func SetCIDR(o Object, placeholder, cidr string) error {
	n := 0
	walk(o, func(m map[string]any) {
		if ip, ok := m["ipBlock"].(map[string]any); ok && str(ip["cidr"]) == placeholder {
			ip["cidr"] = cidr
			n++
		}
	})
	if n != 1 {
		return fmt.Errorf("%s: expected exactly one ipBlock.cidr %s, found %d", o.Key(), placeholder, n)
	}
	return nil
}

func containers(o Object) []map[string]any {
	spec, _ := dig(o, "spec", "template", "spec").(map[string]any)
	if spec == nil {
		return nil
	}
	var out []map[string]any
	for _, k := range []string{"containers", "initContainers"} {
		cs, _ := spec[k].([]any)
		for _, c := range cs {
			if m, ok := c.(map[string]any); ok {
				out = append(out, m)
			}
		}
	}
	return out
}

func walk(v any, f func(map[string]any)) {
	switch x := v.(type) {
	case map[string]any:
		f(x)
		for _, c := range x {
			walk(c, f)
		}
	case Object:
		walk(map[string]any(x), f)
	case []any:
		for _, c := range x {
			walk(c, f)
		}
	}
}

func dig(v any, keys ...string) any {
	for _, k := range keys {
		var m map[string]any
		switch x := v.(type) {
		case map[string]any:
			m = x
		case Object:
			m = x
		default:
			return nil
		}
		v = m[k]
	}
	return v
}

func str(v any) string { s, _ := v.(string); return s }

// ---- assertions ----------------------------------------------------------

// firstPartyImage matches the image names the bundle's Deployments use for the
// three images Darkplum builds; deploy replaces each with the mirrored digest.
func firstPartyImage(svc string) string { return "darkplum-" + svc + ":latest" }

// vendorKeyObjects are created empty and filled by Admin > Models or `darkplum keys`.
var vendorKeyObjects = map[string]bool{"darkplum-provider-keys": true, "darkplum-billing-keys": true, "darkplum-provider-config": true}

// Check holds a rendered set of objects to the install's invariants. images maps
// each first-party service to the exact reference its containers must run.
func Check(objs []Object, images map[string]string) error {
	var errs []error
	want := map[string]bool{}
	for _, ref := range images {
		want[ref] = true
	}
	for _, o := range objs {
		b, _ := json.Marshal(o)
		if strings.Contains(string(b), "PLACEHOLDER_") {
			errs = append(errs, fmt.Errorf("%s: an unreplaced PLACEHOLDER_ value", o.Key()))
		}
		for _, c := range containers(o) {
			img := str(c["image"])
			switch {
			case want[img]:
			case strings.HasPrefix(img, "darkplum-") || strings.Contains(img, ":latest"):
				errs = append(errs, fmt.Errorf("%s: container %v runs %q, not the mirrored digest", o.Key(), c["name"], img))
			case !strings.Contains(img, "@sha256:"):
				errs = append(errs, fmt.Errorf("%s: container %v runs %q, which is not pinned by digest", o.Key(), c["name"], img))
			}
		}
		if (o.Kind() == "Secret" || o.Kind() == "ConfigMap") && vendorKeyObjects[o.Name()] {
			if _, has := o["data"]; has {
				errs = append(errs, fmt.Errorf("%s has a data stanza: re-applying it would wipe installed vendor keys", o.Key()))
			}
			if _, has := o["stringData"]; has {
				errs = append(errs, fmt.Errorf("%s has a stringData stanza: re-applying it would wipe installed vendor keys", o.Key()))
			}
		}
	}
	return errors.Join(errs...)
}

// Sorted lists object keys, for stable test output.
func Sorted(objs []Object) []string {
	var out []string
	for _, o := range objs {
		out = append(out, o.Key())
	}
	sort.Strings(out)
	return out
}
