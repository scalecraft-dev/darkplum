package objectstore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

// S3Config configures NewS3.
type S3Config struct {
	// URL is s3://bucket/prefix/ (the prefix may be empty).
	URL string
	// Endpoint overrides the S3 endpoint (MinIO in the kind dev stack) and
	// switches to path-style addressing.
	Endpoint string
	Region   string
	// AccessKeyID/SecretAccessKey are static credentials used ONLY by this
	// client, and only when both are set. Everything else in the api (Cost
	// Explorer, Bedrock) keeps the default chain: the mounted profile in
	// dev-aws, Pod Identity in production.
	AccessKeyID     string
	SecretAccessKey string
}

// S3ConfigFromEnv reads LIBRARY_STORE_URL, LIBRARY_S3_ENDPOINT (when set,
// path-style addressing is used, as MinIO needs; otherwise virtual-hosted
// style), LIBRARY_S3_REGION, LIBRARY_S3_ACCESS_KEY_ID and
// LIBRARY_S3_SECRET_ACCESS_KEY. URL is empty when no store is configured.
func S3ConfigFromEnv() S3Config {
	return S3Config{
		URL:             strings.TrimSpace(os.Getenv("LIBRARY_STORE_URL")),
		Endpoint:        strings.TrimSpace(os.Getenv("LIBRARY_S3_ENDPOINT")),
		Region:          strings.TrimSpace(os.Getenv("LIBRARY_S3_REGION")),
		AccessKeyID:     os.Getenv("LIBRARY_S3_ACCESS_KEY_ID"),
		SecretAccessKey: os.Getenv("LIBRARY_S3_SECRET_ACCESS_KEY"),
	}
}

// ParseS3URL splits s3://bucket/prefix/ into bucket and a prefix that is
// empty or ends in "/". Query strings, userinfo and ".." are refused.
func ParseS3URL(raw string) (bucket, prefix string, err error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "s3" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", "", errors.New("LIBRARY_STORE_URL must look like s3://bucket/prefix/")
	}
	p := strings.TrimPrefix(u.Path, "/")
	if strings.Contains(p, "..") || strings.Contains(p, "//") {
		return "", "", errors.New("LIBRARY_STORE_URL prefix is not valid")
	}
	if p != "" && !strings.HasSuffix(p, "/") {
		p += "/"
	}
	return u.Host, p, nil
}

type s3Store struct {
	c      *s3.Client
	bucket string
	prefix string
}

// NewS3 builds the S3 store. Credentials come from config.LoadDefaultConfig
// exactly as billing/bedrock.go does (Pod Identity in production, the mounted
// profile in dev-aws), unless both static keys are set in cfg.
//
// NOTE: without s3:ListBucket on the prefix, S3 answers a missing key with
// 403 AccessDenied instead of 404, and Head/Get would then report a
// permission error for an object that simply is not there. The Terraform
// policy grants ListBucket for exactly this reason.
func NewS3(ctx context.Context, cfg S3Config) (Store, error) {
	bucket, prefix, err := ParseS3URL(cfg.URL)
	if err != nil {
		return nil, err
	}
	opts := []func(*config.LoadOptions) error{}
	if cfg.Region != "" {
		opts = append(opts, config.WithRegion(cfg.Region))
	}
	if cfg.AccessKeyID != "" && cfg.SecretAccessKey != "" {
		opts = append(opts, config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(cfg.AccessKeyID, cfg.SecretAccessKey, "")))
	}
	ac, err := config.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("load aws config: %w", err)
	}
	if ac.Region == "" {
		ac.Region = "us-east-1"
	}
	cl := s3.NewFromConfig(ac, func(o *s3.Options) {
		if cfg.Endpoint != "" {
			o.BaseEndpoint = aws.String(cfg.Endpoint)
		}
		o.UsePathStyle = cfg.Endpoint != ""
	})
	return &s3Store{c: cl, bucket: bucket, prefix: prefix}, nil
}

func (s *s3Store) Bucket() string        { return s.bucket }
func (s *s3Store) URI(key string) string { return "s3://" + s.bucket + "/" + s.prefix + key }
func (s *s3Store) full(key string) *string {
	return aws.String(s.prefix + key)
}

// wrap reduces an SDK error to an Error (or ErrNotFound for a missing key).
func wrap(op string, err error, notFoundOK bool) error {
	if err == nil {
		return nil
	}
	e := &Error{Op: op}
	var ae smithy.APIError
	if errors.As(err, &ae) {
		e.Code = ae.ErrorCode()
	}
	var re *awshttp.ResponseError
	if errors.As(err, &re) {
		e.Status = re.HTTPStatusCode()
		e.RequestID = re.ServiceRequestID()
	}
	if notFoundOK {
		var nsk *s3types.NoSuchKey
		var nf *s3types.NotFound
		if errors.As(err, &nsk) || errors.As(err, &nf) || e.Code == "NoSuchKey" || e.Code == "NotFound" || e.Status == 404 {
			return ErrNotFound
		}
	}
	if e.Code == "" && e.Status == 0 {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			return err
		}
		e.Code = "TransportError"
	}
	return e
}

