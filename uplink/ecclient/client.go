// Copyright (C) 2019 Storj Labs, Inc.
// See LICENSE for copying information.

package ecclient

import (
	"context"
	"io"
	"io/ioutil"
	"sort"
	"sync/atomic"
	"time"

	"github.com/zeebo/errs"
	"go.uber.org/zap"
	monkit "gopkg.in/spacemonkeygo/monkit.v2"

	"storj.io/storj/internal/sync2"
	"storj.io/storj/pkg/pb"
	"storj.io/storj/pkg/ranger"
	"storj.io/storj/pkg/storj"
	"storj.io/storj/pkg/transport"
	"storj.io/storj/uplink/eestream"
	"storj.io/storj/uplink/piecestore"
)

var mon = monkit.Package()

// Client defines an interface for storing erasure coded data to piece store nodes
type Client interface {
	Put(ctx context.Context, limits []*pb.AddressedOrderLimit, privateKey storj.PiecePrivateKey, rs eestream.RedundancyStrategy, data io.Reader, expiration time.Time) (successfulNodes []*pb.Node, successfulHashes []*pb.PieceHash, err error)
	Repair(ctx context.Context, limits []*pb.AddressedOrderLimit, privateKey storj.PiecePrivateKey, rs eestream.RedundancyStrategy, data io.Reader, expiration time.Time, timeout time.Duration, path storj.Path) (successfulNodes []*pb.Node, successfulHashes []*pb.PieceHash, err error)
	Get(ctx context.Context, limits []*pb.AddressedOrderLimit, privateKey storj.PiecePrivateKey, es eestream.ErasureScheme, size int64) (ranger.Ranger, error)
	Delete(ctx context.Context, limits []*pb.AddressedOrderLimit, privateKey storj.PiecePrivateKey) error
	WithForceErrorDetection(force bool) Client
}

type dialPiecestoreFunc func(context.Context, *pb.Node) (*piecestore.Client, error)

type ecClient struct {
	log                 *zap.Logger
	transport           transport.Client
	memoryLimit         int
	forceErrorDetection bool
}

// NewClient from the given identity and max buffer memory
func NewClient(log *zap.Logger, tc transport.Client, memoryLimit int) Client {
	return &ecClient{
		log:         log,
		transport:   tc,
		memoryLimit: memoryLimit,
	}
}

func (ec *ecClient) WithForceErrorDetection(force bool) Client {
	ec.forceErrorDetection = force
	return ec
}

func (ec *ecClient) dialPiecestore(ctx context.Context, n *pb.Node) (*piecestore.Client, error) {
	logger := ec.log.Named(n.Id.String())
	return piecestore.Dial(ctx, ec.transport, n, logger, piecestore.DefaultConfig)
}

