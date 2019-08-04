// Copyright (C) 2019 Storj Labs, Inc.
// See LICENSE for copying information.

package pieces

import (
	"context"
	"os"
	"sync"
	"time"

	"github.com/zeebo/errs"
	"go.uber.org/zap"
	"gopkg.in/spacemonkeygo/monkit.v2"

	"storj.io/storj/internal/memory"
	"storj.io/storj/pkg/pb"
	"storj.io/storj/pkg/storj"
	"storj.io/storj/storage"
	"storj.io/storj/storage/filestore"
)

const (
	preallocSize = 4 * memory.MiB
)

var (
	// Error is the default error class.
	Error = errs.Class("pieces error")

	mon = monkit.Package()
)

// Info contains all the information we need to know about a Piece to manage them.
type Info struct {
	SatelliteID storj.NodeID

	PieceID         storj.PieceID
	PieceSize       int64
	PieceCreation   time.Time
	PieceExpiration time.Time

	OrderLimit      *pb.OrderLimit
	UplinkPieceHash *pb.PieceHash
}

// ExpiredInfo is a fully namespaced piece id
type ExpiredInfo struct {
	SatelliteID storj.NodeID
	PieceID     storj.PieceID
	InPieceInfo bool
}

// PieceExpirationDB stores information about pieces with expiration dates.
type PieceExpirationDB interface {
	// GetExpired gets piece IDs that expire or have expired before the given time
	GetExpired(ctx context.Context, expiredAt time.Time, limit int64) ([]ExpiredInfo, error)
	// SetExpiration sets an expiration time for the given piece ID on the given satellite
	SetExpiration(ctx context.Context, satellite storj.NodeID, pieceID storj.PieceID, expiresAt time.Time) error
	// DeleteExpiration removes an expiration record for the given piece ID on the given satellite
	DeleteExpiration(ctx context.Context, satellite storj.NodeID, pieceID storj.PieceID) (found bool, err error)
	// DeleteFailed marks an expiration record as having experienced a failure in deleting the
	// piece from the disk
	DeleteFailed(ctx context.Context, satelliteID storj.NodeID, pieceID storj.PieceID, failedAt time.Time) error
}

// V0PieceInfoDB stores meta information about pieces stored with storage format V0 (where
// metadata goes in the "pieceinfo" table in the storagenodedb). The actual pieces are stored
// behind something providing the storage.Blobs interface.
type V0PieceInfoDB interface {
	// Get returns Info about a piece.
	Get(ctx context.Context, satelliteID storj.NodeID, pieceID storj.PieceID) (*Info, error)
	// Delete deletes Info about a piece.
	Delete(ctx context.Context, satelliteID storj.NodeID, pieceID storj.PieceID) error
	// DeleteFailed marks piece deletion from disk failed
	DeleteFailed(ctx context.Context, satelliteID storj.NodeID, pieceID storj.PieceID, failedAt time.Time) error
	// GetExpired gets piece IDs stored with storage format V0 that expire or have expired
	// before the given time
	GetExpired(ctx context.Context, expiredAt time.Time, limit int64) ([]ExpiredInfo, error)
	// ForAllV0PieceIDsOwnedBySatellite executes doForEach for each locally stored piece, stored
	// with storage format V0 in the namespace of the given satellite. If doForEach returns a
	// non-nil error, ForAllV0PieceIDsOwnedBySatellite will stop iterating and return the error
	// immediately.
	ForAllV0PieceIDsOwnedBySatellite(ctx context.Context, blobStore storage.Blobs, satellite storj.NodeID, doForEach func(StoredPieceAccess) error) error
}

// V0PieceInfoDBForTest is like V0PieceInfoDB, but adds on the Add() method so
// that test environments with V0 piece data can be set up.
type V0PieceInfoDBForTest interface {
	V0PieceInfoDB

	// Add inserts Info to the database. This is only a valid thing to do, now,
	// during tests, to replicate the environment of a storage node not yet fully
	// migrated to V1 storage.
	Add(context.Context, *Info) error
}

