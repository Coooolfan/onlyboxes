package objectstore

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

type Config struct {
	Endpoint   string
	Region     string
	BucketName string
	AccessKey  string
	SecretKey  string
}

type Store struct {
	bucketName string
	presign    *s3.PresignClient
}

func New(cfg Config) (*Store, error) {
	endpoint := strings.TrimSpace(cfg.Endpoint)
	region := strings.TrimSpace(cfg.Region)
	bucketName := strings.TrimSpace(cfg.BucketName)
	accessKey := strings.TrimSpace(cfg.AccessKey)
	secretKey := strings.TrimSpace(cfg.SecretKey)

	missing := make([]string, 0, 5)
	if endpoint == "" {
		missing = append(missing, "endpoint")
	}
	if region == "" {
		missing = append(missing, "region")
	}
	if bucketName == "" {
		missing = append(missing, "bucket_name")
	}
	if accessKey == "" {
		missing = append(missing, "access_key")
	}
	if secretKey == "" {
		missing = append(missing, "secret_key")
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("objectstore config is incomplete: missing %s", strings.Join(missing, ", "))
	}

	parsedEndpoint, err := url.Parse(endpoint)
	if err != nil || parsedEndpoint.Scheme == "" || parsedEndpoint.Host == "" {
		return nil, errors.New("objectstore endpoint must be a valid absolute URL")
	}

	awsCfg := aws.Config{
		Region:      region,
		Credentials: aws.NewCredentialsCache(credentials.NewStaticCredentialsProvider(accessKey, secretKey, "")),
	}
	client := s3.NewFromConfig(awsCfg, func(opts *s3.Options) {
		opts.BaseEndpoint = aws.String(endpoint)
	})

	return &Store{
		bucketName: bucketName,
		presign:    s3.NewPresignClient(client),
	}, nil
}

func (s *Store) PresignUpload(ctx context.Context, objectKey string, expiresIn time.Duration) (string, error) {
	if s == nil || s.presign == nil {
		return "", errors.New("objectstore is unavailable")
	}
	key := strings.TrimSpace(objectKey)
	if key == "" {
		return "", errors.New("object key is required")
	}
	if expiresIn <= 0 {
		return "", errors.New("presign expiry must be positive")
	}

	request, err := s.presign.PresignPutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(s.bucketName),
		Key:    aws.String(key),
	}, func(opts *s3.PresignOptions) {
		opts.Expires = expiresIn
	})
	if err != nil {
		return "", fmt.Errorf("presign upload: %w", err)
	}
	return request.URL, nil
}

func (s *Store) PresignDownload(ctx context.Context, objectKey string, expiresIn time.Duration) (string, error) {
	if s == nil || s.presign == nil {
		return "", errors.New("objectstore is unavailable")
	}
	key := strings.TrimSpace(objectKey)
	if key == "" {
		return "", errors.New("object key is required")
	}
	if expiresIn <= 0 {
		return "", errors.New("presign expiry must be positive")
	}

	request, err := s.presign.PresignGetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucketName),
		Key:    aws.String(key),
	}, func(opts *s3.PresignOptions) {
		opts.Expires = expiresIn
	})
	if err != nil {
		return "", fmt.Errorf("presign download: %w", err)
	}
	return request.URL, nil
}
