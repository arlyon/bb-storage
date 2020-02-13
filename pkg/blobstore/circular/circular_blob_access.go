package circular

import (
	"context"
	"errors"
	"io"
	"io/ioutil"
	"sync"

	"github.com/buildbarn/bb-storage/pkg/blobstore"
	"github.com/buildbarn/bb-storage/pkg/blobstore/buffer"
	"github.com/buildbarn/bb-storage/pkg/util"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"go.opencensus.io/trace"
)

// OffsetStore maps a digest to an offset within the data file. This is
// where the blob's contents may be found.
type OffsetStore interface {
	Get(digest *util.Digest, cursors Cursors) (uint64, int64, bool, error)
	Put(digest *util.Digest, offset uint64, length int64, cursors Cursors) error
}

// DataStore is where the data corresponding with a blob is stored. Data
// can be accessed by providing an offset within the data store and its
// length.
type DataStore interface {
	Put(r io.Reader, offset uint64) error
	Get(offset uint64, size int64) io.Reader
}

// StateStore is where global metadata of the circular storage backend
// is stored, namely the read/write cursors where data is currently
// being stored in the data file.
type StateStore interface {
	GetCursors() Cursors
	Allocate(sizeBytes int64) (uint64, error)
	Invalidate(offset uint64, sizeBytes int64) error
}

type circularBlobAccess struct {
	// Fields that are constant or lockless.
	dataStore   DataStore
	storageType blobstore.StorageType

	// Fields protected by the lock.
	lock        sync.Mutex
	offsetStore OffsetStore
	stateStore  StateStore
}

// NewCircularBlobAccess creates a new circular storage backend. Instead
// of writing data to storage directly, all three storage files are
// injected through separate interfaces.
func NewCircularBlobAccess(offsetStore OffsetStore, dataStore DataStore, stateStore StateStore, storageType blobstore.StorageType) blobstore.BlobAccess {
	return &circularBlobAccess{
		offsetStore: offsetStore,
		dataStore:   dataStore,
		stateStore:  stateStore,
		storageType: storageType,
	}
}

func (ba *circularBlobAccess) Get(ctx context.Context, digest *util.Digest) buffer.Buffer {
	ctx, span := trace.StartSpan(ctx, "blobstore.CircularBlobAccess.Get")
	defer span.End()

	ba.lock.Lock()
	span.Annotate(nil, "Lock obtained, calling GetCursors")
	cursors := ba.stateStore.GetCursors()
	offset, length, ok, err := ba.offsetStore.Get(digest, cursors)
	span.Annotate([]trace.Attribute{
		trace.Int64Attribute("offset", int64(offset)),
		trace.Int64Attribute("length", length),
		trace.BoolAttribute("object_found", ok),
	}, "offsetStore.Get completed")
	ba.lock.Unlock()
	if err != nil {
		return buffer.NewBufferFromError(err)
	} else if ok {
		return ba.storageType.NewBufferFromReader(
			digest,
			ioutil.NopCloser(ba.dataStore.Get(offset, length)),
			buffer.Reparable(digest, func() error {
				ba.lock.Lock()
				defer ba.lock.Unlock()
				return ba.stateStore.Invalidate(offset, length)
			}))
	}
	return buffer.NewBufferFromError(status.Errorf(codes.NotFound, "Blob not found"))
}

func (ba *circularBlobAccess) Put(ctx context.Context, digest *util.Digest, b buffer.Buffer) error {
	sizeBytes, err := b.GetSizeBytes()
	if err != nil {
		b.Discard()
		return err
	}

	// TODO: This would be more efficient if it passed the buffer
	// down, so IntoWriter() could be used.
	r := b.ToReader()
	defer r.Close()

	ctx, span := trace.StartSpan(ctx, "blobstore.CircularBlobAccess.Put")
	defer span.End()

	// Allocate space in the data store.
	ba.lock.Lock()
	span.Annotatef(nil, "Lock obtained, allocating %d bytes", sizeBytes)
	offset, err := ba.stateStore.Allocate(sizeBytes)
	ba.lock.Unlock()
	if err != nil {
		return err
	}
	span.Annotatef(nil, "Store allocated, offset %d", offset)

	// Write the data to storage.
	if err := ba.dataStore.Put(r, offset); err != nil {
		return err
	}

	span.Annotate(nil, "Obtaining lock")
	ba.lock.Lock()
	span.Annotate(nil, "Lock obtained, calling GetCursors")
	cursors := ba.stateStore.GetCursors()
	if cursors.Contains(offset, sizeBytes) {
		span.Annotate(nil, "Updating offsetStore")
		err = ba.offsetStore.Put(digest, offset, sizeBytes, cursors)
	} else {
		err = errors.New("Data became stale before write completed")
	}
	ba.lock.Unlock()
	return err
}

func (ba *circularBlobAccess) FindMissing(ctx context.Context, digests []*util.Digest) ([]*util.Digest, error) {
	ctx, span := trace.StartSpan(ctx, "blobstore.CircularBlobAccess.FindMissing")
	defer span.End()

	ba.lock.Lock()
	defer ba.lock.Unlock()

	cursors := ba.stateStore.GetCursors()
	var missingDigests []*util.Digest
	for _, digest := range digests {
		if _, _, ok, err := ba.offsetStore.Get(digest, cursors); err != nil {
			return nil, err
		} else if !ok {
			missingDigests = append(missingDigests, digest)
		}
	}
	return missingDigests, nil
}
