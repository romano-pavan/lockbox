package awsx

import (
	"bytes"
	"crypto/md5"
	"encoding/base64"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Object is one stored backup as returned by ListObjects.
type Object struct {
	Key          string
	Size         int64
	LastModified time.Time
	StorageClass string
}

// endpointFor builds the host and canonical path for a bucket and key.
// Virtual hosted style is used against real Amazon S3, path style against a
// custom endpoint or a bucket whose name contains a dot, because such names
// break certificate matching.
func (c *Client) endpointFor(bucket, key string) (scheme, host, canonicalPath string) {
	escapedKey := uriEncode(key, true)

	if c.Endpoint == "" {
		if strings.Contains(bucket, ".") {
			host = "s3." + c.Region + ".amazonaws.com"
			return "https", host, "/" + bucket + "/" + escapedKey
		}
		host = bucket + ".s3." + c.Region + ".amazonaws.com"
		if key == "" {
			return "https", host, "/"
		}
		return "https", host, "/" + escapedKey
	}

	custom := c.Endpoint
	if !strings.Contains(custom, "://") {
		custom = "https://" + custom
	}
	parsed, err := url.Parse(custom)
	if err != nil {
		return "https", c.Endpoint, "/" + bucket + "/" + escapedKey
	}
	scheme = parsed.Scheme
	if scheme == "" {
		scheme = "https"
	}
	path := "/" + bucket
	if key != "" {
		path += "/" + escapedKey
	}
	return scheme, parsed.Host, path
}

// s3send signs and performs one Amazon S3 request. The caller owns the
// response body when the call succeeds.
func (c *Client) s3send(operation, method, bucket, key string, query, headers map[string]string,
	body io.Reader, size int64, payloadHash string) (*http.Response, error) {

	scheme, host, path := c.endpointFor(bucket, key)

	req, err := http.NewRequest(method, scheme+"://"+host+"/", body)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", operation, err)
	}
	// Opaque holds the already encoded path, so Go sends exactly the bytes that
	// were signed instead of re-encoding them on the way out.
	req.URL.Opaque = path
	req.URL.RawQuery = canonicalQuery(query)
	req.Host = host
	req.ContentLength = size

	for name, value := range headers {
		req.Header.Set(name, value)
	}

	sign(req, c.Creds, c.Region, "s3", payloadHash, c.now())

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", operation, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		defer resp.Body.Close()
		return nil, parseError(operation, resp)
	}
	return resp, nil
}

// s3sendBytes is the common case: a small body held in memory, response discarded.
func (c *Client) s3sendBytes(operation, method, bucket, key string, query, headers map[string]string, body []byte) error {
	if headers == nil {
		headers = map[string]string{}
	}
	if len(body) > 0 {
		sum := md5.Sum(body)
		headers["Content-MD5"] = base64.StdEncoding.EncodeToString(sum[:])
		headers["Content-Type"] = "application/xml"
	}
	resp, err := c.s3send(operation, method, bucket, key, query, headers,
		bytes.NewReader(body), int64(len(body)), hashHex(body))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	return nil
}

// --- bucket creation and hardening, used by 'lockbox init' -------------------

// CreateBucket creates a bucket with Object Lock enabled.
//
// Since November 2023 Object Lock can also be turned on for an existing bucket
// with PutObjectLockConfiguration, provided versioning is on. lockbox still
// asks for it at creation because that is the one moment where nothing has been
// stored yet: enabling it later does not retroactively lock objects that are
// already in the bucket.
func (c *Client) CreateBucket(bucket string) error {
	var body []byte
	// us-east-1 is the one region that must not be named in the request.
	if c.Region != "us-east-1" {
		body = []byte(fmt.Sprintf(
			`<CreateBucketConfiguration xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><LocationConstraint>%s</LocationConstraint></CreateBucketConfiguration>`,
			c.Region))
	}
	headers := map[string]string{"x-amz-bucket-object-lock-enabled": "true"}
	return c.s3sendBytes("CreateBucket", http.MethodPut, bucket, "", nil, headers, body)
}

