/*
Package s3 provides an abstraction layer for file management on S3 compatible
storage. Minio's go library is used to provide basic S3 compatibility.

Features

S3 supports following features:

    - listing files inside a bucket
    - creating new files
    - reading files
    - encrypting all files using an external key provider

Encryption

To support encryption s3 requires an external key provider that can provide the
storage correct key for the current bucket / user ID.

Storing metadata

Metadata is stored inside the file name. The end file name on S3 storage will
look like this

	FILENAME.VERSION.OPERATION.TIMESTAMP.CHECKSUM.ARCHETYPE
	-- 40 --.- 1-40-.--- 1 ---.-- 13 ---.-- 44 --.--- * ---

Filenames on S3 are limited to around 1024 bytes meaning that the last archetype
value can be up to 886 characters long.

@TODO How to add new values to file name?
*/
package s3

//go:generate ../../bin/mockgen.sh storage/s3 Storage,KeyProvider,Minio $GOFILE

import (
	"context"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"

	"github.com/iryonetwork/wwm/service/tracing"

	"github.com/go-openapi/strfmt"
	"github.com/minio/minio-go/pkg/encrypt"
	"github.com/pkg/errors"
	"github.com/rs/zerolog"

	"github.com/iryonetwork/wwm/gen/storage/models"
	"github.com/iryonetwork/wwm/storage/s3/object"
	"github.com/minio/minio-go"
)

const metaArchetype = "x-archetype"
const metaCreated = "x-created"
const metaChecksum = "x-checksum"
const metaLabels = "x-labels"

// Storage provides an interface for s3 public functions
type Storage interface {
	BucketExists(ctx context.Context, bucketID string) (bool, error)
	MakeBucket(ctx context.Context, bucketID string) error
	ListBuckets(ctx context.Context) ([]*models.BucketDescriptor, error)
	List(ctx context.Context, bucketID, prefix string) ([]*models.FileDescriptor, error)
	Read(ctx context.Context, bucketID, fileID, version string) (io.ReadCloser, *models.FileDescriptor, error)
	Write(ctx context.Context, bucketID string, newFile *object.NewObjectInfo, r io.Reader) (*models.FileDescriptor, error)
	Delete(ctx context.Context, bucketID, fileID, version string) error
}

// KeyProvider lists methods required for reading encryption keys
type KeyProvider interface {
	Get(string) (string, error)
}

// Minio interface describes functions used in minio-go package for mocking
// purposes.
type Minio interface {
	MakeBucket(bucketName, location string) error
	BucketExists(bucketName string) (bool, error)
	ListBuckets() ([]minio.BucketInfo, error)
	ListObjectsV2(bucketName, prefix string, recursive bool, doneCh <-chan struct{}) <-chan minio.ObjectInfo
	GetObjectWithContext(ctx context.Context, bucketName, objectName string, opts minio.GetObjectOptions) (io.ReadCloser, error)
	GetEncryptedObject(bucketName, objectName string, encryptMaterials encrypt.Materials) (io.ReadCloser, error)
	PutObjectWithContext(ctx context.Context, bucketName, objectName string, reader io.Reader, objectSize int64,
		opts minio.PutObjectOptions) (n int64, err error)
	PutEncryptedObject(bucketName, objectName string, reader io.Reader, encryptMaterials encrypt.Materials) (n int64, err error)
	RemoveObjects(bucketName string, objectsCh <-chan string) <-chan minio.RemoveObjectError
}

var nameVersionRE = regexp.MustCompile("^(.*)\\.(\\d+)$")

// Config holds all details required to connect to an S3 storage
type Config struct {
	Endpoint     string
	AccessKey    string
	AccessSecret string
	Secure       bool
	Region       string
}

type s3storage struct {
	cfg    *Config
	client Minio
	keys   KeyProvider
	logger zerolog.Logger
}

// Operation represents a single character operation
type Operation string

// Write represents write operation
const Write Operation = Operation(models.FileDescriptorOperationW)

// Delete represents read operation
const Delete Operation = Operation(models.FileDescriptorOperationD)

const bucketExistsErrMsg = "Your previous request to create the named bucket succeeded and you already own it."

// ErrAlreadyExists indicates bucket or file already exists
var ErrAlreadyExists = errors.New("Item already exists")