func (s *s3Store) Put(ctx context.Context, key string, body []byte, meta Meta) error {
	in := &s3.PutObjectInput{Bucket: &s.bucket, Key: s.full(key), Body: bytes.NewReader(body), ContentLength: aws.Int64(int64(len(body)))}
	if len(meta) > 0 {
		in.Metadata = map[string]string(meta)
	}
	switch {
	case strings.HasSuffix(key, ".md"):
		in.ContentType = aws.String("text/markdown; charset=utf-8")
	case strings.HasSuffix(key, ".json"):
		in.ContentType = aws.String("application/json")
	}
	_, err := s.c.PutObject(ctx, in)
	return wrap("put", err, false)
}

func (s *s3Store) Head(ctx context.Context, key string) (Info, error) {
	out, err := s.c.HeadObject(ctx, &s3.HeadObjectInput{Bucket: &s.bucket, Key: s.full(key)})
	if err != nil {
		return Info{}, wrap("head", err, true)
	}
	return Info{Key: key, Size: aws.ToInt64(out.ContentLength), LastModified: aws.ToTime(out.LastModified), ETag: strings.Trim(aws.ToString(out.ETag), `"`), Meta: lowerMeta(out.Metadata)}, nil
}

func (s *s3Store) Get(ctx context.Context, key string) ([]byte, Info, error) {
	out, err := s.c.GetObject(ctx, &s3.GetObjectInput{Bucket: &s.bucket, Key: s.full(key)})
	if err != nil {
		return nil, Info{}, wrap("get", err, true)
	}
	defer out.Body.Close()
	// The library's objects are at most ~130 KiB; cap defensively so a
	// foreign huge object in the bucket cannot exhaust memory.
	b, err := io.ReadAll(io.LimitReader(out.Body, 1<<20))
	if err != nil {
		return nil, Info{}, wrap("get", err, false)
	}
	return b, Info{Key: key, Size: int64(len(b)), LastModified: aws.ToTime(out.LastModified), ETag: strings.Trim(aws.ToString(out.ETag), `"`), Meta: lowerMeta(out.Metadata)}, nil
}

func lowerMeta(m map[string]string) Meta {
	out := Meta{}
	for k, v := range m {
		out[strings.ToLower(k)] = v
	}
	return out
}

func (s *s3Store) Delete(ctx context.Context, key string) error {
	_, err := s.c.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: &s.bucket, Key: s.full(key)})
	if err := wrap("delete", err, true); err != nil && !errors.Is(err, ErrNotFound) {
		return err
	}
	return nil
}

func (s *s3Store) PurgeVersions(ctx context.Context, key string) error {
	full := s.prefix + key
	var keyMarker, versionMarker *string
	for {
		out, err := s.c.ListObjectVersions(ctx, &s3.ListObjectVersionsInput{Bucket: &s.bucket, Prefix: aws.String(full), KeyMarker: keyMarker, VersionIdMarker: versionMarker})
		if err != nil {
			return wrap("purge", err, false)
		}
		var ids []s3types.ObjectIdentifier
		for _, v := range out.Versions {
			if aws.ToString(v.Key) == full {
				ids = append(ids, s3types.ObjectIdentifier{Key: v.Key, VersionId: v.VersionId})
			}
		}
		for _, d := range out.DeleteMarkers {
			if aws.ToString(d.Key) == full {
				ids = append(ids, s3types.ObjectIdentifier{Key: d.Key, VersionId: d.VersionId})
			}
		}
		for _, id := range ids {
			if _, err := s.c.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: &s.bucket, Key: id.Key, VersionId: id.VersionId}); err != nil {
				if w := wrap("purge", err, true); !errors.Is(w, ErrNotFound) {
					return w
				}
			}
		}
		if !aws.ToBool(out.IsTruncated) {
			return nil
		}
		keyMarker, versionMarker = out.NextKeyMarker, out.NextVersionIdMarker
	}
}

func (s *s3Store) List(ctx context.Context, prefix, cursor string, limit int) ([]Info, string, error) {
	if limit <= 0 || limit > 1000 {
		limit = 1000
	}
	in := &s3.ListObjectsV2Input{Bucket: &s.bucket, Prefix: aws.String(s.prefix + prefix), MaxKeys: aws.Int32(int32(limit))}
	if cursor != "" {
		in.ContinuationToken = aws.String(cursor)
	}
	out, err := s.c.ListObjectsV2(ctx, in)
	if err != nil {
		return nil, "", wrap("list", err, false)
	}
	res := make([]Info, 0, len(out.Contents))
	for _, o := range out.Contents {
		k := strings.TrimPrefix(aws.ToString(o.Key), s.prefix)
		res = append(res, Info{Key: k, Size: aws.ToInt64(o.Size), LastModified: aws.ToTime(o.LastModified), ETag: strings.Trim(aws.ToString(o.ETag), `"`)})
	}
	next := ""
	if aws.ToBool(out.IsTruncated) {
		next = aws.ToString(out.NextContinuationToken)
	}
	return res, next, nil
}
