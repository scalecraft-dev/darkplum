package commands

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"golang.org/x/term"
)

// InitCmd is `darkplum init`: prompt for the few choices that shape an install,
// check them against the operator's own cloud account, and write
// terraform.tfvars (and the S3 backend settings in backend.hcl). It deploys nothing — `make deploy`
// does that — and holds no credential: every cloud call is the operator's own
// `aws` CLI.
type InitCmd struct {
	Cloud         string `help:"Target cloud provider." enum:"aws,gcp" default:"aws"`
	Profile       string `help:"AWS profile to install with. Prompted when omitted; leave empty for the default credential chain." default:""`
	Region        string `help:"AWS region for the cluster. Defaults to the profile's region." default:""`
	Mode          string `help:"How models run: bedrock (cloud-managed models only, no GPU) or gpu (adds a self-hosted model)." enum:"bedrock,gpu," default:""`
	Model         string `help:"Self-hosted Hugging Face model id (gpu mode). Catalog ids use a reviewed pinned revision." default:""`
	ModelRevision string `help:"Full 40-character commit SHA of --model. Required with --offline for a model outside the catalog." name:"model-revision" default:""`
	GPUInstance   string `help:"GPU instance type (gpu mode). Defaults to the one the model needs." name:"gpu-instance" default:""`
	GPUCount      int    `help:"Number of GPU nodes (gpu mode); each self-hosted model takes one." name:"gpu-count" default:"1"`
	ClusterName   string `help:"EKS cluster name; prefix of related resources." name:"cluster-name" default:"darkplum"`
	Throwaway     bool   `help:"Test install: one-day backups, no deletion protection, no final snapshot. Never for real data."`
	Offline       bool   `help:"Make no Hugging Face call. A model outside the catalog then needs --model-revision."`
	Yes           bool   `help:"Take every default and ask nothing (for scripts). Refuses to overwrite or accept a gated model."`
	DryRun        bool   `help:"Run the checks and print what would be written and created, without writing or creating anything." name:"dry-run" aliases:"stdout"`
	Force         bool   `help:"Overwrite an existing terraform.tfvars without asking (a .bak copy is kept)."`
	Dir           string `help:"Terraform root to write into. Defaults to terraform/<cloud> under the current directory." default:""`
	Adopt         bool   `help:"Take over an install the Makefile flow created (terraform/aws, state in S3): record it in ~/.darkplum/installs/<name>, leave its state where it is, and rename its backend.tf and terraform.tfvars to *.adopted. Use --dir for another Terraform root."`
	Install       string `help:"Name of the install workspace to create with --adopt. Defaults to the cluster name." default:""`

	Domain         string `help:"A domain you control. The team reaches Darkplum privately at https://darkplum.<domain> (the name is public, its address is not). Required with --yes." default:""`
	Hostname       string `help:"Name the team opens instead of darkplum.<domain>; must be inside --domain." default:""`
	ZoneID         string `name:"zone-id" help:"Use this existing public Route53 zone for the name instead of creating one." default:""`
	CertificateARN string `name:"certificate-arn" help:"Use this existing ACM certificate (this account and region) instead of issuing one." default:""`
	AllowedCIDRs   string `name:"allowed-cidrs" help:"Comma-separated IPv4 networks allowed to reach the address (VPN, office). Default: the VPC." default:""`
}

// initCloud is the per-cloud half of init: identity, zones, quota, name
// collisions and state-backend creation. Only AWS exists; the file writer,
// model catalog and prompter are shared.
type initCloud interface {
	Profiles() []string
	ProfileRegion() string
	Identity() (identity, error)
	OfferedZones(region string, types ...string) ([]string, error)
	GPUQuota(region, instance string) checkResult
	NameCollisions(region, name string) []checkResult
	CheckRegion(region string) error
	Inspect(bucket, region string) (backendState, error)
	StateBuckets(region string) ([]stateBucket, error)
	PrefixBuckets(region string) ([]string, error)
	ReadState(bucket, key, region string) (sc stateContents, found bool, err error)
	CreateBackend(bucket, region string, st backendState) (string, error)

	// Private web address (init_web.go). Read-only except CreateWebZone.
	PublicZone(name string) (hostedZone, bool, error)
	HostedZone(id string) (hostedZone, error)
	RecordTypes(zoneID, name string) ([]string, error)
	Certificate(arn string) (certInfo, error)
	CreateWebZone(hostname string) (hostedZone, error)
}

type modelLookup interface {
	Lookup(ctx context.Context, id string) (hfModel, error)
	endpoint() string
}