// StoredPieceAccess allows inspection and manipulation of a piece during iteration with
// ForAllPieceIDsOwnedBySatellite-type methods
type StoredPieceAccess interface {
	storage.StoredBlobAccess

	// PieceID gives the pieceID of the piece
	PieceID() storj.PieceID
	// Satellite gives the nodeID of the satellite which owns the piece
	Satellite() (storj.NodeID, error)
	// ContentSize gives the size of the piece content (not including the piece header, if
	// applicable)
	ContentSize(ctx context.Context) (int64, error)
	// CreationTime returns the piece creation time as given in the original PieceHash (which is
	// likely not the same as the file mtime). For non-FormatV0 pieces, this requires opening
	// the file and unmarshaling the piece header. If exact precision is not required, ModTime()
	// may be a better solution.
	CreationTime(ctx context.Context) (time.Time, error)
	// ModTime returns a less-precise piece creation time than CreationTime, but is generally
	// much faster. For non-FormatV0 pieces, this gets the piece creation time from to the
	// filesystem instead of the piece header.
	ModTime(ctx context.Context) (time.Time, error)
}

// Store implements storing pieces onto a blob storage implementation.
type Store struct {
	log            *zap.Logger
	blobs          storage.Blobs
	v0PieceInfo    V0PieceInfoDB
	expirationInfo PieceExpirationDB

	SpaceUsedLive  SpaceUsedLive
	liveSpaceMutex sync.RWMutex

	// The value of reservedSpace is always added to the return value from the
	// SpaceUsedForPieces() method.
	// The reservedSpace field is part of an unfortunate hack that enables testing of low-space
	// or no-space conditions. It is not (or should not be) used under regular operating
	// conditions.
	reservedSpace int64
}

// SpaceUsedLive stores the live totals of used space for all
// piece content (not including headers) and also total used space
// by satellites
type SpaceUsedLive struct {
	totalUsed             int64
	usedSpaceBySatellites map[storj.NodeID]int64
}

// StoreForTest is a wrapper around Store to be used only in test scenarios. It enables writing
// pieces with older storage formats and allows use of the ReserveSpace() method.
type StoreForTest struct {
	*Store
}

// NewStore creates a new piece store
func NewStore(log *zap.Logger, blobs storage.Blobs, v0PieceInfo V0PieceInfoDB, expirationInfo PieceExpirationDB) (*Store, error) {
	newStore := Store{
		log:            log,
		blobs:          blobs,
		v0PieceInfo:    v0PieceInfo,
		expirationInfo: expirationInfo,
		SpaceUsedLive:  SpaceUsedLive{},
	}
	err := newStore.InitSpaceUsedLive()
	if err != nil {
		return nil, Error.Wrap(err)
	}

	return &newStore, nil
}

// Writer returns a new piece writer.
func (store *Store) Writer(ctx context.Context, satellite storj.NodeID, pieceID storj.PieceID) (_ *Writer, err error) {
	defer mon.Task()(&ctx)(&err)
	blob, err := store.blobs.Create(ctx, storage.BlobRef{
		Namespace: satellite.Bytes(),
		Key:       pieceID.Bytes(),
	}, preallocSize.Int64())
	if err != nil {
		return nil, Error.Wrap(err)
	}

	writer, err := NewWriter(blob)
	return writer, Error.Wrap(err)
}

// WriterForFormatVersion allows opening a piece writer with a specified storage format version.
// This is meant to be used externally only in test situations (thus the StoreForTest receiver
// type).
func (store StoreForTest) WriterForFormatVersion(ctx context.Context, satellite storj.NodeID, pieceID storj.PieceID, formatVersion storage.FormatVersion) (_ *Writer, err error) {
	defer mon.Task()(&ctx)(&err)

	blobRef := storage.BlobRef{
		Namespace: satellite.Bytes(),
		Key:       pieceID.Bytes(),
	}
	var blob storage.BlobWriter
	switch formatVersion {
	case storage.FormatV0:
		fStore, ok := store.blobs.(*filestore.Store)
		if !ok {
			return nil, Error.New("can't make a WriterForFormatVersion with this blob store (%T)", store.blobs)
		}
		tStore := filestore.StoreForTest{Store: fStore}
		blob, err = tStore.CreateV0(ctx, blobRef)
	case storage.FormatV1:
		blob, err = store.blobs.Create(ctx, blobRef, preallocSize.Int64())
	default:
		return nil, Error.New("please teach me how to make V%d pieces", formatVersion)
	}
	if err != nil {
		return nil, Error.Wrap(err)
	}
	writer, err := NewWriter(blob)
	return writer, Error.Wrap(err)
}

