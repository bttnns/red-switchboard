package v1

import "encoding/json"

// marshalGraphQL builds a JSON GraphQL request body, JSON-marshaling the
// operation name, query and variables so special characters in tokens, ids,
// usernames or passwords cannot corrupt the body. A nil variables map is
// marshaled as a JSON null, matching the Rivian API's expectation for
// operations that take no variables.
func marshalGraphQL(operationName, query string, variables map[string]any) string {
	body := struct {
		OperationName string         `json:"operationName"`
		Query         string         `json:"query"`
		Variables     map[string]any `json:"variables"`
	}{operationName, query, variables}
	b, _ := json.Marshal(body)
	return string(b)
}