// ErrNotFound indicates file or bucket were not found
var ErrNotFound = errors.New("File not found")

// ErrDeleted indicates file or bucket were already deleted
var ErrDeleted = errors.New("File was deleted")

// New creates a new instance of s3 storage
func New(cfg *Config, keys KeyProvider, logger zerolog.Logger) (Storage, error) {
	logger = logger.With().Str("component", "storage/s3").Logger()

	c, err := minio.NewWithRegion(cfg.Endpoint, cfg.AccessKey, cfg.AccessSecret, cfg.Secure, cfg.Region)
	if err != nil {
		logger.Info().Err(err).Str("cmd", "s3::New").Msg("Failed to initialize minio with region")
		return nil, errors.Wrap(err, "Failed to initialize minio with region")
	}

	obj := &s3storage{
		cfg:    cfg,
		client: iminio{*c},
		keys:   keys,
		logger: logger,
	}

	return obj, nil
}

// Check if bucket already exists
func (s *s3storage) BucketExists(ctx context.Context, bucketID string) (bool, error) {
	exists := false
	err := tracing.TraceFunctionSpan("s3::BucketExists", ctx, func() (err error) {

		s.logger.Debug().Str("cmd", "s3::BucketExists").Msgf("('%s')", bucketID)

		exists, err = s.client.BucketExists(bucketID)
		if err != nil {
			s.logger.Info().Err(err).Str("cmd", "s3::BucketExists").Msg("Failed to check if bucket exists")
			return errors.Wrap(err, "Failed to check if bucket exists")
		}

		return nil

	})
	return exists, err
}

// MakeBucket creates a bucket, return ErrAlreadyExists if bucket already exists
func (s *s3storage) MakeBucket(ctx context.Context, bucketID string) error {
	return tracing.TraceFunctionSpan("s3::MakeBucket", ctx, func() (err error) {

		s.logger.Debug().Str("cmd", "s3::MakeBucket").Msgf("('%s')", bucketID)

		exists, err := s.client.BucketExists(bucketID)
		if err != nil {
			return errors.Wrap(err, "Failed to check if bucket exists")
		}
		if exists {
			return ErrAlreadyExists
		}

		if !exists {
			if err := s.client.MakeBucket(bucketID, s.cfg.Region); err != nil && strings.Contains(err.Error(), bucketExistsErrMsg) {
				s.logger.Debug().Err(err).Msg("Looks like bucket actually existed when MakeBucket was called")
				return ErrAlreadyExists
			} else if err != nil {
				s.logger.Info().Err(err).Str("cmd", "s3::MakeBucket").Msg("Failed to create a new bucket")
				return errors.Wrap(err, "Failed to create a new bucket")
			}
		}
		return nil
	})
}

// ListBuckets returns a list of buckets
func (s *s3storage) ListBuckets(ctx context.Context) ([]*models.BucketDescriptor, error) {
	var buckets []*models.BucketDescriptor
	err := tracing.TraceFunctionSpan("s3::ListBuckets", ctx, func() (err error) {

		s.logger.Debug().Str("cmd", "s3::ListBuckets")

		b, err := s.client.ListBuckets()

		if err != nil {
			s.logger.Info().Err(err).Str("cmd", "s3::ListBuckets").Msg("Failed to list buckets")
			return errors.Wrap(err, "Failed to list buckets")
		}

		for _, info := range b {
			bd, err := bucketInfoToBucketDescriptor(info)
			if err != nil {
				s.logger.Info().Err(err).Str("cmd", "s3::ListBuckets").Msg("Failed to convert bucketInfo to bucketDescriptor")
				return errors.Wrap(err, "Failed to convert bucketInfo to bucketDescriptor")
			}
			buckets = append(buckets, bd)
		}

		return nil

	})
	return buckets, err
}

