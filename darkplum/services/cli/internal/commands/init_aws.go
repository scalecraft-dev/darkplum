package commands

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/scalecraft-dev/darkplum/cli/internal/runner"
)

// cmdRunner runs an external command and returns trimmed stdout. On failure the
// error carries the command's stderr, which is how the AWS CLI reports codes
// such as ResourceNotFoundException.
type cmdRunner func(name string, args ...string) (string, error)

// runWithStderr is the real command runner: the runner package starts aws and
// terraform and nothing else.
func runWithStderr(name string, args ...string) (string, error) { return runner.Tool(name, args...) }

// identity is who the AWS CLI resolved the session to.
type identity struct {
	Account string `json:"Account"`
	Arn     string `json:"Arn"`
}

func (i identity) isRoot() bool { return strings.HasSuffix(i.Arn, ":root") }

// awsCloud is the AWS implementation of init's cloud interface. It shells out
// to the operator's own `aws` CLI under their own credentials; nothing here
// uses an SDK and nothing is created except by createBackend.
type awsCloud struct {
	profile string
	account string // set by Identity; every bucket call asserts it as the owner
	run     cmdRunner
}

func newAWSCloud(profile string, run cmdRunner) *awsCloud {
	if run == nil {
		run = runWithStderr
	}
	return &awsCloud{profile: profile, run: run}
}

func (a *awsCloud) aws(args ...string) (string, error) {
	return a.run("aws", awsArgs(a.profile, args...)...)
}

// Profiles lists configured profiles; failure is not an error (it is optional).
func (a *awsCloud) Profiles() []string {
	out, err := a.run("aws", "configure", "list-profiles")
	if err != nil {
		return nil
	}
	var ps []string
	for _, l := range strings.Split(out, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			ps = append(ps, l)
		}
	}
	return ps
}

// ProfileRegion returns the region configured for the profile, or "".
func (a *awsCloud) ProfileRegion() string {
	if a.profile == "" {
		if r := os.Getenv("AWS_REGION"); r != "" {
			return r
		}
		if r := os.Getenv("AWS_DEFAULT_REGION"); r != "" {
			return r
		}
	}
	out, err := a.aws("configure", "get", "region")
	if err != nil {
		return ""
	}
	return out
}

func (a *awsCloud) Identity() (identity, error) {
	out, err := a.aws("sts", "get-caller-identity", "--output", "json")
	if err != nil {
		return identity{}, err
	}
	var id identity
	if err := json.Unmarshal([]byte(out), &id); err != nil || id.Account == "" {
		return identity{}, fmt.Errorf("unexpected sts get-caller-identity output")
	}
	a.account = id.Account
	return id, nil
}

// OfferedZones returns the AZs of region (sorted) where every instance type is
// offered — subnets span all azs, so a node group fails if one lacks its type.
func (a *awsCloud) OfferedZones(region string, types ...string) ([]string, error) {
	var common map[string]bool
	for _, t := range types {
		out, err := a.aws("ec2", "describe-instance-type-offerings", "--location-type", "availability-zone",
			"--filters", "Name=instance-type,Values="+t, "--region", region,
			"--query", "InstanceTypeOfferings[].Location", "--output", "json")
		if err != nil {
			return nil, err
		}
		var zs []string
		if err := json.Unmarshal([]byte(out), &zs); err != nil {
			return nil, fmt.Errorf("unexpected describe-instance-type-offerings output")
		}
		cur := map[string]bool{}
		for _, z := range zs {
			if common == nil || common[z] {
				cur[z] = true
			}
		}
		common = cur
	}
	var zs []string
	for z := range common {
		zs = append(zs, z)
	}
	sort.Strings(zs)
	return zs, nil
}

