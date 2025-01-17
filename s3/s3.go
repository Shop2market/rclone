// Package s3 provides an interface to Amazon S3 oject storage
package s3

// FIXME need to prevent anything but ListDir working for s3://

/*
Progress of port to aws-sdk

 * Don't really need o.meta at all?

What happens if you CTRL-C a multipart upload
  * get an incomplete upload
  * disappears when you delete the bucket
*/

import (
	"errors"
	"fmt"
	"io"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/aws/corehandlers"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/request"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/aws/aws-sdk-go/service/s3/s3manager"
	"github.com/Shop2market/rclone/fs"
	"github.com/ncw/swift"
)

// Register with Fs
func init() {
	fs.Register(&fs.Info{
		Name:  "s3",
		NewFs: NewFs,
		// AWS endpoints: http://docs.amazonwebservices.com/general/latest/gr/rande.html#s3_region
		Options: []fs.Option{{
			Name: "access_key_id",
			Help: "AWS Access Key ID - leave blank for anonymous access.",
		}, {
			Name: "secret_access_key",
			Help: "AWS Secret Access Key (password) - leave blank for anonymous access.",
		}, {
			Name: "region",
			Help: "Region to connect to.",
			Examples: []fs.OptionExample{{
				Value: "us-east-1",
				Help:  "The default endpoint - a good choice if you are unsure.\nUS Region, Northern Virginia or Pacific Northwest.\nLeave location constraint empty.",
			}, {
				Value: "us-west-2",
				Help:  "US West (Oregon) Region\nNeeds location constraint us-west-2.",
			}, {
				Value: "us-west-1",
				Help:  "US West (Northern California) Region\nNeeds location constraint us-west-1.",
			}, {
				Value: "eu-west-1",
				Help:  "EU (Ireland) Region Region\nNeeds location constraint EU or eu-west-1.",
			}, {
				Value: "eu-central-1",
				Help:  "EU (Frankfurt) Region\nNeeds location constraint eu-central-1.",
			}, {
				Value: "ap-southeast-1",
				Help:  "Asia Pacific (Singapore) Region\nNeeds location constraint ap-southeast-1.",
			}, {
				Value: "ap-southeast-2",
				Help:  "Asia Pacific (Sydney) Region\nNeeds location constraint ap-southeast-2.",
			}, {
				Value: "ap-northeast-1",
				Help:  "Asia Pacific (Tokyo) Region\nNeeds location constraint ap-northeast-1.",
			}, {
				Value: "sa-east-1",
				Help:  "South America (Sao Paulo) Region\nNeeds location constraint sa-east-1.",
			}, {
				Value: "other-v2-signature",
				Help:  "If using an S3 clone that only understands v2 signatures - eg Ceph - set this and make sure you set the endpoint.",
			}, {
				Value: "other-v4-signature",
				Help:  "If using an S3 clone that understands v4 signatures set this and make sure you set the endpoint.",
			}},
		}, {
			Name: "endpoint",
			Help: "Endpoint for S3 API.\nLeave blank if using AWS to use the default endpoint for the region.\nSpecify if using an S3 clone such as Ceph.",
		}, {
			Name: "location_constraint",
			Help: "Location constraint - must be set to match the Region. Used when creating buckets only.",
			Examples: []fs.OptionExample{{
				Value: "",
				Help:  "Empty for US Region, Northern Virginia or Pacific Northwest.",
			}, {
				Value: "us-west-2",
				Help:  "US West (Oregon) Region.",
			}, {
				Value: "us-west-1",
				Help:  "US West (Northern California) Region.",
			}, {
				Value: "eu-west-1",
				Help:  "EU (Ireland) Region.",
			}, {
				Value: "EU",
				Help:  "EU Region.",
			}, {
				Value: "ap-southeast-1",
				Help:  "Asia Pacific (Singapore) Region.",
			}, {
				Value: "ap-southeast-2",
				Help:  "Asia Pacific (Sydney) Region.",
			}, {
				Value: "ap-northeast-1",
				Help:  "Asia Pacific (Tokyo) Region.",
			}, {
				Value: "sa-east-1",
				Help:  "South America (Sao Paulo) Region.",
			}},
		}},
	})
}

// Constants
const (
	metaMtime     = "Mtime" // the meta key to store mtime in - eg X-Amz-Meta-Mtime
	listChunkSize = 1024    // number of items to read at once
	maxRetries    = 10      // number of retries to make of operations
)

