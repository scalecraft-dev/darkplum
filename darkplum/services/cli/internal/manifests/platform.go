package manifests

import (
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"strings"
)

// Inputs are everything the platform's manifests need from the rest of the
// install. Nothing is read from the cluster here except VLLMUpstreams, which the
// caller carries over from the live gateway (models.* own that value; a redeploy
// must not drop the models added since).
type Inputs struct {
	// Images maps gateway, api and web to the mirrored reference
	// <registry>/<repo>@sha256:<digest> that their containers must run.
	Images             map[string]string
	BedrockRegion      string
	BedrockModelFilter string
	VLLMUpstreams      string
	// DefaultVLLM wires the gateway to the default vllm-inference Service. It is
	// false on a Bedrock-only install, where no such Service exists.
	DefaultVLLM bool
	ServiceCIDR string // the cluster's Kubernetes service CIDR
	VPCCIDR     string
	// LibraryStoreURL is where the api writes the Library's files: the Terraform
	// output library_store_url, s3://<bucket>/library/v1/ (gs://... on GCP). Empty
	// leaves the api's LIBRARY_STORE_URL empty, which keeps its library worker off:
	// the library then lives in Postgres only, as before.
	LibraryStoreURL string
	// LibraryRegion is the region of an s3:// library bucket (the provider_region
	// output): the api's S3 client has no other way to learn it, and falls back to
	// us-east-1, which fails for a bucket anywhere else.
	LibraryRegion string
}

var awsRegionRe = regexp.MustCompile(`^[a-z]{2}(-[a-z]+)+-[0-9]{1,2}$`)

// libraryURLRe is the one shape the api is given: a bucket the cloud would accept
// and the fixed key prefix of the layout's first version.
var libraryURLRe = regexp.MustCompile(`^(?:s3://[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]|gs://[a-z0-9][a-z0-9._-]{1,220}[a-z0-9])/library/v1/$`)

// libraryBucketOK refuses what is not a bucket name: consecutive dots, an
// address, which a path-style request would resolve as a host.
func libraryBucketOK(url string) bool {
	if !libraryURLRe.MatchString(url) {
		return false
	}
	host := url[strings.Index(url, "://")+3 : strings.Index(url, "/library/")]
	return !strings.Contains(host, "..") && !strings.Contains(host, ".-") && !strings.Contains(host, "-.") &&
		!ipv4Re.MatchString(host)
}

var ipv4Re = regexp.MustCompile(`^[0-9]{1,3}(\.[0-9]{1,3}){3}$`)

// DefaultVLLMURL is the in-cluster address of the default vllm-inference Service.
const DefaultVLLMURL = "http://vllm-inference.darkplum-inference.svc.cluster.local:8000"

// Phase is a group of objects applied together; Rollouts are the Deployments (in
// darkplum-system) that must finish rolling out before the next phase starts.
type Phase struct {
	Name     string
	Objects  []Object
	Rollouts []string
}

var (
	cidrRe = regexp.MustCompile(`^[0-9]{1,3}(\.[0-9]{1,3}){3}/[0-9]{1,2}$`)
	refRe  = regexp.MustCompile(`^[^\s@]+@sha256:[0-9a-f]{64}$`)
)

func (in Inputs) validate() error {
	var errs []error
	for _, svc := range []string{"gateway", "api", "web"} {
		if !refRe.MatchString(in.Images[svc]) {
			errs = append(errs, fmt.Errorf("image for %s must be repository@sha256:<digest>, got %q", svc, in.Images[svc]))
		}
	}
	if !cidrRe.MatchString(in.ServiceCIDR) {
		errs = append(errs, fmt.Errorf("the cluster's service CIDR %q is not an IPv4 CIDR", in.ServiceCIDR))
	}
	if !cidrRe.MatchString(in.VPCCIDR) {
		errs = append(errs, fmt.Errorf("the VPC CIDR %q is not an IPv4 CIDR", in.VPCCIDR))
	}
	if in.BedrockRegion == "" {
		errs = append(errs, errors.New("no Bedrock region (the inference_region output; terraform always sets it, so deploy again)"))
	}
	if in.LibraryStoreURL != "" && !libraryBucketOK(in.LibraryStoreURL) {
		errs = append(errs, fmt.Errorf("the library store URL %q must be s3://<bucket>/library/v1/ (or gs://<bucket>/library/v1/) with a valid bucket name", in.LibraryStoreURL))
	}
	if in.LibraryRegion != "" && !awsRegionRe.MatchString(in.LibraryRegion) {
		errs = append(errs, fmt.Errorf("the library bucket's region %q is not an AWS region", in.LibraryRegion))
	}
	if strings.HasPrefix(in.LibraryStoreURL, "s3://") && in.LibraryRegion == "" {
		errs = append(errs, errors.New("an s3:// library store needs its region (the provider_region output), or the api's S3 client uses us-east-1"))
	}
	return errors.Join(errs...)
}

