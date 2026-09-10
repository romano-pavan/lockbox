// Package awsx speaks to Amazon Web Services using nothing but the Go standard
// library. It implements Signature Version 4 request signing, credential
// discovery and the handful of Amazon S3 and Amazon Identity and Access
// Management calls that lockbox needs.
package awsx

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	signingAlgorithm = "AWS4-HMAC-SHA256"

	// EmptyPayloadHash is the SHA-256 of an empty body, required on every
	// request that carries no content.
	EmptyPayloadHash = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

	// DefaultCredentialsPath is where 'lockbox init' writes the restricted
	// upload key it creates.
	DefaultCredentialsPath = "/etc/lockbox/credentials"
)

// Credentials is one Amazon Web Services access key pair.
type Credentials struct {
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string
	Source          string // human readable origin, shown by 'lockbox doctor'
}

// LoadCredentials looks for credentials in the order that causes the least
// surprise: environment first, then the lockbox specific file, then the shared
// file used by every other Amazon tool.
func LoadCredentials() (Credentials, error) {
	if id := os.Getenv("AWS_ACCESS_KEY_ID"); id != "" {
		secret := os.Getenv("AWS_SECRET_ACCESS_KEY")
		if secret == "" {
			return Credentials{}, fmt.Errorf("AWS_ACCESS_KEY_ID is set but AWS_SECRET_ACCESS_KEY is not")
		}
		return Credentials{
			AccessKeyID:     id,
			SecretAccessKey: secret,
			SessionToken:    os.Getenv("AWS_SESSION_TOKEN"),
			Source:          "environment variables",
		}, nil
	}

	paths := []string{}
	if p := os.Getenv("LOCKBOX_CREDENTIALS"); p != "" {
		paths = append(paths, p)
	} else {
		paths = append(paths, DefaultCredentialsPath)
	}
	if p := os.Getenv("AWS_SHARED_CREDENTIALS_FILE"); p != "" {
		paths = append(paths, p)
	} else if home, err := os.UserHomeDir(); err == nil {
		paths = append(paths, filepath.Join(home, ".aws", "credentials"))
	}

	profile := os.Getenv("AWS_PROFILE")
	if profile == "" {
		profile = "default"
	}

	for _, p := range paths {
		creds, err := credentialsFromFile(p, profile)
		if err == nil {
			return creds, nil
		}
	}
	return Credentials{}, fmt.Errorf(
		"no credentials found: set AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY, or create %s",
		paths[0])
}

func credentialsFromFile(path, profile string) (Credentials, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Credentials{}, err
	}
	section := ""
	found := Credentials{Source: path}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = strings.TrimSpace(line[1 : len(line)-1])
			continue
		}
		if section != profile {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		switch key {
		case "aws_access_key_id":
			found.AccessKeyID = value
		case "aws_secret_access_key":
			found.SecretAccessKey = value
		case "aws_session_token":
			found.SessionToken = value
		}
	}
	if found.AccessKeyID == "" || found.SecretAccessKey == "" {
		return Credentials{}, fmt.Errorf("profile %q not found in %s", profile, path)
	}
	return found, nil
}

// RenderCredentialsFile returns the contents of a shared credentials file.
func RenderCredentialsFile(c Credentials, region string) string {
	return fmt.Sprintf("[default]\naws_access_key_id = %s\naws_secret_access_key = %s\nregion = %s\n",
		c.AccessKeyID, c.SecretAccessKey, region)
}

// Client performs signed requests against one region.
type Client struct {
	Creds    Credentials
	Region   string
	Endpoint string // empty means real Amazon S3
	HTTP     *http.Client
	Now      func() time.Time // overridable for tests
}

// NewClient builds a client with sensible timeouts.
func NewClient(creds Credentials, region, endpoint string) *Client {
	return &Client{
		Creds:    creds,
		Region:   region,
		Endpoint: endpoint,
		HTTP:     &http.Client{Timeout: 30 * time.Minute},
		Now:      time.Now,
	}
}

func (c *Client) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

// APIError is a non 2xx answer from Amazon, with the fields that matter.
type APIError struct {
	StatusCode int
	Code       string
	Message    string
	Operation  string
}

func (e *APIError) Error() string {
	if e.Code == "" {
		return fmt.Sprintf("%s failed with HTTP status %d", e.Operation, e.StatusCode)
	}
	return fmt.Sprintf("%s failed: %s (%s)", e.Operation, e.Message, e.Code)
}

// IsAccessDenied reports whether an error is Amazon refusing permission. The
// 'doctor' command relies on this: a refusal is the expected, correct answer
// when it probes whether the upload key can delete or read objects.
func IsAccessDenied(err error) bool {
	var apiErr *APIError
	if !asAPIError(err, &apiErr) {
		return false
	}
	if apiErr.StatusCode == 403 {
		return true
	}
	switch apiErr.Code {
	case "AccessDenied", "AccessDeniedException", "UnauthorizedOperation":
		return true
	}
	return false
}

// StatusCode returns the HTTP status of an API error, or zero.
func StatusCode(err error) int {
	var apiErr *APIError
	if asAPIError(err, &apiErr) {
		return apiErr.StatusCode
	}
	return 0
}

