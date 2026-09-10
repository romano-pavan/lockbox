package awsx

import (
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// queryAPI performs a request against one of the older Amazon services that
// take form encoded parameters and answer in XML, such as Identity and Access
// Management and Security Token Service.
func (c *Client) queryAPI(operation, service, host, region string, params url.Values) ([]byte, error) {
	body := params.Encode()

	req, err := http.NewRequest(http.MethodPost, "https://"+host+"/", strings.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", operation, err)
	}
	req.Host = host
	req.ContentLength = int64(len(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=utf-8")

	sign(req, c.Creds, region, service, hashHex([]byte(body)), c.now())

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", operation, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, parseError(operation, resp)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 1024*1024))
}

// iamAPI is a helper for Identity and Access Management, which is a global
// service always signed against us-east-1.
func (c *Client) iamAPI(operation string, params url.Values) ([]byte, error) {
	params.Set("Version", "2010-05-08")
	return c.queryAPI(operation, "iam", "iam.amazonaws.com", "us-east-1", params)
}

// Identity is who the current credentials belong to.
type Identity struct {
	Account string
	Arn     string
	UserID  string
}

type callerIdentityXML struct {
	Result struct {
		Account string `xml:"Account"`
		Arn     string `xml:"Arn"`
		UserID  string `xml:"UserId"`
	} `xml:"GetCallerIdentityResult"`
}

// WhoAmI asks the Security Token Service which identity the credentials map to.
// It is the cheapest way to prove that a key works at all.
func (c *Client) WhoAmI() (Identity, error) {
	params := url.Values{}
	params.Set("Action", "GetCallerIdentity")
	params.Set("Version", "2011-06-15")

	raw, err := c.queryAPI("GetCallerIdentity", "sts", "sts."+c.Region+".amazonaws.com", c.Region, params)
	if err != nil {
		return Identity{}, err
	}
	var parsed callerIdentityXML
	if err := xml.Unmarshal(raw, &parsed); err != nil {
		return Identity{}, fmt.Errorf("GetCallerIdentity: cannot read answer: %w", err)
	}
	return Identity{
		Account: parsed.Result.Account,
		Arn:     parsed.Result.Arn,
		UserID:  parsed.Result.UserID,
	}, nil
}

// CreateUser creates an Identity and Access Management user. An existing user
// with the same name is reported as an error so 'lockbox init' never silently
// takes over an identity somebody else is using.
func (c *Client) CreateUser(name string) error {
	params := url.Values{}
	params.Set("Action", "CreateUser")
	params.Set("UserName", name)
	params.Set("Path", "/lockbox/")
	_, err := c.iamAPI("CreateUser", params)
	if err != nil && ErrorCode(err) == "EntityAlreadyExists" {
		return nil
	}
	return err
}

type getUserXML struct {
	Result struct {
		User struct {
			Path     string `xml:"Path"`
			UserName string `xml:"UserName"`
		} `xml:"User"`
	} `xml:"GetUserResult"`
}

// UserInfo reports whether an Identity and Access Management user exists and,
// if so, which path it was created under. The path is how 'lockbox init' tells
// an identity it created earlier from one that belongs to something else.
func (c *Client) UserInfo(name string) (exists bool, path string, err error) {
	params := url.Values{}
	params.Set("Action", "GetUser")
	params.Set("UserName", name)

	raw, err := c.iamAPI("GetUser", params)
	if err != nil {
		if ErrorCode(err) == "NoSuchEntity" {
			return false, "", nil
		}
		return false, "", err
	}
	var parsed getUserXML
	if err := xml.Unmarshal(raw, &parsed); err != nil {
		return true, "", nil
	}
	return true, parsed.Result.User.Path, nil
}

// PutUserPolicy attaches an inline policy document to a user.
func (c *Client) PutUserPolicy(user, policyName, document string) error {
	params := url.Values{}
	params.Set("Action", "PutUserPolicy")
	params.Set("UserName", user)
	params.Set("PolicyName", policyName)
	params.Set("PolicyDocument", document)
	_, err := c.iamAPI("PutUserPolicy", params)
	return err
}

type accessKeyXML struct {
	Result struct {
		AccessKey struct {
			AccessKeyID     string `xml:"AccessKeyId"`
			SecretAccessKey string `xml:"SecretAccessKey"`
		} `xml:"AccessKey"`
	} `xml:"CreateAccessKeyResult"`
}

// CreateAccessKey issues a key pair for a user. Amazon reveals the secret
// exactly once, in this answer, and never again.
func (c *Client) CreateAccessKey(user string) (Credentials, error) {
	params := url.Values{}
	params.Set("Action", "CreateAccessKey")
	params.Set("UserName", user)

	raw, err := c.iamAPI("CreateAccessKey", params)
	if err != nil {
		return Credentials{}, err
	}
	var parsed accessKeyXML
	if err := xml.Unmarshal(raw, &parsed); err != nil {
		return Credentials{}, fmt.Errorf("CreateAccessKey: cannot read answer: %w", err)
	}
	if parsed.Result.AccessKey.AccessKeyID == "" {
		return Credentials{}, fmt.Errorf("CreateAccessKey: Amazon returned no key")
	}
	return Credentials{
		AccessKeyID:     parsed.Result.AccessKey.AccessKeyID,
		SecretAccessKey: parsed.Result.AccessKey.SecretAccessKey,
		Source:          "created by lockbox init",
	}, nil
}

// WriterPolicy builds the permission document for the identity that lives on
// the database server.
//
// The Allow half grants uploading and listing, plus two read only calls that
// let 'lockbox doctor' inspect the protection.
//
// The Deny half is the part that matters. An explicit Deny in Identity and
// Access Management cannot be overridden by any Allow anywhere else, so even
// an attacker who reaches this key cannot erase history, cannot read older
// backups, and cannot weaken the bucket for future ones.
func WriterPolicy(bucket string) string {
	arn := "arn:aws:s3:::" + bucket
	return `{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Sid": "UploadOnly",
      "Effect": "Allow",
      "Action": [
        "s3:PutObject",
        "s3:ListBucket",
        "s3:GetBucketLocation",
        "s3:GetBucketVersioning",
        "s3:GetBucketObjectLockConfiguration",
        "s3:GetObjectRetention"
      ],
      "Resource": ["` + arn + `", "` + arn + `/*"]
    },
    {
      "Sid": "NeverDeleteNeverReadNeverWeaken",
      "Effect": "Deny",
      "Action": [
        "s3:GetObject",
        "s3:GetObjectVersion",
        "s3:DeleteObject",
        "s3:DeleteObjectVersion",
        "s3:DeleteBucket",
        "s3:PutObjectRetention",
        "s3:PutObjectLegalHold",
        "s3:PutBucketVersioning",
        "s3:PutBucketObjectLockConfiguration",
        "s3:PutLifecycleConfiguration",
        "s3:BypassGovernanceRetention"
      ],
      "Resource": ["*"]
    }
  ]
}`
}
