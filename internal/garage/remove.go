package garage

import (
	"encoding/xml"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/paisans-software/paisans-stack/internal/deployment"
)

// What `app remove --delete-data` asks Garage, and the commands it sends.
// Every output format here is dxflrs/garage:v1.0.1's, read from
// src/garage/cli/util.rs (print_key_info, print_bucket_info) and
// src/garage/admin/bucket.rs (handle_delete_bucket).

// KeyBuckets is what `garage key info <id>` says about one key: whether it
// exists, and the buckets it is authorized on, by the 16 hex character bucket
// ID prefix the table prints.
type KeyBuckets struct {
	Absent  bool
	Buckets []string
}

// ReadKeyBuckets asks Garage which buckets a key is authorized on.
//
// `key info` prints the key's secret (print_key_info, "Secret key:"), so the
// output is read here and never returned, printed or put into an error: a
// failure names the key ID and nothing Garage answered. The authorized
// buckets are the rows under "Authorized buckets:", each a tab separated
// flags, global aliases, local aliases and `{:?}` of the bucket ID, which
// for Garage's FixedBytes32 is the first 8 bytes in hex (src/util/data.rs).
// The aliases columns may be empty, so only the last field of a row is read.
func ReadKeyBuckets(t Transport, d deployment.Deployment, keyID string) (KeyBuckets, error) {
	out, err := t.Run(Command(d) + " key info " + keyID)
	if err != nil {
		if strings.Contains(out, "0 matching keys") {
			return KeyBuckets{Absent: true}, nil
		}
		return KeyBuckets{}, fmt.Errorf("checking key %s on %s: garage key info failed (its output carries the key's secret and is not shown)", keyID, t.Describe())
	}
	return KeyBuckets{Buckets: parseKeyBuckets(out)}, nil
}

func parseKeyBuckets(out string) []string {
	var buckets []string
	in := false
	for _, raw := range strings.Split(ansi.ReplaceAllString(out, ""), "\n") {
		line := strings.TrimRight(raw, "\r ")
		if strings.HasPrefix(line, "Authorized buckets:") {
			in = true
			continue
		}
		if !in {
			continue
		}
		if strings.TrimSpace(line) == "" || (line[0] != ' ' && line[0] != '\t') {
			break
		}
		fields := strings.Fields(line)
		if id := fields[len(fields)-1]; isHex(id) {
			buckets = append(buckets, id)
		}
	}
	return buckets
}

func isHex(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !strings.ContainsRune("0123456789abcdef", r) {
			return false
		}
	}
	return true
}

// BucketState is what `garage bucket info` says about one bucket, for
// deciding whether it may be deleted.
type BucketState struct {
	// Absent is set when Garage has no such bucket.
	Absent bool
	// Parsed is set when every section below was found. A bucket whose
	// answer did not parse is kept: nothing about it is proven.
	Parsed bool
	// ID is the full bucket ID, from the "Bucket:" line.
	ID string
	// Aliases are its global aliases, LocalAliases how many key specific
	// aliases it has.
	Aliases      []string
	LocalAliases int
	// Keys are the IDs of every key authorized on it.
	Keys []string
	// Objects is its object count.
	Objects int
}

// ReadBucketState asks Garage about one bucket by name or ID prefix
// (admin_get_existing_matching_bucket, src/model/helper/bucket.rs).
func ReadBucketState(t Transport, d deployment.Deployment, bucket string) (BucketState, error) {
	out, err := t.Run(Command(d) + " bucket info " + bucket)
	gone, err := absent(out, err, "Bucket not found")
	if err != nil {
		return BucketState{}, fmt.Errorf("checking bucket %s on %s: %w", bucket, t.Describe(), err)
	}
	if gone {
		return BucketState{Absent: true}, nil
	}
	return parseBucketState(out), nil
}