// Reader returns a new piece reader.
func (store *Store) Reader(ctx context.Context, satellite storj.NodeID, pieceID storj.PieceID) (_ *Reader, err error) {
	defer mon.Task()(&ctx)(&err)
	blob, err := store.blobs.Open(ctx, storage.BlobRef{
		Namespace: satellite.Bytes(),
		Key:       pieceID.Bytes(),
	})
	if err != nil {
		if os.IsNotExist(err) {
			return nil, err
		}
		return nil, Error.Wrap(err)
	}

	reader, err := NewReader(blob)
	return reader, Error.Wrap(err)
}

// ReaderSpecific returns a new piece reader for a located piece, which avoids the potential
// need to check multiple storage formats to find the right blob.
func (store *Store) ReaderSpecific(ctx context.Context, satellite storj.NodeID, pieceID storj.PieceID, formatVersion storage.FormatVersion) (_ *Reader, err error) {
	defer mon.Task()(&ctx)(&err)
	ref := storage.BlobRef{Namespace: satellite.Bytes(), Key: pieceID.Bytes()}
	blob, err := store.blobs.OpenSpecific(ctx, ref, formatVersion)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, err
		}
		return nil, Error.Wrap(err)
	}

	reader, err := NewReader(blob)
	return reader, Error.Wrap(err)
}

// Delete deletes the specified piece.
func (store *Store) Delete(ctx context.Context, satellite storj.NodeID, pieceID storj.PieceID) (err error) {
	defer mon.Task()(&ctx)(&err)

	blobRef := storage.BlobRef{
		Namespace: satellite.Bytes(),
		Key:       pieceID.Bytes(),
	}

	pieceSize, err := store.getPieceSize(ctx, blobRef)
	if err != nil {
		return Error.Wrap(err)
	}

	err = store.blobs.Delete(ctx, blobRef)
	if err != nil {
		return Error.Wrap(err)
	}
	store.UpdateSpaceUsedLiveTotals(ctx, satellite, -pieceSize)

	// delete records in both the piece_expirations and pieceinfo DBs, wherever we find it.
	// both of these calls should return no error if the requested record is not found.
	if store.expirationInfo != nil {
		_, err = store.expirationInfo.DeleteExpiration(ctx, satellite, pieceID)
	}
	if store.v0PieceInfo != nil {
		err = errs.Combine(err, store.v0PieceInfo.Delete(ctx, satellite, pieceID))
	}
	return Error.Wrap(err)
}

func (store *Store) getPieceSize(ctx context.Context, blobRef storage.BlobRef) (int64, error) {
	blobAccess, err := store.blobs.Lookup(ctx, blobRef)
	if err != nil {
		return 0, err
	}
	pieceAccess, err := newStoredPieceAccess(store, blobAccess)
	if err != nil {
		return 0, err
	}
	return pieceAccess.ContentSize(ctx)
}

// UpdateSpaceUsedLiveTotals updates the live used space totals
// with a pieceSize that was either created or deleted where the pieceSize is
// only the content size and does not include header bytes
func (store *Store) UpdateSpaceUsedLiveTotals(ctx context.Context, satelliteID storj.NodeID, pieceSize int64) {
	store.liveSpaceMutex.Lock()
	defer store.liveSpaceMutex.Unlock()
	store.SpaceUsedLive.totalUsed += pieceSize
	store.SpaceUsedLive.usedSpaceBySatellites[satelliteID] += pieceSize
}

// GetV0PieceInfoDB returns this piece-store's reference to the V0 piece info DB (or nil,
// if this piece-store does not have one). This is ONLY intended for use with testing
// functionality.
func (store *Store) GetV0PieceInfoDB() V0PieceInfoDB {
	return store.v0PieceInfo
}

// ForAllPieceIDsOwnedBySatellite executes doForEach for each locally stored piece in the namespace
// of the given satellite. If doForEach returns a non-nil error, ForAllPieceIDsInNamespace will stop
// iterating and return the error immediately.
//
// Note that this method includes all locally stored pieces, both V0 and higher.
func (store *Store) ForAllPieceIDsOwnedBySatellite(ctx context.Context, satellite storj.NodeID, doForEach func(StoredPieceAccess) error) (err error) {
	defer mon.Task()(&ctx)(&err)
	// first iterate over all in V1 storage, then all in V0
	err = store.blobs.ForAllKeysInNamespace(ctx, satellite.Bytes(), func(blobAccess storage.StoredBlobAccess) error {
		if blobAccess.StorageFormatVersion() < storage.FormatV1 {
			// we'll address this piece while iterating over the V0 pieces below.
			return nil
		}
		pieceAccess, err := newStoredPieceAccess(store, blobAccess)
		if err != nil {
			// something is wrong with internals; blob storage thinks this key was stored, but
			// it is not a valid PieceID.
			return err
		}
		return doForEach(pieceAccess)
	})
	if err == nil && store.v0PieceInfo != nil {
		err = store.v0PieceInfo.ForAllV0PieceIDsOwnedBySatellite(ctx, store.blobs, satellite, doForEach)
	}
	return err
}

