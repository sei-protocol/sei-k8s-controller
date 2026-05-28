package v1alpha1

import "testing"

func TestNetworkingConfig_HTTPEnabled(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		cfg  *NetworkingConfig
		want bool
	}{
		{
			name: "nil receiver",
			cfg:  nil,
			want: false,
		},
		{
			name: "legacy empty struct enables HTTP",
			cfg:  &NetworkingConfig{},
			want: true,
		},
		{
			name: "explicit HTTP set",
			cfg:  &NetworkingConfig{HTTP: &HTTPConfig{}},
			want: true,
		},
		{
			name: "TCP-only does not enable HTTP",
			cfg:  &NetworkingConfig{TCP: &TCPConfig{}},
			want: false,
		},
		{
			name: "HTTP and TCP both set",
			cfg:  &NetworkingConfig{HTTP: &HTTPConfig{}, TCP: &TCPConfig{}},
			want: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.cfg.HTTPEnabled(); got != tc.want {
				t.Errorf("HTTPEnabled() = %v, want %v", got, tc.want)
			}
		})
	}
}
