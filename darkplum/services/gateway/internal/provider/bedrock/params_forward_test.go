package bedrock

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"

	"github.com/scalecraft-dev/darkplum/gateway/internal/provider"
	"github.com/scalecraft-dev/darkplum/gateway/internal/userid"
)

// Phase A2 on Bedrock Converse: `user` becomes requestMetadata exactly as the
// handler supplied it (already pseudonymised); every other A2 parameter has
// no Converse equivalent and is not sent. parallel_tool_calls:false stays
// refused by the handler (ParamCaps.SerialToolCalls is false): Converse's
// toolChoice is auto/any/tool and none of them limits a turn to one call.

// Bedrock's requestMetadata constraints (AWS API reference, Converse):
// key and value up to 256 characters from [a-zA-Z0-9\s:_@$#=/+,-.], at most
// 16 pairs.
var bedrockMetadataCharset = regexp.MustCompile(`^[a-zA-Z0-9\s:_@$#=/+,\-.]{0,256}$`)

func userRequest() provider.ChatRequest {
	return provider.ChatRequest{
		Model:    "us.amazon.nova-lite-v1:0",
		Messages: []provider.Message{{Role: "user", Content: "hi"}},
		// The shape the userid package produces: key id, colon, 64 hex.
		User: "k1:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
	}
}

func TestComplete_UserBecomesRequestMetadata(t *testing.T) {
	fake := &fakeConverseAPI{converseOut: converseOutput("ok", types.StopReasonEndTurn, 1, 1)}
	req := userRequest()
	if _, err := newProviderWithFake(fake).Complete(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	md := fake.lastConverseInput.RequestMetadata
	if len(md) != 1 || md["user_id"] != req.User {
		t.Fatalf("RequestMetadata = %v, want {user_id: %s}", md, req.User)
	}
	for k, v := range md {
		if !bedrockMetadataCharset.MatchString(k) || !bedrockMetadataCharset.MatchString(v) || len(k) < 1 {
			t.Errorf("requestMetadata %q=%q violates Bedrock's constraints", k, v)
		}
	}
}

func TestStream_UserBecomesRequestMetadata(t *testing.T) {
	fake := &fakeConverseAPI{stream: newFakeStream([]types.ConverseStreamOutput{
		&types.ConverseStreamOutputMemberMessageStop{Value: types.MessageStopEvent{StopReason: types.StopReasonEndTurn}},
	}, nil)}
	req := userRequest()
	if _, err := newProviderWithFake(fake).Stream(context.Background(), req, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if md := fake.lastConverseStreamInput.RequestMetadata; md["user_id"] != req.User {
		t.Fatalf("RequestMetadata = %v", md)
	}
}

func TestNoUser_NoRequestMetadata(t *testing.T) {
	fake := &fakeConverseAPI{converseOut: converseOutput("ok", types.StopReasonEndTurn, 1, 1)}
	req := userRequest()
	req.User = ""
	if _, err := newProviderWithFake(fake).Complete(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if fake.lastConverseInput.RequestMetadata != nil {
		t.Errorf("RequestMetadata = %v, want nil", fake.lastConverseInput.RequestMetadata)
	}
}

func TestInapplicableParametersAreNotSent(t *testing.T) {
	seed := int64(1)
	no := false
	fake := &fakeConverseAPI{converseOut: converseOutput("ok", types.StopReasonEndTurn, 1, 1)}
	req := userRequest()
	req.User = ""
	req.Seed = &seed
	req.Logprobs = true
	req.ParallelToolCalls = &no
	if _, err := newProviderWithFake(fake).Complete(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if fake.lastConverseInput.RequestMetadata != nil || fake.lastConverseInput.AdditionalModelRequestFields != nil {
		t.Errorf("unexpected fields on the Converse request: %+v", fake.lastConverseInput)
	}
}

// Composition with the pseudonymiser, as the handler wires it: the email the
// caller sent is nowhere in the Converse request, only its k1: pseudonym.
func TestRequestMetadata_NeverCarriesTheRawIdentifier(t *testing.T) {
	const email = "alice@example.com"
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, userid.KeyFile), []byte("org-secret-org-secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	pseudonym, ok := userid.New(dir).Pseudonym(email)
	if !ok {
		t.Fatal("no pseudonym")
	}
	req := userRequest()
	req.User = pseudonym // what the handler hands a UserIDHMAC backend

	fake := &fakeConverseAPI{converseOut: converseOutput("ok", types.StopReasonEndTurn, 1, 1)}
	if _, err := newProviderWithFake(fake).Complete(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	for k, v := range fake.lastConverseInput.RequestMetadata {
		if strings.Contains(k+v, email) || strings.Contains(k+v, "alice") {
			t.Errorf("requestMetadata %q=%q carries the raw identifier", k, v)
		}
		if !bedrockMetadataCharset.MatchString(v) {
			t.Errorf("value %q violates Bedrock's charset", v)
		}
	}
	if got := fake.lastConverseInput.RequestMetadata["user_id"]; got != pseudonym || len(got) > 256 {
		t.Errorf("user_id = %q", got)
	}
}
