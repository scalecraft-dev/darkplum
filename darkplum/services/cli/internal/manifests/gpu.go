package manifests

import (
	"fmt"
	"io/fs"
	"regexp"
)

// Names of the platform phases Platform returns, for callers that must do
// something between two of them (deploy writes the Secrets the workloads read
// after the identities and before the api).
const (
	PhaseIdentities = "identities and vendor-key objects"
	PhaseAPI        = "api"
	PhaseGateway    = "gateway"
	PhaseWeb        = "web"
)

// GPUInputs describe the one self-hosted model an install is created with (the
// Terraform model_name, model_revision and served_model_name outputs). Further
// models are added later with `darkplum models`.
type GPUInputs struct {
	ModelName, ModelRevision, ServedModelName string
}

var (
	modelNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*(/[A-Za-z0-9][A-Za-z0-9._-]*)?$`)
	revisionRe  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]*$`)
	servedRe    = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)
)

func (in GPUInputs) validate() error {
	switch {
	case !modelNameRe.MatchString(in.ModelName):
		return fmt.Errorf("model_name %q is not a Hugging Face model id", in.ModelName)
	case !revisionRe.MatchString(in.ModelRevision):
		return fmt.Errorf("model_revision %q is not a branch, tag or commit", in.ModelRevision)
	case !servedRe.MatchString(in.ServedModelName):
		return fmt.Errorf("served_model_name %q is not a plain model name", in.ServedModelName)
	}
	return nil
}

// GPUPlan is the self-hosted model's objects in apply order. DevicePlugin goes
// first and is waited on (a GPU is not allocatable until it runs); Workload is
// applied once a GPU is. The namespace and the vLLM NetworkPolicies are part of
// Platform, which is applied before either.
type GPUPlan struct {
	DevicePlugin []Object
	Workload     []Object
}

// GPU renders the NVIDIA device plugin and the default vLLM model. Every
// PLACEHOLDER_ value is replaced by structured patches that must each match
// exactly once, and the result passes the same checks as the platform (third-party
// images pinned by digest, nothing unreplaced).
func GPU(k8s fs.FS, in GPUInputs) (GPUPlan, error) {
	if err := in.validate(); err != nil {
		return GPUPlan{}, err
	}
	const dir = "overlays/prod/resources/"
	read := func(file string) ([]Object, error) { return File(k8s, dir+file) }
	var plan GPUPlan
	var err error
	if plan.DevicePlugin, err = read("nvidia-device-plugin.yaml"); err != nil {
		return GPUPlan{}, err
	}
	for _, f := range []string{"vllm-service-account.yaml", "vllm-deployment.yaml", "vllm-service.yaml"} {
		objs, err := read(f)
		if err != nil {
			return GPUPlan{}, err
		}
		plan.Workload = append(plan.Workload, objs...)
	}
	d, err := find(plan.Workload, "Deployment", "vllm-inference")
	if err != nil {
		return GPUPlan{}, err
	}
	if err := SetEnv(d, "MODEL_NAME", in.ModelName); err != nil {
		return GPUPlan{}, err
	}
	if err := SetEnv(d, "MODEL_REVISION", in.ModelRevision); err != nil {
		return GPUPlan{}, err
	}
	if err := SetArg(d, "PLACEHOLDER_SERVED_MODEL_NAME", in.ServedModelName); err != nil {
		return GPUPlan{}, err
	}
	if err := Check(append(append([]Object{}, plan.DevicePlugin...), plan.Workload...), map[string]string{}); err != nil {
		return GPUPlan{}, err
	}
	return plan, nil
}

// SetArg replaces the one container argument that is exactly from. Zero or
// several matches are errors.
func SetArg(o Object, from, to string) error {
	n := 0
	for _, c := range containers(o) {
		args, _ := c["args"].([]any)
		for i, a := range args {
			if str(a) == from {
				args[i] = to
				n++
			}
		}
	}
	if n != 1 {
		return fmt.Errorf("%s: expected exactly one container argument %q, found %d", o.Key(), from, n)
	}
	return nil
}
