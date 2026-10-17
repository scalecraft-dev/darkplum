package billing

import (
	"context"
	"errors"
	"math/big"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/costexplorer"
	cetypes "github.com/aws/aws-sdk-go-v2/service/costexplorer/types"
	"github.com/aws/smithy-go"
)

// CostExplorer is the slice of the Cost Explorer client used here (fakeable).
type CostExplorer interface {
	GetCostAndUsage(ctx context.Context, in *costexplorer.GetCostAndUsageInput, optFns ...func(*costexplorer.Options)) (*costexplorer.GetCostAndUsageOutput, error)
}

// BedrockSource reads Bedrock spend from AWS Cost Explorer with the api's own
// default credential chain (Pod Identity role in prod, developer creds in
// dev). There is no key: Creds reports whether the chain resolves.
type BedrockSource struct {
	CE    CostExplorer
	Creds func(ctx context.Context) error
}

// NewAWSBedrockSource builds the production source. Cost Explorer is a
// global service served from us-east-1. Credentials resolve lazily.
func NewAWSBedrockSource(ctx context.Context) (*BedrockSource, error) {
	cfg, err := config.LoadDefaultConfig(ctx, config.WithRegion("us-east-1"))
	if err != nil {
		return nil, err
	}
	return &BedrockSource{
		CE: costexplorer.NewFromConfig(cfg),
		Creds: func(ctx context.Context) error {
			_, err := cfg.Credentials.Retrieve(ctx)
			return err
		},
	}, nil
}

// Configured reports whether the credential chain resolves.
func (b *BedrockSource) Configured(ctx context.Context) bool {
	if b == nil || b.CE == nil {
		return false
	}
	if b.Creds == nil {
		return true
	}
	return b.Creds(ctx) == nil
}

func classifyAWS(err error) *Error {
	var api smithy.APIError
	if errors.As(err, &api) {
		switch api.ErrorCode() {
		case "AccessDeniedException", "UnrecognizedClientException", "ExpiredTokenException",
			"InvalidClientTokenId", "AccessDenied", "UnauthorizedOperation", "SignatureDoesNotMatch":
			return &Error{Kind: KindAuthFailed}
		}
		return &Error{Kind: KindVendorError}
	}
	return &Error{Kind: KindUnreachable}
}

// Pull returns Bedrock daily cost per usage type for [from, to). Model is
// left NULL; the reconciliation maps usage types at query time.
func (b *BedrockSource) Pull(ctx context.Context, from, to time.Time) ([]CostRow, *Error) {
	var rows []CostRow
	var token *string
	for i := 0; i < maxPages; i++ {
		out, err := b.CE.GetCostAndUsage(ctx, &costexplorer.GetCostAndUsageInput{
			TimePeriod:  &cetypes.DateInterval{Start: aws.String(dayOf(from)), End: aws.String(dayOf(to))},
			Granularity: cetypes.GranularityDaily,
			Metrics:     []string{"UnblendedCost"},
			Filter: &cetypes.Expression{Dimensions: &cetypes.DimensionValues{
				Key: cetypes.DimensionService, Values: []string{"Amazon Bedrock"}}},
			GroupBy:       []cetypes.GroupDefinition{{Type: cetypes.GroupDefinitionTypeDimension, Key: aws.String("USAGE_TYPE")}},
			NextPageToken: token,
		})
		if err != nil {
			return nil, classifyAWS(err)
		}
		for _, rt := range out.ResultsByTime {
			if rt.TimePeriod == nil || rt.TimePeriod.Start == nil {
				continue
			}
			day := *rt.TimePeriod.Start
			for _, g := range rt.Groups {
				if len(g.Keys) == 0 {
					continue
				}
				m, ok := g.Metrics["UnblendedCost"]
				if !ok || m.Amount == nil {
					continue
				}
				usd, ok := parseRat(*m.Amount)
				if !ok {
					return nil, &Error{Kind: KindVendorError}
				}
				rows = append(rows, CostRow{Day: day, Description: g.Keys[0], Cents: new(big.Rat).Mul(usd, newRat100())})
			}
		}
		if out.NextPageToken == nil || *out.NextPageToken == "" {
			break
		}
		token = out.NextPageToken
	}
	return AggregateCosts(rows), nil
}

var (
	usageRegionPrefixRe = regexp.MustCompile(`^[A-Z]{2,4}[0-9]?-`)
	nonAlnumRe          = regexp.MustCompile(`[^a-z0-9]+`)
	profilePrefixes     = []string{"us.", "eu.", "apac.", "global.", "us-gov.", "jp.", "au."}
	vendorPrefixes      = []string{"anthropic.", "amazon.", "meta.", "mistral.", "cohere.", "ai21.", "deepseek."}
)

func bedrockFamily(usageType string) string {
	u := usageRegionPrefixRe.ReplaceAllString(usageType, "")
	head, _, _ := strings.Cut(u, "-")
	return nonAlnumRe.ReplaceAllString(strings.ToLower(head), "")
}

func bedrockBase(modelID string) string {
	m := strings.ToLower(modelID)
	for _, p := range profilePrefixes {
		m = strings.TrimPrefix(m, p)
	}
	for _, p := range vendorPrefixes {
		m = strings.TrimPrefix(m, p)
	}
	return nonAlnumRe.ReplaceAllString(m, "")
}

// MapBedrockUsageType is the documented best-effort Cost Explorer usage type
// -> model id mapping. A usage type looks like "USE1-ClaudeHaiku45-input-tokens":
// strip the leading region code ("USE1-"), take the family before the next
// "-" ("ClaudeHaiku45"), lowercase it and drop punctuation ("claudehaiku45"),
// and prefix-match it against the same normalisation of candidate model ids
// (inference profile prefix such as "us."/"global." and the vendor prefix
// dropped; "us.anthropic.claude-haiku-4-5-2025…-v1:0" -> "claudehaiku4520251001v10").
// It returns a candidate only when every match shares ONE normalised base
// (so "us." and "global." profiles of the same model are fine, while two
// dated versions of one family are ambiguous); nil otherwise -- the
// reconciliation then keeps the row keyed by its usage type.
func MapBedrockUsageType(usageType string, candidates []string) *string {
	fam := bedrockFamily(usageType)
	if fam == "" {
		return nil
	}
	var matches []string
	bases := map[string]struct{}{}
	for _, c := range candidates {
		b := bedrockBase(c)
		if strings.HasPrefix(b, fam) {
			matches = append(matches, c)
			bases[b] = struct{}{}
		}
	}
	if len(matches) == 0 || len(bases) != 1 {
		return nil
	}
	sort.Strings(matches)
	return &matches[0]
}