// ErrorCode returns the Amazon error code of an API error, or an empty string.
func ErrorCode(err error) string {
	var apiErr *APIError
	if asAPIError(err, &apiErr) {
		return apiErr.Code
	}
	return ""
}

func asAPIError(err error, target **APIError) bool {
	for err != nil {
		if e, ok := err.(*APIError); ok {
			*target = e
			return true
		}
		unwrapper, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = unwrapper.Unwrap()
	}
	return false
}

// xmlError covers both shapes Amazon uses for errors. Amazon S3 answers with
// <Error> as the root element, while Identity and Access Management and the
// Security Token Service wrap the same fields inside <ErrorResponse>.
type xmlError struct {
	Code    string `xml:"Code"`
	Message string `xml:"Message"`
	Nested  struct {
		Code    string `xml:"Code"`
		Message string `xml:"Message"`
	} `xml:"Error"`
}

// parseError turns a failed response into an APIError.
func parseError(operation string, resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	apiErr := &APIError{StatusCode: resp.StatusCode, Operation: operation}

	var parsed xmlError
	if err := xml.Unmarshal(body, &parsed); err == nil {
		apiErr.Code = parsed.Code
		apiErr.Message = parsed.Message
		if apiErr.Code == "" {
			apiErr.Code = parsed.Nested.Code
			apiErr.Message = parsed.Nested.Message
		}
	}
	if apiErr.Message == "" {
		text := strings.TrimSpace(string(body))
		if len(text) > 300 {
			text = text[:300]
		}
		apiErr.Message = text
	}
	return apiErr
}

// --- Signature Version 4 -----------------------------------------------------

// sign adds the Authorization header to a prepared request. It signs the host
// header, every x-amz-* header, and content-type and content-md5 when present.
func sign(req *http.Request, creds Credentials, region, service, payloadHash string, now time.Time) {
	now = now.UTC()
	amzDate := now.Format("20060102T150405Z")
	dateStamp := now.Format("20060102")

	req.Header.Set("X-Amz-Date", amzDate)
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)
	if creds.SessionToken != "" {
		req.Header.Set("X-Amz-Security-Token", creds.SessionToken)
	}

	host := req.Host
	if host == "" {
		host = req.URL.Host
	}

	// Collect the headers that go into the signature.
	signed := map[string]string{"host": host}
	for name, values := range req.Header {
		lower := strings.ToLower(name)
		if lower == "x-amz-content-sha256" || strings.HasPrefix(lower, "x-amz-") ||
			lower == "content-type" || lower == "content-md5" {
			signed[lower] = strings.TrimSpace(strings.Join(values, ","))
		}
	}
	names := make([]string, 0, len(signed))
	for name := range signed {
		names = append(names, name)
	}
	sort.Strings(names)

	var canonicalHeaders strings.Builder
	for _, name := range names {
		canonicalHeaders.WriteString(name)
		canonicalHeaders.WriteString(":")
		canonicalHeaders.WriteString(signed[name])
		canonicalHeaders.WriteString("\n")
	}
	signedHeaders := strings.Join(names, ";")

	canonicalURI := req.URL.Opaque
	if canonicalURI == "" {
		canonicalURI = req.URL.EscapedPath()
		if canonicalURI == "" {
			canonicalURI = "/"
		}
	}

	canonicalRequest := strings.Join([]string{
		req.Method,
		canonicalURI,
		req.URL.RawQuery,
		canonicalHeaders.String(),
		signedHeaders,
		payloadHash,
	}, "\n")

	scope := strings.Join([]string{dateStamp, region, service, "aws4_request"}, "/")
	stringToSign := strings.Join([]string{
		signingAlgorithm,
		amzDate,
		scope,
		hashHex([]byte(canonicalRequest)),
	}, "\n")

	key := hmacSHA256([]byte("AWS4"+creds.SecretAccessKey), dateStamp)
	key = hmacSHA256(key, region)
	key = hmacSHA256(key, service)
	key = hmacSHA256(key, "aws4_request")
	signature := hex.EncodeToString(hmacSHA256(key, stringToSign))

	req.Header.Set("Authorization", fmt.Sprintf("%s Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		signingAlgorithm, creds.AccessKeyID, scope, signedHeaders, signature))
}

func hmacSHA256(key []byte, data string) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(data))
	return mac.Sum(nil)
}

func hashHex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// uriEncode percent encodes a string per RFC 3986, which is what Amazon
// expects in canonical requests. Slashes survive inside paths only.
func uriEncode(s string, keepSlash bool) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		ch := s[i]
		switch {
		case ch >= 'A' && ch <= 'Z', ch >= 'a' && ch <= 'z', ch >= '0' && ch <= '9',
			ch == '-', ch == '_', ch == '.', ch == '~':
			b.WriteByte(ch)
		case ch == '/' && keepSlash:
			b.WriteByte('/')
		default:
			fmt.Fprintf(&b, "%%%02X", ch)
		}
	}
	return b.String()
}

// canonicalQuery builds the sorted, encoded query string Amazon signs.
func canonicalQuery(params map[string]string) string {
	if len(params) == 0 {
		return ""
	}
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, uriEncode(k, false)+"="+uriEncode(params[k], false))
	}
	return strings.Join(parts, "&")
}
