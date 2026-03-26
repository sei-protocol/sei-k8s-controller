package platform

import (
	"context"
	"errors"
	"sync"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// S3ObjectStore implements ObjectStore using the AWS S3 API.
// Regional clients are cached after first creation.
type S3ObjectStore struct {
	mu      sync.Mutex
	clients map[string]*s3.Client
}

// NewS3ObjectStore returns an S3-backed ObjectStore.
func NewS3ObjectStore() *S3ObjectStore {
	return &S3ObjectStore{clients: make(map[string]*s3.Client)}
}

func (s *S3ObjectStore) clientFor(ctx context.Context, region string) (*s3.Client, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if c, ok := s.clients[region]; ok {
		return c, nil
	}

	cfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(region))
	if err != nil {
		return nil, err
	}
	c := s3.NewFromConfig(cfg)
	s.clients[region] = c
	return c, nil
}

func (s *S3ObjectStore) HeadObject(ctx context.Context, bucket, key, region string) (bool, error) {
	c, err := s.clientFor(ctx, region)
	if err != nil {
		return false, err
	}

	_, err = c.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		var notFound *types.NotFound
		if errors.As(err, &notFound) {
			return false, nil
		}
		var noSuchKey *types.NoSuchKey
		if errors.As(err, &noSuchKey) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func (s *S3ObjectStore) ListPrefix(ctx context.Context, bucket, prefix, region string) ([]string, error) {
	c, err := s.clientFor(ctx, region)
	if err != nil {
		return nil, err
	}

	var keys []string
	paginator := s3.NewListObjectsV2Paginator(c, &s3.ListObjectsV2Input{
		Bucket: aws.String(bucket),
		Prefix: aws.String(prefix),
	})

	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		for _, obj := range page.Contents {
			if obj.Key != nil {
				keys = append(keys, *obj.Key)
			}
		}
	}
	return keys, nil
}