// HeadBucket reports whether the bucket exists and is reachable with the
// current credentials.
func (c *Client) HeadBucket(bucket string) error {
	resp, err := c.s3send("HeadBucket", http.MethodHead, bucket, "", nil, nil, nil, 0, EmptyPayloadHash)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// PutBucketVersioning enables versioning, which Object Lock requires.
func (c *Client) PutBucketVersioning(bucket string) error {
	body := []byte(`<VersioningConfiguration xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Status>Enabled</Status></VersioningConfiguration>`)
	return c.s3sendBytes("PutBucketVersioning", http.MethodPut, bucket, "",
		map[string]string{"versioning": ""}, nil, body)
}

// PutPublicAccessBlock closes every public access route.
func (c *Client) PutPublicAccessBlock(bucket string) error {
	body := []byte(`<PublicAccessBlockConfiguration xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><BlockPublicAcls>true</BlockPublicAcls><IgnorePublicAcls>true</IgnorePublicAcls><BlockPublicPolicy>true</BlockPublicPolicy><RestrictPublicBuckets>true</RestrictPublicBuckets></PublicAccessBlockConfiguration>`)
	return c.s3sendBytes("PutPublicAccessBlock", http.MethodPut, bucket, "",
		map[string]string{"publicAccessBlock": ""}, nil, body)
}

// PutBucketEncryption turns on server side encryption with Amazon managed keys.
func (c *Client) PutBucketEncryption(bucket string) error {
	body := []byte(`<ServerSideEncryptionConfiguration xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Rule><ApplyServerSideEncryptionByDefault><SSEAlgorithm>AES256</SSEAlgorithm></ApplyServerSideEncryptionByDefault><BucketKeyEnabled>true</BucketKeyEnabled></Rule></ServerSideEncryptionConfiguration>`)
	return c.s3sendBytes("PutBucketEncryption", http.MethodPut, bucket, "",
		map[string]string{"encryption": ""}, nil, body)
}

// PutObjectLockConfiguration sets the default retention applied to every new
// object. Using a bucket wide default instead of per object headers means the
// upload key never needs the s3:PutObjectRetention permission.
func (c *Client) PutObjectLockConfiguration(bucket, mode string, days int) error {
	body := []byte(fmt.Sprintf(
		`<ObjectLockConfiguration xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><ObjectLockEnabled>Enabled</ObjectLockEnabled><Rule><DefaultRetention><Mode>%s</Mode><Days>%d</Days></DefaultRetention></Rule></ObjectLockConfiguration>`,
		mode, days))
	return c.s3sendBytes("PutObjectLockConfiguration", http.MethodPut, bucket, "",
		map[string]string{"object-lock": ""}, nil, body)
}

// ObjectLockInfo is the bucket wide default retention, as read back from Amazon.
type ObjectLockInfo struct {
	Enabled bool
	Mode    string
	Days    int
}

type objectLockConfigXML struct {
	ObjectLockEnabled string `xml:"ObjectLockEnabled"`
	Rule              struct {
		DefaultRetention struct {
			Mode  string `xml:"Mode"`
			Days  int    `xml:"Days"`
			Years int    `xml:"Years"`
		} `xml:"DefaultRetention"`
	} `xml:"Rule"`
}

// GetObjectLockConfiguration reads back what protection the bucket really has.
func (c *Client) GetObjectLockConfiguration(bucket string) (ObjectLockInfo, error) {
	resp, err := c.s3send("GetObjectLockConfiguration", http.MethodGet, bucket, "",
		map[string]string{"object-lock": ""}, nil, nil, 0, EmptyPayloadHash)
	if err != nil {
		return ObjectLockInfo{}, err
	}
	defer resp.Body.Close()

	var parsed objectLockConfigXML
	if err := xml.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return ObjectLockInfo{}, fmt.Errorf("GetObjectLockConfiguration: cannot read answer: %w", err)
	}
	info := ObjectLockInfo{
		Enabled: strings.EqualFold(parsed.ObjectLockEnabled, "Enabled"),
		Mode:    parsed.Rule.DefaultRetention.Mode,
		Days:    parsed.Rule.DefaultRetention.Days,
	}
	if info.Days == 0 && parsed.Rule.DefaultRetention.Years > 0 {
		info.Days = parsed.Rule.DefaultRetention.Years * 365
	}
	return info, nil
}

// PutBucketLifecycle expires objects once their lock has run out, so old
// backups stop costing money. Amazon silently keeps objects that are still
// locked, so the grace period only has to cover clock differences.
func (c *Client) PutBucketLifecycle(bucket string, retentionDays int) error {
	expire := retentionDays + 5
	body := []byte(fmt.Sprintf(
		`<LifecycleConfiguration xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Rule><ID>lockbox-expire-after-retention</ID><Status>Enabled</Status><Filter></Filter><Expiration><Days>%d</Days></Expiration><NoncurrentVersionExpiration><NoncurrentDays>%d</NoncurrentDays></NoncurrentVersionExpiration><AbortIncompleteMultipartUpload><DaysAfterInitiation>3</DaysAfterInitiation></AbortIncompleteMultipartUpload></Rule></LifecycleConfiguration>`,
		expire, expire))
	return c.s3sendBytes("PutBucketLifecycleConfiguration", http.MethodPut, bucket, "",
		map[string]string{"lifecycle": ""}, nil, body)
}

// --- objects -----------------------------------------------------------------

