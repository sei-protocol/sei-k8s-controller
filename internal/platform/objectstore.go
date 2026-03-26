package platform

import "context"

// ObjectStore provides read-only access to an object-storage backend.
// The controller uses this to check whether genesis artifacts exist in S3
// during ceremony orchestration.
type ObjectStore interface {
	// HeadObject returns true if the object exists at bucket/key in the given region.
	HeadObject(ctx context.Context, bucket, key, region string) (bool, error)

	// ListPrefix returns all object keys under bucket/prefix in the given region.
	ListPrefix(ctx context.Context, bucket, prefix, region string) ([]string, error)
}
