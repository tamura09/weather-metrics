package main

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"io"
	"strings"
	"testing"

	"github.com/aws/aws-lambda-go/events"
)

func TestCompressIfAccepted(t *testing.T) {
	body := strings.Repeat(`{"time":1794715301000,"temp":11.7},`, 200)
	plain := events.LambdaFunctionURLResponse{StatusCode: 200, Headers: map[string]string{"Content-Type": "application/json"}, Body: body}

	got := compressIfAccepted(map[string]string{"accept-encoding": "gzip, deflate"}, plain)
	if !got.IsBase64Encoded || got.Headers["Content-Encoding"] != "gzip" || got.Headers["Content-Type"] != "application/json" {
		t.Fatalf("got headers %v, base64 %v", got.Headers, got.IsBase64Encoded)
	}
	raw, err := base64.StdEncoding.DecodeString(got.Body)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	decoded, _ := io.ReadAll(reader)
	if string(decoded) != body {
		t.Error("decompressed body differs from the original")
	}
	if len(got.Body) >= len(body) {
		t.Errorf("compressed %d bytes is not smaller than %d", len(got.Body), len(body))
	}
	// The original response's headers are not modified in place.
	if _, ok := plain.Headers["Content-Encoding"]; ok {
		t.Error("original headers were modified")
	}

	for name, headers := range map[string]map[string]string{
		"no header":    nil,
		"other coding": {"accept-encoding": "br"},
		"refused":      {"Accept-Encoding": "gzip;q=0"},
	} {
		if got := compressIfAccepted(headers, plain); got.IsBase64Encoded || got.Body != body {
			t.Errorf("%s: compressed", name)
		}
	}

	small := events.LambdaFunctionURLResponse{StatusCode: 200, Body: `[]`}
	if got := compressIfAccepted(map[string]string{"accept-encoding": "gzip"}, small); got.IsBase64Encoded {
		t.Error("a tiny body was compressed")
	}
}
