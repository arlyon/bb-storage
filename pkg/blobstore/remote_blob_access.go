package blobstore

import (
	"context"
	"fmt"
	"net/http"

	"github.com/buildbarn/bb-storage/pkg/blobstore/buffer"
	"github.com/buildbarn/bb-storage/pkg/util"

	"golang.org/x/net/context/ctxhttp"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"go.opencensus.io/trace"
)

type remoteBlobAccess struct {
	address     string
	prefix      string
	storageType StorageType
}

func convertHTTPUnexpectedStatus(resp *http.Response) error {
	return status.Errorf(codes.Unknown, "Unexpected status code from remote cache: %d - %s", resp.StatusCode, http.StatusText(resp.StatusCode))
}

// NewRemoteBlobAccess for use of HTTP/1.1 cache backend.
//
// See: https://docs.bazel.build/versions/master/remote-caching.html#http-caching-protocol
func NewRemoteBlobAccess(address string, prefix string, storageType StorageType) BlobAccess {
	return &remoteBlobAccess{
		address:     address,
		prefix:      prefix,
		storageType: storageType,
	}
}

func (ba *remoteBlobAccess) Get(ctx context.Context, digest *util.Digest) buffer.Buffer {
	ctx, span := trace.StartSpan(ctx, "blobstore.RemoteBlobAccess.Get")
	defer span.End()

	url := fmt.Sprintf("%s/%s/%s", ba.address, ba.prefix, digest.GetHashString())
	resp, err := ctxhttp.Get(ctx, http.DefaultClient, url)
	if err != nil {
		return buffer.NewBufferFromError(err)
	}

	switch resp.StatusCode {
	case http.StatusNotFound:
		resp.Body.Close()
		return buffer.NewBufferFromError(status.Error(codes.NotFound, url))
	case http.StatusOK:
		return ba.storageType.NewBufferFromReader(digest, resp.Body, buffer.Irreparable)
	default:
		resp.Body.Close()
		return buffer.NewBufferFromError(convertHTTPUnexpectedStatus(resp))
	}
}

func (ba *remoteBlobAccess) Put(ctx context.Context, digest *util.Digest, b buffer.Buffer) error {
	ctx, span := trace.StartSpan(ctx, "blobstore.RemoteBlobAccess.Put")
	defer span.End()

	sizeBytes, err := b.GetSizeBytes()
	if err != nil {
		b.Discard()
		return err
	}
	url := fmt.Sprintf("%s/%s/%s", ba.address, ba.prefix, digest.GetHashString())
	r := b.ToReader()
	req, err := http.NewRequest(http.MethodPut, url, r)
	if err != nil {
		r.Close()
		return err
	}
	req.ContentLength = sizeBytes
	_, err = ctxhttp.Do(ctx, http.DefaultClient, req)
	return err
}

func (ba *remoteBlobAccess) FindMissing(ctx context.Context, digests []*util.Digest) ([]*util.Digest, error) {
	ctx, span := trace.StartSpan(ctx, "blobstore.RemoteBlobAccess.FindMissing")
	defer span.End()

	var missing []*util.Digest
	for _, digest := range digests {
		url := fmt.Sprintf("%s/%s/%s", ba.address, ba.prefix, digest.GetHashString())
		resp, err := ctxhttp.Head(ctx, http.DefaultClient, url)
		if err != nil {
			return nil, err
		}

		switch resp.StatusCode {
		case http.StatusNotFound:
			missing = append(missing, digest)
		case http.StatusOK:
			continue
		default:
			return nil, convertHTTPUnexpectedStatus(resp)
		}
	}

	return missing, nil
}
