package bedrock

import (
	"context"

	"github.com/aws/aws-sdk-go-v2/service/bedrock"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"
)

// converseStream abstracts the subset of *bedrockruntime.ConverseStreamEventStream
// that Stream needs. The concrete SDK type (returned by
// (*bedrockruntime.ConverseStreamOutput).GetStream()) already satisfies this
// interface structurally, so no adapter type is needed for the real path — only
// for tests, which supply a fake that feeds synthetic events without spinning up
// the SDK's HTTP event-stream reader.
type converseStream interface {
	// Events returns the channel of decoded stream events. The channel is closed
	// when the stream ends (either normally via message_stop / EOF, or on error —
	// callers must additionally check Err() after the channel closes).
	Events() <-chan types.ConverseStreamOutput
	// Err returns any error that occurred while reading events from the stream.
	// Must be checked after Events() closes; a mid-stream error (e.g.
	// ModelStreamErrorException, ThrottlingException) surfaces here rather than
	// as an event.
	Err() error
	// Close releases the underlying stream resources. Must always be called.
	Close() error
}

// converseAPI abstracts the two Bedrock runtime operations BedrockProvider
// calls, so Complete and Stream can be unit-tested against fakes without any
// AWS SDK or network involvement.
type converseAPI interface {
	Converse(ctx context.Context, params *bedrockruntime.ConverseInput, optFns ...func(*bedrockruntime.Options)) (*bedrockruntime.ConverseOutput, error)
	ConverseStream(ctx context.Context, params *bedrockruntime.ConverseStreamInput, optFns ...func(*bedrockruntime.Options)) (converseStream, error)
}

// controlAPI abstracts the Bedrock control-plane operation DiscoverModels
// calls, so pagination can be unit-tested against a fake without any AWS SDK
// or network involvement. *bedrock.Client satisfies this structurally.
type controlAPI interface {
	ListInferenceProfiles(ctx context.Context, params *bedrock.ListInferenceProfilesInput, optFns ...func(*bedrock.Options)) (*bedrock.ListInferenceProfilesOutput, error)
	GetAccountDataRetention(ctx context.Context, params *bedrock.GetAccountDataRetentionInput, optFns ...func(*bedrock.Options)) (*bedrock.GetAccountDataRetentionOutput, error)
}

// runtimeClient adapts *bedrockruntime.Client to the converseAPI interface.
// This is the only piece of production code that touches the concrete SDK
// client; everything else in this package depends on the interface.
type runtimeClient struct {
	client *bedrockruntime.Client
}

func newRuntimeClient(client *bedrockruntime.Client) *runtimeClient {
	return &runtimeClient{client: client}
}

func (r *runtimeClient) Converse(ctx context.Context, params *bedrockruntime.ConverseInput, optFns ...func(*bedrockruntime.Options)) (*bedrockruntime.ConverseOutput, error) {
	return r.client.Converse(ctx, params, optFns...)
}

func (r *runtimeClient) ConverseStream(ctx context.Context, params *bedrockruntime.ConverseStreamInput, optFns ...func(*bedrockruntime.Options)) (converseStream, error) {
	out, err := r.client.ConverseStream(ctx, params, optFns...)
	if err != nil {
		return nil, err
	}
	// *bedrockruntime.ConverseStreamEventStream implements converseStream
	// structurally (Events/Err/Close all match), so no wrapper type is needed.
	return out.GetStream(), nil
}

// compile-time interface check.
var _ converseAPI = (*runtimeClient)(nil)
