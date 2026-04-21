package task

import sidecar "github.com/sei-protocol/seictl/sidecar/client"

// SnapshotUploadParams are the serialized fields for snapshot-upload.
// The sidecar reads its upload target (bucket/region/prefix) from env
// (SEI_SNAPSHOT_BUCKET / SEI_SNAPSHOT_REGION), so this task takes no
// parameters. Submitting it arms the sidecar's continuous upload loop;
// absent this task the sidecar never starts uploading even when seid
// produces snapshots on disk.
type SnapshotUploadParams struct{}

func (p *SnapshotUploadParams) taskType() string { return sidecar.TaskTypeSnapshotUpload }

func (p *SnapshotUploadParams) toRequestParams() *map[string]any { return nil }