func isWorkload(o Object) bool { return o.Kind() == "Deployment" || o.Kind() == "Service" }

func split(objs []Object) (static, workloads []Object) {
	for _, o := range objs {
		if isWorkload(o) {
			workloads = append(workloads, o)
		} else {
			static = append(static, o)
		}
	}
	return
}

func find(objs []Object, kind, name string) (Object, error) {
	for _, o := range objs {
		if o.Kind() == kind && o.Name() == name {
			return o, nil
		}
	}
	return nil, fmt.Errorf("the bundle has no %s %s", kind, name)
}

// Platform renders the platform (api, gateway, web and what they need) in apply
// order: namespaces; network policies; identities and the empty vendor-key
// objects; then api, gateway and web, each with the Deployments that must be
// rolled out before the next starts. The api comes first because it owns the
// schema (the gateway fails closed until the tables exist). The Secrets the
// workloads read (database URL, auth, pseudonym key) are created by deploy
// between "identities" and "api".
func Platform(k8s fs.FS, in Inputs) ([]Phase, error) {
	if err := in.validate(); err != nil {
		return nil, err
	}
	ns, err := Kustomize(k8s, "base/namespaces")
	if err != nil {
		return nil, err
	}
	pol, err := Kustomize(k8s, "base/network-policies")
	if err != nil {
		return nil, err
	}
	// The two policies that need a CIDR are not in the kustomization (their
	// placeholders would not build), so they are read as files and patched.
	for _, p := range []struct{ file, placeholder, cidr string }{
		{"base/network-policies/gateway-egress-services.yaml", "PLACEHOLDER_SERVICE_CIDR", in.ServiceCIDR},
		{"base/network-policies/gateway-egress-database.yaml", "PLACEHOLDER_VPC_CIDR", in.VPCCIDR},
	} {
		objs, err := File(k8s, p.file)
		if err != nil {
			return nil, err
		}
		if len(objs) != 1 {
			return nil, fmt.Errorf("%s: expected one NetworkPolicy, found %d", p.file, len(objs))
		}
		if err := SetCIDR(objs[0], p.placeholder, p.cidr); err != nil {
			return nil, err
		}
		pol = append(pol, objs[0])
	}

	gw, err := Kustomize(k8s, "base/gateway")
	if err != nil {
		return nil, err
	}
	api, err := Kustomize(k8s, "base/api")
	if err != nil {
		return nil, err
	}
	web, err := Kustomize(k8s, "base/web")
	if err != nil {
		return nil, err
	}
	gwStatic, gwWork := split(gw)
	apiStatic, apiWork := split(api)
	webStatic, webWork := split(web)
	if len(webStatic) != 0 {
		return nil, fmt.Errorf("web has objects other than its Deployment and Service: %v", Sorted(webStatic))
	}

	for _, w := range []struct {
		objs []Object
		svc  string
	}{{apiWork, "api"}, {gwWork, "gateway"}, {webWork, "web"}} {
		d, err := find(w.objs, "Deployment", "darkplum-"+w.svc)
		if err != nil {
			return nil, err
		}
		if err := SetContainerImage(d, firstPartyImage(w.svc), in.Images[w.svc]); err != nil {
			return nil, err
		}
	}
	ad, _ := find(apiWork, "Deployment", "darkplum-api")
	if err := SetEnv(ad, "LIBRARY_STORE_URL", in.LibraryStoreURL); err != nil {
		return nil, err
	}
	if err := SetEnv(ad, "LIBRARY_S3_REGION", in.LibraryRegion); err != nil {
		return nil, err
	}
	gd, _ := find(gwWork, "Deployment", "darkplum-gateway")
	defaultVLLM := ""
	if in.DefaultVLLM {
		defaultVLLM = DefaultVLLMURL
	}
	for _, e := range []struct{ name, value string }{
		{"BEDROCK_REGION", in.BedrockRegion},
		{"BEDROCK_MODEL_FILTER", in.BedrockModelFilter},
		{"VLLM_UPSTREAM", defaultVLLM},
		{"VLLM_UPSTREAMS", in.VLLMUpstreams},
	} {
		if err := SetEnv(gd, e.name, e.value); err != nil {
			return nil, err
		}
	}

	phases := []Phase{
		{Name: "namespaces", Objects: ns},
		{Name: "network policies", Objects: pol},
		{Name: PhaseIdentities, Objects: append(append([]Object{}, gwStatic...), apiStatic...)},
		{Name: PhaseAPI, Objects: apiWork, Rollouts: []string{"darkplum-api"}},
		{Name: PhaseGateway, Objects: gwWork, Rollouts: []string{"darkplum-gateway"}},
		{Name: PhaseWeb, Objects: webWork, Rollouts: []string{"darkplum-web"}},
	}
	var all []Object
	for _, p := range phases {
		all = append(all, p.Objects...)
	}
	if err := Check(all, in.Images); err != nil {
		return nil, err
	}
	return phases, nil
}
