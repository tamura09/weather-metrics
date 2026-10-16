package main

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	"github.com/aws/aws-sdk-go-v2/service/ssm/types"
)

type fakeParameters map[string]string

func (f fakeParameters) GetParameter(_ context.Context, in *ssm.GetParameterInput, _ ...func(*ssm.Options)) (*ssm.GetParameterOutput, error) {
	value := f[*in.Name]
	return &ssm.GetParameterOutput{Parameter: &types.Parameter{Value: &value}}, nil
}

// The Function URL has no auth of its own, so a token parameter left blank must
// not turn into "no header required".
func TestServeRefusesWhenTheTokenParameterIsBlank(t *testing.T) {
	t.Setenv("API_TOKEN_PARAMETER_NAME", "/weather-metrics/api-token")
	// No bucket, as below: a request let through fails on ARCHIVE_BUCKET
	// instead, which the body tells apart from the refusal.
	instance := &app{parameters: fakeParameters{"/weather-metrics/api-token": " \n"}}

	for name, headers := range map[string]map[string]string{
		"no header":    nil,
		"empty header": {authHeader: ""},
	} {
		response, err := instance.serve(context.Background(), events.LambdaFunctionURLRequest{Headers: headers})
		if err != nil {
			t.Fatalf("%s: serve: %v", name, err)
		}
		if response.StatusCode != http.StatusInternalServerError || !strings.Contains(response.Body, "server misconfigured") {
			t.Errorf("%s: got %d %s, want 500 server misconfigured", name, response.StatusCode, response.Body)
		}
	}
}

func TestServeChecksTheToken(t *testing.T) {
	t.Setenv("API_TOKEN_PARAMETER_NAME", "/weather-metrics/api-token")
	// No bucket: a request that gets past the token check stops at the next
	// guard instead of reaching S3.
	instance := &app{parameters: fakeParameters{"/weather-metrics/api-token": "secret"}}

	for _, testCase := range []struct {
		name    string
		headers map[string]string
		want    int
	}{
		{name: "no header", headers: nil, want: http.StatusForbidden},
		{name: "empty header", headers: map[string]string{authHeader: ""}, want: http.StatusForbidden},
		{name: "wrong token", headers: map[string]string{authHeader: "guess"}, want: http.StatusForbidden},
		{name: "right token", headers: map[string]string{"X-Weather-Token": "secret"}, want: http.StatusInternalServerError},
	} {
		response, err := instance.serve(context.Background(), events.LambdaFunctionURLRequest{Headers: testCase.headers})
		if err != nil {
			t.Fatalf("%s: serve: %v", testCase.name, err)
		}
		if response.StatusCode != testCase.want {
			t.Errorf("%s: got %d %s, want %d", testCase.name, response.StatusCode, response.Body, testCase.want)
		}
	}
}
