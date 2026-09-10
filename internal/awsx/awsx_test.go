package awsx

import (
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestSignatureAgainstPublishedExample rebuilds the canonical request from the
// Amazon S3 "GET Object" example and checks that the final signature matches
// the published value.
func TestSignatureAgainstPublishedExample(t *testing.T) {
	canonicalRequest := strings.Join([]string{
		"GET",
		"/test.txt",
		"",
		"host:examplebucket.s3.amazonaws.com\nrange:bytes=0-9\nx-amz-content-sha256:" + EmptyPayloadHash + "\nx-amz-date:20130524T000000Z\n",
		"host;range;x-amz-content-sha256;x-amz-date",
		EmptyPayloadHash,
	}, "\n")

	wantHash := "7344ae5b7ee6c3e7e6b0fe0640412a37625d1fbfff95c48bbb2dc43964946972"
	if got := hashHex([]byte(canonicalRequest)); got != wantHash {
		t.Fatalf("canonical request hash\n got %s\nwant %s", got, wantHash)
	}

	stringToSign := strings.Join([]string{
		signingAlgorithm,
		"20130524T000000Z",
		"20130524/us-east-1/s3/aws4_request",
		wantHash,
	}, "\n")

	key := hmacSHA256([]byte("AWS4wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"), "20130524")
	key = hmacSHA256(key, "us-east-1")
	key = hmacSHA256(key, "s3")
	key = hmacSHA256(key, "aws4_request")

	want := "f0e8bdb87c964420e857bd35b5d6ed310bd44f0170aba48dd91039c6036bdb41"
	if got := hex.EncodeToString(hmacSHA256(key, stringToSign)); got != want {
		t.Fatalf("signature\n got %s\nwant %s", got, want)
	}
}

// TestSignSetsExpectedHeaders checks the part that assembles a real request:
// which headers end up signed, and in which order.
func TestSignSetsExpectedHeaders(t *testing.T) {
	req, err := http.NewRequest(http.MethodPut, "https://bucket.s3.eu-central-1.amazonaws.com/", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.URL.Opaque = "/lockbox/db01/2026-01-02T03-04-05Z.sql.gz"
	req.Host = "bucket.s3.eu-central-1.amazonaws.com"
	req.Header.Set("x-amz-storage-class", "STANDARD_IA")
	req.Header.Set("Content-Type", "application/gzip")

	creds := Credentials{AccessKeyID: "AKIAEXAMPLE", SecretAccessKey: "secret"}
	sign(req, creds, "eu-central-1", "s3", EmptyPayloadHash, time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))

	auth := req.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "AWS4-HMAC-SHA256 Credential=AKIAEXAMPLE/20260102/eu-central-1/s3/aws4_request") {
		t.Fatalf("unexpected credential scope: %s", auth)
	}
	want := "SignedHeaders=content-type;host;x-amz-content-sha256;x-amz-date;x-amz-storage-class"
	if !strings.Contains(auth, want) {
		t.Fatalf("signed headers wrong\n got %s\nwant to contain %s", auth, want)
	}
	if req.Header.Get("X-Amz-Date") != "20260102T030405Z" {
		t.Fatalf("date header wrong: %s", req.Header.Get("X-Amz-Date"))
	}
}

func TestURIEncode(t *testing.T) {
	cases := []struct {
		in        string
		keepSlash bool
		want      string
	}{
		{"lockbox/db01/file.sql.gz", true, "lockbox/db01/file.sql.gz"},
		{"lockbox/db01/", false, "lockbox%2Fdb01%2F"},
		{"a b", false, "a%20b"},
		{"tilde~dot.dash-underscore_", false, "tilde~dot.dash-underscore_"},
		{"plus+sign", false, "plus%2Bsign"},
	}
	for _, c := range cases {
		if got := uriEncode(c.in, c.keepSlash); got != c.want {
			t.Errorf("uriEncode(%q, %v) = %q, want %q", c.in, c.keepSlash, got, c.want)
		}
	}
}

func TestCanonicalQuerySorts(t *testing.T) {
	got := canonicalQuery(map[string]string{
		"prefix":    "lockbox/db01/",
		"list-type": "2",
		"max-keys":  "1000",
	})
	want := "list-type=2&max-keys=1000&prefix=lockbox%2Fdb01%2F"
	if got != want {
		t.Fatalf("canonicalQuery\n got %s\nwant %s", got, want)
	}
}

func TestEndpointStyles(t *testing.T) {
	amazon := &Client{Region: "eu-central-1"}
	_, host, path := amazon.endpointFor("my-backup", "lockbox/db01/x.sql.gz")
	if host != "my-backup.s3.eu-central-1.amazonaws.com" || path != "/lockbox/db01/x.sql.gz" {
		t.Fatalf("virtual hosted style wrong: %s %s", host, path)
	}

	dotted := &Client{Region: "eu-central-1"}
	_, host, path = dotted.endpointFor("my.backup", "x.gz")
	if host != "s3.eu-central-1.amazonaws.com" || path != "/my.backup/x.gz" {
		t.Fatalf("dotted bucket should use path style: %s %s", host, path)
	}

	custom := &Client{Region: "eu-central-1", Endpoint: "https://minio.example.com:9000"}
	scheme, host, path := custom.endpointFor("my-backup", "x.gz")
	if scheme != "https" || host != "minio.example.com:9000" || path != "/my-backup/x.gz" {
		t.Fatalf("custom endpoint wrong: %s %s %s", scheme, host, path)
	}
}