// Fs represents a remote s3 server
type Fs struct {
	name               string           // the name of the remote
	c                  *s3.S3           // the connection to the s3 server
	ses                *session.Session // the s3 session
	bucket             string           // the bucket we are working on
	perm               string           // permissions for new buckets / objects
	root               string           // root of the bucket - ignore all objects above this
	locationConstraint string           // location constraint of new buckets
}

// Object describes a s3 object
type Object struct {
	// Will definitely have everything but meta which may be nil
	//
	// List will read everything but meta - to fill that in need to call
	// readMetaData
	fs           *Fs                // what this object is part of
	remote       string             // The remote path
	etag         string             // md5sum of the object
	bytes        int64              // size of the object
	lastModified time.Time          // Last modified
	meta         map[string]*string // The object metadata if known - may be nil
}

// ------------------------------------------------------------

// Name of the remote (as passed into NewFs)
func (f *Fs) Name() string {
	return f.name
}

// Root of the remote (as passed into NewFs)
func (f *Fs) Root() string {
	if f.root == "" {
		return f.bucket
	}
	return f.bucket + "/" + f.root
}

// String converts this Fs to a string
func (f *Fs) String() string {
	if f.root == "" {
		return fmt.Sprintf("S3 bucket %s", f.bucket)
	}
	return fmt.Sprintf("S3 bucket %s path %s", f.bucket, f.root)
}

// Pattern to match a s3 path
var matcher = regexp.MustCompile(`^([^/]*)(.*)$`)

// parseParse parses a s3 'url'
func s3ParsePath(path string) (bucket, directory string, err error) {
	parts := matcher.FindStringSubmatch(path)
	if parts == nil {
		err = fmt.Errorf("Couldn't parse bucket out of s3 path %q", path)
	} else {
		bucket, directory = parts[1], parts[2]
		directory = strings.Trim(directory, "/")
	}
	return
}

// s3Connection makes a connection to s3
func s3Connection(name string) (*s3.S3, *session.Session, error) {
	// Make the auth
	accessKeyID := fs.ConfigFile.MustValue(name, "access_key_id")
	secretAccessKey := fs.ConfigFile.MustValue(name, "secret_access_key")
	var auth *credentials.Credentials
	switch {
	case accessKeyID == "" && secretAccessKey == "":
		fs.Debug(name, "Using anonymous access for S3")
		auth = credentials.AnonymousCredentials
	case accessKeyID == "":
		return nil, nil, errors.New("access_key_id not found")
	case secretAccessKey == "":
		return nil, nil, errors.New("secret_access_key not found")
	default:
		auth = credentials.NewStaticCredentials(accessKeyID, secretAccessKey, "")
	}

	endpoint := fs.ConfigFile.MustValue(name, "endpoint")
	region := fs.ConfigFile.MustValue(name, "region")
	if region == "" && endpoint == "" {
		endpoint = "https://s3.amazonaws.com/"
	}
	if region == "" {
		region = "us-east-1"
	}
	awsConfig := aws.NewConfig().
		WithRegion(region).
		WithMaxRetries(maxRetries).
		WithCredentials(auth).
		WithEndpoint(endpoint).
		WithHTTPClient(fs.Config.Client()).
		WithS3ForcePathStyle(true)
	// awsConfig.WithLogLevel(aws.LogDebugWithSigning)
	ses := session.New()
	c := s3.New(ses, awsConfig)
	if region == "other-v2-signature" {
		fs.Debug(name, "Using v2 auth")
		signer := func(req *request.Request) {
			// Ignore AnonymousCredentials object
			if req.Config.Credentials == credentials.AnonymousCredentials {
				return
			}
			sign(accessKeyID, secretAccessKey, req.HTTPRequest)
		}
		c.Handlers.Sign.Clear()
		c.Handlers.Sign.PushBackNamed(corehandlers.BuildContentLengthHandler)
		c.Handlers.Sign.PushBack(signer)
	}
	// Add user agent
	c.Handlers.Build.PushBack(func(r *request.Request) {
		r.HTTPRequest.Header.Set("User-Agent", fs.UserAgent)
	})
	return c, ses, nil
}

