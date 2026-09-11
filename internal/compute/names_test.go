package compute

import (
	"testing"

	"github.com/meigma/codemode"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidateName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		input   string
		wantErr string
	}{
		{name: "single alphanumeric", input: "a"},
		{name: "max length", input: "a2345678901234567890123456789012"},
		{name: "internal hyphen", input: "amber-cedar"},
		{name: "digits", input: "web1"},
		{name: "empty", input: "", wantErr: `invalid name ""`},
		{name: "uppercase", input: "Demo", wantErr: `invalid name "Demo"`},
		{name: "leading hyphen", input: "-web", wantErr: `invalid name "-web"`},
		{name: "trailing hyphen", input: "web-", wantErr: `invalid name "web-"`},
		{name: "underscore", input: "web_1", wantErr: `invalid name "web_1"`},
		{
			name:    "too long",
			input:   "a23456789012345678901234567890123",
			wantErr: `invalid name "a23456789012345678901234567890123"`,
		},
		{name: "reserved default", input: "default", wantErr: `name "default" is reserved`},
		{name: "reserved none", input: "none", wantErr: `name "none" is reserved`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := validateName(tt.input)
			if tt.wantErr == "" {
				require.NoError(t, err)
				return
			}
			requireAgentMessage(t, err, tt.wantErr)
		})
	}
}

func TestGenerateNameMatchesGrammar(t *testing.T) {
	t.Parallel()

	for range 32 {
		name, err := generateName()
		require.NoError(t, err)
		require.NoError(t, validateName(name), "generated %q", name)
		assert.Contains(t, name, "-")
	}
}

func requireAgentMessage(t *testing.T, err error, message string) {
	t.Helper()
	require.Error(t, err)
	var agent *codemode.AgentError
	require.ErrorAs(t, err, &agent)
	assert.Equal(t, message, agent.Message)
}
