package bedrock

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	bedrocktypes "github.com/aws/aws-sdk-go-v2/service/bedrock/types"
	"github.com/aws/smithy-go"

	"github.com/scalecraft-dev/darkplum/gateway/internal/provider"
)

// Both phrasings AWS has used for the end-of-life refusal.
const (
	eolText  = "This model version has reached the end of its life. Please refer to the AWS documentation for more details."
	eolText2 = "This model version has reached the end of its life. Use an active model version instead."
)

var lifecycleNow = time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)

func profileFor(id, base string) bedrocktypes.InferenceProfileSummary {
	p := activeProfile(id)
	p.Models = []bedrocktypes.InferenceProfileModel{{ModelArn: aws.String("arn:aws:bedrock:us-east-1::foundation-model/" + base)}}
	return p
}

func foundation(id string, status string, eol *time.Time) bedrocktypes.FoundationModelSummary {
	return bedrocktypes.FoundationModelSummary{
		ModelId:        aws.String(id),
		ModelLifecycle: &bedrocktypes.FoundationModelLifecycle{Status: bedrocktypes.FoundationModelLifecycleStatus(status), EndOfLifeTime: eol},
	}
}

func eolErr(msg string) error {
	return &smithy.GenericAPIError{Code: "ResourceNotFoundException", Message: msg}
}

func newLifecycleProvider(fake *fakeControlAPI, clock *time.Time) *BedrockProvider {
	return &BedrockProvider{controlClient: fake, region: "us-east-2", now: func() time.Time { return *clock }}
}

func availState(p *BedrockProvider, id string) string { return p.ModelAvailability(id).State }

func TestIsModelRetiredMessage_BothPhrasings(t *testing.T) {
	for _, m := range []string{eolText, eolText2} {
		if !IsModelRetiredMessage(m) {
			t.Errorf("not recognised: %q", m)
		}
	}
	if IsModelRetiredMessage("Could not resolve the foundation model") {
		t.Error("an ordinary not-found is not end of life")
	}
}

