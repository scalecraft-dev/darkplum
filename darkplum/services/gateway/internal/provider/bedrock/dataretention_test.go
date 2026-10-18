package bedrock

import (
	"context"
	"errors"
	"strings"
	"testing"

	bedrocktypes "github.com/aws/aws-sdk-go-v2/service/bedrock/types"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"
	smithy "github.com/aws/smithy-go"

	"github.com/scalecraft-dev/darkplum/gateway/internal/provider"
)

const (
	fable = "us.anthropic.claude-fable-5-v1:0"
	nova  = "us.amazon.nova-lite-v1:0"
	// rawBedrockText is what Bedrock really says; it must never be relayed.
	rawBedrockText = "The model returned the following errors: data retention mode 'default' is not available for this model"
)

var drReq = func(m string) provider.ChatRequest {
	return provider.ChatRequest{Model: m, Messages: []provider.Message{{Role: "user", Content: "hi"}}}
}

func validation(msg string) error {
	return &smithy.GenericAPIError{Code: "ValidationException", Message: msg}
}

func TestComplete_DataRetentionValidation_ClassifiedAndScrubbed(t *testing.T) {
	p := newProviderWithFake(&fakeConverseAPI{converseErr: validation(rawBedrockText)})
	p.retention.mode = "default"

	_, err := p.Complete(context.Background(), drReq(fable))
	var dr *DataRetentionError
	if !errors.As(err, &dr) {
		t.Fatalf("expected *DataRetentionError, got %T: %v", err, err)
	}
	var api provider.APIError
	if !errors.As(err, &api) || api.HTTPStatus() != 422 {
		t.Fatalf("expected 422 APIError")
	}
	want := `{"error":{"message":"` + fable + ` on Bedrock requires your AWS account's Bedrock data-retention mode to be raised before it can answer. An administrator changes that in AWS; other models still work.","type":"invalid_request_error","code":"model_requires_data_retention","model":"` + fable + `","account_mode":"default","required_mode":"aws_review"}}`
	if string(api.ResponseBody()) != want {
		t.Errorf("body\n got: %s\nwant: %s", api.ResponseBody(), want)
	}
	if strings.Contains(string(api.ResponseBody()), "returned the following") || strings.Contains(err.Error(), "returned the following") {
		t.Errorf("raw Bedrock text leaked")
	}
}

func TestStream_DataRetentionValidation_BeforeFirstByte(t *testing.T) {
	// ConverseStream fails at call time.
	p := newProviderWithFake(&fakeConverseAPI{streamErr: validation(rawBedrockText)})
	var sb strings.Builder
	_, err := p.Stream(context.Background(), drReq(fable), &sb)
	var dr *DataRetentionError
	if !errors.As(err, &dr) || dr.AccountMode != "unknown" {
		t.Fatalf("expected DataRetentionError with unknown mode, got %v", err)
	}
	if sb.Len() != 0 {
		t.Errorf("nothing should be written, got %q", sb.String())
	}
	if p.ModelAvailability(fable).State != provider.AvailabilityNeedsOptIn {
		t.Errorf("refusal must flag the model")
	}
}

func TestStream_DataRetentionValidation_MidStream(t *testing.T) {
	p := newProviderWithFake(&fakeConverseAPI{stream: newFakeStream(nil, validation(rawBedrockText))})
	var sb strings.Builder
	_, err := p.Stream(context.Background(), drReq(fable), &sb)
	var dr *DataRetentionError
	if !errors.As(err, &dr) {
		t.Fatalf("expected DataRetentionError, got %v", err)
	}
}

func TestOtherValidationExceptionsUnchanged(t *testing.T) {
	p := newProviderWithFake(&fakeConverseAPI{converseErr: validation("messages must not be empty")})
	_, err := p.Complete(context.Background(), drReq(nova))
	var be *BedrockAPIError
	if !errors.As(err, &be) || be.StatusCode != 400 {
		t.Fatalf("expected ordinary 400 BedrockAPIError, got %v", err)
	}
	// Same marker under a different exception code is not classified.
	p = newProviderWithFake(&fakeConverseAPI{converseErr: &smithy.GenericAPIError{Code: "AccessDeniedException", Message: rawBedrockText}})
	_, err = p.Complete(context.Background(), drReq(nova))
	if errors.As(err, new(*DataRetentionError)) {
		t.Fatal("only ValidationException is classified")
	}
}

