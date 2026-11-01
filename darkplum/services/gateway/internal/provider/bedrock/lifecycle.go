package bedrock

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/bedrock"
	bedrocktypes "github.com/aws/aws-sdk-go-v2/service/bedrock/types"
	"github.com/aws/smithy-go"
)

// A model AWS has retired keeps an ACTIVE inference profile, and a call to it
// fails with "This model version has reached the end of its life". Discovery
// keeps such a model in the catalogue and reports it as availability "retired"
// (ModelAvailability); chat to it is refused before any call to AWS
// (ModelRetired). Three signals mark a model retired:
//
//   - ListFoundationModels shows its lifecycle LEGACY with an EndOfLifeTime
//     already past;
//   - AWS omits it from ListFoundationModels while a profile still points at it,
//     and GetFoundationModel answers the end-of-life ResourceNotFoundException;
//   - a real call just failed with that error (marks the profile at once).
//
// An unknown lifecycle status, or any other error, keeps the model usable.

const (
	// notEOLTTL: how long "absent from the list, but not end of life" is trusted.
	notEOLTTL = time.Hour
	// probeErrTTL: how long a failed probe (denied, throttled) waits before a retry.
	probeErrTTL = 5 * time.Minute
	// maxProbesPerDiscovery bounds the GetFoundationModel calls of one tick.
	maxProbesPerDiscovery = 25
	// warnEvery rate-limits each repeated warning.
	warnEvery = 10 * time.Minute
)

// retiredModels is the retirement knowledge. An end-of-life verdict is permanent
// and kept per foundation-model id, so it survives a later throttled or denied
// run; it is lost only with the process.
type retiredModels struct {
	mu       sync.Mutex
	eol      map[string]bool      // foundation-model id -> retired
	skip     map[string]time.Time // foundation-model id -> do not ask again before
	profiles map[string][]string  // profile id -> its foundation-model ids (latest discovery)
	called   map[string]bool      // profile ids a real call proved retired
	warned   map[string]time.Time
}

func (r *retiredModels) markEOL(fm string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.eol == nil {
		r.eol = make(map[string]bool)
	}
	r.eol[fm] = true
}

func (r *retiredModels) markCalled(profile string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.called == nil {
		r.called = make(map[string]bool)
	}
	r.called[profile] = true
}

func (r *retiredModels) setSkip(fm string, until time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.skip == nil {
		r.skip = make(map[string]time.Time)
	}
	r.skip[fm] = until
}

func (r *retiredModels) known(fm string, now time.Time) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.eol[fm] || now.Before(r.skip[fm])
}

func (r *retiredModels) setProfiles(m map[string][]string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.profiles = m
}

// profileRetired: a call proved it, or every foundation model behind it is
// end of life.
func (r *retiredModels) profileRetired(profile string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.called[profile] {
		return true
	}
	fms := r.profiles[profile]
	if len(fms) == 0 {
		return false
	}
	for _, fm := range fms {
		if !r.eol[fm] {
			return false
		}
	}
	return true
}

// warnDue is true at most once per warnEvery for a key.
func (r *retiredModels) warnDue(key string, now time.Time) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.warned == nil {
		r.warned = make(map[string]time.Time)
	}
	if last, ok := r.warned[key]; ok && now.Sub(last) < warnEvery {
		return false
	}
	r.warned[key] = now
	return true
}

// ModelRetired implements provider.RetiredChecker.
func (p *BedrockProvider) ModelRetired(model string) bool { return p.retired.profileRetired(model) }

// lifecycleRetired: retired only when LEGACY with an end of life that has
// passed. An unfamiliar status is reported unknown and keeps the model.
func lifecycleRetired(lc *bedrocktypes.FoundationModelLifecycle, now time.Time) (retired, unknown bool) {
	if lc == nil {
		return false, false
	}
	switch strings.ToUpper(string(lc.Status)) {
	case "ACTIVE", "":
		return false, false
	case "LEGACY":
		return lc.EndOfLifeTime != nil && !lc.EndOfLifeTime.After(now), false
	default:
		return false, true
	}
}

// foundationModelID is the model id at the end of an ARN
// ("arn:aws:bedrock:us-east-1::foundation-model/anthropic.claude-3-haiku-...";
// a global.* profile's ARN has no region: "arn:aws:bedrock:::foundation-model/...").
func foundationModelID(arn string) string {
	if i := strings.LastIndex(arn, "foundation-model/"); i >= 0 {
		return arn[i+len("foundation-model/"):]
	}
	return ""
}