// initEnv carries everything init touches outside its flags, so tests swap the
// cloud, Hugging Face, prompter and clock without a TTY or a network.
type initEnv struct {
	out         io.Writer
	prompt      Prompter
	interactive bool
	newCloud    func(profile string) initCloud
	hf          modelLookup
	now         func() time.Time
	suffix      func() string // random state-bucket suffix; injected for goldens

	// Waiting for a customer's NS delegation (init_web.go).
	lookupNS          func(name string) ([]string, error)
	sleep             func(time.Duration)
	delegationTimeout time.Duration
}

// fail shows a ✗ row with its → hint and returns an error main will not print again.
func (e *initEnv) fail(name, detail, hint, msg string) error {
	printCheckTo(e.out, checkResult{name: name, status: checkFail, detail: detail, hint: hint})
	return reportedError{msg}
}

func randomSuffix() string {
	b := make([]byte, 3)
	if _, err := rand.Read(b); err != nil {
		panic(err) // the OS entropy source failing is not recoverable
	}
	return hex.EncodeToString(b)
}

// Approximate always-on base cost of an install with no GPU, USD per day, from
// on-demand us-east-2 list prices (README "Cost"): EKS control plane $0.10/h,
// 2x m5.large $0.19/h, one NAT gateway $0.05/h, RDS db.t4g.micro $0.02/h and the
// Bedrock interface endpoints $0.06/h = ~$0.42/h. Shown as approximate; update
// with the README table.
const baseUSDPerDay = 10

// Run executes `darkplum init`.
func (c *InitCmd) Run(_ *Globals) error {
	if c.Adopt {
		return c.runAdopt(os.Stdout)
	}
	tty := term.IsTerminal(int(os.Stdin.Fd()))
	env := &initEnv{
		out:         os.Stdout,
		prompt:      newIOPrompter(os.Stdin, os.Stdout),
		interactive: tty && !c.Yes,
		newCloud:    func(p string) initCloud { return newAWSCloud(p, nil) },
		hf:          newHFClient(),
		now:         time.Now,
		suffix:      randomSuffix,

		lookupNS:          lookupNS,
		sleep:             time.Sleep,
		delegationTimeout: 30 * time.Minute,
	}
	if !tty && !c.Yes {
		return c.missingFlagsError()
	}
	err := c.execute(env)
	if errors.Is(err, errAborted) {
		fmt.Fprintln(env.out, sDim("nothing was written"))
	}
	return err
}

// missingFlagsError is the non-TTY, no --yes exit: say exactly what to pass.
func (c *InitCmd) missingFlagsError() error {
	var missing []string
	if c.Region == "" {
		missing = append(missing, "--region")
	}
	if c.Mode == "" && c.Model == "" {
		missing = append(missing, "--mode bedrock|gpu")
	}
	if c.Domain == "" {
		missing = append(missing, "--domain <a domain you control>")
	}
	return fmt.Errorf("stdin is not a terminal, so init cannot prompt. Pass %s, or --yes --domain <domain> to take every other default (bedrock mode, S3 state)",
		strings.Join(missing, " ")+" (and --profile if not using the default credential chain)")
}

// settings is the result of the interview, before any file is touched.
type settings struct {
	profile, region, account, cluster string
	azs                               []string
	gpu                               bool
	model                             catalogModel
	gpuInstance                       string
	gpuCount                          int
	geo, bedrockRegion                string
	throwaway                         bool
	liveChecked                       string // cluster name the live-install lookup already ran for
	web                               webSettings
	checks                            []checkResult
}

func reportedErr(format string, a ...any) error { return reportedError{fmt.Sprintf(format, a...)} }

// validateLocal checks every flag that needs no network, before any call is made.
func (c *InitCmd) validateLocal(env *initEnv) error {
	if err := validateClusterName(c.ClusterName); err != nil {
		return env.fail("cluster name", c.ClusterName, "use 2-40 lowercase letters, digits and single hyphens, starting with a letter (e.g. darkplum-trial)", "invalid --cluster-name")
	}
	if c.GPUCount < 1 || c.GPUCount > 8 {
		return env.fail("gpu count", strconv.Itoa(c.GPUCount), "--gpu-count takes 1 to 8", "invalid --gpu-count")
	}
	if c.Region != "" {
		if err := validateRegion(c.Region); err != nil {
			return env.fail("region", c.Region, "use an AWS region name such as us-east-2", "invalid --region")
		}
	}
	if c.Model != "" {
		if err := validateModelID(c.Model); err != nil {
			return env.fail("model", c.Model, "use a Hugging Face id of the form org/name", "invalid --model")
		}
	}
	if c.ModelRevision != "" {
		if err := validateSHA(c.ModelRevision); err != nil {
			return env.fail("model revision", c.ModelRevision, "pass the full 40-character commit SHA from the model's Hugging Face Files page, not a branch or tag", "invalid --model-revision")
		}
	}
	if c.GPUInstance != "" {
		if _, ok := gpuInstances[c.GPUInstance]; !ok {
			return env.fail("gpu instance", c.GPUInstance, "supported: g5.xlarge, g5.2xlarge, g5.4xlarge", "unknown --gpu-instance")
		}
	}
	if c.Domain != "" {
		if d := normalizeDNSName(c.Domain); validateTFWebDomain(d) != nil {
			return env.fail("domain", c.Domain, "use a lowercase DNS name you control, such as acme.com", "invalid --domain")
		}
	}
	if (c.Hostname != "" || c.ZoneID != "" || c.CertificateARN != "" || c.AllowedCIDRs != "") && c.Domain == "" {
		return env.fail("domain", "not given", "--hostname, --zone-id, --certificate-arn and --allowed-cidrs need --domain", "web flags need --domain")
	}
	if c.AllowedCIDRs != "" {
		if err := validateCIDRList(c.AllowedCIDRs); err != nil {
			return env.fail("allowed networks", c.AllowedCIDRs, err.Error(), "invalid --allowed-cidrs")
		}
	}
	if _, inCatalog := catalogFor(c.Model); c.Offline && c.Model != "" && !inCatalog && c.ModelRevision == "" {
		return env.fail("model revision", c.Model, "--offline makes no Hugging Face call, so also pass --model-revision <40-hex SHA>", "--offline needs --model-revision for a model outside the catalog")
	}
	return nil
}