// Availability state machine (a)-(d).
func TestModelAvailability_StateMachine(t *testing.T) {
	ctrl := &fakeControlAPI{}
	api := &fakeConverseAPI{converseOut: converseOutput("ok", types.StopReasonEndTurn, 1, 1)}
	p := &BedrockProvider{client: api, controlClient: ctrl, region: "us-east-1"}

	// (d) mode never read.
	if a := p.ModelAvailability(fable); a.State != "unknown" || a.Reason != "data_retention" || a.AccountMode != "unknown" || a.RequiredMode != "aws_review" {
		t.Errorf("(d) = %+v", a)
	}

	// (b) known low mode + known family; (c) known mode, other family.
	ctrl.drMode = bedrocktypes.DataRetentionModeDefault
	p.RefreshAccountDataRetention(context.Background())
	if a := p.ModelAvailability(fable); a.State != "needs_aws_opt_in" || a.Reason != "data_retention" || a.AccountMode != "default" {
		t.Errorf("(b) = %+v", a)
	}
	if a := p.ModelAvailability(nova); a.State != "ok" || a.Reason != "" || a.AccountMode != "default" {
		t.Errorf("(c) = %+v", a)
	}
	for _, m := range []bedrocktypes.DataRetentionMode{"none", "inherit"} {
		ctrl.drMode = m
		p.RefreshAccountDataRetention(context.Background())
		if a := p.ModelAvailability(fable); a.State != "needs_aws_opt_in" {
			t.Errorf("mode %s: %+v", m, a)
		}
	}
	// Raised mode: the family is fine.
	ctrl.drMode = bedrocktypes.DataRetentionModeAwsReview
	p.RefreshAccountDataRetention(context.Background())
	if a := p.ModelAvailability(fable); a.State != "ok" {
		t.Errorf("aws_review should be ok, got %+v", a)
	}

	// (a) a classified failure wins even when the mode says ok, and any other
	// model is unaffected; a later 2xx clears it.
	api.converseErr = validation(rawBedrockText)
	_, _ = p.Complete(context.Background(), drReq(fable))
	if a := p.ModelAvailability(fable); a.State != "needs_aws_opt_in" {
		t.Errorf("(a) = %+v", a)
	}
	if a := p.ModelAvailability(nova); a.State != "ok" {
		t.Errorf("flag is per model, nova = %+v", a)
	}
	api.converseErr = nil
	if _, err := p.Complete(context.Background(), drReq(fable)); err != nil {
		t.Fatal(err)
	}
	if a := p.ModelAvailability(fable); a.State != "ok" {
		t.Errorf("flag must clear on success, got %+v", a)
	}

	// Clears on a successful stream too.
	api.converseErr = validation(rawBedrockText)
	_, _ = p.Complete(context.Background(), drReq(fable))
	api.stream = newFakeStream(nil, nil)
	var sb strings.Builder
	if _, err := p.Stream(context.Background(), drReq(fable), &sb); err != nil {
		t.Fatal(err)
	}
	if a := p.ModelAvailability(fable); a.State != "ok" {
		t.Errorf("flag must clear on stream success, got %+v", a)
	}
}

func TestRefreshAccountDataRetention_AccessDeniedIsUnknownAndKeepsLastGood(t *testing.T) {
	ctrl := &fakeControlAPI{drErr: &smithy.GenericAPIError{Code: "AccessDeniedException", Message: "not authorized"}}
	p := &BedrockProvider{controlClient: ctrl, region: "us-east-1"}

	p.RefreshAccountDataRetention(context.Background())
	p.RefreshAccountDataRetention(context.Background())
	mode, at := p.AccountDataRetention()
	if mode != "unknown" || at != nil {
		t.Errorf("AccessDenied: mode=%q at=%v, want unknown/nil", mode, at)
	}
	if ctrl.drCalls != 2 {
		t.Errorf("calls = %d", ctrl.drCalls)
	}

	ctrl.drErr, ctrl.drMode = nil, bedrocktypes.DataRetentionModeAwsReview
	p.RefreshAccountDataRetention(context.Background())
	mode, at = p.AccountDataRetention()
	if mode != "aws_review" || at == nil {
		t.Fatalf("mode=%q at=%v", mode, at)
	}

	// A later failure keeps the last good value.
	ctrl.drErr = errors.New("boom")
	p.RefreshAccountDataRetention(context.Background())
	if m, _ := p.AccountDataRetention(); m != "aws_review" {
		t.Errorf("last good value lost: %q", m)
	}

	// A mode outside the closed vocabulary is reported as unknown.
	ctrl.drErr, ctrl.drMode = nil, "something_new"
	p.RefreshAccountDataRetention(context.Background())
	if m, _ := p.AccountDataRetention(); m != "unknown" {
		t.Errorf("out-of-vocabulary mode = %q", m)
	}
}