// Retired models stay discoverable and are reported retired, not deleted.
func TestDiscoverModels_RetiredModelsStayListedAndAreReportedRetired(t *testing.T) {
	past := lifecycleNow.Add(-24 * time.Hour)
	future := lifecycleNow.Add(90 * 24 * time.Hour)
	fake := &fakeControlAPI{
		pages: [][]bedrocktypes.InferenceProfileSummary{{
			profileFor("us.a.legacy-past", "a.legacy-past"),
			profileFor("us.a.legacy-future", "a.legacy-future"),
			profileFor("us.a.active", "a.active"),
			profileFor("us.a.absent-eol", "a.absent-eol"),
		}},
		fms: []bedrocktypes.FoundationModelSummary{
			foundation("a.legacy-past", "LEGACY", &past),
			foundation("a.legacy-future", "LEGACY", &future),
			foundation("a.active", "ACTIVE", nil),
		},
		getErr: map[string]error{"a.absent-eol": eolErr(eolText2)},
	}
	clock := lifecycleNow
	p := newLifecycleProvider(fake, &clock)
	models, err := p.DiscoverModels(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 4 {
		t.Errorf("a retired model must stay in the catalogue: %v", models)
	}
	for id, want := range map[string]string{
		"us.a.legacy-past": provider.AvailabilityRetired, "us.a.absent-eol": provider.AvailabilityRetired,
		"us.a.legacy-future": provider.AvailabilityUnknown, "us.a.active": provider.AvailabilityUnknown,
	} {
		if got := availState(p, id); got != want {
			t.Errorf("%s availability = %q, want %q", id, got, want)
		}
	}
	if !p.ModelRetired("us.a.absent-eol") || p.ModelRetired("us.a.active") {
		t.Error("ModelRetired disagrees with availability")
	}
	if av := p.ModelAvailability("us.a.absent-eol"); av.Reason != provider.AvailabilityReasonRetired {
		t.Errorf("reason = %q", av.Reason)
	} else if av.RequiredMode != "" {
		// required_mode belongs to the data-retention opt-in; nothing is
		// required of the account for a model AWS has retired.
		t.Errorf("retired availability carries required_mode %q", av.RequiredMode)
	}
}

// M3: a status AWS adds later is not a reason to retire a model.
func TestDiscoverModels_UnknownLifecycleStatusKeepsTheModel(t *testing.T) {
	fake := &fakeControlAPI{
		pages: [][]bedrocktypes.InferenceProfileSummary{{profileFor("us.a.m", "a.m")}},
		fms:   []bedrocktypes.FoundationModelSummary{foundation("a.m", "DEPRECATING_SOON", nil)},
	}
	clock := lifecycleNow
	p := newLifecycleProvider(fake, &clock)
	_, _ = p.DiscoverModels(context.Background())
	if p.ModelRetired("us.a.m") {
		t.Error("an unfamiliar status must keep the model")
	}
}

func TestDiscoverModels_AbsentButUnreadableIsNotRetired(t *testing.T) {
	for name, err := range map[string]error{
		"access denied": &smithy.GenericAPIError{Code: "AccessDeniedException", Message: "not authorized"},
		"throttled":     &smithy.GenericAPIError{Code: "ThrottlingException", Message: "slow down"},
		"other 404":     &smithy.GenericAPIError{Code: "ResourceNotFoundException", Message: "no such model"},
		"network":       errors.New("connection reset"),
	} {
		t.Run(name, func(t *testing.T) {
			fake := &fakeControlAPI{
				pages:  [][]bedrocktypes.InferenceProfileSummary{{profileFor("us.meta.llama3-2-3b", "meta.llama3-2-3b")}},
				getErr: map[string]error{"meta.llama3-2-3b": err},
			}
			clock := lifecycleNow
			p := newLifecycleProvider(fake, &clock)
			_, _ = p.DiscoverModels(context.Background())
			if p.ModelRetired("us.meta.llama3-2-3b") {
				t.Error("a model whose lifecycle could not be read must not be retired")
			}
		})
	}
}

// global.* profiles have no region in the ARN; one foundation model backing two
// profiles is asked about once.
func TestDiscoverModels_GlobalProfileEmptyRegionARNAndProbeOncePerModel(t *testing.T) {
	const base = "anthropic.claude-sonnet-4-20250514-v1:0"
	g := activeProfile("global." + base)
	g.Models = []bedrocktypes.InferenceProfileModel{{ModelArn: aws.String("arn:aws:bedrock:::foundation-model/" + base)}}
	fake := &fakeControlAPI{
		pages:  [][]bedrocktypes.InferenceProfileSummary{{g, profileFor("us."+base, base)}},
		getErr: map[string]error{base: eolErr(eolText)},
	}
	clock := lifecycleNow
	p := newLifecycleProvider(fake, &clock)
	_, _ = p.DiscoverModels(context.Background())
	if !p.ModelRetired("global."+base) || !p.ModelRetired("us."+base) {
		t.Error("both profiles are retired")
	}
	if fake.getCalls[base] != 1 {
		t.Errorf("GetFoundationModel calls = %d, want 1", fake.getCalls[base])
	}
}

// M2: an end-of-life verdict is permanent, cached per foundation model, and a
// later throttled or denied run does not undo it. "Absent but not EOL" is cached
// for an hour and then asked again.
func TestLifecycleVerdictsAreCached(t *testing.T) {
	const eolFM, liveFM = "a.eol", "a.live"
	fake := &fakeControlAPI{
		pages:  [][]bedrocktypes.InferenceProfileSummary{{profileFor("us.a.eol", eolFM), profileFor("us.a.live", liveFM)}},
		getErr: map[string]error{eolFM: eolErr(eolText)},
	}
	clock := lifecycleNow
	p := newLifecycleProvider(fake, &clock)
	discover := func() { fake.calls = 0; _, _ = p.DiscoverModels(context.Background()) }

	discover()
	discover()
	if fake.getCalls[eolFM] != 1 || fake.getCalls[liveFM] != 1 {
		t.Errorf("second tick re-asked: %v", fake.getCalls)
	}
	clock = clock.Add(30 * time.Minute)
	discover()
	if fake.getCalls[liveFM] != 1 {
		t.Errorf("a not-EOL verdict is trusted for an hour: %v", fake.getCalls)
	}
	clock = clock.Add(31 * time.Minute)
	discover()
	if fake.getCalls[liveFM] != 2 {
		t.Errorf("a not-EOL verdict expires after an hour: %v", fake.getCalls)
	}
	if fake.getCalls[eolFM] != 1 {
		t.Errorf("an EOL verdict is never re-asked: %v", fake.getCalls)
	}

	// The list call starts failing (throttled / denied): nothing is un-retired.
	fake.fmErr = &smithy.GenericAPIError{Code: "ThrottlingException", Message: "slow down"}
	clock = clock.Add(2 * time.Hour)
	discover()
	if !p.ModelRetired("us.a.eol") {
		t.Error("a failed lifecycle read un-retired a model")
	}
}

func TestLifecycleProbesAreBounded(t *testing.T) {
	var profs []bedrocktypes.InferenceProfileSummary
	for i := 0; i < maxProbesPerDiscovery+10; i++ {
		id := "m" + string(rune('A'+i%26)) + string(rune('a'+i/26))
		profs = append(profs, profileFor("us."+id, id))
	}
	fake := &fakeControlAPI{pages: [][]bedrocktypes.InferenceProfileSummary{profs}}
	clock := lifecycleNow
	p := newLifecycleProvider(fake, &clock)
	_, _ = p.DiscoverModels(context.Background())
	total := 0
	for _, n := range fake.getCalls {
		total += n
	}
	if total != maxProbesPerDiscovery {
		t.Errorf("probes = %d, want the bound %d", total, maxProbesPerDiscovery)
	}
}

// A call that fails with the end-of-life text marks the model retired at once,
// with no discovery tick in between.
func TestEndOfLifeCallMarksModelRetiredImmediately(t *testing.T) {
	clock := lifecycleNow
	p := newLifecycleProvider(&fakeControlAPI{}, &clock)
	if availState(p, "us.a.m") == provider.AvailabilityRetired {
		t.Fatal("precondition")
	}
	err := p.wrapError("us.a.m", eolErr(eolText2), false)
	var be *BedrockAPIError
	if !errors.As(err, &be) || be.StatusCode != 404 {
		t.Fatalf("err = %v", err)
	}
	if availState(p, "us.a.m") != provider.AvailabilityRetired || !p.ModelRetired("us.a.m") {
		t.Error("the model is not retired right after the failing call")
	}
}

func TestOtherNotFoundDoesNotRetire(t *testing.T) {
	p := &BedrockProvider{}
	_ = p.wrapError("us.x", &smithy.GenericAPIError{Code: "ResourceNotFoundException", Message: "Could not resolve the foundation model"}, false)
	if p.ModelRetired("us.x") {
		t.Error("an ordinary not-found must not retire the model")
	}
}

// The warnings repeat every tick; each is rate limited.
func TestWarningsAreRateLimited(t *testing.T) {
	var r retiredModels
	now := lifecycleNow
	if !r.warnDue("k", now) || r.warnDue("k", now.Add(time.Minute)) || !r.warnDue("k", now.Add(warnEvery+time.Second)) {
		t.Error("warnDue must allow one per interval")
	}
}