// GetExpired gets piece IDs that are expired and were created before the given time
func (store *Store) GetExpired(ctx context.Context, expiredAt time.Time, limit int64) (_ []ExpiredInfo, err error) {
	defer mon.Task()(&ctx)(&err)

	expired, err := store.expirationInfo.GetExpired(ctx, expiredAt, limit)
	if err != nil {
		return nil, err
	}
	if int64(len(expired)) < limit && store.v0PieceInfo != nil {
		v0Expired, err := store.v0PieceInfo.GetExpired(ctx, expiredAt, limit-int64(len(expired)))
		if err != nil {
			return nil, err
		}
		expired = append(expired, v0Expired...)
	}
	return expired, nil
}

// SetExpiration records an expiration time for the specified piece ID owned by the specified satellite
func (store *Store) SetExpiration(ctx context.Context, satellite storj.NodeID, pieceID storj.PieceID, expiresAt time.Time) (err error) {
	return store.expirationInfo.SetExpiration(ctx, satellite, pieceID, expiresAt)
}

// DeleteFailed marks piece as a failed deletion.
func (store *Store) DeleteFailed(ctx context.Context, expired ExpiredInfo, when time.Time) (err error) {
	defer mon.Task()(&ctx)(&err)

	if expired.InPieceInfo {
		return store.v0PieceInfo.DeleteFailed(ctx, expired.SatelliteID, expired.PieceID, when)
	}
	return store.expirationInfo.DeleteFailed(ctx, expired.SatelliteID, expired.PieceID, when)
}

// SpaceUsedForPiecesSlow returns the disk space used by all local pieces (both V0 and later).
// We call this method "Slow" since it iterates over each piecefile to sum the total space used
// instead of using the in-memory used space values, which is much slower.
// Important note: this metric does not include space used by piece headers, whereas
// storj/filestore/store.(*Store).SpaceUsed() includes all space used by the blobs.
//
// The value of reservedSpace for this Store is added to the result, but this should only
// affect tests (reservedSpace should always be 0 in real usage).
func (store *Store) SpaceUsedForPiecesSlow(ctx context.Context) (int64, error) {
	satellites, err := store.getAllStoringSatellites(ctx)
	if err != nil {
		return 0, err
	}
	var total int64
	for _, satellite := range satellites {
		spaceUsed, err := store.SpaceUsedBySatelliteSlow(ctx, satellite)
		if err != nil {
			return 0, err
		}
		total += spaceUsed
	}
	return total + store.reservedSpace, nil
}

func (store *Store) getAllStoringSatellites(ctx context.Context) ([]storj.NodeID, error) {
	namespaces, err := store.blobs.GetAllNamespaces(ctx)
	if err != nil {
		return nil, err
	}
	satellites := make([]storj.NodeID, len(namespaces))
	for i, namespace := range namespaces {
		satellites[i], err = storj.NodeIDFromBytes(namespace)
		if err != nil {
			return nil, err
		}
	}
	return satellites, nil
}

// SpaceUsedBySatelliteSlow calculates disk space used for local piece storage in the given
// satellite's namespace. Important note: this metric does not include space used by
// piece headers, whereas storj/filestore/store.(*Store).SpaceUsedInNamespace() does
// include all space used by the blobs.
// We call this method "Slow" since it iterates over each piecefile to sum the total space used
// instead of using the in-memory used space values, which is much slower.
func (store *Store) SpaceUsedBySatelliteSlow(ctx context.Context, satelliteID storj.NodeID) (int64, error) {
	var totalUsed int64
	err := store.ForAllPieceIDsOwnedBySatellite(ctx, satelliteID, func(access StoredPieceAccess) error {
		contentSize, statErr := access.ContentSize(ctx)
		if statErr != nil {
			store.log.Error("failed to stat", zap.Error(statErr), zap.String("pieceID", access.PieceID().String()), zap.String("satellite", satelliteID.String()))
			// keep iterating; we want a best effort total here.
			return nil
		}
		totalUsed += contentSize
		return nil
	})
	if err != nil {
		return 0, err
	}
	return totalUsed, nil
}

