package v1

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMarshalGraphQLEscapesVariables pins the fix for the old fmt.Sprintf-into-JSON
// bug: a password (or any variable) containing quotes or backslashes must produce a
// valid JSON body that round-trips back to the exact same value.
func TestMarshalGraphQLEscapesVariables(t *testing.T) {
	const nasty = `p"a\s s'w"ord\` // quotes, backslashes, a trailing backslash

	body := marshalGraphQL("Login",
		"mutation Login($email: String!, $password: String!) { login }",
		map[string]any{"email": "a@b.co", "password": nasty},
	)

	var got struct {
		OperationName string `json:"operationName"`
		Variables     struct {
			Email    string `json:"email"`
			Password string `json:"password"`
		} `json:"variables"`
	}
	// We expect the body to be valid JSON despite the nasty characters...
	require.NoError(t, json.Unmarshal([]byte(body), &got), "request body must be valid JSON: %s", body)
	// ...and the password to survive the round-trip exactly.
	assert.Equal(t, nasty, got.Variables.Password, "password must be preserved verbatim")
	assert.Equal(t, "Login", got.OperationName)
}
