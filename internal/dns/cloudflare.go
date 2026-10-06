package dns

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// recordComment is written on every record this package creates, so that a
// person looking at the zone can tell which records the toolkit made. It is
// informational only: nothing reads it back to decide ownership, because a
// comment anyone can edit is not a claim anyone can trust.
const recordComment = "created by paisans dns init"

// cloudflare is Cloudflare's API v4.
//
// Request and response shapes are from Cloudflare's API reference:
//
//   - list zones:   GET  /zones?name=<name>
//     https://developers.cloudflare.com/api/resources/zones/methods/list/
//   - list records: GET  /zones/{zone_id}/dns_records?name.exact=<name>
//     https://developers.cloudflare.com/api/resources/dns/subresources/records/methods/list/
//   - create:       POST /zones/{zone_id}/dns_records
//     https://developers.cloudflare.com/api/resources/dns/subresources/records/methods/create/
//
// Every response is the same envelope: success, errors (code and message),
// result, and on lists result_info.
type cloudflare struct {
	token string
	// base is the API root. It is a field rather than a constant only so that
	// tests can point it at an httptest server; there is deliberately no flag
	// for it, because an operator has no reason to send this token anywhere
	// but Cloudflare.
	base   string
	client *http.Client
}

func newCloudflare(token string) *cloudflare {
	return &cloudflare{
		token:  token,
		base:   "https://api.cloudflare.com/client/v4",
		client: &http.Client{Timeout: 30 * time.Second},
	}
}

func (c *cloudflare) Name() string { return "cloudflare" }

type cfEnvelope struct {
	Success bool `json:"success"`
	Errors  []struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"errors"`
	Result     json.RawMessage `json:"result"`
	ResultInfo *cfResultInfo   `json:"result_info"`
}

type cfResultInfo struct {
	Page       int `json:"page"`
	TotalPages int `json:"total_pages"`
	TotalCount int `json:"total_count"`
}

type cfRecord struct {
	Type    string `json:"type"`
	Name    string `json:"name"`
	Content string `json:"content"`
	TTL     int    `json:"ttl"`
	Proxied bool   `json:"proxied"`
	Comment string `json:"comment,omitempty"`
}

func (c *cloudflare) LookupZone(ctx context.Context, name string) (string, bool, error) {
	var zones []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	query := url.Values{"name": {name}, "per_page": {"50"}}
	if _, err := c.do(ctx, http.MethodGet, "/zones", query, nil, &zones); err != nil {
		return "", false, err
	}
	// The name filter matches exactly by default, but this does not rely on
	// it: a zone that is not exactly this name is not this zone.
	for _, z := range zones {
		if strings.EqualFold(z.Name, name) {
			return z.ID, true, nil
		}
	}
	return "", false, nil
}

func (c *cloudflare) Records(ctx context.Context, zoneID, name string) ([]Record, error) {
	var records []cfRecord
	query := url.Values{"name.exact": {name}, "per_page": {"100"}}
	info, err := c.do(ctx, http.MethodGet, "/zones/"+url.PathEscape(zoneID)+"/dns_records", query, nil, &records)
	if err != nil {
		return nil, err
	}
	// A hundred records at one name is not a deployment this toolkit made. It
	// is refused rather than paged through, because a plan built from the
	// first page of a longer list could miss the record that conflicts.
	if info != nil && info.TotalPages > 1 {
		return nil, fmt.Errorf("cloudflare holds %d records at %s, more than one page. Nothing is planned from a partial list", info.TotalCount, name)
	}
	out := make([]Record, 0, len(records))
	for _, r := range records {
		if !strings.EqualFold(strings.TrimSuffix(r.Name, "."), name) {
			continue
		}
		out = append(out, Record{Type: strings.ToUpper(r.Type), Name: r.Name, Content: r.Content, Proxied: r.Proxied})
	}
	return out, nil
}

// Create adds an unproxied record with automatic TTL. In Cloudflare's API a
// ttl of 1 means automatic.
func (c *cloudflare) Create(ctx context.Context, zoneID string, record Record) error {
	body := cfRecord{
		Type:    record.Type,
		Name:    record.Name,
		Content: record.Content,
		TTL:     1,
		Proxied: false,
		Comment: recordComment,
	}
	var created cfRecord
	_, err := c.do(ctx, http.MethodPost, "/zones/"+url.PathEscape(zoneID)+"/dns_records", nil, body, &created)
	return err
}

// do sends one request and decodes the envelope. The token travels only in
// the Authorization header. Every error it returns passes through redact, so
// that a provider echoing the token back, or a transport error quoting a
// request, cannot carry it into a terminal or a log.
func (c *cloudflare) do(ctx context.Context, method, path string, query url.Values, body, result any) (info *cfResultInfo, err error) {
	defer func() {
		if err != nil {
			err = errors.New(c.redact(err.Error()))
		}
	}()
	target := c.base + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, reader)
	if err != nil {
		return nil, fmt.Errorf("cloudflare: %s %s: %v", method, path, err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("cloudflare: %s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, fmt.Errorf("cloudflare: %s %s: reading the response: %v", method, path, err)
	}
	var envelope cfEnvelope
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, fmt.Errorf("cloudflare: %s %s: HTTP %d with a body that is not the API's JSON envelope", method, path, resp.StatusCode)
	}
	if !envelope.Success || resp.StatusCode/100 != 2 {
		var reasons []string
		for _, e := range envelope.Errors {
			reasons = append(reasons, fmt.Sprintf("[%d] %s", e.Code, e.Message))
		}
		if len(reasons) == 0 {
			reasons = append(reasons, "no reason given")
		}
		return nil, fmt.Errorf("cloudflare: %s %s: HTTP %d: %s", method, path, resp.StatusCode, strings.Join(reasons, "; "))
	}
	if result != nil && len(envelope.Result) > 0 {
		if err := json.Unmarshal(envelope.Result, result); err != nil {
			return nil, fmt.Errorf("cloudflare: %s %s: unexpected result shape: %v", method, path, err)
		}
	}
	return envelope.ResultInfo, nil
}

func (c *cloudflare) redact(s string) string {
	if c.token == "" {
		return s
	}
	return strings.ReplaceAll(s, c.token, "[token redacted]")
}