func (c *InitCmd) execute(env *initEnv) error {
	w := env.out
	if c.Cloud == "gcp" {
		fmt.Fprintf(w, "%s\n\n", sHeader("▸ init · gcp"))
		return env.fail("gcp terraform root", "not yet available", "the terraform/gcp root module is planned; AWS is the only installable cloud today", "gcp is not supported yet")
	}
	fmt.Fprintf(w, "%s\n\n", sHeader("▸ init · aws"))
	if err := c.validateLocal(env); err != nil {
		return err
	}

	dir := c.Dir
	if dir == "" {
		dir = filepath.Join("terraform", "aws")
	}
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		return env.fail("terraform root", dir, "run from the darkplum/ directory or pass --dir", "no Terraform root at "+dir)
	}

	// A backend.tf here is a Makefile-era install: its state is already somewhere,
	// and init no longer writes Terraform code into the checkout. Adopt it first.
	if _, err := os.Stat(filepath.Join(dir, "backend.tf")); err == nil {
		return env.fail("existing install", filepath.Join(dir, "backend.tf")+" is from the Makefile flow",
			"run `darkplum init --adopt` to take this install over (its state stays where it is)", "this directory holds an install from the Makefile flow; run darkplum init --adopt")
	}

	// An install from before the private address: offer to add just that.
	// Interactive only: a script re-running init (--yes) keeps today's
	// behaviour, a full rewrite guarded by --force.
	if raw := existingTFVars(filepath.Join(dir, "terraform.tfvars")); env.interactive && raw != nil && raw["web_domain"] == "" && !c.Force {
		merge := false
		{
			i, err := env.prompt.Select("This install has no private web address yet. What should init do?", []Option{
				{"Add one", "asks only for the address; keeps every other setting"},
				{"Start over", "asks every question again"},
			}, 0)
			if err != nil {
				return err
			}
			merge = i == 0
		}
		if merge {
			return c.mergeWeb(env, dir)
		}
	}

	s := &settings{throwaway: c.Throwaway, gpuCount: c.GPUCount}
	record := func(r checkResult) {
		printCheckTo(w, r)
		s.checks = append(s.checks, r)
	}

	// 1. Profile and identity.
	var cloud initCloud
	profile := c.Profile
	askedProfile := c.Profile != ""
	for {
		if profile == "" && env.interactive && !askedProfile {
			boot := env.newCloud("")
			if ps := boot.Profiles(); len(ps) > 0 {
				fmt.Fprintf(w, "  %s %s\n", sDim("profiles:"), strings.Join(ps, ", "))
			}
			var err error
			profile, err = env.prompt.Ask("AWS profile (empty = default credential chain)", os.Getenv("AWS_PROFILE"), nil)
			if err != nil {
				return err
			}
		}
		cloud = env.newCloud(profile)
		fmt.Fprintf(w, "%s resolving AWS identity…\n", sDim("…"))
		id, err := cloud.Identity()
		if err != nil {
			printCheckTo(w, checkResult{name: "aws credentials", status: checkFail, detail: firstLine(err.Error()),
				hint: "run `aws sso login` / `aws configure`, or choose another profile"})
			if !env.interactive || askedProfile {
				return reportedErr("cannot resolve AWS credentials for profile %q", profile)
			}
			profile = ""
			continue
		}
		if id.isRoot() {
			return env.fail("aws identity", id.Arn, "the root user cannot be used to install — sign in as an IAM or SSO role", "refusing to install as the account root user")
		}
		if err := validateAccountID(id.Account); err != nil {
			return reportedErr("unexpected account id %q from sts", id.Account)
		}
		s.profile, s.account = profile, id.Account
		fmt.Fprintf(w, "  Installing into account %s as %s\n", sInfo(id.Account), sInfo(id.Arn))
		record(checkResult{name: "aws account", status: checkPass, detail: id.Account})
		break
	}

	// 2. Region, probed before anything else so an opt-in region that is not
	// enabled fails here and not after the model questions.
	defRegion, regionNote := c.Region, ""
	if defRegion == "" {
		if r := cloud.ProfileRegion(); validateRegion(r) == nil {
			defRegion, regionNote = r, " (from profile)"
		} else {
			defRegion, regionNote = defaultRegion("aws"), " (default)"
		}
	}
	var region string
	for {
		region = c.Region
		if region == "" {
			if !env.interactive {
				region = defRegion
			} else {
				var err error
				if region, err = env.prompt.Ask("Region"+regionNote, defRegion, validateRegion); err != nil {
					return err
				}
			}
		}
		perr := cloud.CheckRegion(region)
		if perr == nil {
			break
		}
		if regionNotEnabled(perr) {
			printCheckTo(w, checkResult{name: "region", status: checkFail, detail: region + " is not enabled for account " + s.account,
				hint: "enable it under Account → AWS Regions, or choose another region"})
		} else {
			printCheckTo(w, checkResult{name: "region", status: checkFail, detail: "could not reach ec2." + region + ".amazonaws.com",
				hint: "check your network and permissions (ec2:DescribeAvailabilityZones), then retry"})
		}
		if !env.interactive || c.Region != "" {
			return reportedErr("region %s is not usable", region)
		}
	}
	note := ""
	if region == defRegion {
		note = regionNote
	}
	s.region = region
	record(checkResult{name: "region", status: checkPass, detail: region + note})

	// 3. Mode.
	mode := c.Mode
	if mode == "" && c.Model != "" {
		mode = "gpu"
	}
	if mode == "" {
		if env.interactive {
			i, err := env.prompt.Select("How should Darkplum run models?", []Option{
				{"Bedrock only", fmt.Sprintf("no GPU · ~US$%d/day base · ~20 min · recommended for a trial", baseUSDPerDay)},
				{"Self-hosted GPU", "base + ~US$1/h per g5.xlarge · ~35-50 min"},
			}, 0)
			if err != nil {
				return err
			}
			mode = []string{"bedrock", "gpu"}[i]
			fmt.Fprintf(w, "  %s\n", sDim("Switch later by re-running ./bin/darkplum init."))
		} else {
			mode = "bedrock"
		}
	}
	s.gpu = mode == "gpu"
	record(checkResult{name: "model hosting", status: checkPass, detail: map[bool]string{false: "Bedrock only", true: "Bedrock + self-hosted GPU"}[s.gpu]})

	// 4. Model. Bedrock-only installs still write the default model explicitly.
	s.model = modelCatalog[0]
	if s.gpu {
		if err := c.chooseModel(env, s, record); err != nil {
			return err
		}
	}

	// 5. Private web address.
	if err := c.chooseWeb(env, s, cloud, existingTFVars(filepath.Join(dir, "terraform.tfvars")), record); err != nil {
		return err
	}

	// Zones: where every needed instance type is offered.
	types := []string{defaultCPUInstance}
	if s.gpu {
		types = append(types, s.gpuInstance)
	}
	fmt.Fprintf(w, "%s resolving availability zones offering %s in %s…\n", sDim("…"), strings.Join(types, " + "), region)
	zones, err := cloud.OfferedZones(region, types...)
	if err != nil {
		if regionNotEnabled(err) {
			return env.fail("region", region+" is not enabled for account "+s.account, "enable it under Account → AWS Regions, or choose another region", "region not enabled")
		}
		return env.fail("availability zones", firstLine(err.Error()), "needs ec2:DescribeInstanceTypeOfferings in "+region+" (endpoint ec2."+region+".amazonaws.com)", "could not read instance offerings")
	}
	if len(zones) > 3 {
		zones = zones[:3]
	}
	if err := validateAZs(region, zones); err != nil {
		return env.fail("availability zones", strings.Join(types, " + ")+" in "+region+": "+err.Error(),
			"pick a region where they are offered in 2+ zones, or another --gpu-instance", "not enough availability zones")
	}
	s.azs = zones
	record(checkResult{name: "availability zones", status: checkPass, detail: strings.Join(zones, ", ") + " (derived)"})

	// Bedrock geography follows the region.
	geo, ok := geoForRegion(region)
	s.geo = geo
	if ok {
		record(checkResult{name: "inference geography", status: checkPass, detail: geo + " (derived from region)"})
	} else {
		s.bedrockRegion = "us-east-1"
		record(checkResult{name: "inference geography", status: checkWarn, detail: "us via us-east-1",
			hint: region + " is outside the us/eu/apac Bedrock geographies; Bedrock calls leave the VPC through the NAT gateway"})
	}

	// GPU quota for the chosen type.
	if s.gpu {
		q := cloud.GPUQuota(region, s.gpuInstance)
		record(q)
		if q.status == checkFail {
			if !env.interactive {
				return reportedErr("GPU quota too low for %s in %s", s.gpuInstance, region)
			}
			ok, err := env.prompt.Confirm("Continue anyway? The GPU node will not schedule until the quota is raised", false)
			if err != nil {
				return err
			}
			if !ok {
				return reportedErr("stopped: raise the GPU quota (or choose Bedrock only) and re-run ./bin/darkplum init")
			}
		}
	}

	// A fresh checkout of a live install: its state answers before the name
	// check, which would otherwise call the install's own cluster a clash.
	s.cluster = c.ClusterName
	if err := c.preflightLiveInstall(env, s, dir, cloud); err != nil {
		return err
	}

	// Cluster name collisions.
	cluster, err := chooseCluster(env, cloud, region, c.ClusterName, record)
	if err != nil {
		return err
	}
	s.cluster = cluster

	return c.finish(env, s, dir, cloud)
}