// List returns a list of files stored inside a bucket
func (s *s3storage) List(ctx context.Context, bucketID, prefix string) ([]*models.FileDescriptor, error) {
	var files []*models.FileDescriptor
	err := tracing.TraceFunctionSpan("s3::List", ctx, func() (err error) {

		s.logger.Debug().Str("cmd", "s3::List").Msgf("('%s', '%s')", bucketID, prefix)

		// Check if bucket exists first
		exists, err := s.client.BucketExists(bucketID)
		if err != nil {
			s.logger.Info().Err(err).Str("cmd", "s3::List").Msg("Failed to check if bucket exists")
			return errors.Wrap(err, "Failed to check if bucket exists")
		}
		if !exists {
			// Nothing to list
			files = []*models.FileDescriptor{}
			return nil
		}

		ch := make(chan struct{})
		defer close(ch)
		infos := s.client.ListObjectsV2(bucketID, prefix, false, ch)

		for info := range infos {
			if info.Err != nil {
				s.logger.Info().Err(info.Err).Str("cmd", "s3::List").Msg("Failed to read object from a list")
				return errors.Wrap(info.Err, "Failed to read object from a list")
			}

			fd, err := objectInfoToFileDescriptor(info, bucketID)
			if err != nil {
				s.logger.Info().Err(err).Str("cmd", "s3::List").Msg("Failed to convert object to fileDescriptor")
				return errors.Wrap(err, "Failed to convert object to fileDescriptor")
			}

			files = append(files, fd)
		}

		sort.Sort(byCreated(files))
		return nil
	})
	return files, err
}

// Read fetches contents from the storage
func (s *s3storage) Read(ctx context.Context, bucketID, fileID, version string) (io.ReadCloser, *models.FileDescriptor, error) {
	var fd *models.FileDescriptor
	var reader io.ReadCloser
	err := tracing.TraceFunctionSpan("s3::Read", ctx, func() (err error) {

		s.logger.Debug().Str("cmd", "s3::Read").Msgf("('%s', '%s', '%s')", bucketID, fileID, version)

		// find the file
		prefix := fmt.Sprintf("%s.", fileID)
		if version != "" {
			prefix += fmt.Sprintf("%s.", version)
		}
		list, err := s.List(ctx, bucketID, prefix)
		if err != nil {
			s.logger.Info().Err(err).Str("cmd", "s3::Read").Msg("Failed to list files")
			return errors.Wrap(err, "Failed to list files")
		}
		if len(list) == 0 {
			return ErrNotFound
		}
		md, err := metadataFromFileDescriptor(list[0])
		if err != nil {
			s.logger.Info().Err(err).Str("cmd", "s3::Read").Msg("Failed to parse metadata from fileDescriptor")
			return errors.Wrap(err, "Failed to parse metadata from fileDescriptor")
		}

		// read the key
		em, err := getCBCKey(bucketID, s.keys)
		if err != nil {
			s.logger.Info().Err(err).Str("cmd", "s3::Read").Msg("Failed to set CBC key")
			return errors.Wrap(err, "Failed to set CBC key")
		}

		// fetch the file
		reader, err = s.client.GetObjectWithContext(ctx, bucketID, md.String(), minio.GetObjectOptions{Materials: em})

		if err != nil {
			s.logger.Info().Err(err).Str("cmd", "s3::Read").Msg("Failed to fetch enc. object")
			return errors.Wrap(err, "Failed to fetch enc. object")
		}

		fd = list[0]
		return nil
	})
	return reader, fd, err
}

// Write creates a new file in the storage
func (s *s3storage) Write(ctx context.Context, bucketID string, newFile *object.NewObjectInfo, r io.Reader) (fd *models.FileDescriptor, err error) {
	err = tracing.TraceFunctionSpan("s3::Write", ctx, func() (err error) {

		s.logger.Debug().Str("cmd", "s3::Write").Msgf("('%s', '%+v', reader)", bucketID, newFile)

		// validate operation
		op := Operation(newFile.Operation)
		if op != Write && op != Delete {
			s.logger.Info().Str("cmd", "s3::Write").Msgf("Received an invalid operation '%s'", op)
			return fmt.Errorf("Received an invalid operation '%s'", op)
		}

		// get the key
		em, err := getCBCKey(bucketID, s.keys)
		if err != nil {
			s.logger.Info().Err(err).Str("cmd", "s3::Write").Msg("Failed to set the CBC key")
			return errors.Wrap(err, "Failed to set the CBC key")
		}

		// collect meta data
		meta, err := metadataFromNewFile(newFile)
		if err != nil {
			s.logger.Info().Err(err).Str("cmd", "s3::Write").Msg("Failed to collect metadata from new file")
			return errors.Wrap(err, "Failed to collect metadata from new file")
		}

		// upload the file
		_, err = s.client.PutObjectWithContext(ctx, bucketID, meta.String(), r, -1, minio.PutObjectOptions{EncryptMaterials: em})
		if err != nil {
			s.logger.Info().Err(err).Str("cmd", "s3::Write").Msg("Failed to call PutObject")
			return errors.Wrap(err, "Failed to call PutObjectWithContext")
		}

		// generate the file descriptor
		fd = &models.FileDescriptor{
			Name:        newFile.Name,
			Version:     newFile.Version,
			Archetype:   newFile.Archetype,
			ContentType: newFile.ContentType,
			Checksum:    newFile.Checksum,
			Created:     newFile.Created,
			Labels:      newFile.Labels,
			Path:        fmt.Sprintf("%s/%s/%s", bucketID, meta.filename, meta.version),
			Size:        newFile.Size,
			Operation:   string(op),
		}

		return nil
	})
	return fd, err
}

