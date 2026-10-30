package commands

import (
	"encoding/json"
	"fmt"
)

// The default self-hosted model runs as one vLLM Deployment (deployed with
// the install): an init container downloads the weights, then the vllm
// container loads them onto the GPU. The api has no "loading" state for it (the
// gateway only lists a model once vLLM serves it), so configure reads the pod.
const (
	inferenceNamespace = "darkplum-inference"
	vllmSelector       = "app.kubernetes.io/name=vllm-inference"
)

// vllmState is what the self-hosted model is doing right now.
type vllmState struct {
	present bool   // a vLLM pod exists (a GPU install)
	ready   bool   // vllm container Ready: serving
	broken  bool   // will not get better by waiting
	phase   string // shown to the operator
	hint    string // the command that shows why, when broken
}

type podList struct {
	Items []struct {
		Metadata struct {
			CreationTimestamp string `json:"creationTimestamp"`
		} `json:"metadata"`
		Status struct {
			Phase      string `json:"phase"`
			Conditions []struct {
				Type    string `json:"type"`
				Status  string `json:"status"`
				Reason  string `json:"reason"`
				Message string `json:"message"`
			} `json:"conditions"`
			InitContainerStatuses []containerStatus `json:"initContainerStatuses"`
			ContainerStatuses     []containerStatus `json:"containerStatuses"`
		} `json:"status"`
	} `json:"items"`
}

type containerState struct {
	Running *struct{} `json:"running"`
	Waiting *struct {
		Reason string `json:"reason"`
	} `json:"waiting"`
	Terminated *struct {
		ExitCode int    `json:"exitCode"`
		Reason   string `json:"reason"`
	} `json:"terminated"`
}

type containerStatus struct {
	Name         string         `json:"name"`
	Ready        bool           `json:"ready"`
	RestartCount int            `json:"restartCount"`
	State        containerState `json:"state"`
	LastState    containerState `json:"lastState"`
}

func waitingReason(s containerState) string {
	if s.Waiting != nil {
		return s.Waiting.Reason
	}
	return ""
}

// vllmStateFromPods reads `kubectl get pods -o json` for the vLLM selector.
// The diagnostic commands it suggests validate the install first (kubectlHint).
func vllmStateFromPods(raw []byte) vllmState {
	var pl podList
	if json.Unmarshal(raw, &pl) != nil || len(pl.Items) == 0 {
		return vllmState{}
	}
	// The newest pod is the one that matters (Recreate leaves at most one,
	// but a terminating one can linger).
	pod := pl.Items[0]
	for _, p := range pl.Items[1:] {
		if p.Metadata.CreationTimestamp > pod.Metadata.CreationTimestamp {
			pod = p
		}
	}
	describe := kubectlHint("-n " + inferenceNamespace + " describe pod -l " + vllmSelector)
	logs := func(container string, previous bool) string {
		cmd := "-n " + inferenceNamespace + " logs deploy/vllm-inference -c " + container
		if previous {
			cmd += " --previous"
		}
		return kubectlHint(cmd)
	}
	broken := func(phase, hint string) vllmState {
		return vllmState{present: true, broken: true, phase: phase, hint: hint}
	}
	st := pod.Status

	for _, c := range st.Conditions {
		if c.Type == "PodScheduled" && c.Status == "False" && c.Reason == "Unschedulable" {
			return broken("cannot be placed on a GPU node: "+firstLine(c.Message),
				"see why: "+describe)
		}
	}
	all := append(append([]containerStatus{}, st.InitContainerStatuses...), st.ContainerStatuses...)
	for _, c := range all {
		switch waitingReason(c.State) {
		case "ImagePullBackOff", "ErrImagePull", "InvalidImageName":
			return broken("cannot pull the "+c.Name+" image", "see why: "+describe)
		}
	}
	for _, c := range st.InitContainerStatuses {
		if c.Name != "model-download" {
			continue
		}
		// Once the download has finished cleanly, an earlier failed attempt
		// (LastState) no longer matters.
		succeeded := c.State.Terminated != nil && c.State.Terminated.ExitCode == 0
		failed := c.State.Terminated != nil && c.State.Terminated.ExitCode != 0
		failed = failed || waitingReason(c.State) == "CrashLoopBackOff"
		failed = failed || (!succeeded && c.LastState.Terminated != nil && c.LastState.Terminated.ExitCode != 0)
		if failed {
			return broken("the model download failed", "see why: "+logs("model-download", false))
		}
		if c.State.Running != nil {
			return vllmState{present: true, phase: "downloading the model weights"}
		}
		if c.State.Terminated == nil {
			return vllmState{present: true, phase: "starting"}
		}
	}
	for _, c := range st.ContainerStatuses {
		if c.Name != "vllm" {
			continue
		}
		// Serving now wins over history: a container that restarted (even
		// out of memory) and then came up Ready is ready.
		if c.Ready {
			return vllmState{present: true, ready: true, phase: "ready"}
		}
		if c.LastState.Terminated != nil && c.LastState.Terminated.Reason == "OOMKilled" {
			return broken("vLLM ran out of memory loading the model", "see why: "+logs("vllm", true))
		}
		if waitingReason(c.State) == "CrashLoopBackOff" || c.RestartCount >= 3 {
			return broken(fmt.Sprintf("vLLM keeps restarting (%d restarts)", c.RestartCount), "see why: "+logs("vllm", true))
		}
		if c.State.Running != nil {
			return vllmState{present: true, phase: "loading the model onto the GPU"}
		}
	}
	if st.Phase == "Failed" {
		return broken("the vLLM pod failed", "see why: "+describe)
	}
	return vllmState{present: true, phase: "starting"}
}
