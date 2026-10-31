package commands

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/scalecraft-dev/darkplum/cli/internal/iamcheck"
	"github.com/scalecraft-dev/darkplum/cli/internal/release"
)

func TestDeployFreshBedrockOnlyInstallOrder(t *testing.T) {
	h := newDeployHarness(t, 0)
	if err := h.run(); err != nil {
		t.Fatalf("%v\n%s\n%s", err, h.out.String(), h.tl)
	}
	tl := h.tl
	order := []string{
		"verify images",           // nothing is written before the images verify
		"terraform init",          //
		"terraform state list",    // log group adoption reads the state first
		"terraform plan",          //
		"terraform apply",         //
		"terraform output -json",  // outputs are read ONCE, after apply
		"mirror images us-east-2", //
		"apply -f -",              // namespaces, policies, identities
		"create -f -",             // darkplum-auth and the user-hmac key
		"rollout status deployment/darkplum-api",
		"rollout status deployment/darkplum-gateway",
		"rollout status deployment/darkplum-web",
		"iam simulate",
	}
	prev := -1
	for _, step := range order {
		i := tl.first(step)
		if i < 0 {
			t.Fatalf("step %q never happened:\n%s", step, tl)
		}
		if i < prev {
			t.Errorf("step %q happened before an earlier step (index %d < %d):\n%s", step, i, prev, tl)
		}
		prev = i
	}
	if n := tl.count("terraform output"); n != 1 {
		t.Errorf("terraform output was read %d times, want once", n)
	}

	// Policies before any workload; secrets before the api; the api (schema owner) before the gateway.
	lists := h.appliedLists()
	sawWorkload := false
	for _, kinds := range lists {
		for _, k := range kinds {
			if strings.HasPrefix(k, "Deployment/") {
				sawWorkload = true
			}
			if strings.HasPrefix(k, "NetworkPolicy/") && sawWorkload {
				t.Errorf("%s applied after a workload started: %v", k, lists)
			}
		}
	}
	if tl.first("create -f -") > tl.first("rollout status deployment/darkplum-api") {
		t.Error("the auth secret was created after the api rolled out")
	}
	if tl.first("rollout status deployment/darkplum-api") > tl.first("rollout status deployment/darkplum-gateway") {
		t.Error("gateway rolled out before the api")
	}
	// Bedrock-only: nothing about GPUs.
	if tl.count("nvidia") != 0 || tl.count("vllm-inference") != 0 {
		t.Errorf("a Bedrock-only install touched the GPU model:\n%s", tl)
	}
	// The whole run is pinned and no secret value is in argv.
	h.k.assertClean("s3cr3t-db-pw")
	h.mustContain("Next: darkplum configure", "Darkplum is deployed")
	if h.writes() == 0 {
		t.Error("nothing was applied")
	}
	// The install record was written to the cluster.
	cm := false
	for _, c := range h.k.matching("apply -f -") {
		if strings.Contains(string(c.stdin), `"name":"darkplum-install"`) {
			cm = true
			if !strings.Contains(string(c.stdin), `"image-trust":"verified"`) || !strings.Contains(string(c.stdin), `"version":"1.2.3"`) || !strings.Contains(string(c.stdin), `"time":"`) {
				t.Errorf("darkplum-install = %s", c.stdin)
			}
		}
	}
	if !cm {
		t.Error("darkplum-install ConfigMap was not written")
	}
	// What `status` reads: ConfigMap keys and Deployment annotations.
	if !strings.Contains(h.out.String(), "image-trust verified") {
		t.Error("image trust not reported")
	}
	ann := h.k.matching("annotate deployment/darkplum-web")
	if len(ann) != 1 || !strings.Contains(ann[0].joined(), "darkplum.ai/image-trust=verified") || !strings.Contains(ann[0].joined(), "darkplum.ai/release-channel=release") {
		t.Errorf("annotations = %v", ann)
	}
	if rec, _ := h.in.Load(); rec.Bundle != "1.2.3" {
		t.Errorf("the install record does not remember the applied release: %q", rec.Bundle)
	}
}

func TestDeployWorkloadsRunTheMirroredDigests(t *testing.T) {
	h := newDeployHarness(t, 0)
	if err := h.run(); err != nil {
		t.Fatal(err)
	}
	found := map[string]bool{}
	for _, c := range h.k.matching("apply -f -") {
		for svc, ref := range h.images {
			if strings.Contains(string(c.stdin), `"image":"`+ref+`"`) {
				found[svc] = true
			}
		}
		if strings.Contains(string(c.stdin), "darkplum-gateway:latest") || strings.Contains(string(c.stdin), "PLACEHOLDER_") {
			t.Errorf("an unpatched manifest was applied: %s", c.stdin)
		}
	}
	if len(found) != 3 {
		t.Errorf("deployments applied with the mirrored digests: %v", found)
	}
}