// NewFs contstructs an Fs from the path, bucket:path
func NewFs(name, root string) (fs.Fs, error) {
	bucket, directory, err := s3ParsePath(root)
	if err != nil {
		return nil, err
	}
	c, ses, err := s3Connection(name)
	if err != nil {
		return nil, err
	}
	f := &Fs{
		name:   name,
		c:      c,
		bucket: bucket,
		ses:    ses,
		// FIXME perm:   s3.Private, // FIXME need user to specify
		root:               directory,
		locationConstraint: fs.ConfigFile.MustValue(name, "location_constraint"),
	}
	if f.root != "" {
		f.root += "/"
		// Check to see if the object exists
		req := s3.HeadObjectInput{
			Bucket: &f.bucket,
			Key:    &directory,
		}
		_, err = f.c.HeadObject(&req)
		if err == nil {
			remote := path.Base(directory)
			f.root = path.Dir(directory)
			if f.root == "." {
				f.root = ""
			} else {
				f.root += "/"
			}
			obj := f.NewFsObject(remote)
			// return a Fs Limited to this object
			return fs.NewLimited(f, obj), nil
		}
	}
	// f.listMultipartUploads()
	return f, nil
}

// Return an FsObject from a path
//
// May return nil if an error occurred
func (f *Fs) newFsObjectWithInfo(remote string, info *s3.Object) fs.Object {
	o := &Object{
		fs:     f,
		remote: remote,
	}
	if info != nil {
		// Set info but not meta
		if info.LastModified == nil {
			fs.Log(o, "Failed to read last modified")
			o.lastModified = time.Now()
		} else {
			o.lastModified = *info.LastModified
		}
		o.etag = aws.StringValue(info.ETag)
		o.bytes = aws.Int64Value(info.Size)
	} else {
		err := o.readMetaData() // reads info and meta, returning an error
		if err != nil {
			// logged already FsDebug("Failed to read info: %s", err)
			return nil
		}
	}
	return o
}

// NewFsObject returns an FsObject from a path
//
// May return nil if an error occurred
func (f *Fs) NewFsObject(remote string) fs.Object {
	return f.newFsObjectWithInfo(remote, nil)
}

// list the objects into the function supplied
//
// If directories is set it only sends directories
func (f *Fs) list(directories bool, fn func(string, *s3.Object)) {
	maxKeys := int64(listChunkSize)
	delimiter := ""
	if directories {
		delimiter = "/"
	}
	var marker *string
	for {
		// FIXME need to implement ALL loop
		req := s3.ListObjectsInput{
			Bucket:    &f.bucket,
			Delimiter: &delimiter,
			Prefix:    &f.root,
			MaxKeys:   &maxKeys,
			Marker:    marker,
		}
		resp, err := f.c.ListObjects(&req)
		if err != nil {
			fs.Stats.Error()
			fs.ErrorLog(f, "Couldn't read bucket %q: %s", f.bucket, err)
			break
		} else {
			rootLength := len(f.root)
			if directories {
				for _, commonPrefix := range resp.CommonPrefixes {
					if commonPrefix.Prefix == nil {
						fs.Log(f, "Nil common prefix received")
						continue
					}
					remote := *commonPrefix.Prefix
					if !strings.HasPrefix(remote, f.root) {
						fs.Log(f, "Odd name received %q", remote)
						continue
					}
					remote = remote[rootLength:]
					if strings.HasSuffix(remote, "/") {
						remote = remote[:len(remote)-1]
					}
					fn(remote, &s3.Object{Key: &remote})
				}
			} else {
				for _, object := range resp.Contents {
					key := aws.StringValue(object.Key)
					if !strings.HasPrefix(key, f.root) {
						fs.Log(f, "Odd name received %q", key)
						continue
					}
					remote := key[rootLength:]
					fn(remote, object)
				}
			}
			if !aws.BoolValue(resp.IsTruncated) {
				break
			}
			// Use NextMarker if set, otherwise use last Key
			if resp.NextMarker == nil || *resp.NextMarker == "" {
				marker = resp.Contents[len(resp.Contents)-1].Key
			} else {
				marker = resp.NextMarker
			}
		}
	}
}

// List walks the path returning a channel of FsObjects
func (f *Fs) List() fs.ObjectsChan {
	out := make(fs.ObjectsChan, fs.Config.Checkers)
	if f.bucket == "" {
		// Return no objects at top level list
		close(out)
		fs.Stats.Error()
		fs.ErrorLog(f, "Can't list objects at root - choose a bucket using lsd")
	} else {
		go func() {
			defer close(out)
			f.list(false, func(remote string, object *s3.Object) {
				if fs := f.newFsObjectWithInfo(remote, object); fs != nil {
					out <- fs
				}
			})
		}()
	}
	return out
}