func (ec *ecClient) Put(ctx context.Context, limits []*pb.AddressedOrderLimit, privateKey storj.PiecePrivateKey, rs eestream.RedundancyStrategy, data io.Reader, expiration time.Time) (successfulNodes []*pb.Node, successfulHashes []*pb.PieceHash, err error) {
	defer mon.Task()(&ctx)(&err)

	if len(limits) != rs.TotalCount() {
		return nil, nil, Error.New("size of limits slice (%d) does not match total count (%d) of erasure scheme", len(limits), rs.TotalCount())
	}

	nonNilLimits := nonNilCount(limits)
	if nonNilLimits <= rs.RepairThreshold() && nonNilLimits < rs.OptimalThreshold() {
		return nil, nil, Error.New("number of non-nil limits (%d) is less than or equal to the repair threshold (%d) of erasure scheme", nonNilLimits, rs.RepairThreshold())
	}

	if !unique(limits) {
		return nil, nil, Error.New("duplicated nodes are not allowed")
	}

	ec.log.Sugar().Debugf("Uploading to storage nodes using ErasureShareSize: %d StripeSize: %d RepairThreshold: %d OptimalThreshold: %d",
		rs.ErasureShareSize(), rs.StripeSize(), rs.RepairThreshold(), rs.OptimalThreshold())

	padded := eestream.PadReader(ioutil.NopCloser(data), rs.StripeSize())
	readers, err := eestream.EncodeReader(ctx, ec.log, padded, rs)
	if err != nil {
		return nil, nil, err
	}

	type info struct {
		i    int
		err  error
		hash *pb.PieceHash
	}
	infos := make(chan info, len(limits))

	psCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	for i, addressedLimit := range limits {
		go func(i int, addressedLimit *pb.AddressedOrderLimit) {
			hash, err := ec.putPiece(psCtx, ctx, addressedLimit, privateKey, readers[i], expiration)
			infos <- info{i: i, err: err, hash: hash}
		}(i, addressedLimit)
	}

	successfulNodes = make([]*pb.Node, len(limits))
	successfulHashes = make([]*pb.PieceHash, len(limits))
	var successfulCount int32

	for range limits {
		info := <-infos

		if limits[info.i] == nil {
			continue
		}

		if info.err != nil {
			ec.log.Sugar().Debugf("Upload to storage node %s failed: %v", limits[info.i].GetLimit().StorageNodeId, info.err)
			continue
		}

		successfulNodes[info.i] = &pb.Node{
			Id:      limits[info.i].GetLimit().StorageNodeId,
			Address: limits[info.i].GetStorageNodeAddress(),
		}
		successfulHashes[info.i] = info.hash

		atomic.AddInt32(&successfulCount, 1)

		if int(successfulCount) >= rs.OptimalThreshold() {
			ec.log.Sugar().Infof("Success threshold (%d nodes) reached. Cancelling remaining uploads.", rs.OptimalThreshold())
			cancel()
		}
	}

	defer func() {
		select {
		case <-ctx.Done():
			err = errs.Combine(
				Error.New("upload cancelled by user"),
				// TODO: clean up the partially uploaded segment's pieces
				// ec.Delete(context.Background(), nodes, pieceID, pba.SatelliteId),
			)
		default:
		}
	}()

	successes := int(atomic.LoadInt32(&successfulCount))
	if successes <= rs.RepairThreshold() && successes < rs.OptimalThreshold() {
		return nil, nil, Error.New("successful puts (%d) less than or equal to repair threshold (%d)", successes, rs.RepairThreshold())
	}

	if successes < rs.OptimalThreshold() {
		return nil, nil, Error.New("successful puts (%d) less than success threshold (%d)", successes, rs.OptimalThreshold())
	}

	return successfulNodes, successfulHashes, nil
}

func (ec *ecClient) Repair(ctx context.Context, limits []*pb.AddressedOrderLimit, privateKey storj.PiecePrivateKey, rs eestream.RedundancyStrategy, data io.Reader, expiration time.Time, timeout time.Duration, path storj.Path) (successfulNodes []*pb.Node, successfulHashes []*pb.PieceHash, err error) {
	defer mon.Task()(&ctx)(&err)

	if len(limits) != rs.TotalCount() {
		return nil, nil, Error.New("size of limits slice (%d) does not match total count (%d) of erasure scheme", len(limits), rs.TotalCount())
	}

	if !unique(limits) {
		return nil, nil, Error.New("duplicated nodes are not allowed")
	}

	padded := eestream.PadReader(ioutil.NopCloser(data), rs.StripeSize())
	readers, err := eestream.EncodeReader(ctx, ec.log, padded, rs)
	if err != nil {
		return nil, nil, err
	}

	type info struct {
		i    int
		err  error
		hash *pb.PieceHash
	}
	infos := make(chan info, len(limits))

	psCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	for i, addressedLimit := range limits {
		go func(i int, addressedLimit *pb.AddressedOrderLimit) {
			hash, err := ec.putPiece(psCtx, ctx, addressedLimit, privateKey, readers[i], expiration)
			infos <- info{i: i, err: err, hash: hash}
		}(i, addressedLimit)
	}

	ec.log.Sugar().Infof("Starting a timer for %s for repairing %s up to %d nodes to try to have a number of pieces around the successful threshold (%d)",
		timeout, path, nonNilCount(limits), rs.OptimalThreshold())

	var successfulCount int32
	timer := time.AfterFunc(timeout, func() {
		if ctx.Err() != context.Canceled {
			ec.log.Sugar().Infof("Timer expired. Successfully repaired %s to %d nodes. Canceling the long tail...", path, atomic.LoadInt32(&successfulCount))
			cancel()
		}
	})

	successfulNodes = make([]*pb.Node, len(limits))
	successfulHashes = make([]*pb.PieceHash, len(limits))

	for range limits {
		info := <-infos

		if limits[info.i] == nil {
			continue
		}

		if info.err != nil {
			ec.log.Sugar().Debugf("Repair %s to storage node %s failed: %v", path, limits[info.i].GetLimit().StorageNodeId, info.err)
			continue
		}

		successfulNodes[info.i] = &pb.Node{
			Id:      limits[info.i].GetLimit().StorageNodeId,
			Address: limits[info.i].GetStorageNodeAddress(),
		}
		successfulHashes[info.i] = info.hash
		atomic.AddInt32(&successfulCount, 1)
	}

	// Ensure timer is stopped
	_ = timer.Stop()

	// TODO: clean up the partially uploaded segment's pieces
	defer func() {
		select {
		case <-ctx.Done():
			err = errs.Combine(
				Error.New("repair cancelled"),
				// ec.Delete(context.Background(), nodes, pieceID, pba.SatelliteId), //TODO
			)
		default:
		}
	}()

	if atomic.LoadInt32(&successfulCount) == 0 {
		return nil, nil, Error.New("repair %v to all nodes failed", path)
	}

	ec.log.Sugar().Infof("Successfully repaired %s to %d nodes.", path, atomic.LoadInt32(&successfulCount))

	return successfulNodes, successfulHashes, nil
}