func TestDeployNoInstallRunsNothing(t *testing.T) {
	// A fake aws/terraform/kubectl on PATH that leave a marker if anything starts them.
	bin := t.TempDir()
	marker := filepath.Join(bin, "ran")
	for _, tool := range []string{"aws", "terraform", "kubectl"} {
		if err := os.WriteFile(filepath.Join(bin, tool), []byte("#!/bin/sh\necho "+tool+" >> '"+marker+"'\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin)
	t.Setenv("DARKPLUM_HOME", t.TempDir())
	err := (&DeployCmd{}).Run(&Globals{Namespace: platformNS})
	if err == nil || !IsReported(err) {
		t.Fatalf("err = %v, want a reported refusal", err)
	}
	if _, statErr := os.Stat(marker); statErr == nil {
		b, _ := os.ReadFile(marker)
		t.Fatalf("started external programs with no install: %s", b)
	}
}

func TestDeployRefusesAnotherNamespace(t *testing.T) {
	if err := (&DeployCmd{}).Run(&Globals{Namespace: "elsewhere"}); err == nil {
		t.Fatal("deploy installs into darkplum-system only")
	}
}

func TestDeployPreflightFailureChangesNothing(t *testing.T) {
	h := newDeployHarness(t, 0)
	h.checks = []checkResult{{name: "session lifetime", status: checkFail, detail: "12 min remaining", hint: "aws sso login"}}
	err := h.run()
	if !IsReported(err) {
		t.Fatalf("err = %v", err)
	}
	if h.tl.count("terraform") != 0 || h.writes() != 0 || h.tl.count("verify") != 0 {
		t.Errorf("a failed preflight went on:\n%s", h.tl)
	}
	h.mustContain("Nothing was changed")
}

func TestDeployBadSignaturesStopBeforeTerraform(t *testing.T) {
	h := newDeployHarness(t, 0)
	h.resolveErr = reportedError{"1 of 3 images failed verification"}
	if err := h.run(); err == nil {
		t.Fatal("deployed unverified images")
	}
	if h.tl.count("terraform") != 0 || h.tl.count("mirror") != 0 || h.writes() != 0 {
		t.Errorf("a bad release changed something:\n%s", h.tl)
	}
}

func TestDeployExistingClusterInTheWrongAccountStopsBeforeTerraform(t *testing.T) {
	h := newDeployHarness(t, 0)
	h.awsf.base.describeErr = nil
	h.awsf.base.caller = "999999999999" // credentials for another account
	err := h.run()
	if !IsReported(err) {
		t.Fatalf("err = %v", err)
	}
	if h.tl.count("terraform") != 0 || h.writes() != 0 {
		t.Errorf("went on under another account's credentials:\n%s", h.tl)
	}
	if h.tl.count("describe-cluster") != 0 {
		t.Error("another account's credentials must not even learn whether the cluster exists")
	}
}

func liveCluster(h *deployHarness) { h.awsf.base.describeErr = nil }

func webSvc(typ, scheme, host string) string {
	ann := map[string]string{}
	if scheme != "" {
		ann["service.beta.kubernetes.io/aws-load-balancer-scheme"] = scheme
	}
	b, _ := json.Marshal(map[string]any{
		"metadata": map[string]any{"annotations": ann},
		"spec":     map[string]any{"type": typ},
		"status":   map[string]any{"loadBalancer": map[string]any{"ingress": []any{map[string]any{"hostname": host}}}},
	})
	return string(b)
}

func TestDeployHardStopsOnAnInternetFacingWebService(t *testing.T) {
	for name, svc := range map[string]string{
		"internet-facing":               webSvc("LoadBalancer", "internet-facing", "abc.elb.us-east-2.amazonaws.com"),
		"LoadBalancer without a scheme": webSvc("LoadBalancer", "", "def.elb.us-east-2.amazonaws.com"),
		"internal LoadBalancer":         webSvc("LoadBalancer", "internal", "ghi.elb.us-east-2.amazonaws.com"),
	} {
		t.Run(name, func(t *testing.T) {
			h := newDeployHarness(t, 0)
			liveCluster(h)
			h.k.handlers = append([]skHandler{{"get svc darkplum-web", func([]string, []byte) ([]byte, error) { return []byte(svc), nil }}}, h.k.handlers...)
			err := h.run()
			if !IsReported(err) {
				t.Fatalf("err = %v", err)
			}
			if h.tl.count("terraform") != 0 || h.tl.count("mirror") != 0 || h.writes() != 0 {
				t.Errorf("the run went on past the hard stop:\n%s", h.tl)
			}
			h.mustContain("elb.us-east-2.amazonaws.com", "private load balancer", "Nothing was changed")
		})
	}
}

func TestDeployClusterInternalWebServiceIsFine(t *testing.T) {
	h := newDeployHarness(t, 0)
	liveCluster(h)
	h.k.handlers = append([]skHandler{{"get svc darkplum-web", func([]string, []byte) ([]byte, error) { return []byte(webSvc("ClusterIP", "", "")), nil }}}, h.k.handlers...)
	if err := h.run(); err != nil {
		t.Fatalf("%v\n%s", err, h.out.String())
	}
}

func TestDeployRefusesAHelmInstalledRouter(t *testing.T) {
	h := newDeployHarness(t, 0)
	liveCluster(h)
	h.k.handlers = append([]skHandler{{"owner=helm", func([]string, []byte) ([]byte, error) {
		return []byte("secret/sh.helm.release.v1.semantic-router.v1\n"), nil
	}}}, h.k.handlers...)
	if !IsReported(h.run()) {
		t.Fatal("must refuse")
	}
	if h.tl.count("terraform") != 0 || h.writes() != 0 {
		t.Errorf("went on:\n%s", h.tl)
	}
	h.mustContain("semantic-router", "Nothing was changed")
	// The probe is a read of helm release Secrets only, pinned like every call.
	h.k.assertClean()
}

func TestDeployInspectionFailureIsNotAssumedHealthy(t *testing.T) {
	h := newDeployHarness(t, 0)
	liveCluster(h)
	h.k.handlers = append([]skHandler{{"get svc darkplum-web", func([]string, []byte) ([]byte, error) {
		return nil, errors.New("kubectl: error: You must be logged in to the server (Unauthorized)")
	}}}, h.k.handlers...)
	if !IsReported(h.run()) {
		t.Fatal("must stop")
	}
	if h.tl.count("terraform") != 0 {
		t.Error("terraform ran although the live web service could not be inspected")
	}
}

func TestDeployPlanThatDeletesNeedsApproval(t *testing.T) {
	plan := `{"resource_changes":[
		{"address":"module.rds.aws_db_instance.this","change":{"actions":["delete","create"]}},
		{"address":"module.ecr.aws_ecr_repository.this[\"api\"]","change":{"actions":["delete"]}},
		{"address":"module.vpc.aws_vpc.this","change":{"actions":["no-op"]}},
		{"address":"module.eks.aws_eks_cluster.this","change":{"actions":["update"]}}]}`
	t.Run("non-interactive without the flag", func(t *testing.T) {
		h := newDeployHarness(t, 0)
		h.tf.planJSON = plan
		if !IsReported(h.run()) {
			t.Fatal("applied a destructive plan nobody approved")
		}
		if h.tf.ran("apply") {
			t.Error("apply ran")
		}
		h.mustContain("module.rds.aws_db_instance.this", "replace", "delete", "ecr_repository")
		if strings.Contains(h.out.String(), "module.vpc.aws_vpc.this") || strings.Contains(h.out.String(), "aws_eks_cluster.this") {
			t.Error("harmless changes were listed as destructive")
		}
	})
	t.Run("--confirm-plan approves", func(t *testing.T) {
		h := newDeployHarness(t, 0)
		h.tf.planJSON = plan
		h.env.flags.ConfirmPlan = true
		if err := h.run(); err != nil {
			t.Fatalf("%v\n%s", err, h.out.String())
		}
		if !h.tf.ran("apply") {
			t.Error("apply did not run")
		}
	})
	t.Run("interactive yes", func(t *testing.T) {
		h := newDeployHarness(t, 0)
		h.tf.planJSON = plan
		h.env.interactive = true
		h.env.confirm = func(string) bool { return true }
		if err := h.run(); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("interactive no", func(t *testing.T) {
		h := newDeployHarness(t, 0)
		h.tf.planJSON = plan
		h.env.interactive = true
		h.env.confirm = func(string) bool { return false }
		if !IsReported(h.run()) || h.tf.ran("apply") {
			t.Fatal("a refusal at the prompt must stop")
		}
	})
	t.Run("plan file is applied, not a fresh plan", func(t *testing.T) {
		h := newDeployHarness(t, 0)
		if err := h.run(); err != nil {
			t.Fatal(err)
		}
		var planOut, applyArg string
		for _, c := range h.tf.cmds {
			if c[0] == "plan" {
				for _, a := range c {
					if strings.HasPrefix(a, "-out=") {
						planOut = strings.TrimPrefix(a, "-out=")
					}
				}
			}
			if c[0] == "apply" {
				applyArg = c[len(c)-1]
				for _, a := range c {
					if a == "-auto-approve" {
						t.Error("apply must use the reviewed plan file, never -auto-approve on a fresh plan")
					}
				}
			}
		}
		if planOut == "" || planOut != applyArg {
			t.Errorf("plan wrote %q, apply used %q", planOut, applyArg)
		}
		if _, err := os.Stat(planOut); err == nil {
			t.Error("the plan file was left behind")
		}
	})
	t.Run("a plan with only creates asks nothing", func(t *testing.T) {
		h := newDeployHarness(t, 0)
		if err := h.run(); err != nil {
			t.Fatal(err)
		}
	})
}

func TestParseOutputsRequiresEveryKey(t *testing.T) {
	if _, err := parseOutputs([]byte(outputsJSON(1, nil))); err != nil {
		t.Fatal(err)
	}
	required := []string{"cluster_name", "provider_region", "vpc_cidr", "cluster_service_cidr", "database_url", "inference_region",
		"bedrock_model_filter", "gpu_desired_size", "image_repositories", "gateway_role_arn", "bedrock_invoke_arns",
		"model_name", "model_revision", "served_model_name"}
	for _, key := range required {
		for how, mut := range map[string]func(map[string]any){
			"absent": func(v map[string]any) { delete(v, key) },
			"null":   func(v map[string]any) { v[key] = nil },
		} {
			if _, err := parseOutputs([]byte(outputsJSON(1, mut))); err == nil {
				t.Errorf("%s %s: accepted (no defaults allowed)", key, how)
			}
		}
	}
	// The model outputs matter only when a GPU model is deployed.
	if _, err := parseOutputs([]byte(outputsJSON(0, func(v map[string]any) { delete(v, "model_name") }))); err != nil {
		t.Errorf("a Bedrock-only install does not need model outputs: %v", err)
	}
	// An empty string filter is a value; an empty output set is not.
	if o, err := parseOutputs([]byte(outputsJSON(0, func(v map[string]any) { v["bedrock_model_filter"] = "" }))); err != nil || o.BedrockModelFilter != "" {
		t.Errorf("empty filter: %v", err)
	}
	if _, err := parseOutputs([]byte(`{}`)); err == nil {
		t.Error("{} accepted")
	}
	if _, err := parseOutputs([]byte(outputsJSON(0, func(v map[string]any) { delete(v["image_repositories"].(map[string]string), "web") }))); err == nil {
		t.Error("a missing web repository was accepted")
	}
}

func TestDeployMissingOutputStopsBeforeTheCluster(t *testing.T) {
	h := newDeployHarness(t, 0)
	h.tf.outputs = outputsJSON(0, func(v map[string]any) { delete(v, "gpu_desired_size") })
	if !IsReported(h.run()) {
		t.Fatal("deployed with an output missing")
	}
	if h.writes() != 0 || h.tl.count("mirror") != 0 {
		t.Errorf("went on:\n%s", h.tl)
	}
	h.mustContain("gpu_desired_size")
}

func TestDeployStateBackendOfAnotherInstallIsRefused(t *testing.T) {
	h := newDeployHarness(t, 0)
	h.tf.outputs = outputsJSON(0, func(v map[string]any) { v["cluster_name"] = "someone-elses" })
	if !IsReported(h.run()) || h.writes() != 0 {
		t.Fatal("applied against a state that describes another cluster")
	}
}

func TestDeployLogGroupAdoption(t *testing.T) {
	t.Run("state list fails: abort, nothing applied", func(t *testing.T) {
		h := newDeployHarness(t, 0)
		h.tf.stateErr = &tfFailure{Cmd: "state", Stderr: "Error: error loading state: ExpiredToken: The security token included in the request is expired", Err: errors.New("exit status 1")}
		if !IsReported(h.run()) {
			t.Fatal("an unreadable state was treated as empty")
		}
		if h.tf.ran("plan") || h.tf.ran("import") || h.tf.ran("apply") {
			t.Error("went on after the state could not be read")
		}
	})
	t.Run("fresh backend: no state file is an empty state", func(t *testing.T) {
		h := newDeployHarness(t, 0)
		h.tf.stateErr = &tfFailure{Cmd: "state", Stderr: "No state file was found!\n\nState management commands require a state file.", Err: errors.New("exit status 1")}
		if err := h.run(); err != nil {
			t.Fatalf("%v\n%s", err, h.out.String())
		}
	})
	t.Run("already in the state", func(t *testing.T) {
		h := newDeployHarness(t, 0)
		h.tf.stateOut = "module.vpc.aws_vpc.this\n" + logGroupAddr + "\n"
		if err := h.run(); err != nil {
			t.Fatal(err)
		}
		if h.tf.ran("import") || h.tl.count("logs describe-log-groups") != 0 {
			t.Error("looked for or imported a log group that is already managed")
		}
	})
	t.Run("kept from a previous install: imported", func(t *testing.T) {
		h := newDeployHarness(t, 0)
		h.awsf.logGroups = "1\n"
		if err := h.run(); err != nil {
			t.Fatal(err)
		}
		var imp []string
		for _, c := range h.tf.cmds {
			if c[0] == "import" {
				imp = c
			}
		}
		if len(imp) == 0 || imp[len(imp)-2] != logGroupAddr || imp[len(imp)-1] != "/aws/eks/dp-trial/cluster" || !strings.Contains(strings.Join(imp, " "), "-var-file=") {
			t.Errorf("import = %v", imp)
		}
		if h.tl.first("terraform import") > h.tl.first("terraform plan") {
			t.Error("the log group must be adopted before the plan")
		}
	})
	t.Run("describe fails: abort", func(t *testing.T) {
		h := newDeployHarness(t, 0)
		h.awsf.logErr = errors.New("ExpiredToken")
		if !IsReported(h.run()) || h.tf.ran("plan") {
			t.Fatal("a failed lookup was read as 'not there'")
		}
	})
	t.Run("import fails: abort", func(t *testing.T) {
		h := newDeployHarness(t, 0)
		h.awsf.logGroups = "1"
		h.tf.importErr = errors.New("boom")
		if !IsReported(h.run()) || h.tf.ran("apply") {
			t.Fatal("applied after a failed import")
		}
	})
}

func TestDeployApplyHeartbeatAndLogTee(t *testing.T) {
	h := newDeployHarness(t, 0)
	f, path := writeLogFile(t, h.in)
	h.env.out = newLockedWriter(io.MultiWriter(h.out, f))
	h.env.heartbeat = 10 * time.Millisecond
	h.tf.streamDelay = 120 * time.Millisecond
	h.tf.streamText["apply"] = "module.eks.aws_eks_cluster.this: Creating..."
	if err := h.run(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(h.out.String(), "terraform apply 0m00s") {
		t.Errorf("no heartbeat while apply was quiet:\n%s", h.out.String())
	}
	logged, _ := os.ReadFile(path)
	if !strings.Contains(string(logged), "module.eks.aws_eks_cluster.this: Creating...") {
		t.Errorf("terraform's output is not in the workspace log:\n%s", logged)
	}
	if st, _ := os.Stat(path); st.Mode().Perm() != 0o600 {
		t.Errorf("log mode = %v", st.Mode().Perm())
	}
	if strings.Contains(string(logged), "s3cr3t-db-pw") {
		t.Error("a secret is in the log")
	}
}

func TestDeployExpiredSessionDuringApply(t *testing.T) {
	h := newDeployHarness(t, 0)
	h.tf.streamText["apply"] = "Error: ExpiredToken: The security token included in the request is expired"
	h.tf.streamErr["apply"] = errors.New("exit status 1")
	err := h.run()
	if !IsReported(err) {
		t.Fatalf("err = %v", err)
	}
	h.mustContain("aws sso login --profile acme", "re-run")
	if h.tl.count("terraform output") != 0 || h.writes() != 0 || h.tl.count("mirror") != 0 {
		t.Errorf("went on after the apply failed:\n%s", h.tl)
	}
	if rec, _ := h.in.Load(); rec.Bundle != "" {
		t.Error("the install claims a release that was never applied")
	}
}

func TestDeployStateLockHint(t *testing.T) {
	h := newDeployHarness(t, 0)
	h.tf.streamText["apply"] = "Error: Error acquiring the state lock\nLock Info:\n  ID: abc"
	h.tf.streamErr["apply"] = errors.New("exit status 1")
	_ = h.run()
	h.mustContain("force-unlock")
}

func TestDeployInterruptStopsCleanly(t *testing.T) {
	h := newDeployHarness(t, 0)
	h.tf.streamDelay = time.Minute
	ctx, cancel := context.WithCancel(context.Background())
	h.passChecks()
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()
	err := h.env.run(ctx)
	if err == nil {
		t.Fatal("an interrupted deploy reported success")
	}
	h.mustContain("interrupted")
	if h.tl.count("terraform output") != 0 {
		t.Error("went on after the interrupt")
	}
}

func TestDeployCarriesTheLiveModelList(t *testing.T) {
	gw := `{"spec":{"template":{"spec":{"containers":[{"name":"gateway","env":[{"name":"VLLM_UPSTREAMS","value":"qwen=http://vllm-qwen.darkplum-inference:8000"}]}]}}}}`
	h := newDeployHarness(t, 0)
	h.k.handlers = append([]skHandler{{"get deploy darkplum-gateway", func([]string, []byte) ([]byte, error) { return []byte(gw), nil }}}, h.k.handlers...)
	if err := h.run(); err != nil {
		t.Fatal(err)
	}
	ok := false
	for _, c := range h.k.matching("apply -f -") {
		s := string(c.stdin)
		if strings.Contains(s, `"name":"darkplum-gateway"`) && strings.Contains(s, "qwen=http://vllm-qwen.darkplum-inference:8000") {
			ok = true
		}
	}
	if !ok {
		t.Error("the applied gateway dropped the models added with `darkplum models`")
	}
}

func TestDeployUnreadableGatewayIsNotTreatedAsNoModels(t *testing.T) {
	h := newDeployHarness(t, 0)
	h.k.handlers = append([]skHandler{{"get deploy darkplum-gateway", func([]string, []byte) ([]byte, error) {
		return nil, errors.New("kubectl: Unable to connect to the server: i/o timeout")
	}}}, h.k.handlers...)
	if !IsReported(h.run()) {
		t.Fatal("applied the gateway without knowing its models")
	}
	if h.writes() != 0 {
		t.Errorf("wrote to the cluster:\n%s", h.tl)
	}
}

func TestDeployKeepsInstalledOIDCAndSigningKey(t *testing.T) {
	h := newDeployHarness(t, 0)
	existing := secretJSON(map[string]string{
		"OIDC_ISSUER": "https://idp.example", "OIDC_CLIENT_ID": "cid", "OIDC_CLIENT_SECRET": "oidc-secret-value", "OIDC_REDIRECT_URL": "https://x/cb",
		"SESSION_SIGNING_KEY": "live-signing-key", "SECURE_COOKIES": "true"})
	h.k.handlers = append([]skHandler{{"get secret darkplum-auth", func([]string, []byte) ([]byte, error) { return []byte(existing), nil }}}, h.k.handlers...)
	h.k.on("patch secret darkplum-auth", "", nil)
	if err := h.run(); err != nil {
		t.Fatal(err)
	}
	p := h.k.matching("patch secret darkplum-auth")
	if len(p) != 1 {
		t.Fatalf("patches = %d", len(p))
	}
	sd := stringData(t, p[0])
	if len(sd) != 1 || sd["SERVICE_KEY_SECRET"] == "" {
		t.Errorf("deploy rewrote %v of an installed darkplum-auth", sd)
	}
	h.k.assertClean("oidc-secret-value", "live-signing-key")
}

func TestDeployUnreadableUserHMACAbortsBeforeTheApi(t *testing.T) {
	h := newDeployHarness(t, 0)
	h.k.handlers = append([]skHandler{{"get secret darkplum-user-hmac", func([]string, []byte) ([]byte, error) {
		return nil, errors.New("kubectl: error: You must be logged in to the server (Unauthorized)")
	}}}, h.k.handlers...)
	if !IsReported(h.run()) {
		t.Fatal("generated a new pseudonym key although the old one could not be read")
	}
	if h.tl.count("rollout status") != 0 || len(h.k.matching("create -f -")) > 1 {
		t.Errorf("went on:\n%s", h.tl)
	}
	for _, c := range h.k.matching("create -f -") {
		if strings.Contains(string(c.stdin), "user-hmac-key") {
			t.Error("created the user-hmac secret")
		}
	}
}

func TestDeployChecksTheRunningImageNotJustTheRollout(t *testing.T) {
	for _, svc := range release.Services {
		t.Run(svc, func(t *testing.T) {
			h := newDeployHarness(t, 0)
			h.imageID["darkplum-"+svc] = dpRegistry + "/dp-trial/darkplum-" + svc + "@sha256:" + strings.Repeat("0", 64)
			if !IsReported(h.run()) {
				t.Fatal("accepted pods running a different digest")
			}
			h.mustContain("not on the image that was mirrored")
		})
	}
}

func TestDeployWebRolloutIsWaitedForAndFailureShowsPods(t *testing.T) {
	h := newDeployHarness(t, 0)
	h.k.handlers = append([]skHandler{{"rollout status deployment/darkplum-web", func([]string, []byte) ([]byte, error) {
		return nil, errors.New("kubectl: error: timed out waiting for the condition")
	}}}, h.k.handlers...)
	h.k.on("get events", "0s Warning Failed pod/darkplum-web-1 Failed to pull image", nil)
	h.k.on("logs deployment/darkplum-web", "nginx: [emerg] bad config", nil)
	if !IsReported(h.run()) {
		t.Fatal("a web rollout that timed out passed")
	}
	h.mustContain("Failed to pull image", "bad config", "darkplum-web")
	if h.tl.count("iam simulate") != 0 {
		t.Error("the self-test ran on a platform that is not up")
	}
}

func TestDeployNodeArchitectureMismatchStopsBeforeAnythingIsApplied(t *testing.T) {
	h := newDeployHarness(t, 0)
	h.nodeArch = "arm64"
	h.platforms = []string{"linux/amd64"}
	if !IsReported(h.run()) {
		t.Fatal("deployed images the nodes cannot run")
	}
	if h.tl.count("mirror") != 0 || h.writes() != 0 {
		t.Errorf("went on:\n%s", h.tl)
	}
	h.mustContain("linux/arm64", "exec format error")
}

func TestDeployWaitsForAReadyNodeThenTimesOut(t *testing.T) {
	h := newDeployHarness(t, 0)
	h.k.handlers = append([]skHandler{{"get nodes -o json", func([]string, []byte) ([]byte, error) {
		return []byte(h.nodesJSON("amd64", false)), nil
	}}}, h.k.handlers...)
	if !IsReported(h.run()) {
		t.Fatal("no Ready node must fail")
	}
	if h.writes() != 0 || h.tl.count("mirror") != 0 {
		t.Error("went on without a node")
	}
	if h.clock.now().Sub(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)) < 10*time.Minute {
		t.Error("gave up before 10 minutes")
	}
}

func TestDeploySessionRecheckedBeforeMirroring(t *testing.T) {
	h := newDeployHarness(t, 0)
	h.session = checkResult{name: "session lifetime", status: checkFail, detail: "4 min remaining"}
	if !IsReported(h.run()) {
		t.Fatal("went on with a session about to expire")
	}
	if h.tl.count("mirror") != 0 || h.writes() != 0 {
		t.Errorf("went on:\n%s", h.tl)
	}
	if !h.tf.ran("apply") {
		t.Error("terraform should have finished before the later check")
	}
}

func TestDeployMirrorFailureStopsTheKubePhase(t *testing.T) {
	h := newDeployHarness(t, 0)
	h.mirrorErr = errors.New("api: destination tag exists at a different digest")
	if !IsReported(h.run()) || h.writes() != 0 {
		t.Fatal("applied workloads after the images failed to mirror")
	}
}

func TestDeployWithGPUDeploysTheModelLastAndNeverWaitsForIt(t *testing.T) {
	h := newDeployHarness(t, 1)
	if err := h.run(); err != nil {
		t.Fatalf("%v\n%s\n%s", err, h.out.String(), h.tl)
	}
	tl := h.tl
	// Device plugin, then vLLM, strictly after the web rollout and before the self-test.
	gpuIdx := -1
	for i, kinds := range h.appliedLists() {
		for _, k := range kinds {
			if k == "DaemonSet/nvidia-device-plugin-daemonset" || strings.HasPrefix(k, "DaemonSet/") {
				gpuIdx = i
			}
		}
	}
	if gpuIdx < 0 {
		t.Fatalf("the device plugin was not applied: %v", h.appliedLists())
	}
	vllm := -1
	for i, c := range h.k.calls {
		if strings.Contains(string(c.stdin), `"name":"vllm-inference"`) {
			vllm = i
		}
	}
	if vllm < 0 {
		t.Fatal("vllm-inference was not applied")
	}
	if tl.first("iam simulate") < tl.last("vllm-inference") {
		t.Error("the self-test ran before the model was deployed")
	}
	// Never waits for the model: no rollout status on vllm.
	if tl.count("rollout status deployment/vllm") != 0 {
		t.Error("deploy blocked on the model loading")
	}
	// The model comes from the outputs, patched structurally.
	found := false
	for _, c := range h.k.calls {
		if strings.Contains(string(c.stdin), "Qwen/Qwen2.5-7B-Instruct") && strings.Contains(string(c.stdin), "qwen-2.5-7b") {
			found = true
		}
	}
	if !found {
		t.Error("the model from the outputs was not applied")
	}
	h.k.assertClean()
}

func TestDeployRecreatesAClusterIPVLLMService(t *testing.T) {
	h := newDeployHarness(t, 1)
	h.k.handlers = append([]skHandler{{"get svc vllm-inference", func([]string, []byte) ([]byte, error) { return []byte("172.20.5.9"), nil }}}, h.k.handlers...)
	h.k.on("delete svc vllm-inference", "", nil)
	if err := h.run(); err != nil {
		t.Fatal(err)
	}
	if h.tl.first("delete svc vllm-inference") < 0 || h.tl.first("delete svc vllm-inference") > h.tl.last("vllm-inference") {
		t.Errorf("the old ClusterIP Service must be deleted before the headless one is applied:\n%s", h.tl)
	}
	// And a headless one is left alone.
	h2 := newDeployHarness(t, 1)
	if err := h2.run(); err != nil {
		t.Fatal(err)
	}
	if h2.tl.count("delete svc") != 0 {
		t.Error("deleted a Service that is already headless")
	}
}

func TestDeployNoGPUIsDegradedExitThree(t *testing.T) {
	h := newDeployHarness(t, 1)
	h.gpuNode = func() bool { return false }
	h.env.flags.GPUWait = 2 * time.Minute
	err := h.run()
	code, ok := ExitCode(err)
	if !ok || code != 3 {
		t.Fatalf("err = %v, want exit status 3", err)
	}
	// The platform is complete and healthy.
	for _, d := range []string{"darkplum-api", "darkplum-gateway", "darkplum-web"} {
		if h.tl.first("rollout status deployment/"+d) < 0 {
			t.Errorf("%s was not rolled out", d)
		}
	}
	for _, c := range h.k.calls {
		if strings.Contains(string(c.stdin), `"name":"vllm-inference"`) && strings.Contains(string(c.stdin), `"kind":"Deployment"`) {
			t.Error("the model was deployed with nowhere to run")
		}
	}
	h.mustContain("Degraded", "Next: darkplum configure")
	if h.tl.count("iam simulate") != 1 {
		t.Error("the Layer 1 self-test still runs on a degraded install")
	}
	if strings.Contains(h.out.String(), "Nothing is half-applied") {
		t.Error("a degraded success was reported as a failure")
	}
}

func TestDeployLayer1(t *testing.T) {
	t.Run("a role that does not behave as configured fails the deploy", func(t *testing.T) {
		h := newDeployHarness(t, 0)
		h.iamRep = iamcheck.Report{Checks: []iamcheck.Check{{Resource: "arn:x", Expect: "implicitDeny", Got: "allowed"}}}
		if !IsReported(h.run()) {
			t.Fatal("deploy passed with a gateway role wider than configured")
		}
		h.mustContain("expected implicitDeny, got allowed")
	})
	t.Run("IAM that cannot be asked is a warning, not a pass", func(t *testing.T) {
		h := newDeployHarness(t, 0)
		h.iamErr = &iamcheck.SimulationError{First: "AccessDenied: iam:SimulatePrincipalPolicy"}
		if err := h.run(); err != nil {
			t.Fatal(err)
		}
		h.mustContain("allowlist was not verified")
	})
}

func TestDeployDevFlags(t *testing.T) {
	h := newDeployHarness(t, 0)
	for _, ch := range []string{release.Release, release.RC} {
		for _, f := range []DeployCmd{{ReleaseImages: "/x/release.json"}, {UnsignedImages: "gateway=a@sha256:" + strings.Repeat("a", 64)}} {
			h.env.manifest.Channel = ch
			h.env.flags = f
			if r := h.env.devFlagsRefusal(); r == nil || r.status != checkFail {
				t.Errorf("%s binary accepted %+v", ch, f)
			}
		}
	}
	h.env.manifest.Channel = release.Release
	h.env.flags = DeployCmd{}
	if r := h.env.devFlagsRefusal(); r != nil {
		t.Errorf("a release binary with no dev flags was refused: %+v", r)
	}
	// A dev build deploys nothing unless told which images.
	h.env.manifest = release.Manifest{Schema: 1, Channel: release.Dev, Version: "dev"}
	if r := h.env.devFlagsRefusal(); r == nil {
		t.Error("a dev build with no images was allowed to proceed")
	}
	h.env.flags = DeployCmd{ReleaseImages: "a", UnsignedImages: "b"}
	if r := h.env.devFlagsRefusal(); r == nil {
		t.Error("both dev flags at once were accepted")
	}
	h.env.flags = DeployCmd{UnsignedImages: "x"}
	if r := h.env.devFlagsRefusal(); r != nil {
		t.Error("a dev build with --unsigned-images was refused")
	}
}

func TestDeployDevUnsignedImagesAreParsedStrictlyAndMarked(t *testing.T) {
	d := func(c string) string { return "sha256:" + strings.Repeat(c, 64) }
	ok := "gateway=reg.example/gw@" + d("a") + ",api=reg.example/api@" + d("b") + ",web=reg.example/web@" + d("c")
	h := newDeployHarness(t, 0)
	h.env.manifest = release.Manifest{Schema: 1, Channel: release.Dev, Version: "dev"}
	h.env.flags = DeployCmd{UnsignedImages: ok}
	v, err := h.env.realResolve(context.Background())
	if err != nil || !v.Valid() || !v.Unsigned() {
		t.Fatalf("%+v %v", v, err)
	}
	if v.Manifest().Images["api"].Digest != d("b") {
		t.Errorf("manifest = %+v", v.Manifest())
	}
	h.mustContain("UNSIGNED")
	for _, bad := range []string{
		"gateway=reg/gw:latest,api=reg/api@" + d("b") + ",web=reg/web@" + d("c"), // a tag, not a digest
		"gateway=reg/gw@" + d("a") + ",api=reg/api@" + d("b"),                    // web missing
		"gateway=reg/gw@sha256:abc,api=reg/api@" + d("b") + ",web=reg/web@" + d("c"),
		"evil=reg/gw@" + d("a") + ",api=reg/api@" + d("b") + ",web=reg/web@" + d("c"),
	} {
		h.env.flags = DeployCmd{UnsignedImages: bad}
		if _, err := h.env.realResolve(context.Background()); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
	// A release binary cannot reach Unverified at all.
	h.env.manifest = releaseManifest()
	h.env.flags = DeployCmd{UnsignedImages: ok}
	if r := h.env.devFlagsRefusal(); r == nil {
		t.Error("a release binary accepted --unsigned-images")
	}
}

func TestDeployRecordsUnsignedTrust(t *testing.T) {
	h := newDeployHarness(t, 0)
	h.unsigned = true
	h.env.manifest.Channel = release.Dev
	h.env.manifest.Version = "dev"
	h.env.manifest.Commit = ""
	h.env.flags.UnsignedImages = "gateway=reg/gw@sha256:" + strings.Repeat("a", 64) // the resolve seam is faked; the flag only has to be present
	if err := h.run(); err != nil {
		t.Fatalf("%v\n%s", err, h.out.String())
	}
	for _, c := range h.k.matching("apply -f -") {
		if strings.Contains(string(c.stdin), `"name":"darkplum-install"`) && !strings.Contains(string(c.stdin), `"image-trust":"unsigned"`) {
			t.Errorf("an unsigned install was recorded as signed: %s", c.stdin)
		}
	}
	if a := h.k.matching("annotate deployment/darkplum-api"); len(a) != 1 || !strings.Contains(a[0].joined(), "darkplum.ai/image-trust=unsigned") {
		t.Errorf("deployment annotations = %v", a)
	}
}

func TestDeployMinSessionDefaults(t *testing.T) {
	h := newDeployHarness(t, 0)
	if got := h.env.minSession(); got != 30 {
		t.Errorf("Bedrock-only needs %d minutes, want 30", got)
	}
	h2 := newDeployHarness(t, 1)
	if got := h2.env.minSession(); got != 90 {
		t.Errorf("a GPU install needs %d minutes, want 90", got)
	}
	h2.env.flags.MinSession = 45
	if got := h2.env.minSession(); got != 45 {
		t.Errorf("--min-session ignored: %d", got)
	}
}

func TestWebExposure(t *testing.T) {
	if webExposure(webSvc("ClusterIP", "", "")) != "" {
		t.Error("ClusterIP is not exposure")
	}
	if got := webExposure(webSvc("LoadBalancer", "internet-facing", "x.elb.amazonaws.com")); !strings.Contains(got, "internet-facing") || !strings.Contains(got, "x.elb.amazonaws.com") {
		t.Errorf("got %q", got)
	}
	if webExposure(`{"spec":{"type":"ClusterIP"},"metadata":{"annotations":{"service.beta.kubernetes.io/aws-load-balancer-scheme":"internet-facing"}}}`) == "" {
		t.Error("an internet-facing annotation counts as exposure")
	}
	if webExposure("not json") == "" {
		t.Error("an unreadable Service must not be treated as safe")
	}
}

func TestDestructiveChanges(t *testing.T) {
	got, err := destructiveChanges([]byte(`{"resource_changes":[
		{"address":"b","change":{"actions":["delete","create"]}},
		{"address":"a","change":{"actions":["delete"]}},
		{"address":"c","change":{"actions":["create","delete"]}},
		{"address":"d","change":{"actions":["update"]}},
		{"address":"e","change":{"actions":["no-op"]}},
		{"address":"f","change":{"actions":["read"]}}]}`))
	if err != nil || len(got) != 3 || got[0].Address != "a" || got[0].Replace || !got[1].Replace || !got[2].Replace {
		t.Fatalf("%+v %v", got, err)
	}
	if _, err := destructiveChanges([]byte("garbage")); err == nil {
		t.Error("garbage accepted")
	}
	if got, _ := destructiveChanges([]byte(`{}`)); len(got) != 0 {
		t.Error("an empty plan has no deletes")
	}
}

func TestStateListDistinguishesEmptyFromFailure(t *testing.T) {
	tf := &fakeTF{tl: &timeline{}}
	tf.stateOut = "a\n\n b \n"
	s, err := stateList(tf)
	if err != nil || !s["a"] || !s["b"] || len(s) != 2 {
		t.Fatalf("%v %v", s, err)
	}
	tf.stateErr = &tfFailure{Cmd: "state", Stderr: "Error: No valid credential sources found", Err: errors.New("exit status 1")}
	if s, err := stateList(tf); err == nil || s != nil {
		t.Error("an authentication failure must be an error")
	}
	tf.stateErr = errors.New("plain error")
	if _, err := stateList(tf); err == nil {
		t.Error("an unexplained failure must be an error")
	}
}