func parseBucketState(out string) BucketState {
	var s BucketState
	var sawObjects, sawAliases, sawLocal, sawKeys bool
	section := ""
	for _, raw := range strings.Split(ansi.ReplaceAllString(out, ""), "\n") {
		line := strings.TrimRight(raw, "\r ")
		if line == "" {
			section = ""
			continue
		}
		indented := line[0] == ' ' || line[0] == '\t'
		if indented {
			fields := strings.Fields(line)
			switch section {
			case "aliases":
				s.Aliases = append(s.Aliases, fields[0])
			case "local":
				s.LocalAliases++
			case "keys":
				if len(fields) >= 2 {
					s.Keys = append(s.Keys, fields[1])
				}
			}
			continue
		}
		section = ""
		switch {
		case strings.HasPrefix(line, "Bucket:"):
			s.ID = strings.TrimSpace(strings.TrimPrefix(line, "Bucket:"))
		case strings.HasPrefix(line, "Objects:"):
			n, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "Objects:")))
			if err == nil {
				s.Objects, sawObjects = n, true
			}
		case strings.HasPrefix(line, "Global aliases:"):
			section, sawAliases = "aliases", true
		case strings.HasPrefix(line, "Key-specific aliases:"):
			section, sawLocal = "local", true
		case strings.HasPrefix(line, "Authorized keys:"):
			section, sawKeys = "keys", true
		}
	}
	s.Parsed = s.ID != "" && sawObjects && sawAliases && sawLocal && sawKeys
	return s
}

// DeleteBucketStep deletes an empty bucket by its global alias. Garage
// refuses a bucket that is not empty or has another alias, and deletes
// nothing without --yes (handle_delete_bucket, src/garage/admin/bucket.rs,
// v1.0.1); it removes every key's grant on it as it goes.
func DeleteBucketStep(d deployment.Deployment, bucket string) Step {
	return Step{
		Title:    "delete bucket " + bucket,
		Describe: fmt.Sprintf("delete the bucket %s from Garage", bucket),
		Command:  fmt.Sprintf("%s bucket delete --yes %s", Command(d), bucket),
	}
}

// S3Request is one signed request to a Garage node's S3 API with a query,
// for listing a bucket. S3Object addresses a single object.
type S3Request struct {
	Address string
	Bucket  string
	Query   url.Values
	KeyID   string
	Secret  string
	Region  string
}

// ListConfig is a curl configuration for ListObjectsV2 on the bucket,
// for CurlStdin.
func (r S3Request) ListConfig() string {
	var b strings.Builder
	fmt.Fprintf(&b, "url = \"http://%s:3900/%s?%s\"\n", r.Address, r.Bucket, r.Query.Encode())
	b.WriteString("request = \"GET\"\n")
	fmt.Fprintf(&b, "user = \"%s:%s\"\n", r.KeyID, r.Secret)
	fmt.Fprintf(&b, "aws-sigv4 = \"aws:amz:%s:s3\"\n", r.Region)
	b.WriteString("header = \"x-amz-content-sha256: UNSIGNED-PAYLOAD\"\n")
	return b.String()
}

// DeleteConfig is one curl configuration that deletes every named object,
// one DELETE each, separated by curl's `next` so one ssh round trip carries
// a whole page. A key is percent encoded per path segment, the form S3 signs.
func (r S3Request) DeleteConfig(keys []string) string {
	var b strings.Builder
	for i, key := range keys {
		if i > 0 {
			b.WriteString("next\n")
		}
		segments := strings.Split(key, "/")
		for j, s := range segments {
			segments[j] = url.PathEscape(s)
		}
		fmt.Fprintf(&b, "url = \"http://%s:3900/%s/%s\"\n", r.Address, r.Bucket, strings.Join(segments, "/"))
		b.WriteString("request = \"DELETE\"\n")
		fmt.Fprintf(&b, "user = \"%s:%s\"\n", r.KeyID, r.Secret)
		fmt.Fprintf(&b, "aws-sigv4 = \"aws:amz:%s:s3\"\n", r.Region)
		b.WriteString("header = \"x-amz-content-sha256: UNSIGNED-PAYLOAD\"\n")
	}
	return b.String()
}

// Redact removes the secret from text a request produced.
func (r S3Request) Redact(text string) string { return redact(text, r.Secret) }

// ListPage is one page of ListObjectsV2.
type ListPage struct {
	Keys      []string
	Truncated bool
}

// ParseListPage reads a ListObjectsV2 response body.
func ParseListPage(body string) (ListPage, error) {
	var doc struct {
		XMLName     xml.Name `xml:"ListBucketResult"`
		IsTruncated bool     `xml:"IsTruncated"`
		Contents    []struct {
			Key string `xml:"Key"`
		} `xml:"Contents"`
	}
	if err := xml.Unmarshal([]byte(strings.TrimSpace(body)), &doc); err != nil {
		return ListPage{}, fmt.Errorf("the listing is not a ListBucketResult: %v", err)
	}
	var p ListPage
	p.Truncated = doc.IsTruncated
	for _, c := range doc.Contents {
		p.Keys = append(p.Keys, c.Key)
	}
	return p, nil
}