func (ec *ecClient) putPiece(ctx, parent context.Context, limit *pb.AddressedOrderLimit, privateKey storj.PiecePrivateKey, data io.ReadCloser, expiration time.Time) (hash *pb.PieceHash, err error) {
	nodeName := "nil"
	if limit != nil {
		nodeName = limit.GetLimit().StorageNodeId.String()[0:8]
	}
	defer mon.Task()(&ctx, "node: "+nodeName)(&err)
	defer func() { err = errs.Combine(err, data.Close()) }()

	if limit == nil {
		_, _ = io.Copy(ioutil.Discard, data)
		return nil, nil
	}

	storageNodeID := limit.GetLimit().StorageNodeId
	pieceID := limit.GetLimit().PieceId
	ps, err := ec.dialPiecestore(ctx, &pb.Node{
		Id:      storageNodeID,
		Address: limit.GetStorageNodeAddress(),
	})
	if err != nil {
		ec.log.Debug("Failed dialing for putting piece to node",
			zap.String("pieceID", pieceID.String()),
			zap.String("nodeID", storageNodeID.String()),
			zap.Error(err),
		)
		return nil, err
	}
	defer func() { err = errs.Combine(err, ps.Close()) }()

	upload, err := ps.Upload(ctx, limit.GetLimit(), privateKey)
	if err != nil {
		ec.log.Debug("Failed requesting upload of pieces to node",
			zap.String("pieceID", pieceID.String()),
			zap.String("nodeID", storageNodeID.String()),
			zap.Error(err),
		)
		return nil, err
	}
	defer func() {
		if ctx.Err() != nil || err != nil {
			hash = nil
			err = errs.Combine(err, upload.Cancel(ctx))
			return
		}
		h, closeErr := upload.Commit(ctx)
		hash = h
		err = errs.Combine(err, closeErr)
	}()

	_, err = sync2.Copy(ctx, upload, data)
	// Canceled context means the piece upload was interrupted by user or due
	// to slow connection. No error logging for this case.
	if ctx.Err() == context.Canceled {
		if parent.Err() == context.Canceled {
			ec.log.Sugar().Infof("Upload to node %s canceled by user.", storageNodeID)
		} else {
			ec.log.Sugar().Debugf("Node %s cut from upload due to slow connection.", storageNodeID)
		}
		err = context.Canceled
	} else if err != nil {
		nodeAddress := "nil"
		if limit.GetStorageNodeAddress() != nil {
			nodeAddress = limit.GetStorageNodeAddress().GetAddress()
		}

		ec.log.Debug("Failed uploading piece to node",
			zap.String("pieceID", pieceID.String()),
			zap.String("nodeID", storageNodeID.String()),
			zap.String("nodeAddress", nodeAddress),
			zap.Error(err),
		)
	}

	return hash, err
}

func (ec *ecClient) Get(ctx context.Context, limits []*pb.AddressedOrderLimit, privateKey storj.PiecePrivateKey, es eestream.ErasureScheme, size int64) (rr ranger.Ranger, err error) {
	defer mon.Task()(&ctx)(&err)

	if len(limits) != es.TotalCount() {
		return nil, Error.New("size of limits slice (%d) does not match total count (%d) of erasure scheme", len(limits), es.TotalCount())
	}

	if nonNilCount(limits) < es.RequiredCount() {
		return nil, Error.New("number of non-nil limits (%d) is less than required count (%d) of erasure scheme", nonNilCount(limits), es.RequiredCount())
	}

	paddedSize := calcPadded(size, es.StripeSize())
	pieceSize := paddedSize / int64(es.RequiredCount())

	rrs := map[int]ranger.Ranger{}
	for i, addressedLimit := range limits {
		if addressedLimit == nil {
			continue
		}

		rrs[i] = &lazyPieceRanger{
			dialPiecestore: ec.dialPiecestore,
			limit:          addressedLimit,
			privateKey:     privateKey,
			size:           pieceSize,
		}
	}

	rr, err = eestream.Decode(ec.log, rrs, es, ec.memoryLimit, ec.forceErrorDetection)
	if err != nil {
		return nil, Error.Wrap(err)
	}

	ranger, err := eestream.Unpad(rr, int(paddedSize-size))
	return ranger, Error.Wrap(err)
}