func (p *BedrockProvider) clock() time.Time {
	if p.now != nil {
		return p.now()
	}
	return time.Now()
}

// refreshLifecycles updates what is known about retirement. It never removes a
// model and never forgets a verdict: a failed or throttled run leaves the last
// knowledge in place.
func (p *BedrockProvider) refreshLifecycles(ctx context.Context, profiles []bedrocktypes.InferenceProfileSummary) {
	now := p.clock()
	pm := make(map[string][]string, len(profiles))
	for _, prof := range profiles {
		if prof.InferenceProfileId == nil {
			continue
		}
		for _, m := range prof.Models {
			if m.ModelArn != nil {
				if id := foundationModelID(*m.ModelArn); id != "" {
					pm[*prof.InferenceProfileId] = append(pm[*prof.InferenceProfileId], id)
				}
			}
		}
	}
	p.retired.setProfiles(pm)

	out, err := p.controlClient.ListFoundationModels(ctx, &bedrock.ListFoundationModelsInput{})
	if err != nil {
		if p.retired.warnDue("list-fm", now) {
			slog.Warn("bedrock: cannot read model lifecycles; retired models are only caught when a call fails", "err", err)
		}
		return
	}
	listed := make(map[string]bool, len(out.ModelSummaries))
	for _, fm := range out.ModelSummaries {
		if fm.ModelId == nil {
			continue
		}
		listed[*fm.ModelId] = true
		retired, unknown := lifecycleRetired(fm.ModelLifecycle, now)
		if retired {
			p.retired.markEOL(*fm.ModelId)
		}
		if unknown && p.retired.warnDue("status:"+*fm.ModelId, now) {
			slog.Warn("bedrock: unfamiliar model lifecycle status; keeping the model", "model", *fm.ModelId, "status", string(fm.ModelLifecycle.Status))
		}
	}

	// AWS leaves a retired model out of ListFoundationModels altogether, while
	// its profile stays ACTIVE: ask about those directly, one at a time, a bounded
	// number per tick, and remember the answer.
	probes := 0
	asked := make(map[string]bool)
	for _, fms := range pm {
		for _, id := range fms {
			if listed[id] || asked[id] || p.retired.known(id, now) {
				continue
			}
			if probes >= maxProbesPerDiscovery || ctx.Err() != nil {
				return
			}
			asked[id] = true
			probes++
			switch p.foundationModelVerdict(ctx, id, now) {
			case verdictRetired:
				p.retired.markEOL(id)
				if p.retired.warnDue("retired:"+id, now) {
					slog.Warn("bedrock: model is retired by AWS", "model", id)
				}
			case verdictActive:
				p.retired.setSkip(id, now.Add(notEOLTTL))
			default:
				p.retired.setSkip(id, now.Add(probeErrTTL))
			}
		}
	}
}

type verdict int

const (
	verdictUnknown verdict = iota
	verdictActive
	verdictRetired
)

// foundationModelVerdict asks AWS about one foundation model. Only the
// end-of-life ResourceNotFoundException (or a lifecycle that says so) is
// retired; any other error is unknown and keeps the model usable.
func (p *BedrockProvider) foundationModelVerdict(ctx context.Context, id string, now time.Time) verdict {
	out, err := p.controlClient.GetFoundationModel(ctx, &bedrock.GetFoundationModelInput{ModelIdentifier: aws.String(id)})
	if err != nil {
		var apiErr smithy.APIError
		if errors.As(err, &apiErr) && apiErr.ErrorCode() == "ResourceNotFoundException" && IsModelRetiredMessage(apiErr.ErrorMessage()) {
			return verdictRetired
		}
		if p.retired.warnDue("get-fm:"+id, now) {
			slog.Warn("bedrock: cannot read a model's lifecycle; keeping it usable", "model", id, "err", err)
		}
		return verdictUnknown
	}
	if out == nil || out.ModelDetails == nil {
		return verdictUnknown
	}
	retired, unknown := lifecycleRetired(out.ModelDetails.ModelLifecycle, now)
	switch {
	case retired:
		return verdictRetired
	case unknown:
		if p.retired.warnDue("status:"+id, now) {
			slog.Warn("bedrock: unfamiliar model lifecycle status; keeping the model", "model", id)
		}
		return verdictUnknown
	}
	return verdictActive
}