// chooseCluster returns the first cluster name, starting at name, that collides
// with nothing in the account. Interactive runs ask for another name; --yes stops.
func chooseCluster(env *initEnv, cloud initCloud, region, name string, record func(checkResult)) (string, error) {
	for {
		rows := cloud.NameCollisions(region, name)
		failed := false
		for _, r := range rows {
			// Only rows that need attention are shown; passes are summarised below.
			if r.status != checkPass {
				record(r)
			}
			failed = failed || r.status == checkFail
		}
		if !failed {
			record(checkResult{name: "cluster name", status: checkPass, detail: name + " is free"})
			return name, nil
		}
		if !env.interactive {
			return "", reportedErr("cluster name %q collides with an existing resource; pass another --cluster-name", name)
		}
		var err error
		if name, err = env.prompt.Ask("Cluster name", name+"-2", validateClusterName); err != nil {
			return "", err
		}
	}
}

// chooseModel fills the model fields for gpu mode.
func (c *InitCmd) chooseModel(env *initEnv, s *settings, record func(checkResult)) error {
	w := env.out
	id := c.Model
	if id == "" {
		if !env.interactive {
			id = modelCatalog[0].ID
		} else {
			opts := make([]Option, 0, len(modelCatalog)+1)
			for _, m := range modelCatalog {
				opts = append(opts, Option{m.ID, fmt.Sprintf("%d GB GPU, %s", m.GPUMemGB, m.Note)})
			}
			opts = append(opts, Option{"custom Hugging Face id", "validated against huggingface.co"})
			i, err := env.prompt.Select("Which model?", opts, 0)
			if err != nil {
				return err
			}
			if i < len(modelCatalog) {
				id = modelCatalog[i].ID
			} else {
				var err error
				if id, err = env.prompt.Ask("Hugging Face model id (org/name)", "", validateModelID); err != nil {
					return err
				}
			}
		}
	}
	rev := c.ModelRevision

	if m, ok := catalogFor(id); ok {
		if rev != "" {
			m.Revision = rev
		}
		s.model = m
		s.gpuInstance = m.GPUType
		record(checkResult{name: "model", status: checkPass, detail: fmt.Sprintf("%s @ %s (reviewed)", m.ID, shortSHA(m.Revision))})
	} else {
		m, err := c.resolveCustom(env, id, rev)
		if err != nil {
			return err
		}
		s.model, s.gpuInstance = m, defaultGPUInstance
		record(checkResult{name: "model", status: checkPass, detail: fmt.Sprintf("%s @ %s", m.ID, m.Revision)})
		fmt.Fprintf(w, "  %s\n", sDim("pinned revision "+m.Revision+" — weights cannot change under you"))
	}
	if c.GPUInstance != "" {
		s.gpuInstance = c.GPUInstance
	}
	record(checkResult{name: "gpu instance", status: checkPass, detail: s.gpuInstance + " (follows from the model)"})
	return nil
}