func (ec *ecClient) Delete(ctx context.Context, limits []*pb.AddressedOrderLimit, privateKey storj.PiecePrivateKey) (err error) {
	defer mon.Task()(&ctx)(&err)

	errch := make(chan error, len(limits))
	for _, addressedLimit := range limits {
		if addressedLimit == nil {
			errch <- nil
			continue
		}

		go func(addressedLimit *pb.AddressedOrderLimit) {
			limit := addressedLimit.GetLimit()
			ps, err := ec.dialPiecestore(ctx, &pb.Node{
				Id:      limit.StorageNodeId,
				Address: addressedLimit.GetStorageNodeAddress(),
			})
			if err != nil {
				ec.log.Sugar().Errorf("Failed dialing for deleting piece %s from node %s: %v", limit.PieceId, limit.StorageNodeId, err)
				errch <- err
				return
			}
			err = ps.Delete(ctx, limit, privateKey)
			err = errs.Combine(err, ps.Close())
			if err != nil {
				ec.log.Sugar().Errorf("Failed deleting piece %s from node %s: %v", limit.PieceId, limit.StorageNodeId, err)
			}
			errch <- err
		}(addressedLimit)
	}

	allerrs := collectErrors(errch, len(limits))
	if len(allerrs) > 0 && len(allerrs) == len(limits) {
		return allerrs[0]
	}

	return nil
}

func collectErrors(errs <-chan error, size int) []error {
	var result []error
	for i := 0; i < size; i++ {
		err := <-errs
		if err != nil {
			result = append(result, err)
		}
	}
	return result
}

func unique(limits []*pb.AddressedOrderLimit) bool {
	if len(limits) < 2 {
		return true
	}
	ids := make(storj.NodeIDList, len(limits))
	for i, addressedLimit := range limits {
		if addressedLimit != nil {
			ids[i] = addressedLimit.GetLimit().StorageNodeId
		}
	}

	// sort the ids and check for identical neighbors
	sort.Sort(ids)
	// sort.Slice(ids, func(i, k int) bool { return ids[i].Less(ids[k]) })
	for i := 1; i < len(ids); i++ {
		if ids[i] != (storj.NodeID{}) && ids[i] == ids[i-1] {
			return false
		}
	}

	return true
}

func calcPadded(size int64, blockSize int) int64 {
	mod := size % int64(blockSize)
	if mod == 0 {
		return size
	}
	return size + int64(blockSize) - mod
}

type lazyPieceRanger struct {
	dialPiecestore dialPiecestoreFunc
	limit          *pb.AddressedOrderLimit
	privateKey     storj.PiecePrivateKey
	size           int64
}

// Size implements Ranger.Size
func (lr *lazyPieceRanger) Size() int64 {
	return lr.size
}

// Range implements Ranger.Range to be lazily connected
func (lr *lazyPieceRanger) Range(ctx context.Context, offset, length int64) (_ io.ReadCloser, err error) {
	defer mon.Task()(&ctx)(&err)
	ps, err := lr.dialPiecestore(ctx, &pb.Node{
		Id:      lr.limit.GetLimit().StorageNodeId,
		Address: lr.limit.GetStorageNodeAddress(),
	})
	if err != nil {
		return nil, err
	}

	download, err := ps.Download(ctx, lr.limit.GetLimit(), lr.privateKey, offset, length)
	if err != nil {
		return nil, errs.Combine(err, ps.Close())
	}
	return &clientCloser{download, ps}, nil
}

type clientCloser struct {
	piecestore.Downloader
	client *piecestore.Client
}

func (client *clientCloser) Close() error {
	return errs.Combine(
		client.Downloader.Close(),
		client.client.Close(),
	)
}

func nonNilCount(limits []*pb.AddressedOrderLimit) int {
	total := 0
	for _, limit := range limits {
		if limit != nil {
			total++
		}
	}
	return total
}