// ListDir lists the buckets
func (f *Fs) ListDir() fs.DirChan {
	out := make(fs.DirChan, fs.Config.Checkers)
	if f.bucket == "" {
		// List the buckets
		go func() {
			defer close(out)
			req := s3.ListBucketsInput{}
			resp, err := f.c.ListBuckets(&req)
			if err != nil {
				fs.Stats.Error()
				fs.ErrorLog(f, "Couldn't list buckets: %s", err)
			} else {
				for _, bucket := range resp.Buckets {
					out <- &fs.Dir{
						Name:  aws.StringValue(bucket.Name),
						When:  aws.TimeValue(bucket.CreationDate),
						Bytes: -1,
						Count: -1,
					}
				}
			}
		}()
	} else {
		// List the directories in the path in the bucket
		go func() {
			defer close(out)
			f.list(true, func(remote string, object *s3.Object) {
				size := int64(0)
				if object.Size != nil {
					size = *object.Size
				}
				out <- &fs.Dir{
					Name:  remote,
					Bytes: size,
					Count: 0,
				}
			})
		}()
	}
	return out
}

// Put the FsObject into the bucket
func (f *Fs) Put(in io.Reader, remote string, modTime time.Time, size int64) (fs.Object, error) {
	// Temporary Object under construction
	fs := &Object{
		fs:     f,
		remote: remote,
	}
	return fs, fs.Update(in, modTime, size)
}

// Mkdir creates the bucket if it doesn't exist
func (f *Fs) Mkdir() error {
	req := s3.CreateBucketInput{
		Bucket: &f.bucket,
		ACL:    &f.perm,
	}
	if f.locationConstraint != "" {
		req.CreateBucketConfiguration = &s3.CreateBucketConfiguration{
			LocationConstraint: &f.locationConstraint,
		}
	}
	_, err := f.c.CreateBucket(&req)
	if err, ok := err.(awserr.Error); ok {
		if err.Code() == "BucketAlreadyOwnedByYou" {
			return nil
		}
	}
	return err
}

// Rmdir deletes the bucket if the fs is at the root
//
// Returns an error if it isn't empty
func (f *Fs) Rmdir() error {
	if f.root != "" {
		return nil
	}
	req := s3.DeleteBucketInput{
		Bucket: &f.bucket,
	}
	_, err := f.c.DeleteBucket(&req)
	return err
}

// Precision of the remote
func (f *Fs) Precision() time.Duration {
	return time.Nanosecond
}

// Copy src to this remote using server side copy operations.
//
// This is stored with the remote path given
//
// It returns the destination Object and a possible error
//
// Will only be called if src.Fs().Name() == f.Name()
//
// If it isn't possible then return fs.ErrorCantCopy
func (f *Fs) Copy(src fs.Object, remote string) (fs.Object, error) {
	srcObj, ok := src.(*Object)
	if !ok {
		fs.Debug(src, "Can't copy - not same remote type")
		return nil, fs.ErrorCantCopy
	}
	srcFs := srcObj.fs
	key := f.root + remote
	source := srcFs.bucket + "/" + srcFs.root + srcObj.remote
	req := s3.CopyObjectInput{
		Bucket:            &f.bucket,
		Key:               &key,
		CopySource:        &source,
		MetadataDirective: aws.String(s3.MetadataDirectiveCopy),
	}
	_, err := f.c.CopyObject(&req)
	if err != nil {
		return nil, err
	}
	return f.NewFsObject(remote), err
}

// ------------------------------------------------------------

// Fs returns the parent Fs
func (o *Object) Fs() fs.Fs {
	return o.fs
}

// Return a string version
func (o *Object) String() string {
	if o == nil {
		return "<nil>"
	}
	return o.remote
}

// Remote returns the remote path
func (o *Object) Remote() string {
	return o.remote
}

var matchMd5 = regexp.MustCompile(`^[0-9a-f]{32}$`)

// Md5sum returns the Md5sum of an object returning a lowercase hex string
func (o *Object) Md5sum() (string, error) {
	etag := strings.Trim(strings.ToLower(o.etag), `"`)
	// Check the etag is a valid md5sum
	if !matchMd5.MatchString(etag) {
		// fs.Debug(o, "Invalid md5sum (probably multipart uploaded) - ignoring: %q", etag)
		return "", nil
	}
	return etag, nil
}

// Size returns the size of an object in bytes
func (o *Object) Size() int64 {
	return o.bytes
}