// collision looks for a leftover resource that would make the install fail or
// silently adopt something else. Returned rows: fail = exists, warn = could not
// tell (or a leftover that deploy-infra imports), pass = free.
func (a *awsCloud) NameCollisions(region, name string) []checkResult {
	var rows []checkResult
	notFound := func(err error, codes ...string) bool {
		for _, c := range codes {
			if strings.Contains(err.Error(), c) {
				return true
			}
		}
		return false
	}
	unknown := func(what string, err error) checkResult {
		return checkResult{name: what, status: checkWarn, detail: "could not check",
			hint: "needs read access: " + firstLine(err.Error())}
	}
	suggest := "choose another --cluster-name (e.g. " + name + "-2) or remove the existing one"

	// EKS cluster.
	if _, err := a.aws("eks", "describe-cluster", "--name", name, "--region", region, "--query", "cluster.status", "--output", "text"); err == nil {
		rows = append(rows, checkResult{name: "cluster name free", status: checkFail, detail: "EKS cluster " + name + " exists", hint: suggest})
	} else if notFound(err, "ResourceNotFoundException") {
		rows = append(rows, checkResult{name: "cluster name free", status: checkPass, detail: "no EKS cluster " + name})
	} else {
		rows = append(rows, unknown("cluster name free", err))
	}
	// VPC (the vpc module tags it Name=<cluster>).
	if out, err := a.aws("ec2", "describe-vpcs", "--filters", "Name=tag:Name,Values="+name, "--region", region, "--query", "length(Vpcs)", "--output", "text"); err != nil {
		rows = append(rows, unknown("vpc name free", err))
	} else if out != "0" {
		rows = append(rows, checkResult{name: "vpc name free", status: checkFail, detail: "a VPC tagged Name=" + name + " exists", hint: suggest})
	} else {
		rows = append(rows, checkResult{name: "vpc name free", status: checkPass})
	}
	// RDS instance.
	if _, err := a.aws("rds", "describe-db-instances", "--db-instance-identifier", name+"-db", "--region", region, "--query", "length(DBInstances)", "--output", "text"); err == nil {
		rows = append(rows, checkResult{name: "database name free", status: checkFail, detail: "RDS instance " + name + "-db exists", hint: suggest})
	} else if notFound(err, "DBInstanceNotFound") {
		rows = append(rows, checkResult{name: "database name free", status: checkPass})
	} else {
		rows = append(rows, unknown("database name free", err))
	}
	// Log group left behind by an earlier install: deploy-infra imports it, so
	// this is a heads-up, not a failure.
	lg := "/aws/eks/" + name + "/cluster"
	if out, err := a.aws("logs", "describe-log-groups", "--log-group-name-prefix", lg, "--region", region,
		"--query", "length(logGroups[?logGroupName=='"+lg+"'])", "--output", "text"); err != nil {
		rows = append(rows, unknown("log group", err))
	} else if out == "1" {
		rows = append(rows, checkResult{name: "log group", status: checkWarn, detail: lg + " exists",
			hint: "left from an earlier install; make deploy imports and reuses it"})
	} else {
		rows = append(rows, checkResult{name: "log group", status: checkPass})
	}
	return rows
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// stateBucketName: the random suffix keeps the globally unique S3 namespace from
// being squatted or colliding; a re-run reuses the name recorded in backend.hcl,
// and a new install reuses an existing Darkplum bucket (StateBuckets).
func stateBucketName(account, region, suffix string) string {
	return stateBucketPrefix(account, region) + suffix
}

func stateBucketPrefix(account, region string) string {
	return fmt.Sprintf("darkplum-tfstate-%s-%s-", account, region)
}

// stateKeyFor is the state object of a new install. The cluster name keeps two
// installs that share an account, region and bucket apart.
func stateKeyFor(cluster string) string {
	return "darkplum/aws/" + cluster + "/terraform.tfstate"
}

const (
	stateKeyAlias = "alias/darkplum-tfstate"
	// legacyStateKey is the key every install used before the cluster name was
	// part of it. An existing backend.hcl keeps whatever key it names.
	legacyStateKey    = "darkplum/aws/terraform.tfstate"
	kmsKeyPlaceholder = "<kms-key-arn>"
	kmsWarning        = "Deleting the KMS key alias/darkplum-tfstate makes every state file in the bucket unrecoverable."
)

// backendState is what the read-only pre-checks found.
type backendState struct {
	KeyARN       string // key to use: the alias's key, else the existing bucket's key; empty = create one
	BucketExists bool
	Versioning   string   // an existing bucket's versioning status ("" = could not tell)
	Checks       []string // the read-only commands that were run, for --dry-run
}

func isNotFound(err error, codes ...string) bool {
	for _, c := range codes {
		if strings.Contains(err.Error(), c) {
			return true
		}
	}
	return false
}

func (a *awsCloud) keyARN(region, id string) (string, error) {
	return a.aws("kms", "describe-key", "--region", region, "--key-id", id, "--query", "KeyMetadata.Arn", "--output", "text")
}

// Inspect runs only read-only calls. The alias is the source of truth for the
// key; an existing bucket's encryption key is never replaced, so a mismatch
// between the two is an error, not something to paper over.
func (a *awsCloud) Inspect(bucket, region string) (backendState, error) {
	var st backendState
	rec := func(args ...string) (string, error) {
		st.Checks = append(st.Checks, formatAWS(awsArgs(a.profile, args...)))
		return a.aws(args...)
	}
	aliasARN, err := rec("kms", "describe-key", "--region", region, "--key-id", stateKeyAlias, "--query", "KeyMetadata.Arn", "--output", "text")
	if err != nil {
		if !isNotFound(err, "NotFoundException") {
			return st, fmt.Errorf("check KMS alias %s: %w", stateKeyAlias, err)
		}
		aliasARN = ""
	}
	_, err = rec("s3api", "head-bucket", "--bucket", bucket, "--region", region, "--expected-bucket-owner", a.account)
	switch {
	case err == nil:
		st.BucketExists = true
	case isNotFound(err, "(404)", "Not Found", "NoSuchBucket"):
	default:
		return st, fmt.Errorf("bucket %s is not reachable or not owned by account %s: %w", bucket, a.account, err)
	}
	st.KeyARN = aliasARN
	if st.BucketExists {
		out, err := rec("s3api", "get-bucket-encryption", "--bucket", bucket, "--expected-bucket-owner", a.account,
			"--query", "ServerSideEncryptionConfiguration.Rules[0].ApplyServerSideEncryptionByDefault", "--output", "json")
		if err != nil {
			return st, fmt.Errorf("read encryption of bucket %s: %w", bucket, err)
		}
		var sse struct {
			Alg string `json:"SSEAlgorithm"`
			Key string `json:"KMSMasterKeyID"`
		}
		if err := json.Unmarshal([]byte(out), &sse); err != nil || sse.Alg != "aws:kms" || sse.Key == "" {
			return st, fmt.Errorf("bucket %s is not encrypted with a customer KMS key; refusing to reuse it", bucket)
		}
		bucketARN, err := rec("kms", "describe-key", "--region", region, "--key-id", sse.Key, "--query", "KeyMetadata.Arn", "--output", "text")
		if err != nil {
			return st, fmt.Errorf("resolve bucket key %s: %w", sse.Key, err)
		}
		if aliasARN != "" && aliasARN != bucketARN {
			return st, fmt.Errorf("bucket %s is encrypted with %s but %s points at %s; init never replaces a bucket's key, so reconcile them (point the alias at the bucket's key, or name a new bucket) and re-run",
				bucket, bucketARN, stateKeyAlias, aliasARN)
		}
		st.KeyARN = bucketARN
		// Not fatal: CreateBackend turns versioning on again; the caller warns.
		if v, err := rec("s3api", "get-bucket-versioning", "--bucket", bucket, "--expected-bucket-owner", a.account,
			"--query", "Status", "--output", "text"); err == nil {
			if v == "" || v == "None" {
				v = "Off"
			}
			st.Versioning = v
		}
	}
	return st, nil
}

const (
	purposeTagKey   = "darkplum:purpose"
	purposeTagValue = "terraform-state"
)

// stateBucket is an existing Darkplum state bucket that StateBuckets found
// usable: owned by the account, versioned, public access fully blocked,
// encrypted with the state key.
type stateBucket struct {
	Name     string
	Created  time.Time
	Untagged bool // no darkplum:purpose tag: made by an earlier release
}

// StateBuckets lists this account's Darkplum state buckets for region, newest
// first. Read-only. A bucket that is not owned by the account, not versioned,
// not fully blocked from public access, not encrypted with the
// alias/darkplum-tfstate key, or tagged for another purpose is left out: init
// only reuses a bucket it would have created itself. An untagged bucket that
// passes every other check is accepted (buckets from before the tag).
func (a *awsCloud) StateBuckets(region string) ([]stateBucket, error) {
	prefix := stateBucketPrefix(a.account, region)
	out, err := a.aws("s3api", "list-buckets", "--query",
		fmt.Sprintf("Buckets[?starts_with(Name, '%s')].{Name:Name,Created:CreationDate}", prefix), "--output", "json")
	if err != nil {
		return nil, err
	}
	var listed []struct {
		Name    string `json:"Name"`
		Created string `json:"Created"`
	}
	if out != "" && out != "null" {
		if err := json.Unmarshal([]byte(out), &listed); err != nil {
			return nil, fmt.Errorf("unexpected list-buckets output")
		}
	}
	if len(listed) == 0 {
		return nil, nil
	}
	aliasARN, err := a.keyARN(region, stateKeyAlias)
	if err != nil {
		if isNotFound(err, "NotFoundException") {
			return nil, nil // without the state key no bucket qualifies
		}
		return nil, fmt.Errorf("check KMS alias %s: %w", stateKeyAlias, err)
	}
	owner := []string{"--expected-bucket-owner", a.account}
	var found []stateBucket
	for _, l := range listed {
		if !strings.HasPrefix(l.Name, prefix) {
			continue
		}
		v, err := a.aws(append([]string{"s3api", "get-bucket-versioning", "--bucket", l.Name, "--region", region,
			"--query", "Status", "--output", "text"}, owner...)...)
		if err != nil || v != "Enabled" {
			continue
		}
		pab, err := a.aws(append([]string{"s3api", "get-public-access-block", "--bucket", l.Name, "--region", region,
			"--query", "PublicAccessBlockConfiguration", "--output", "json"}, owner...)...)
		if err != nil {
			continue
		}
		var block struct {
			A bool `json:"BlockPublicAcls"`
			B bool `json:"IgnorePublicAcls"`
			C bool `json:"BlockPublicPolicy"`
			D bool `json:"RestrictPublicBuckets"`
		}
		if json.Unmarshal([]byte(pab), &block) != nil || !(block.A && block.B && block.C && block.D) {
			continue
		}
		untagged := false
		tags, err := a.aws(append([]string{"s3api", "get-bucket-tagging", "--bucket", l.Name, "--region", region,
			"--query", "TagSet", "--output", "json"}, owner...)...)
		switch {
		case err != nil && isNotFound(err, "NoSuchTagSet"):
			untagged = true
		case err != nil:
			continue
		default:
			var ts []struct{ Key, Value string }
			if json.Unmarshal([]byte(tags), &ts) != nil {
				continue
			}
			purpose := ""
			for _, t := range ts {
				if t.Key == purposeTagKey {
					purpose = t.Value
				}
			}
			if purpose == "" {
				untagged = true
			} else if purpose != purposeTagValue {
				continue
			}
		}
		enc, err := a.aws(append([]string{"s3api", "get-bucket-encryption", "--bucket", l.Name, "--region", region,
			"--query", "ServerSideEncryptionConfiguration.Rules[0].ApplyServerSideEncryptionByDefault", "--output", "json"}, owner...)...)
		if err != nil {
			continue
		}
		var sse struct {
			Alg string `json:"SSEAlgorithm"`
			Key string `json:"KMSMasterKeyID"`
		}
		if json.Unmarshal([]byte(enc), &sse) != nil || sse.Alg != "aws:kms" || sse.Key == "" {
			continue
		}
		if sse.Key != aliasARN {
			if arn, err := a.keyARN(region, sse.Key); err != nil || arn != aliasARN {
				continue
			}
		}
		created, _ := time.Parse(time.RFC3339, l.Created)
		found = append(found, stateBucket{Name: l.Name, Created: created, Untagged: untagged})
	}
	sort.SliceStable(found, func(i, j int) bool { return found[i].Created.After(found[j].Created) })
	return found, nil
}

// PrefixBuckets lists every bucket with this account's Darkplum state prefix in
// region, with ownership asserted by head-bucket, and nothing else checked: it
// answers "where could an install keep state", not "which bucket may a new
// install reuse". Read-only. Any failure is returned, never read as "none".
func (a *awsCloud) PrefixBuckets(region string) ([]string, error) {
	prefix := stateBucketPrefix(a.account, region)
	out, err := a.aws("s3api", "list-buckets", "--query",
		fmt.Sprintf("Buckets[?starts_with(Name, '%s')].Name", prefix), "--output", "json")
	if err != nil {
		return nil, err
	}
	var listed []string
	if out != "" && out != "null" {
		if err := json.Unmarshal([]byte(out), &listed); err != nil {
			return nil, fmt.Errorf("unexpected list-buckets output")
		}
	}
	var names []string
	for _, n := range listed {
		if !strings.HasPrefix(n, prefix) {
			continue
		}
		if _, err := a.aws("s3api", "head-bucket", "--bucket", n, "--region", region, "--expected-bucket-owner", a.account); err != nil {
			if isNotFound(err, "(404)", "Not Found", "NoSuchBucket") {
				continue // deleted since the listing
			}
			return nil, fmt.Errorf("bucket %s: %w", n, err)
		}
		names = append(names, n)
	}
	return names, nil
}

// stateContents is what a Terraform state object holds.
type stateContents struct {
	Resources, Outputs int
	ClusterName        string // the EKS cluster the state manages; "" = unknown
}

// empty: no resources and no outputs. A state with outputs but no resources
// still belongs to someone.
func (c stateContents) empty() bool { return c.Resources == 0 && c.Outputs == 0 }

func (c stateContents) String() string {
	s := fmt.Sprintf("%d resources", c.Resources)
	if c.Outputs > 0 {
		s += fmt.Sprintf(", %d outputs", c.Outputs)
	}
	return s
}

// ReadState reads a Terraform state object and counts its resources and
// outputs. found is false when the bucket or the object does not exist.
// Read-only; the bucket's owner is asserted before the object is read.
func (a *awsCloud) ReadState(bucket, key, region string) (sc stateContents, found bool, err error) {
	if _, err := a.aws("s3api", "head-bucket", "--bucket", bucket, "--region", region, "--expected-bucket-owner", a.account); err != nil {
		if isNotFound(err, "(404)", "Not Found", "NoSuchBucket") {
			return sc, false, nil
		}
		return sc, false, fmt.Errorf("bucket %s is not readable or not owned by account %s: %w", bucket, a.account, err)
	}
	out, err := a.aws("s3", "cp", "s3://"+bucket+"/"+key, "-", "--region", region)
	if err != nil {
		if isNotFound(err, "(404)", "does not exist", "NoSuchKey", "Not Found") {
			return sc, false, nil
		}
		return sc, false, err
	}
	sc, err = parseState([]byte(out))
	if err != nil {
		return sc, true, fmt.Errorf("s3://%s/%s is not a Terraform state file", bucket, key)
	}
	return sc, true, nil
}

// parseState counts a state's resources and outputs and finds the EKS cluster
// it manages: the aws_eks_cluster resource's name, else the cluster_name output.
func parseState(b []byte) (stateContents, error) {
	var st struct {
		Resources []struct {
			Type      string `json:"type"`
			Instances []struct {
				Attributes struct {
					Name string `json:"name"`
				} `json:"attributes"`
			} `json:"instances"`
		} `json:"resources"`
		Outputs map[string]struct {
			Value json.RawMessage `json:"value"`
		} `json:"outputs"`
	}
	if err := json.Unmarshal(b, &st); err != nil {
		return stateContents{}, err
	}
	sc := stateContents{Resources: len(st.Resources), Outputs: len(st.Outputs)}
	for _, r := range st.Resources {
		if r.Type == "aws_eks_cluster" {
			for _, in := range r.Instances {
				if in.Attributes.Name != "" {
					sc.ClusterName = in.Attributes.Name
					return sc, nil
				}
			}
		}
	}
	if o, ok := st.Outputs["cluster_name"]; ok {
		var name string
		if json.Unmarshal(o.Value, &name) == nil {
			sc.ClusterName = name
		}
	}
	return sc, nil
}

// bucketCommands are the s3api calls that create and harden the bucket, with
// --expected-bucket-owner on every call that accepts it (create-bucket does not).
// Pure: --dry-run prints exactly this list.
func bucketCommands(profile, account, bucket, region, kmsARN string, exists bool) [][]string {
	owner := []string{"--expected-bucket-owner", account}
	create := []string{"s3api", "create-bucket", "--bucket", bucket, "--region", region}
	if region != "us-east-1" {
		create = append(create, "--create-bucket-configuration", "LocationConstraint="+region)
	}
	policy := fmt.Sprintf(`{"Version":"2012-10-17","Statement":[`+
		`{"Sid":"DenyInsecureTransport","Effect":"Deny","Principal":"*","Action":"s3:*","Resource":["arn:aws:s3:::%[1]s","arn:aws:s3:::%[1]s/*"],"Condition":{"Bool":{"aws:SecureTransport":"false"}}},`+
		`{"Sid":"DenyWrongKmsKey","Effect":"Deny","Principal":"*","Action":"s3:PutObject","Resource":"arn:aws:s3:::%[1]s/*","Condition":{"StringNotEqualsIfExists":{"s3:x-amz-server-side-encryption-aws-kms-key-id":%[2]q}}}]}`, bucket, kmsARN)
	enc := fmt.Sprintf(`{"Rules":[{"ApplyServerSideEncryptionByDefault":{"SSEAlgorithm":"aws:kms","KMSMasterKeyID":%q},"BucketKeyEnabled":true}]}`, kmsARN)
	var cmds [][]string
	if !exists {
		// Tagged like the KMS key, so a later init can tell its own buckets.
		// Only at creation: put-bucket-tagging replaces the whole tag set.
		cmds = append(cmds, create, append([]string{"s3api", "put-bucket-tagging", "--bucket", bucket, "--tagging",
			`{"TagSet":[{"Key":"` + purposeTagKey + `","Value":"` + purposeTagValue + `"}]}`}, owner...))
	}
	cmds = append(cmds,
		append([]string{"s3api", "put-public-access-block", "--bucket", bucket, "--public-access-block-configuration",
			"BlockPublicAcls=true,IgnorePublicAcls=true,BlockPublicPolicy=true,RestrictPublicBuckets=true"}, owner...),
		append([]string{"s3api", "put-bucket-versioning", "--bucket", bucket, "--versioning-configuration", "Status=Enabled"}, owner...),
	)
	if !exists { // an existing bucket's encryption key is never replaced
		cmds = append(cmds, append([]string{"s3api", "put-bucket-encryption", "--bucket", bucket, "--server-side-encryption-configuration", enc}, owner...))
	}
	cmds = append(cmds, append([]string{"s3api", "put-bucket-policy", "--bucket", bucket, "--policy", policy}, owner...))
	for i := range cmds {
		cmds[i] = awsArgs(profile, cmds[i]...)
	}
	return cmds
}

func kmsCreateArgs(region string) []string {
	return []string{"kms", "create-key", "--region", region, "--description", "Darkplum Terraform state encryption",
		"--tags", "TagKey=darkplum:purpose,TagValue=terraform-state", "--query", "KeyMetadata.Arn", "--output", "text"}
}

func kmsAliasArgs(region, arn string) []string {
	return []string{"kms", "create-alias", "--region", region, "--alias-name", stateKeyAlias, "--target-key-id", arn}
}

// plannedBackendCommands is the full mutating plan for --dry-run, given what
// the pre-checks found.
func plannedBackendCommands(profile, account, bucket, region string, st backendState) [][]string {
	arn := st.KeyARN
	var cmds [][]string
	if arn == "" {
		arn = kmsKeyPlaceholder
		cmds = append(cmds, awsArgs(profile, kmsCreateArgs(region)...), awsArgs(profile, kmsAliasArgs(region, kmsKeyPlaceholder)...))
	}
	return append(cmds, bucketCommands(profile, account, bucket, region, arn, st.BucketExists)...)
}

// formatAWS renders an aws invocation for display.
func formatAWS(args []string) string {
	parts := []string{"aws"}
	for _, a := range args {
		if strings.ContainsAny(a, " {}\"'<>") {
			a = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
		}
		parts = append(parts, a)
	}
	return strings.Join(parts, " ")
}

// CreateBackend creates what Inspect found missing and returns the KMS key ARN
// for backend.hcl. The alias is created immediately after the key and the key is
// scheduled for deletion if that fails, so a re-run can never accumulate keys.
func (a *awsCloud) CreateBackend(bucket, region string, st backendState) (string, error) {
	arn := st.KeyARN
	if arn == "" {
		var err error
		if arn, err = a.aws(kmsCreateArgs(region)...); err != nil {
			return "", fmt.Errorf("create KMS key: %w", err)
		}
		if _, err = a.aws(kmsAliasArgs(region, arn)...); err != nil {
			_, rbErr := a.aws("kms", "schedule-key-deletion", "--region", region, "--key-id", arn, "--pending-window-in-days", "7")
			msg := fmt.Sprintf("create KMS alias %s: %v", stateKeyAlias, err)
			if rbErr != nil {
				msg += fmt.Sprintf(" (and the new key %s could not be scheduled for deletion: %v)", arn, rbErr)
			}
			return "", errors.New(msg)
		}
		if _, err = a.aws("kms", "enable-key-rotation", "--region", region, "--key-id", arn); err != nil {
			return "", fmt.Errorf("enable KMS key rotation: %w", err)
		}
	}
	for _, c := range bucketCommands("", a.account, bucket, region, arn, st.BucketExists) {
		if _, err := a.aws(c...); err != nil {
			return "", fmt.Errorf("aws %s: %w", c[0]+" "+c[1], err)
		}
	}
	return arn, nil
}

// GPUQuota is doctor's quota check, parameterised by the instance's vCPUs and
// run under init's own runner.
func (a *awsCloud) GPUQuota(region, instance string) checkResult {
	out, err := a.aws(quotaArgs(region)...)
	return gpuQuotaResult(out, err, region, instance)
}

// CheckRegion probes the region with a read-only call. An opt-in region that is
// not enabled for the account answers AuthFailure / OptInRequired.
func (a *awsCloud) CheckRegion(region string) error {
	_, err := a.aws("ec2", "describe-availability-zones", "--region", region, "--query", "AvailabilityZones[].ZoneName", "--output", "text")
	return err
}

// regionNotEnabled reports whether err means the region is not enabled for the account.
func regionNotEnabled(err error) bool {
	return err != nil && isNotFound(err, "AuthFailure", "OptInRequired", "not enabled", "InvalidClientTokenId")
}