// SpaceUsedForPiecesAndBySatellitesSlow walks through all the pieces stored on disk
// and sums up the bytes for all pieces (not including headers) currently stored. These
// new summed up values are used for the live in memory values.
func (store *Store) SpaceUsedForPiecesAndBySatellitesSlow() (SpaceUsedLive, error) {
	satelliteIDs, err := store.getAllStoringSatellites(nil)
	if err != nil {
		return SpaceUsedLive{}, err
	}

	var totalUsed int64
	totalsBySatellites := map[storj.NodeID]int64{}
	for _, satelliteID := range satelliteIDs {
		spaceUsed, err := store.SpaceUsedBySatelliteSlow(nil, satelliteID)
		if err != nil {
			return SpaceUsedLive{}, err
		}
		totalsBySatellites[satelliteID] = spaceUsed
		totalUsed += spaceUsed
	}

	return SpaceUsedLive{
		totalUsed:             totalUsed,
		usedSpaceBySatellites: totalsBySatellites,
	}, nil
}

// InitSpaceUsedLive sets the live values for space used
// where the sums are calculated by iterating over all saved pieces on disk
// to get their size
func (store *Store) InitSpaceUsedLive() error {
	newSpaceUsedLive, err := store.SpaceUsedForPiecesAndBySatellitesSlow()
	if err != nil {
		return err
	}

	store.liveSpaceMutex.Lock()
	defer store.liveSpaceMutex.Unlock()
	store.SpaceUsedLive = newSpaceUsedLive
	return nil
}

// SpaceUsedForPiecesLive returns the current total used space for
// all pieces content (not including header bytes)
func (store *Store) SpaceUsedForPiecesLive(ctx context.Context) int64 {
	store.liveSpaceMutex.Lock()
	defer store.liveSpaceMutex.Unlock()
	return store.SpaceUsedLive.totalUsed
}

// SpaceUsedBySatelliteLive returns the current total space used for a specific
// satellite for all pieces (not including header bytes)
func (store *Store) SpaceUsedBySatelliteLive(ctx context.Context, satelliteID storj.NodeID) int64 {
	store.liveSpaceMutex.Lock()
	defer store.liveSpaceMutex.Unlock()
	return store.SpaceUsedLive.usedSpaceBySatellites[satelliteID]
}

// RecalculateSpaceUsedLive sums up the bytes for all pieces (not including headers) currently stored.
// The live values for used space are initially calculated when the storagenode starts up, then
// incrememted/decremeted when pieces are created/deleted. In addition we want to occasionally recalculate
// the live values to confirm correctness. This method RecalculateSpaceUsedLive is responsible for doing that.
func (store *Store) RecalculateSpaceUsedLive(ctx context.Context) error {
	store.liveSpaceMutex.Lock()
	// Save the current live values before we start recalculating
	// so we can compare them to what we recalculate
	spaceUsedWhenIterationStarted := store.SpaceUsedLive
	store.liveSpaceMutex.Unlock()

	// Iterate over all the pieces currently stored to get their size and sum those sizes.
	// This iteration can take a long time if there are a lot of pieces stored.
	spaceUsedResultOfIteration, err := store.SpaceUsedForPiecesAndBySatellitesSlow()
	if err != nil {
		return err
	}

	store.liveSpaceMutex.Lock()
	defer store.liveSpaceMutex.Unlock()
	// Since it might have taken a long time to iterate over all the pieces, here we need to check if
	// we missed any additions/deletions of pieces while we were iterating and add those bytes to
	// the space used result of iteration.
	store.SpaceUsedLive.estimateAndSave(spaceUsedWhenIterationStarted, spaceUsedResultOfIteration)

	return nil
}

// estimateAndSave estimates how many bytes were missed for writes/deletes while iterating over pieces
// to sum size. Then we save that estimation along with the result of the iteration to the live in-memory
// used space values.
func (live *SpaceUsedLive) estimateAndSave(oldSpaceUsedLive, newSpaceUsed SpaceUsedLive) {
	estimatedTotalsBySatellites := map[storj.NodeID]int64{}
	for satelliteID, newSpaceUsedTotal := range newSpaceUsed.usedSpaceBySatellites {
		estimatedTotalsBySatellites[satelliteID] = estimateRecalculation(newSpaceUsedTotal,
			oldSpaceUsedLive.usedSpaceBySatellites[satelliteID],
			live.usedSpaceBySatellites[satelliteID],
		)
	}

	live.usedSpaceBySatellites = estimatedTotalsBySatellites
	live.totalUsed = estimateRecalculation(newSpaceUsed.totalUsed,
		oldSpaceUsedLive.totalUsed,
		live.totalUsed,
	)
}