// PutObject uploads one object.
//
// payloadHash is the SHA-256 of the body in lowercase hexadecimal, used for the
// request signature. contentMD5 is the same body's MD5 digest in base64, which
// a bucket with Object Lock enabled refuses the upload without. Both are
// computed while the dump is being written, so the file is hashed once and sent
// once.
func (c *Client) PutObject(bucket, key string, body io.Reader, size int64, payloadHash, contentMD5, storageClass string) error {
	headers := map[string]string{
		"Content-Type":      "application/gzip",
		"x-amz-meta-sha256": payloadHash,
	}
	if contentMD5 != "" {
		headers["Content-MD5"] = contentMD5
	}
	if storageClass != "" {
		headers["x-amz-storage-class"] = storageClass
	}
	resp, err := c.s3send("PutObject", http.MethodPut, bucket, key, nil, headers, body, size, payloadHash)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	return nil
}

type listResultXML struct {
	IsTruncated           bool   `xml:"IsTruncated"`
	NextContinuationToken string `xml:"NextContinuationToken"`
	Contents              []struct {
		Key          string    `xml:"Key"`
		Size         int64     `xml:"Size"`
		LastModified time.Time `xml:"LastModified"`
		StorageClass string    `xml:"StorageClass"`
	} `xml:"Contents"`
}

// ListObjects returns every object under a prefix, oldest first. It uses
// ListObjectsV2 rather than HeadObject because the restricted upload key
// deliberately lacks the s3:GetObject permission that HeadObject requires.
func (c *Client) ListObjects(bucket, prefix string) ([]Object, error) {
	var out []Object
	token := ""

	for {
		query := map[string]string{"list-type": "2", "prefix": prefix, "max-keys": "1000"}
		if token != "" {
			query["continuation-token"] = token
		}
		resp, err := c.s3send("ListObjectsV2", http.MethodGet, bucket, "", query, nil, nil, 0, EmptyPayloadHash)
		if err != nil {
			return nil, err
		}
		var parsed listResultXML
		err = xml.NewDecoder(resp.Body).Decode(&parsed)
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("ListObjectsV2: cannot read answer: %w", err)
		}
		for _, item := range parsed.Contents {
			out = append(out, Object{
				Key:          item.Key,
				Size:         item.Size,
				LastModified: item.LastModified,
				StorageClass: item.StorageClass,
			})
		}
		if !parsed.IsTruncated || parsed.NextContinuationToken == "" {
			break
		}
		token = parsed.NextContinuationToken
	}
	return out, nil
}

// StatObject returns the stored size of one exact key, or an error if absent.
func (c *Client) StatObject(bucket, key string) (Object, error) {
	objects, err := c.ListObjects(bucket, key)
	if err != nil {
		return Object{}, err
	}
	for _, obj := range objects {
		if obj.Key == key {
			return obj, nil
		}
	}
	return Object{}, fmt.Errorf("object %s is not in the bucket", key)
}

// GetObject opens an object for reading. The caller must close the reader.
// This needs the s3:GetObject permission, which the upload key does not have,
// so restores run with an administrator key.
func (c *Client) GetObject(bucket, key string) (io.ReadCloser, int64, error) {
	resp, err := c.s3send("GetObject", http.MethodGet, bucket, key, nil, nil, nil, 0, EmptyPayloadHash)
	if err != nil {
		return nil, 0, err
	}
	return resp.Body, resp.ContentLength, nil
}

// DeleteObject is never used to delete a real backup. The 'doctor' command
// calls it against a name that does not exist and expects to be refused,
// which is how it proves the protection is genuinely in place.
func (c *Client) DeleteObject(bucket, key string) error {
	resp, err := c.s3send("DeleteObject", http.MethodDelete, bucket, key, nil, nil, nil, 0, EmptyPayloadHash)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	return nil
}

type retentionXML struct {
	Mode            string `xml:"Mode"`
	RetainUntilDate string `xml:"RetainUntilDate"`
}

// GetObjectRetention reports when an object stops being immutable.
func (c *Client) GetObjectRetention(bucket, key string) (mode string, until time.Time, err error) {
	resp, err := c.s3send("GetObjectRetention", http.MethodGet, bucket, key,
		map[string]string{"retention": ""}, nil, nil, 0, EmptyPayloadHash)
	if err != nil {
		return "", time.Time{}, err
	}
	defer resp.Body.Close()

	var parsed retentionXML
	if err := xml.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return "", time.Time{}, fmt.Errorf("GetObjectRetention: cannot read answer: %w", err)
	}
	until, perr := time.Parse(time.RFC3339, parsed.RetainUntilDate)
	if perr != nil {
		return parsed.Mode, time.Time{}, nil
	}
	return parsed.Mode, until, nil
}

// FormatSize prints a byte count the way a person reads it.
func FormatSize(bytes int64) string {
	const unit = 1024
	if bytes < unit {
		return strconv.FormatInt(bytes, 10) + " B"
	}
	div, exp := int64(unit), 0
	for n := bytes / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(bytes)/float64(div), "KMGTPE"[exp])
}
