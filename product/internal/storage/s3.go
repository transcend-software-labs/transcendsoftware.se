package storage

import (
	"context"
	"io"
	"net/url"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// S3 is an S3-compatible object store (MinIO in dev, Fly Tigris in prod).
type S3 struct {
	client *minio.Client
	bucket string
}

// NewS3Params configures an S3-compatible store.
type NewS3Params struct {
	Endpoint  string // host:port, e.g. localhost:9000 (dev) or t3.storage.dev (prod)
	AccessKey string
	SecretKey string
	Bucket    string
	Region    string
	UseSSL    bool
}

// NewS3 connects to an S3-compatible backend and ensures the bucket exists.
func NewS3(p NewS3Params) (*S3, error) {
	cl, err := minio.New(p.Endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(p.AccessKey, p.SecretKey, ""),
		Secure: p.UseSSL,
		Region: p.Region,
	})
	if err != nil {
		return nil, err
	}
	s := &S3{client: cl, bucket: p.Bucket}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	ok, err := cl.BucketExists(ctx, p.Bucket)
	if err != nil {
		return nil, err
	}
	if !ok {
		if err := cl.MakeBucket(ctx, p.Bucket, minio.MakeBucketOptions{Region: p.Region}); err != nil {
			return nil, err
		}
	}
	return s, nil
}

func (s *S3) Put(ctx context.Context, key, contentType string, r io.Reader, size int64) error {
	_, err := s.client.PutObject(ctx, s.bucket, key, r, size, minio.PutObjectOptions{ContentType: contentType})
	return err
}

func (s *S3) Get(ctx context.Context, key string) ([]byte, error) {
	obj, err := s.client.GetObject(ctx, s.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, err
	}
	defer obj.Close()
	return io.ReadAll(obj)
}

func (s *S3) PresignGet(ctx context.Context, key string, expiry time.Duration) (string, error) {
	u, err := s.client.PresignedGetObject(ctx, s.bucket, key, expiry, url.Values{})
	if err != nil {
		return "", err
	}
	return u.String(), nil
}

func (s *S3) PresignPut(ctx context.Context, key string, expiry time.Duration) (string, error) {
	u, err := s.client.PresignedPutObject(ctx, s.bucket, key, expiry)
	if err != nil {
		return "", err
	}
	return u.String(), nil
}