func estimateRecalculation(newSpaceUsedTotal, spaceUsedWhenIterationStarted, spaceUsedWhenIterationEnded int64) int64 {
	// If the new space used result from the iterations matches the
	// live value of space used when the iteration ended, then there is no
	// missed bytes during the iteration
	if newSpaceUsedTotal == spaceUsedWhenIterationEnded {
		return newSpaceUsedTotal
	}

	// If we missed writes/deletes while iterating, we will assume that half of those missed occurred before
	// the iteration and half occurred after. So here we add half of the delta to the result space used totals
	// from the iteration to account for those missed.
	spaceUsedDeltaDuringIteration := spaceUsedWhenIterationStarted - spaceUsedWhenIterationEnded
	return newSpaceUsedTotal + (spaceUsedDeltaDuringIteration / 2)
}

// ReserveSpace marks some amount of free space as used, even if it's not, so that future calls
// to SpaceUsedForPieces() are raised by this amount. Calls to ReserveSpace invalidate earlier
// calls, so ReserveSpace(0) undoes all prior space reservation. This should only be used in
// test scenarios.
func (store StoreForTest) ReserveSpace(amount int64) {
	store.reservedSpace = amount
}

// StorageStatus contains information about the disk store is using.
type StorageStatus struct {
	DiskUsed int64
	DiskFree int64
}

// StorageStatus returns information about the disk.
func (store *Store) StorageStatus(ctx context.Context) (_ StorageStatus, err error) {
	defer mon.Task()(&ctx)(&err)
	diskFree, err := store.blobs.FreeSpace()
	if err != nil {
		return StorageStatus{}, err
	}
	return StorageStatus{
		DiskUsed: -1, // TODO set value
		DiskFree: diskFree,
	}, nil
}

type storedPieceAccess struct {
	storage.StoredBlobAccess
	store   *Store
	pieceID storj.PieceID
}

func newStoredPieceAccess(store *Store, blobAccess storage.StoredBlobAccess) (storedPieceAccess, error) {
	pieceID, err := storj.PieceIDFromBytes(blobAccess.BlobRef().Key)
	if err != nil {
		return storedPieceAccess{}, err
	}
	return storedPieceAccess{
		StoredBlobAccess: blobAccess,
		store:            store,
		pieceID:          pieceID,
	}, nil
}

// PieceID returns the piece ID of the piece
func (access storedPieceAccess) PieceID() storj.PieceID {
	return access.pieceID
}

// Satellite returns the satellite ID that owns the piece
func (access storedPieceAccess) Satellite() (storj.NodeID, error) {
	return storj.NodeIDFromBytes(access.BlobRef().Namespace)
}

// ContentSize gives the size of the piece content (not including the piece header, if applicable)
func (access storedPieceAccess) ContentSize(ctx context.Context) (size int64, err error) {
	defer mon.Task()(&ctx)(&err)
	stat, err := access.Stat(ctx)
	if err != nil {
		return 0, err
	}
	size = stat.Size()
	if access.StorageFormatVersion() >= storage.FormatV1 {
		size -= V1PieceHeaderReservedArea
	}
	return size, nil
}

// CreationTime returns the piece creation time as given in the original PieceHash (which is likely
// not the same as the file mtime). This requires opening the file and unmarshaling the piece
// header. If exact precision is not required, ModTime() may be a better solution.
func (access storedPieceAccess) CreationTime(ctx context.Context) (cTime time.Time, err error) {
	defer mon.Task()(&ctx)(&err)
	satellite, err := access.Satellite()
	if err != nil {
		return time.Time{}, err
	}
	reader, err := access.store.ReaderSpecific(ctx, satellite, access.PieceID(), access.StorageFormatVersion())
	if err != nil {
		return time.Time{}, err
	}
	header, err := reader.GetPieceHeader()
	if err != nil {
		return time.Time{}, err
	}
	return header.CreationTime, nil
}

// ModTime returns a less-precise piece creation time than CreationTime, but is generally
// much faster. This gets the piece creation time from to the filesystem instead of the
// piece header.
func (access storedPieceAccess) ModTime(ctx context.Context) (mTime time.Time, err error) {
	defer mon.Task()(&ctx)(&err)
	stat, err := access.Stat(ctx)
	if err != nil {
		return time.Time{}, err
	}
	return stat.ModTime(), nil
}
