package task

import (
	"encoding/json"
	"testing"

	. "github.com/onsi/gomega"
)

// The sidecar handler keys its deserialization on a literal "files" JSON
// field (sidecar/tasks/config.go ConfigPatchRequest). Dropping the json
// tag on ConfigPatchTask.Files would silently break every patch at
// runtime — Go's default would emit "Files" capitalized and the sidecar
// would see an empty params block. Pin the wire format here so a
// regression in the tag fails fast at unit-test time.
func TestConfigPatchTask_JSONWireFormatUsesFilesKey(t *testing.T) {
	g := NewWithT(t)
	in := ConfigPatchTask{Files: map[string]map[string]any{
		"config.toml": {"p2p": map[string]any{"external-address": "host:26656"}},
	}}

	raw, err := json.Marshal(in)
	g.Expect(err).NotTo(HaveOccurred())

	var asMap map[string]any
	g.Expect(json.Unmarshal(raw, &asMap)).To(Succeed())
	g.Expect(asMap).To(HaveKey("files"), "sidecar deserializes by 'files' key — drop the json tag and patches silently no-op")
	g.Expect(asMap).NotTo(HaveKey("Files"), "capitalised 'Files' would mean the json tag was dropped")

	// Roundtrip back into the controller-side type.
	var out ConfigPatchTask
	g.Expect(json.Unmarshal(raw, &out)).To(Succeed())
	g.Expect(out.Files).To(Equal(in.Files))
}