func shortSHA(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

// resolveCustom validates a model id outside the catalog against Hugging Face
// and pins its current commit SHA.
func (c *InitCmd) resolveCustom(env *initEnv, id, rev string) (catalogModel, error) {
	w := env.out
	m := catalogModel{ID: id, Served: servedNameFor(id), GPUMemGB: 24, GPUType: defaultGPUInstance, Revision: rev}
	if c.Offline {
		if rev == "" {
			return m, reportedErr("--offline needs --model-revision <40-hex commit SHA> for %s", id)
		}
		return m, nil
	}
	for {
		fmt.Fprintf(w, "%s resolving %s on %s…\n", sDim("…"), id, env.hf.endpoint())
		ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
		info, err := env.hf.Lookup(ctx, id)
		cancel()
		switch {
		case err == nil:
			if !info.Safetensors {
				printCheckTo(w, checkResult{name: "model weights", status: checkFail, detail: id + " has no .safetensors files",
					hint: "Darkplum only loads safetensors weights; pick another model"})
				return m, reportedErr("%s has no safetensors weights", id)
			}
			if info.WeightBytes > 0 {
				gib := float64(info.WeightBytes) / (1 << 30)
				if gib > float64(m.GPUMemGB)*0.85 {
					printCheckTo(w, checkResult{name: "model size", status: checkFail, detail: fmt.Sprintf("~%.0f GiB of weights vs %d GB GPU", gib, m.GPUMemGB),
						hint: "it will not fit a g5 GPU; choose a smaller or quantized model"})
					return m, reportedErr("%s does not fit in %d GB of GPU memory", id, m.GPUMemGB)
				}
			}
			if rev == "" {
				m.Revision = info.SHA
			}
			return m, nil
		case errors.Is(err, errHFNotFound):
			printCheckTo(w, checkResult{name: "model", status: checkFail, detail: id + " not found on Hugging Face", hint: "check the spelling (org/name) and that the repo is public"})
			return m, reportedErr("model %s not found on Hugging Face", id)
		case errors.Is(err, errHFGated):
			printCheckTo(w, checkResult{name: "model", status: checkWarn, detail: id + " is gated",
				hint: "needs a Hugging Face token: after make deploy run `make set-hf-token`"})
			if !env.interactive {
				return m, reportedErr("%s is gated; run init interactively to confirm, or choose an ungated model", id)
			}
			ok, cerr := env.prompt.Confirm("Use the gated model anyway?", false)
			if cerr != nil {
				return m, cerr
			}
			if !ok {
				return m, reportedErr("stopped: %s is gated", id)
			}
			if rev == "" {
				m.Revision, cerr = env.prompt.Ask("Commit SHA to pin (40 hex, from the model's Files page)", "", validateSHA)
				if cerr != nil {
					return m, cerr
				}
			}
			return m, nil
		default:
			printCheckTo(w, checkResult{name: "huggingface", status: checkFail, detail: firstLine(err.Error()),
				hint: "network error reaching " + env.hf.endpoint()})
			if !env.interactive {
				return m, reportedErr("could not reach %s (use --offline with --model-revision to skip)", env.hf.endpoint())
			}
			i, serr := env.prompt.Select("What now?", []Option{{"retry", ""}, {"skip the check and enter the revision myself", "unverified"}}, 0)
			if serr != nil {
				return m, serr
			}
			if i == 1 {
				if rev == "" {
					m.Revision, serr = env.prompt.Ask("Commit SHA to pin (40 hex)", "", validateSHA)
					if serr != nil {
						return m, serr
					}
				}
				return m, nil
			}
		}
	}
}

func (s *settings) tfvars() []tfvarsKV {
	var kv []tfvarsKV
	if s.profile != "" {
		kv = append(kv, tfvarsKV{"aws_profile", s.profile})
	}
	kv = append(kv,
		tfvarsKV{"allowed_account_ids", []string{s.account}},
		tfvarsKV{"aws_region", s.region},
		tfvarsKV{"azs", s.azs},
		tfvarsKV{"cluster_name", s.cluster},
		tfvarsKV{"bedrock_inference_geo", s.geo},
	)
	if s.bedrockRegion != "" {
		kv = append(kv, tfvarsKV{"bedrock_region", s.bedrockRegion})
	}
	if s.gpu {
		kv = append(kv, tfvarsKV{"gpu_desired_size", s.gpuCount}, tfvarsKV{"gpu_instance_types", []string{s.gpuInstance}})
	} else {
		kv = append(kv, tfvarsKV{"gpu_desired_size", 0})
	}
	// Always explicit, so a later change to variables.tf defaults cannot move an
	// install to another model.
	kv = append(kv,
		tfvarsKV{"model_name", s.model.ID},
		tfvarsKV{"model_revision", s.model.Revision},
		tfvarsKV{"served_model_name", s.model.Served},
	)
	kv = append(kv, s.web.kvs()...)
	if s.throwaway {
		kv = append(kv,
			tfvarsKV{"db_backup_retention_days", 1},
			tfvarsKV{"db_deletion_protection", false},
			tfvarsKV{"db_skip_final_snapshot", true},
		)
	}
	return kv
}

// existingBackend reads the bucket, key and region named in an existing backend.hcl.
func existingBackend(path string) (bucket, key, region string, ok bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", "", "", false
	}
	get := func(name string) string {
		m := regexp.MustCompile(`(?m)^\s*` + name + `\s*=\s*"([^"]+)"`).FindSubmatch(b)
		if m == nil {
			return ""
		}
		return string(m[1])
	}
	bucket, key, region = get("bucket"), get("key"), get("region")
	return bucket, key, region, bucket != "" && key != "" && region != ""
}

