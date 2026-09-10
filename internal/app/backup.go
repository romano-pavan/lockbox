package app

import (
	"compress/gzip"
	"crypto/md5"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/romano-pavan/lockbox/internal/awsx"
	"github.com/romano-pavan/lockbox/internal/dbtool"
)

// maxSingleUpload is the largest object a single PutObject call accepts.
// Anything larger would need multipart upload, which lockbox does not do.
const maxSingleUpload = 5 * 1024 * 1024 * 1024

// Backup dumps the database, compresses it, uploads it and proves it arrived.
//
// The order matters. The dump is written to a temporary file first rather than
// streamed straight to Amazon, because the exact size and the SHA-256 checksum
// of the finished object must be known before the upload request can be signed.
// A dump that fails half way therefore never becomes a locked object that can
// never be deleted.
func Backup(args []string) error {
	flags := flag.NewFlagSet("backup", flag.ExitOnError)
	quiet := flags.Bool("quiet", false, "print nothing unless something goes wrong")
	tempDir := flags.String("temp-dir", "/var/tmp", "where to build the dump before uploading")
	flags.Parse(args)

	say := func(format string, a ...any) {
		if !*quiet {
			fmt.Printf(format, a...)
		}
	}

	cfg, client, err := load()
	if err != nil {
		return err
	}
	tools, err := dbtool.Find(cfg.Database.Type)
	if err != nil {
		return err
	}

	started := time.Now()
	key := objectPrefix(cfg) + timestamp(started) + ".sql.gz"

	temp, err := os.CreateTemp(*tempDir, "lockbox-*.sql.gz")
	if err != nil {
		return fmt.Errorf("cannot create a temporary file in %s: %w", *tempDir, err)
	}
	defer os.Remove(temp.Name())
	defer temp.Close()

	// The dump is compressed on the way out, and both digests are taken of the
	// compressed bytes, because those are the bytes Amazon will store. The
	// SHA-256 signs the request; the MD5 goes in the Content-MD5 header, which
	// a bucket with Object Lock enabled insists on.
	sha := sha256.New()
	digest := md5.New()
	compressor, err := gzip.NewWriterLevel(io.MultiWriter(temp, sha, digest), gzip.BestSpeed)
	if err != nil {
		return err
	}

	say("Dumping %s ...\n", cfg.DatabaseLabel())
	if err := dbtool.Dump(cfg, tools, compressor); err != nil {
		return err
	}
	if err := compressor.Close(); err != nil {
		return fmt.Errorf("cannot finish compressing: %w", err)
	}
	if err := temp.Sync(); err != nil {
		return fmt.Errorf("cannot flush the dump to disk: %w", err)
	}

	info, err := temp.Stat()
	if err != nil {
		return err
	}
	size := info.Size()
	checksum := hex.EncodeToString(sha.Sum(nil))
	contentMD5 := base64.StdEncoding.EncodeToString(digest.Sum(nil))

	// A truncated dump is still a valid gzip file, so size is the only cheap
	// sanity check available before the upload.
	if size < 1024 {
		return fmt.Errorf("the dump is only %d bytes, refusing to upload it", size)
	}
	if size > maxSingleUpload {
		return fmt.Errorf("the dump is %s, larger than the %s single upload limit",
			awsx.FormatSize(size), awsx.FormatSize(maxSingleUpload))
	}

	if _, err := temp.Seek(0, io.SeekStart); err != nil {
		return err
	}

	say("Uploading %s to s3://%s/%s ...\n", awsx.FormatSize(size), cfg.Storage.Bucket, key)
	if err := client.PutObject(cfg.Storage.Bucket, key, temp, size, checksum, contentMD5, "STANDARD_IA"); err != nil {
		return err
	}

	// Verification uses ListObjectsV2 rather than HeadObject, because the
	// upload key deliberately has no permission to read objects and
	// HeadObject counts as reading one.
	stored, err := client.StatObject(cfg.Storage.Bucket, key)
	if err != nil {
		return fmt.Errorf("upload reported success but the object is not listed: %w", err)
	}
	if stored.Size != size {
		return fmt.Errorf("size mismatch: %d bytes locally, %d bytes in the bucket", size, stored.Size)
	}

	lockNote := ""
	if mode, until, err := client.GetObjectRetention(cfg.Storage.Bucket, key); err == nil && !until.IsZero() {
		lockNote = fmt.Sprintf(", locked in %s mode until %s", mode, until.Format("2006-01-02 15:04 MST"))
	}

	say("OK %s (%s, sha256 %s)%s\n", key, awsx.FormatSize(size), checksum[:12], lockNote)
	say("Finished in %s\n", time.Since(started).Round(time.Second))
	return nil
}
