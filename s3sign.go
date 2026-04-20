package s3mapping

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// s3Client performs SigV4-signed GET requests against a virtual-hosted S3 bucket.
// Credential resolution mirrors caddy-s3-transport: IAM → static → SDK default.
type s3Client struct {
	bucket     string
	region     string
	httpClient *http.Client
	signer     *v4.Signer

	minioCreds *credentials.Credentials
	awsConfig  *aws.Config
}

func newS3Client(bucket, region string, useIAM bool, accessKeyID, secretAccessKey string) (*s3Client, error) {
	if bucket == "" || region == "" {
		return nil, fmt.Errorf("s3 client: bucket and region are required")
	}

	c := &s3Client{
		bucket: bucket,
		region: region,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
		signer: v4.NewSigner(),
	}

	if !useIAM {
		if b, err := strconv.ParseBool(os.Getenv("S3_USE_IAM_PROVIDER")); err == nil && b {
			useIAM = true
		}
	}

	switch {
	case useIAM:
		c.minioCreds = credentials.NewIAM("")
	case accessKeyID != "" && secretAccessKey != "":
		c.minioCreds = credentials.NewStaticV4(accessKeyID, secretAccessKey, "")
	default:
		cfg, err := config.LoadDefaultConfig(context.Background(), config.WithRegion(region))
		if err != nil {
			return nil, fmt.Errorf("s3 client: loading AWS config: %w", err)
		}
		c.awsConfig = &cfg
	}

	return c, nil
}

func (c *s3Client) resolveCredentials(ctx context.Context) (aws.Credentials, error) {
	if c.minioCreds != nil {
		val, err := c.minioCreds.Get()
		if err != nil {
			return aws.Credentials{}, fmt.Errorf("get credentials: %w", err)
		}
		return aws.Credentials{
			AccessKeyID:     val.AccessKeyID,
			SecretAccessKey: val.SecretAccessKey,
			SessionToken:    val.SessionToken,
		}, nil
	}
	if c.awsConfig != nil {
		return c.awsConfig.Credentials.Retrieve(ctx)
	}
	return aws.Credentials{}, fmt.Errorf("no credential provider configured")
}

// getObject performs a SigV4-signed GET for the given object key.
func (c *s3Client) getObject(ctx context.Context, key string) (*http.Response, error) {
	u := &url.URL{
		Scheme: "https",
		Host:   fmt.Sprintf("%s.s3.%s.amazonaws.com", c.bucket, c.region),
		Path:   "/" + key,
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}

	host := u.Host
	req.Header.Set("Host", host)
	req.Host = host
	req.Header.Set("User-Agent", "Caddy-S3-MappingTransport/1.0")
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Accept-Encoding", "identity")
	req.Header.Set("x-amz-content-sha256", emptySHA256)

	creds, err := c.resolveCredentials(ctx)
	if err != nil {
		return nil, err
	}
	if creds.SessionToken != "" {
		req.Header.Set("x-amz-security-token", creds.SessionToken)
	}

	if err := c.signer.SignHTTP(ctx, creds, req, emptySHA256, "s3", c.region, time.Now()); err != nil {
		return nil, fmt.Errorf("sign request: %w", err)
	}

	return c.httpClient.Do(req)
}

var emptySHA256 = func() string {
	h := sha256.Sum256(nil)
	return hex.EncodeToString(h[:])
}()
