package mcpserver

import (
	"math"
	"testing"
	"time"

	"github.com/meigma/codemode"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMinutesToDuration(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		minutes int64
		want    time.Duration
		wantErr bool
	}{
		{name: "one minute", minutes: 1, want: time.Minute},
		{name: "zero", minutes: 0, wantErr: true},
		{name: "negative", minutes: -1, wantErr: true},
		{name: "overflow", minutes: math.MaxInt64, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := minutesToDuration(tt.minutes)
			if tt.wantErr {
				var actionable *codemode.AgentError
				require.ErrorAs(t, err, &actionable)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestSecondsToDuration(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		seconds int64
		want    time.Duration
		wantErr bool
	}{
		{name: "zero", seconds: 0, want: 0},
		{name: "one second", seconds: 1, want: time.Second},
		{name: "negative", seconds: -1, wantErr: true},
		{name: "overflow", seconds: math.MaxInt64, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := secondsToDuration(tt.seconds)
			if tt.wantErr {
				var actionable *codemode.AgentError
				require.ErrorAs(t, err, &actionable)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestParseEnv(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		raw     string
		want    map[string]string
		wantErr bool
	}{
		{name: "empty", raw: "", want: map[string]string{}},
		{name: "one pair", raw: "FOO=bar", want: map[string]string{"FOO": "bar"}},
		{name: "splits at first equals", raw: "FOO=bar=baz", want: map[string]string{"FOO": "bar=baz"}},
		{
			name: "multiple lines",
			raw:  "A=1\nB=2\r\nC=3",
			want: map[string]string{"A": "1", "B": "2", "C": "3"},
		},
		{name: "skips blank lines", raw: "A=1\n\nB=2\n", want: map[string]string{"A": "1", "B": "2"}},
		{name: "missing equals", raw: "FOO", wantErr: true},
		{name: "empty key", raw: "=bar", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := parseEnv(tt.raw)
			if tt.wantErr {
				var actionable *codemode.AgentError
				require.ErrorAs(t, err, &actionable)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}
