package s3

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
)

// MarkerOutcome describes how a snapshot height relates to the shared marker.
type MarkerOutcome string

const (
	MarkerAdvanced MarkerOutcome = "advanced"
	MarkerEqual    MarkerOutcome = "equal"
	MarkerNewer    MarkerOutcome = "newer"
)

// LatestMarker advances a snapshot marker without replacing a higher height.
// An error leaves the publication unresolved and safe to retry.
type LatestMarker interface {
	Advance(context.Context, string, string, int64) (MarkerOutcome, error)
}

// LatestMarkerFactory builds a marker publisher for a region.
type LatestMarkerFactory func(context.Context, string) (LatestMarker, error)

// MarkerObjectClient provides the S3 operations used to publish latest.txt.
type MarkerObjectClient interface {
	GetObject(context.Context, *awss3.GetObjectInput, ...func(*awss3.Options)) (*awss3.GetObjectOutput, error)
	PutObject(context.Context, *awss3.PutObjectInput, ...func(*awss3.Options)) (*awss3.PutObjectOutput, error)
}

type latestMarker struct {
	client MarkerObjectClient
}

// NewLatestMarker uses S3 conditional writes to preserve a marker's maximum height.
// All publishers must use this protocol; unconditional writes can bypass it.
func NewLatestMarker(client MarkerObjectClient) LatestMarker {
	return &latestMarker{client: client}
}

func (m *latestMarker) Advance(ctx context.Context, bucket, key string, height int64) (MarkerOutcome, error) {
	if height <= 0 {
		return "", fmt.Errorf("snapshot marker height must be positive")
	}
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		current, etag, exists, err := m.read(ctx, bucket, key)
		if err != nil {
			return "", err
		}
		if current == height {
			return MarkerEqual, nil
		}
		if current > height {
			return MarkerNewer, nil
		}
		input := &awss3.PutObjectInput{
			Bucket: aws.String(bucket), Key: aws.String(key),
			Body: strings.NewReader(strconv.FormatInt(height, 10)),
		}
		if exists {
			if etag == "" {
				return "", fmt.Errorf("snapshot marker has no ETag for conditional replacement")
			}
			input.IfMatch = aws.String(etag)
		} else {
			input.IfNoneMatch = aws.String("*")
		}
		_, err = m.client.PutObject(ctx, input)
		if err == nil {
			return MarkerAdvanced, nil
		}
		// Another publisher may have won after our read. Reusing its old ETag
		// or retrying unconditionally could replace that publisher's height.
		if hasS3Code(err, "PreconditionFailed", "ConditionalRequestConflict") {
			continue
		}
		// A lost response may hide an accepted write. Leave it unresolved:
		// the next attempt can observe equal or newer without rewriting it.
		return "", fmt.Errorf("writing snapshot marker: %w", err)
	}
}

func (m *latestMarker) read(ctx context.Context, bucket, key string) (int64, string, bool, error) {
	out, err := m.client.GetObject(ctx, &awss3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)})
	if hasS3Code(err, "NoSuchKey") {
		return 0, "", false, nil
	}
	if err != nil {
		return 0, "", false, fmt.Errorf("reading snapshot marker: %w", err)
	}
	if out == nil || out.Body == nil {
		return 0, "", false, fmt.Errorf("snapshot marker has no body")
	}
	// The format is at most 19 decimal digits with an optional newline.
	const maxMarkerBytes = 20
	body, readErr := io.ReadAll(io.LimitReader(out.Body, maxMarkerBytes+1))
	closeErr := out.Body.Close()
	if readErr != nil || closeErr != nil {
		return 0, "", false, fmt.Errorf("reading snapshot marker body: %w", errors.Join(readErr, closeErr))
	}
	height, err := strconv.ParseInt(strings.TrimSpace(string(body)), 10, 64)
	if err != nil || height <= 0 || len(body) > maxMarkerBytes {
		return 0, "", false, fmt.Errorf("invalid snapshot marker height %q", body)
	}
	return height, aws.ToString(out.ETag), true, nil
}

func hasS3Code(err error, codes ...string) bool {
	var apiError smithy.APIError
	if !errors.As(err, &apiError) {
		return false
	}
	return slices.Contains(codes, apiError.ErrorCode())
}