// readMetaData gets the metadata if it hasn't already been fetched
//
// it also sets the info
func (o *Object) readMetaData() (err error) {
	if o.meta != nil {
		return nil
	}
	key := o.fs.root + o.remote
	req := s3.HeadObjectInput{
		Bucket: &o.fs.bucket,
		Key:    &key,
	}
	resp, err := o.fs.c.HeadObject(&req)
	if err != nil {
		fs.Debug(o, "Failed to read info: %s", err)
		return err
	}
	var size int64
	// Ignore missing Content-Length assuming it is 0
	// Some versions of ceph do this due their apache proxies
	if resp.ContentLength != nil {
		size = *resp.ContentLength
	}
	o.etag = aws.StringValue(resp.ETag)
	o.bytes = size
	o.meta = resp.Metadata
	if resp.LastModified == nil {
		fs.Log(o, "Failed to read last modified from HEAD: %s", err)
		o.lastModified = time.Now()
	} else {
		o.lastModified = *resp.LastModified
	}
	return nil
}

// ModTime returns the modification time of the object
//
// It attempts to read the objects mtime and if that isn't present the
// LastModified returned in the http headers
func (o *Object) ModTime() time.Time {
	err := o.readMetaData()
	if err != nil {
		fs.Log(o, "Failed to read metadata: %s", err)
		return time.Now()
	}
	// read mtime out of metadata if available
	d, ok := o.meta[metaMtime]
	if !ok || d == nil {
		// fs.Debug(o, "No metadata")
		return o.lastModified
	}
	modTime, err := swift.FloatStringToTime(*d)
	if err != nil {
		fs.Log(o, "Failed to read mtime from object: %s", err)
		return o.lastModified
	}
	return modTime
}

// SetModTime sets the modification time of the local fs object
func (o *Object) SetModTime(modTime time.Time) {
	err := o.readMetaData()
	if err != nil {
		fs.Stats.Error()
		fs.ErrorLog(o, "Failed to read metadata: %s", err)
		return
	}
	o.meta[metaMtime] = aws.String(swift.TimeToFloatString(modTime))

	// Guess the content type
	contentType := fs.MimeType(o)

	// Copy the object to itself to update the metadata
	key := o.fs.root + o.remote
	sourceKey := o.fs.bucket + "/" + key
	directive := s3.MetadataDirectiveReplace // replace metadata with that passed in
	req := s3.CopyObjectInput{
		Bucket:            &o.fs.bucket,
		ACL:               &o.fs.perm,
		Key:               &key,
		ContentType:       &contentType,
		CopySource:        &sourceKey,
		Metadata:          o.meta,
		MetadataDirective: &directive,
	}
	_, err = o.fs.c.CopyObject(&req)
	if err != nil {
		fs.Stats.Error()
		fs.ErrorLog(o, "Failed to update remote mtime: %s", err)
	}
}

// Storable raturns a boolean indicating if this object is storable
func (o *Object) Storable() bool {
	return true
}

// Open an object for read
func (o *Object) Open() (in io.ReadCloser, err error) {
	key := o.fs.root + o.remote
	req := s3.GetObjectInput{
		Bucket: &o.fs.bucket,
		Key:    &key,
	}
	resp, err := o.fs.c.GetObject(&req)
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

// Update the Object from in with modTime and size
func (o *Object) Update(in io.Reader, modTime time.Time, size int64) error {
	uploader := s3manager.NewUploader(o.fs.ses, func(u *s3manager.Uploader) {
		u.Concurrency = 2
		u.LeavePartsOnError = false
		u.S3 = o.fs.c
	})

	// Set the mtime in the meta data
	metadata := map[string]*string{
		metaMtime: aws.String(swift.TimeToFloatString(modTime)),
	}

	// Guess the content type
	contentType := fs.MimeType(o)

	key := o.fs.root + o.remote
	req := s3manager.UploadInput{
		Bucket:      &o.fs.bucket,
		ACL:         &o.fs.perm,
		Key:         &key,
		Body:        in,
		ContentType: &contentType,
		Metadata:    metadata,
		//ContentLength: &size,
	}
	_, err := uploader.Upload(&req)
	if err != nil {
		return err
	}

	// Read the metadata from the newly created object
	o.meta = nil // wipe old metadata
	err = o.readMetaData()
	return err
}

// Remove an object
func (o *Object) Remove() error {
	key := o.fs.root + o.remote
	req := s3.DeleteObjectInput{
		Bucket: &o.fs.bucket,
		Key:    &key,
	}
	_, err := o.fs.c.DeleteObject(&req)
	return err
}

// Check the interfaces are satisfied
var (
	_ fs.Fs     = &Fs{}
	_ fs.Copier = &Fs{}
	_ fs.Object = &Object{}
)
