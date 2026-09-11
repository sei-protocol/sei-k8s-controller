package wire

import "testing"

// The snapshot-upload outcome values are a wire contract: the CLI poller and the
// controller classify against these exact strings. A rename here that drifted a
// value would silently reclassify every upload, so pin the bytes.
func TestSnapshotUploadWireValues(t *testing.T) {
	cases := []struct {
		got  string
		want string
	}{
		{string(OutcomeUploaded), "uploaded"},
		{string(OutcomeNoop), "noop"},
		{string(OutcomeError), "error"},
		{string(NoopFewerThanTwoSnapshots), "fewer-than-2-snapshots"},
		{string(NoopAlreadyUploaded), "already-uploaded"},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("wire value = %q, want %q", c.got, c.want)
		}
	}
}

func TestParseVoteOption(t *testing.T) {
	cases := []struct {
		in   string
		want VoteOption
		err  bool
	}{
		{"yes", OptionYes, false},
		{"YES", OptionYes, false},
		{"no", OptionNo, false},
		{"abstain", OptionAbstain, false},
		{"no_with_veto", OptionNoWithVeto, false},
		{"no-with-veto", OptionNoWithVeto, false},
		{"NO_WITH_VETO", OptionNoWithVeto, false},
		{"", 0, true},
		{"maybe", 0, true},
	}
	for _, c := range cases {
		got, err := ParseVoteOption(c.in)
		if c.err {
			if err == nil {
				t.Errorf("ParseVoteOption(%q): want err, got %v", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseVoteOption(%q): unexpected err: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("ParseVoteOption(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestUpCheckValidate(t *testing.T) {
	cases := []struct {
		name  string
		check UpCheck
		ok    bool
	}{
		{"tcp", UpCheck{Scheme: UpCheckTCP, Port: 26656}, true},
		{"http", UpCheck{Scheme: UpCheckHTTP, Port: 26657, Path: "/status"}, true},
		{"tcp with path", UpCheck{Scheme: UpCheckTCP, Port: 26656, Path: "/"}, false},
		{"http no path", UpCheck{Scheme: UpCheckHTTP, Port: 8545}, false},
		{"http relative path", UpCheck{Scheme: UpCheckHTTP, Port: 8545, Path: "status"}, false},
		{"port zero", UpCheck{Scheme: UpCheckTCP, Port: 0}, false},
		{"port high", UpCheck{Scheme: UpCheckTCP, Port: 70000}, false},
		{"unknown scheme", UpCheck{Scheme: "grpc", Port: 9090}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.check.Validate()
			if (err == nil) != tc.ok {
				t.Fatalf("Validate() = %v, want ok=%v", err, tc.ok)
			}
		})
	}
}
