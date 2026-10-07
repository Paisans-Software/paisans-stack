package garage

import (
	"fmt"
	"strings"
)

// CurlStdin runs curl with its configuration on stdin, the only way this
// toolkit sends a signed S3 request: the configuration carries the secret
// key, and stdin is not in a command line where `ps` would show it.
const CurlStdin = "curl -fsS --max-time 20 -K -"

// S3Object is one object addressed through one Garage node's S3 API, with the
// key that signs the request. `storage add` writes its smoke probe with one,
// and `storage rotate-key` proves a new key with one, so the request is
// shaped in one place.
type S3Object struct {
	// Address is the node's mesh address; the S3 API listens on 3900.
	Address string
	Bucket  string
	Key     string
	KeyID   string
	Secret  string
	Region  string
}

// CurlConfig is a curl configuration for one signed request, for CurlStdin.
// body is sent only with PUT.
func (o S3Object) CurlConfig(method, body string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "url = \"http://%s:3900/%s/%s\"\n", o.Address, o.Bucket, o.Key)
	fmt.Fprintf(&b, "request = \"%s\"\n", method)
	fmt.Fprintf(&b, "user = \"%s:%s\"\n", o.KeyID, o.Secret)
	fmt.Fprintf(&b, "aws-sigv4 = \"aws:amz:%s:s3\"\n", o.Region)
	b.WriteString("header = \"x-amz-content-sha256: UNSIGNED-PAYLOAD\"\n")
	if method == "PUT" {
		fmt.Fprintf(&b, "data-binary = \"%s\"\n", body)
	}
	return b.String()
}

// Redact removes the secret from text a request produced.
func (o S3Object) Redact(text string) string {
	return redact(text, o.Secret)
}
