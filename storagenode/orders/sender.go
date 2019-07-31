// Copyright (C) 2019 Storj Labs, Inc.
// See LICENSE for copying information.

package orders

import (
	"context"
	"io"
	"time"

	"github.com/zeebo/errs"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
	monkit "gopkg.in/spacemonkeygo/monkit.v2"

	"storj.io/storj/internal/sync2"
	"storj.io/storj/pkg/pb"
	"storj.io/storj/pkg/storj"
	"storj.io/storj/pkg/transport"
	"storj.io/storj/storagenode/trust"
)

var (
	// OrderError represents errors with orders
	OrderError = errs.Class("order")

	mon = monkit.Package()
)

// Info contains full information about an order.
type Info struct {
	Limit *pb.OrderLimit
	Order *pb.Order
}

// ArchivedInfo contains full information about an archived order.
type ArchivedInfo struct {
	Limit *pb.OrderLimit
	Order *pb.Order

	Status     Status
	ArchivedAt time.Time
}

// Status is the archival status of the order.
type Status byte

// Statuses for satellite responses.
const (
	StatusUnsent Status = iota
	StatusAccepted
	StatusRejected
)

// ArchiveRequest defines arguments for archiving a single order.
type ArchiveRequest struct {
	Satellite storj.NodeID
	Serial    storj.SerialNumber
	Status    Status
}

// DB implements storing orders for sending to the satellite.
type DB interface {
	// Enqueue inserts order to the list of orders needing to be sent to the satellite.
	Enqueue(ctx context.Context, info *Info) error
	// ListUnsent returns orders that haven't been sent yet.
	ListUnsent(ctx context.Context, limit int) ([]*Info, error)
	// ListUnsentBySatellite returns orders that haven't been sent yet grouped by satellite.
	ListUnsentBySatellite(ctx context.Context) (map[storj.NodeID][]*Info, error)

	// Archive marks order as being handled.
	Archive(ctx context.Context, requests ...ArchiveRequest) error
	// ListArchived returns orders that have been sent.
	ListArchived(ctx context.Context, limit int) ([]*ArchivedInfo, error)
}

// SenderConfig defines configuration for sending orders.
type SenderConfig struct {
	Interval time.Duration `help:"duration between sending" default:"1h0m0s"`
	Timeout  time.Duration `help:"timeout for sending" default:"1h0m0s"`
}

// Sender sends every interval unsent orders to the satellite.
type Sender struct {
	log    *zap.Logger
	config SenderConfig

	transport transport.Client
	orders    DB
	trust     *trust.Pool

	Loop sync2.Cycle
}

// NewSender creates an order sender.
func NewSender(log *zap.Logger, transport transport.Client, orders DB, trust *trust.Pool, config SenderConfig) *Sender {
	return &Sender{
		log:       log,
		transport: transport,
		orders:    orders,
		config:    config,
		trust:     trust,

		Loop: *sync2.NewCycle(config.Interval),
	}
}

// Run sends orders on every interval to the appropriate satellites.
func (sender *Sender) Run(ctx context.Context) (err error) {
	defer mon.Task()(&ctx)(&err)
	return sender.Loop.Run(ctx, sender.runOnce)
}

func (sender *Sender) runOnce(ctx context.Context) (err error) {
	defer mon.Task()(&ctx)(&err)
	sender.log.Debug("sending")

	const batchSize = 1000

	ordersBySatellite, err := sender.orders.ListUnsentBySatellite(ctx)
	if err != nil {
		sender.log.Error("listing orders", zap.Error(err))
		return nil
	}

	requests := make(chan ArchiveRequest, batchSize)
	var batchGroup errgroup.Group
	batchGroup.Go(func() error { return sender.handleBatches(ctx, requests) })

	if len(ordersBySatellite) > 0 {
		var group errgroup.Group
		ctx, cancel := context.WithTimeout(ctx, sender.config.Timeout)
		defer cancel()

		for satelliteID, orders := range ordersBySatellite {
			satelliteID, orders := satelliteID, orders
			group.Go(func() error {
				sender.Settle(ctx, satelliteID, orders, requests)
				return nil
			})
		}

		_ = group.Wait() // doesn't return errors
	} else {
		sender.log.Debug("no orders to send")
	}

	close(requests)
	return batchGroup.Wait()
}