// localStateResources counts resources in dir/terraform.tfstate (0 when there is
// no state, an empty one, or one that does not parse).
func localStateResources(dir string) int {
	b, err := os.ReadFile(filepath.Join(dir, "terraform.tfstate"))
	if err != nil {
		return 0
	}
	var st struct {
		Resources []json.RawMessage `json:"resources"`
	}
	if json.Unmarshal(b, &st) != nil {
		return 0
	}
	return len(st.Resources)
}

// finish writes (or, with --dry-run, prints) the files and creates the state
// backend. Read-only pre-checks run first; nothing is touched until the final
// confirmation, and tfvars is written last so its presence means the install is
// ready for make deploy.
func (c *InitCmd) finish(env *initEnv, s *settings, dir string, cloud initCloud) error {
	w := env.out
	tfvarsPath := filepath.Join(dir, "terraform.tfvars")
	backendPath := filepath.Join(dir, backendFileName)

	// Where state lives: kept from backend.tf, an existing Darkplum bucket, or a
	// new one (init_state.go). May rename the cluster, so tfvars renders after.
	plan, err := c.planState(env, s, dir, cloud)
	if err != nil {
		return err
	}
	bucket, reused, st := plan.bucket, plan.kept, plan.st
	tfvarsText := renderTFVars(s.tfvars(), env.now())
	spec := backendSpec{Bucket: bucket, Key: plan.key, Region: s.region, KMSKeyARN: st.KeyARN}
	if spec.KMSKeyARN == "" {
		spec.KMSKeyARN = kmsKeyPlaceholder
	}
	localResources := localStateResources(dir)
	_, backendExists := os.Stat(backendPath)

	fmt.Fprintln(w)
	if c.DryRun {
		fmt.Fprintf(w, "%s\n%s\n", sHeader("terraform.tfvars (dry run — not written)"), tfvarsText)
		fmt.Fprintf(w, "%s\n%s\n", sHeader(backendFileName+" (dry run — not written)"), renderBackend(spec))
		fmt.Fprintf(w, "%s\n", sHeader("read-only pre-checks that ran"))
		for _, ch := range st.Checks {
			fmt.Fprintf(w, "  %s\n", sDim(ch))
		}
		fmt.Fprintf(w, "\n%s\n", sHeader("would run in account "+s.account+" (dry run — nothing run)"))
		for _, cmd := range plannedBackendCommands(s.profile, s.account, bucket, s.region, st) {
			fmt.Fprintf(w, "  %s\n", sDim(formatAWS(cmd)))
		}
		fmt.Fprintf(w, "\n  %s %s\n\n", sWarn("!"), kmsWarning)
		if s.web.createZone {
			fmt.Fprintf(w, "  %s would create the public hosted zone %s, show its 4 NS records to add at your DNS provider, and wait until they resolve\n\n", sWarn("!"), s.web.hostname)
		}
		if localResources > 0 {
			fmt.Fprintf(w, "  %s %d resources in local state would NOT move to S3 automatically; after writing, run: cd %s && terraform init -migrate-state\n\n", sWarn("!"), localResources, dir)
		}
		if plan.staleEmpty {
			fmt.Fprintf(w, "  %s\n\n", sDim("would set aside Terraform's saved settings for the earlier, empty state at "+plan.cache.url()))
		}
		if plan.staleLive != nil {
			fmt.Fprintf(w, "  %s %s at %s would NOT move automatically; after writing, run: %s\n\n", sWarn("!"), *plan.staleLive, plan.cache.url(), migrateCmd(dir, s.profile))
		}
		c.summary(env, s, nil, true)
		return nil
	}

	// Overwrite guard (a reused backend.hcl names the same bucket, so it is not
	// an overwrite worth asking about).
	var existing []string
	if _, err := os.Stat(tfvarsPath); err == nil {
		existing = append(existing, "terraform.tfvars")
	}
	if backendExists == nil && !reused {
		existing = append(existing, backendFileName)
	}
	if len(existing) > 0 && !c.Force {
		if !env.interactive {
			return env.fail("existing files", strings.Join(existing, " and ")+" in "+dir, "pass --force to overwrite (a .bak copy is kept)", strings.Join(existing, " and ")+" already exists; pass --force to overwrite")
		}
		ok, err := env.prompt.Confirm(fmt.Sprintf("%s exists. Overwrite it (a .bak copy is kept)?", strings.Join(existing, " and ")), false)
		if err != nil {
			return err
		}
		if !ok {
			return errAborted
		}
	}

	// Existing local state does not follow to S3: migrating is a manual step, so
	// make the operator acknowledge it BEFORE backend.hcl is written.
	if localResources > 0 {
		cmd := "cd " + dir + " && terraform init -migrate-state"
		printCheckTo(w, checkResult{name: "local state", status: checkWarn, detail: fmt.Sprintf("%d resources in %s", localResources, filepath.Join(dir, "terraform.tfstate")),
			hint: "it will not move to S3 by itself; after init run: " + cmd})
		if !env.interactive {
			return env.fail("local state", "existing resources would be orphaned", "run interactively to confirm, or migrate by hand: "+cmd, "existing local state; --yes will not switch the backend under it")
		}
		ok, err := env.prompt.Confirm("Keep state in S3 from now on? The existing install's state stays on this machine until you migrate it", false)
		if err != nil {
			return err
		}
		if !ok {
			return errAborted
		}
	}
	// The same for state in an earlier S3 location this checkout still points
	// at: init never moves it, and --yes never writes a backend away from it.
	if plan.staleLive != nil {
		if err := c.confirmStaleState(env, s, dir, plan); err != nil {
			return err
		}
	}
	if env.interactive {
		what := "Save these settings?"
		if !st.BucketExists {
			what = fmt.Sprintf("Save these settings? This creates a private, encrypted bucket in account %s to hold the install's state", s.account)
		}
		if s.web.createZone {
			what += fmt.Sprintf(", and creates a public hosted zone for %s", s.web.hostname)
		}
		ok, err := env.prompt.Confirm(what, true)
		if err != nil {
			return err
		}
		if !ok {
			return errAborted
		}
	}

	var wrote []string
	fmt.Fprintf(w, "%s preparing state bucket %s…\n", sDim("…"), bucket)
	arn, err := cloud.CreateBackend(bucket, s.region, st)
	if err != nil {
		return env.fail("state bucket", firstLine(err.Error()), "needs s3:CreateBucket/PutBucket* and kms:CreateKey/CreateAlias in "+s.region+"; re-run init (it reuses what was created)", "could not create the state backend")
	}
	spec.KMSKeyARN = arn
	if plan.staleEmpty {
		if err := setAsideBackendCache(dir, env.now()); err != nil {
			return reportedErr("could not set aside Terraform's saved backend settings in %s: %v", filepath.Join(dir, ".terraform"), err)
		}
		fmt.Fprintf(w, "  %s\n", sDim("set aside Terraform's saved settings for the earlier, empty state at "+plan.cache.url()))
	}
	if err := writeFileAtomic(backendPath, []byte(renderBackend(spec))); err != nil {
		return err
	}
	wrote = append(wrote, backendPath)
	if s.web.createZone {
		if err := c.createAndDelegate(env, s, cloud); err != nil {
			return err
		}
		tfvarsText = renderTFVars(s.tfvars(), env.now()) // now with web_zone_id
	}
	if err := writeFileAtomic(tfvarsPath, []byte(tfvarsText)); err != nil {
		return err
	}
	wrote = append(wrote, tfvarsPath)
	fmt.Fprintln(w)
	fmt.Fprintf(w, "  %s %s\n", sWarn("!"), kmsWarning)
	if plan.staleLive != nil {
		fmt.Fprintf(w, "  %s before make deploy, move the earlier state: %s\n", sWarn("!"), sCmd(migrateCmd(dir, s.profile)))
	}
	c.summary(env, s, wrote, false)
	return nil
}

