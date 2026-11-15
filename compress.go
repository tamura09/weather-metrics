package main

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"log"
	"strconv"
	"strings"

	"github.com/aws/aws-lambda-go/events"
)

// A Lambda response is capped at 6MB, and the raw observation endpoint returns
// every reading in the window in one. JSON of this shape gzips about 14x
// (1.8MB to 125KB for a month of fifteen-minute readings), so compressing is
// what keeps a long window under the cap; the retention rule (retention.go)
// keeps the row count down, and the two together leave a wide margin.
//
// Only a client that asks for gzip gets it. Grafana's Infinity datasource
// fetches with Go's HTTP client, which asks by default and decompresses
// transparently; anything that does not ask still gets plain JSON.
const (
	compressAbove = 1 << 10 // smaller bodies are not worth the base64 overhead
	logAbove      = 1 << 20 // responses this big are logged, to see how close the cap is
)

func compressIfAccepted(headers map[string]string, response events.LambdaFunctionURLResponse) events.LambdaFunctionURLResponse {
	size := len(response.Body)
	if response.IsBase64Encoded || size < compressAbove || !acceptsGzip(headers) {
		if size >= logAbove {
			log.Printf("response %d bytes, not compressed", size)
		}
		return response
	}

	var compressed bytes.Buffer
	writer := gzip.NewWriter(&compressed)
	if _, err := writer.Write([]byte(response.Body)); err != nil {
		log.Printf("gzip response: %v", err)
		return response
	}
	if err := writer.Close(); err != nil {
		log.Printf("gzip response: %v", err)
		return response
	}

	headers = make(map[string]string, len(response.Headers)+2)
	for name, value := range response.Headers {
		headers[name] = value
	}
	headers["Content-Encoding"] = "gzip"
	headers["Vary"] = "Accept-Encoding"
	response.Headers = headers
	response.Body = base64.StdEncoding.EncodeToString(compressed.Bytes())
	response.IsBase64Encoded = true

	if size >= logAbove {
		log.Printf("response %d bytes, gzip %d bytes", size, compressed.Len())
	}
	return response
}

// acceptsGzip reads Accept-Encoding, which a Function URL passes through with a
// lowercased name. An explicit gzip entry decides on its own, so "gzip;q=0, *"
// is a refusal; "*" counts only when gzip is not named (RFC 9110 12.5.3). A
// q of 0 is a refusal either way.
func acceptsGzip(headers map[string]string) bool {
	gzipAccepted, wildcardAccepted, gzipNamed, wildcardNamed := false, false, false, false
	for name, value := range headers {
		if !strings.EqualFold(name, "accept-encoding") {
			continue
		}
		for _, part := range strings.Split(value, ",") {
			coding, params, _ := strings.Cut(strings.TrimSpace(part), ";")
			accepted := qualityAbove0(params)
			switch strings.ToLower(strings.TrimSpace(coding)) {
			case "gzip", "x-gzip":
				gzipNamed = true
				gzipAccepted = gzipAccepted || accepted
			case "*":
				wildcardNamed = true
				wildcardAccepted = wildcardAccepted || accepted
			}
		}
	}
	if gzipNamed {
		return gzipAccepted
	}
	return wildcardNamed && wildcardAccepted
}

// qualityAbove0 reports whether a coding's parameters leave it acceptable: no
// q at all means 1, and an unreadable q is treated as a refusal.
func qualityAbove0(params string) bool {
	for _, param := range strings.Split(params, ";") {
		key, value, found := strings.Cut(strings.TrimSpace(param), "=")
		if !found || !strings.EqualFold(strings.TrimSpace(key), "q") {
			continue
		}
		q, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
		return err == nil && q > 0
	}
	return true
}