// Settle uploads orders to the satellite.
func (sender *Sender) Settle(ctx context.Context, satelliteID storj.NodeID, orders []*Info, requests chan ArchiveRequest) {
	log := sender.log.Named(satelliteID.String())
	err := sender.settle(ctx, log, satelliteID, orders, requests)
	if err != nil {
		log.Error("failed to settle orders", zap.Error(err))
	}
}

func (sender *Sender) handleBatches(ctx context.Context, requests chan ArchiveRequest) (err error) {
	defer mon.Task()(&ctx)(&err)

	// In case anything goes wrong, discard everything from the channel.
	defer func() {
		for range requests {
		}
	}()

	buffer := make([]ArchiveRequest, 0, cap(requests))

	for request := range requests {
		buffer = append(buffer, request)
		if len(buffer) < cap(buffer) {
			continue
		}

		if err := sender.orders.Archive(ctx, buffer...); err != nil {
			return err
		}
		buffer = buffer[:0]
	}

	if len(buffer) > 0 {
		return sender.orders.Archive(ctx, buffer...)
	}
	return nil
}

func (sender *Sender) settle(ctx context.Context, log *zap.Logger, satelliteID storj.NodeID, orders []*Info, requests chan ArchiveRequest) (err error) {
	defer mon.Task()(&ctx)(&err)

	log.Info("sending", zap.Int("count", len(orders)))
	defer log.Info("finished")

	address, err := sender.trust.GetAddress(ctx, satelliteID)
	if err != nil {
		return OrderError.New("unable to get satellite address: %v", err)
	}
	satellite := pb.Node{
		Id: satelliteID,
		Address: &pb.NodeAddress{
			Transport: pb.NodeTransport_TCP_TLS_GRPC,
			Address:   address,
		},
	}

	conn, err := sender.transport.DialNode(ctx, &satellite)
	if err != nil {
		return OrderError.New("unable to connect to the satellite: %v", err)
	}
	defer func() {
		if cerr := conn.Close(); cerr != nil {
			err = errs.Combine(err, OrderError.New("failed to close connection: %v", cerr))
		}
	}()

	client, err := pb.NewOrdersClient(conn).Settlement(ctx)
	if err != nil {
		return OrderError.New("failed to start settlement: %v", err)
	}

	var group errgroup.Group
	group.Go(func() error {
		for _, order := range orders {
			err := client.Send(&pb.SettlementRequest{
				Limit: order.Limit,
				Order: order.Order,
			})
			if err != nil {
				return err
			}
		}
		return client.CloseSend()
	})

	var errList errs.Group
	errHandle := func(cls errs.Class, format string, args ...interface{}) {
		log.Sugar().Errorf(format, args...)
		errList.Add(cls.New(format, args...))
	}
	for {
		response, err := client.Recv()
		if err != nil {
			if err == io.EOF {
				break
			}

			errHandle(OrderError, "failed to receive response: %v", err)
			break
		}

		var status Status
		switch response.Status {
		case pb.SettlementResponse_ACCEPTED:
			status = StatusAccepted
		case pb.SettlementResponse_REJECTED:
			status = StatusRejected
		default:
			errHandle(OrderError, "unexpected response: %v", response.Status)
			continue
		}

		requests <- ArchiveRequest{
			Satellite: satelliteID,
			Serial:    response.SerialNumber,
			Status:    status,
		}
	}

	if err := group.Wait(); err != nil {
		errHandle(OrderError, "sending agreements returned an error: %v", err)
	}

	return errList.Err()
}

// Close stops the sending service.
func (sender *Sender) Close() error {
	sender.Loop.Close()
	return nil
}