// summary is the final screen: what was checked, what was written, what next.
func (c *InitCmd) summary(env *initEnv, s *settings, wrote []string, dry bool) {
	w := env.out
	warns := 0
	for _, r := range s.checks {
		if r.status == checkWarn {
			warns++
		}
	}
	passed := len(s.checks) - warns
	if dry {
		fmt.Fprintln(w, sSuccess("✓ dry run complete — nothing was written or created"))
		fmt.Fprintf(w, "\n  Next: re-run without --dry-run to write these files.\n")
		return
	}
	line := sSuccess(fmt.Sprintf("✓ %d passed", passed))
	if warns > 0 {
		word := "warnings"
		if warns == 1 {
			word = "warning"
		}
		line += sDim(",") + " " + sWarn(fmt.Sprintf("! %d %s", warns, word))
	}
	fmt.Fprintln(w, line)
	if len(wrote) > 0 {
		fmt.Fprintf(w, "  Settings saved. %s\n", sDim("Change them by re-running ./bin/darkplum init."))
	}
	if s.web.domain != "" {
		fmt.Fprintf(w, "  Your team's address after deploy: %s %s\n", sCmd("https://"+s.web.hostname), sDim("(from "+strings.Join(s.web.allowed, ", ")+")"))
	}
	eta := "~20 min"
	if s.gpu {
		eta = "~35-50 min"
	}
	fmt.Fprintf(w, "\n  Next: %s  %s, then %s\n", sCmd("make deploy"), sDim("("+eta+")"), sCmd("./bin/darkplum configure"))
}