func TestIsAccessDenied(t *testing.T) {
	if !IsAccessDenied(&APIError{StatusCode: 403, Code: "AccessDenied"}) {
		t.Error("403 AccessDenied should count as denied")
	}
	if IsAccessDenied(&APIError{StatusCode: 404, Code: "NoSuchKey"}) {
		t.Error("404 should not count as denied")
	}
}

// TestListObjectsAgainstFakeServer runs a real request through the whole
// stack against a local stand in for Amazon S3, which catches mistakes in the
// path, the query string and the answer parsing.
func TestListObjectsAgainstFakeServer(t *testing.T) {
	var gotPath, gotQuery, gotAuth string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/xml")
		io.WriteString(w, `<?xml version="1.0"?>
<ListBucketResult><IsTruncated>false</IsTruncated>
<Contents><Key>lockbox/db01/2026-01-02T03-04-05Z.sql.gz</Key><Size>1234</Size>
<LastModified>2026-01-02T03:04:06.000Z</LastModified><StorageClass>STANDARD_IA</StorageClass></Contents>
</ListBucketResult>`)
	}))
	defer server.Close()

	client := NewClient(Credentials{AccessKeyID: "AKIAEXAMPLE", SecretAccessKey: "secret"},
		"eu-central-1", server.URL)

	objects, err := client.ListObjects("my-backup", "lockbox/db01/")
	if err != nil {
		t.Fatal(err)
	}
	if len(objects) != 1 || objects[0].Size != 1234 {
		t.Fatalf("unexpected objects: %+v", objects)
	}
	if objects[0].Key != "lockbox/db01/2026-01-02T03-04-05Z.sql.gz" {
		t.Fatalf("key wrong: %s", objects[0].Key)
	}
	if gotPath != "/my-backup" {
		t.Fatalf("path style url wrong: %s", gotPath)
	}
	if !strings.Contains(gotQuery, "list-type=2") || !strings.Contains(gotQuery, "prefix=lockbox%2Fdb01%2F") {
		t.Fatalf("query wrong: %s", gotQuery)
	}
	if !strings.Contains(gotAuth, "AWS4-HMAC-SHA256") {
		t.Fatalf("request was not signed: %s", gotAuth)
	}
}

// TestErrorParsing checks that an Amazon refusal becomes a typed error the
// doctor command can recognise.
func TestErrorParsing(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		io.WriteString(w, `<?xml version="1.0"?><Error><Code>AccessDenied</Code><Message>Access Denied</Message></Error>`)
	}))
	defer server.Close()

	client := NewClient(Credentials{AccessKeyID: "A", SecretAccessKey: "B"}, "eu-central-1", server.URL)

	err := client.DeleteObject("my-backup", "probe")
	if err == nil {
		t.Fatal("expected a refusal")
	}
	if !IsAccessDenied(err) {
		t.Fatalf("should be recognised as access denied: %v", err)
	}
	if ErrorCode(err) != "AccessDenied" {
		t.Fatalf("error code wrong: %s", ErrorCode(err))
	}
}

// TestErrorShapes checks that both of Amazon's error layouts are understood.
// Amazon S3 puts Code at the root, Identity and Access Management nests it one
// level deeper, and missing that difference once turned a routine "user does
// not exist" answer into a fatal error.
func TestErrorShapes(t *testing.T) {
	s3Style := `<?xml version="1.0"?><Error><Code>AccessDenied</Code><Message>Access Denied</Message></Error>`
	iamStyle := `<?xml version="1.0"?><ErrorResponse xmlns="https://iam.amazonaws.com/doc/2010-05-08/">` +
		`<Error><Type>Sender</Type><Code>NoSuchEntity</Code>` +
		`<Message>The user with name x cannot be found.</Message></Error>` +
		`<RequestId>abc</RequestId></ErrorResponse>`

	cases := []struct {
		name string
		body string
		code int
		want string
	}{
		{"amazon s3", s3Style, 403, "AccessDenied"},
		{"identity and access management", iamStyle, 404, "NoSuchEntity"},
	}

	for _, c := range cases {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(c.code)
			io.WriteString(w, c.body)
		}))

		client := NewClient(Credentials{AccessKeyID: "A", SecretAccessKey: "B"}, "eu-central-1", server.URL)
		err := client.DeleteObject("bucket", "key")
		server.Close()

		if got := ErrorCode(err); got != c.want {
			t.Errorf("%s: error code = %q, want %q", c.name, got, c.want)
		}
	}
}
