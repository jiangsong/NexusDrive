package provider

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"io"
)

// UploadRangeHasher computes a SHA1 proof over an inclusive byte range of the
// immutable upload content. Keeping this in the provider contract lets the
// uploader supply content without depending on individual drivers.
type UploadRangeHasher func(context.Context, int64, int64) (string, error)

type uploadRangeKey struct{}

// UploadContentReader supplies a small byte range of immutable upload content
// for ownership proofs. It uses offset/length (unlike the inclusive SHA1 range
// above) and never returns partial content on error.
type UploadContentReader func(ctx context.Context, off, length int64) ([]byte, error)

type uploadContentKey struct{}

func WithUploadContentReader(ctx context.Context, read UploadContentReader) context.Context {
	return context.WithValue(ctx, uploadContentKey{}, read)
}

func UploadContentReaderFrom(ctx context.Context) UploadContentReader {
	read, _ := ctx.Value(uploadContentKey{}).(UploadContentReader)
	return read
}

// MaxUploadProofBytes bounds allocations even for a malformed challenge.
const MaxUploadProofBytes = 64 << 10

// ContentRangeReader's source must stay open and immutable until BeginUpload
// returns. Subtraction-based bounds checks also reject integer overflow.
func ContentRangeReader(src io.ReaderAt, size int64) UploadContentReader {
	return func(ctx context.Context, off, length int64) ([]byte, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if size < 0 || off < 0 || length < 0 || off > size || length > size-off || length > MaxUploadProofBytes {
			return nil, fmt.Errorf("upload: invalid proof range %d+%d for %d bytes", off, length, size)
		}
		b := make([]byte, length)
		if _, err := io.ReadFull(io.NewSectionReader(src, off, length), b); err != nil {
			return nil, fmt.Errorf("upload: read proof content: %w", err)
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return b, nil
	}
}

func WithUploadRangeHasher(ctx context.Context, h UploadRangeHasher) context.Context {
	return context.WithValue(ctx, uploadRangeKey{}, h)
}

func UploadRangeHasherFrom(ctx context.Context) UploadRangeHasher {
	h, _ := ctx.Value(uploadRangeKey{}).(UploadRangeHasher)
	return h
}

// ContentRangeHasher never signs a partial or out-of-bounds range. The source
// must remain open and immutable until BeginUpload has returned.
func ContentRangeHasher(src io.ReaderAt, size int64) UploadRangeHasher {
	return func(ctx context.Context, start, end int64) (string, error) {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		if start < 0 || end < start || end >= size {
			return "", fmt.Errorf("upload: invalid proof range %d-%d for %d bytes", start, end, size)
		}
		h := sha1.New()
		r := io.NewSectionReader(src, start, end-start+1)
		buf := make([]byte, 32<<10)
		remaining := end - start + 1
		for remaining > 0 {
			if err := ctx.Err(); err != nil {
				return "", err
			}
			chunk := int64(len(buf))
			if remaining < chunk {
				chunk = remaining
			}
			n, err := io.ReadFull(r, buf[:chunk])
			if err != nil {
				return "", fmt.Errorf("upload: read proof: %w", err)
			}
			h.Write(buf[:n])
			remaining -= int64(n)
		}
		return hex.EncodeToString(h.Sum(nil)), nil
	}
}

// UploadBlobLinker hard-links the immutable content of the upload in
// progress to dst, so a backend that wants to keep the bytes past the
// transfer — a storage pool replicating them to other members — does not
// have to read them back from the network after the queue reclaims its own
// copy. The link shares the inode the queue and the read cache already
// share; nothing is copied. It fails across filesystems, and the caller
// must treat that as "no local copy", not as an error of the upload.
type UploadBlobLinker func(dst string) error

type uploadBlobKey struct{}

func WithUploadBlobLink(ctx context.Context, link UploadBlobLinker) context.Context {
	return context.WithValue(ctx, uploadBlobKey{}, link)
}

// UploadBlobLinkFrom returns the linker the uploader put in ctx, or nil when
// the caller is not the uploader (a test, a copy pipeline).
func UploadBlobLinkFrom(ctx context.Context) UploadBlobLinker {
	link, _ := ctx.Value(uploadBlobKey{}).(UploadBlobLinker)
	return link
}