// Delete removes files completely from storage, used only in case of conflicting files with the same ID and version
func (s *s3storage) Delete(ctx context.Context, bucketID, fileID, version string) error {
	return tracing.TraceFunctionSpan("s3::Write", ctx, func() (err error) {

		s.logger.Debug().Str("cmd", "s3::Delete").Msgf("('%s', '%s', '%s')", bucketID, fileID, version)

		// Check if bucket exists first
		exists, err := s.client.BucketExists(bucketID)
		if err != nil {
			s.logger.Info().Err(err).Str("cmd", "s3::Delete").Msg("Failed to check if bucket exists")
			return errors.Wrap(err, "Failed to check if bucket exists")
		}
		if !exists {
			// Nothing to delete
			return nil
		}

		// Set object prefix
		prefix := fmt.Sprintf("%s.", fileID)
		if version != "" {
			prefix += fmt.Sprintf("%s.", version)
		}

		// first objects keys will be saved to array to prevent deleting any if listing fails
		objKeys := []string{}
		for info := range s.client.ListObjectsV2(bucketID, prefix, false, nil) {
			if info.Err != nil {
				s.logger.Error().Err(info.Err).Str("cmd", "s3::Delete").Msg("Failed to list all objects")
				return errors.Wrap(info.Err, "Failed to list all objects")
			}
			objKeys = append(objKeys, info.Key)
		}

		// make objects channel
		ch := make(chan string, len(objKeys))
		for _, objKey := range objKeys {
			ch <- objKey
		}
		close(ch)

		for removeObjErr := range s.client.RemoveObjects(bucketID, ch) {
			err = removeObjErr.Err
			s.logger.Error().Err(err).Str("cmd", "s3::Delete").Msg("Failed to delete the object")
		}

		if err != nil {
			return errors.New("Failed to delete all matching objects")
		}

		return nil
	})
}

func objectInfoToFileDescriptor(info minio.ObjectInfo, bucketID string) (*models.FileDescriptor, error) {
	meta, err := metadataFromKey(info.Key)
	if err != nil {
		return nil, errors.Wrap(err, "Failed to extract metadata from key")
	}

	// copy basic data
	fd := &models.FileDescriptor{
		Size:        info.Size,
		ContentType: meta.contentType,
		Path:        fmt.Sprintf("%s/%s/%s", bucketID, meta.filename, meta.version),
		Name:        meta.filename,
		Version:     meta.version,
		Checksum:    meta.checksum,
		Created:     strfmt.DateTime(meta.created),
		Archetype:   meta.archetype,
		Operation:   string(meta.operation),
		Labels:      meta.labels,
	}

	return fd, nil
}

func bucketInfoToBucketDescriptor(info minio.BucketInfo) (*models.BucketDescriptor, error) {
	// copy
	bd := &models.BucketDescriptor{
		Name:    info.Name,
		Created: strfmt.DateTime(info.CreationDate),
	}

	return bd, nil
}

func getCBCKey(bucketID string, keys KeyProvider) (encrypt.Materials, error) {
	// read the key
	secret, err := keys.Get(bucketID)
	if err != nil {
		return nil, err
	}

	// create the materials
	return encrypt.NewCBCSecureMaterials(encrypt.NewSymmetricKey([]byte(secret)))
}
