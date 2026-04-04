package cli

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNormalizeConfigSetArgs(t *testing.T) {
	tests := []struct {
		name      string
		args      []string
		wantKey   string
		wantValue string
		wantErr   bool
	}{
		{
			name:    "clear key - no value arg",
			args:    []string{"setup.commands.clear"},
			wantKey: "setup.commands.clear",
		},
		{
			name:    "clear key - empty string value",
			args:    []string{"setup.commands.clear", ""},
			wantKey: "setup.commands.clear",
		},
		{
			name:    "clear key - equals and empty",
			args:    []string{"setup.commands.clear", "=", ""},
			wantKey: "setup.commands.clear",
		},
		{
			name:    "clear key - post_commands variant",
			args:    []string{"setup.post_commands.clear"},
			wantKey: "setup.post_commands.clear",
		},
		{
			name:      "normal key-value pair",
			args:      []string{"defaults.provider", "digitalocean"},
			wantKey:   "defaults.provider",
			wantValue: "digitalocean",
		},
		{
			name:      "key-value with equals separator",
			args:      []string{"defaults.provider", "=", "digitalocean"},
			wantKey:   "defaults.provider",
			wantValue: "digitalocean",
		},
		{
			name:      "key with trailing equals",
			args:      []string{"defaults.provider=", "digitalocean"},
			wantKey:   "defaults.provider",
			wantValue: "digitalocean",
		},
		{
			name:      "value with leading equals",
			args:      []string{"defaults.provider", "=digitalocean"},
			wantKey:   "defaults.provider",
			wantValue: "digitalocean",
		},
		{
			name:    "empty args",
			args:    []string{},
			wantErr: true,
		},
		{
			name:    "single non-clear key",
			args:    []string{"defaults.provider"},
			wantErr: true,
		},
		{
			name:    "only equals and empty strings",
			args:    []string{"=", "", "="},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			key, value, err := normalizeConfigSetArgs(tt.args)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantKey, key)
			assert.Equal(t, tt.wantValue, value)
		})
	}
}
