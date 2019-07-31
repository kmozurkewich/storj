// Copyright (C) 2019 Storj Labs, Inc.
// See LICENSE for copying information.

package overlay_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest"

	"storj.io/storj/internal/testcontext"
	"storj.io/storj/internal/testplanet"
	"storj.io/storj/internal/testrand"
	"storj.io/storj/pkg/pb"
	"storj.io/storj/pkg/storj"
	"storj.io/storj/satellite"
	"storj.io/storj/satellite/overlay"
	"storj.io/storj/satellite/satellitedb/satellitedbtest"
)

func TestCache_Database(t *testing.T) {
	t.Parallel()

	satellitedbtest.Run(t, func(t *testing.T, db satellite.DB) {
		ctx := testcontext.New(t)
		defer ctx.Cleanup()

		testCache(ctx, t, db.OverlayCache())
	})
}

// returns a NodeSelectionConfig with sensible test values
func testNodeSelectionConfig(auditCount int64, newNodePercentage float64, distinctIP bool) overlay.NodeSelectionConfig {
	return overlay.NodeSelectionConfig{
		UptimeCount:       0,
		AuditCount:        auditCount,
		NewNodePercentage: newNodePercentage,
		OnlineWindow:      time.Hour,
		DistinctIP:        distinctIP,

		AuditReputationRepairWeight:  1,
		AuditReputationUplinkWeight:  1,
		AuditReputationAlpha0:        1,
		AuditReputationBeta0:         0,
		AuditReputationLambda:        1,
		AuditReputationWeight:        1,
		AuditReputationDQ:            0.5,
		UptimeReputationRepairWeight: 1,
		UptimeReputationUplinkWeight: 1,
		UptimeReputationAlpha0:       1,
		UptimeReputationBeta0:        0,
		UptimeReputationLambda:       1,
		UptimeReputationWeight:       1,
		UptimeReputationDQ:           0.5,
	}
}

func testCache(ctx context.Context, t *testing.T, store overlay.DB) {
	valid1ID := testrand.NodeID()
	valid2ID := testrand.NodeID()
	valid3ID := testrand.NodeID()
	missingID := testrand.NodeID()
	address := &pb.NodeAddress{Address: "127.0.0.1:0"}

	nodeSelectionConfig := testNodeSelectionConfig(0, 0, false)
	cacheConfig := overlay.Config{Node: nodeSelectionConfig, UpdateStatsBatchSize: 100}
	cache := overlay.NewCache(zaptest.NewLogger(t), store, cacheConfig)

	{ // Put
		err := cache.Put(ctx, valid1ID, pb.Node{Id: valid1ID, Address: address})
		require.NoError(t, err)

		err = cache.Put(ctx, valid2ID, pb.Node{Id: valid2ID, Address: address})
		require.NoError(t, err)

		err = cache.Put(ctx, valid3ID, pb.Node{Id: valid3ID, Address: address})
		require.NoError(t, err)

		_, err = cache.UpdateUptime(ctx, valid3ID, false)
		require.NoError(t, err)
	}

	{ // Get
		_, err := cache.Get(ctx, storj.NodeID{})
		require.Error(t, err)
		require.True(t, err == overlay.ErrEmptyNode)

		valid1, err := cache.Get(ctx, valid1ID)
		require.NoError(t, err)
		require.Equal(t, valid1.Id, valid1ID)

		valid2, err := cache.Get(ctx, valid2ID)
		require.NoError(t, err)
		require.Equal(t, valid2.Id, valid2ID)

		invalid2, err := cache.Get(ctx, missingID)
		require.Error(t, err)
		require.True(t, overlay.ErrNodeNotFound.Has(err))
		require.Nil(t, invalid2)

		// TODO: add erroring database test
	}

	{ // Paginate

		// should return two nodes
		nodes, more, err := cache.Paginate(ctx, 0, 2)
		assert.NotNil(t, more)
		assert.NoError(t, err)
		assert.Equal(t, len(nodes), 2)

		// should return no nodes
		zero, more, err := cache.Paginate(ctx, 0, 0)
		assert.NoError(t, err)
		assert.NotNil(t, more)
		assert.NotEqual(t, len(zero), 0)
	}

	{ // PaginateQualified

		// should return two nodes
		nodes, more, err := cache.PaginateQualified(ctx, 0, 3)
		assert.NotNil(t, more)
		assert.NoError(t, err)
		assert.Equal(t, len(nodes), 2)
	}

	{ // Reputation
		valid1, err := cache.Get(ctx, valid1ID)
		require.NoError(t, err)
		require.EqualValues(t, valid1.Id, valid1ID)
		require.EqualValues(t, valid1.Reputation.AuditReputationAlpha, nodeSelectionConfig.AuditReputationAlpha0)
		require.EqualValues(t, valid1.Reputation.AuditReputationBeta, nodeSelectionConfig.AuditReputationBeta0)
		require.EqualValues(t, valid1.Reputation.UptimeReputationAlpha, nodeSelectionConfig.UptimeReputationAlpha0)
		require.EqualValues(t, valid1.Reputation.UptimeReputationBeta, nodeSelectionConfig.UptimeReputationBeta0)
		require.Nil(t, valid1.Reputation.Disqualified)

		stats, err := cache.UpdateStats(ctx, &overlay.UpdateRequest{
			NodeID:       valid1ID,
			IsUp:         true,
			AuditSuccess: false,
		})
		require.NoError(t, err)
		newAuditAlpha := 1
		newAuditBeta := 1
		newUptimeAlpha := 2
		newUptimeBeta := 0
		require.EqualValues(t, stats.AuditReputationAlpha, newAuditAlpha)
		require.EqualValues(t, stats.AuditReputationBeta, newAuditBeta)
		require.EqualValues(t, stats.UptimeReputationAlpha, newUptimeAlpha)
		require.EqualValues(t, stats.UptimeReputationBeta, newUptimeBeta)
		require.NotNil(t, stats.Disqualified)
		require.True(t, time.Now().UTC().Sub(*stats.Disqualified) < time.Minute)

		stats, err = cache.UpdateUptime(ctx, valid2ID, false)
		require.NoError(t, err)
		newUptimeAlpha = 1
		newUptimeBeta = 1
		require.EqualValues(t, stats.AuditReputationAlpha, nodeSelectionConfig.AuditReputationAlpha0)
		require.EqualValues(t, stats.AuditReputationBeta, nodeSelectionConfig.AuditReputationBeta0)
		require.EqualValues(t, stats.UptimeReputationAlpha, newUptimeAlpha)
		require.EqualValues(t, stats.UptimeReputationBeta, newUptimeBeta)
		require.NotNil(t, stats.Disqualified)
		require.True(t, time.Now().UTC().Sub(*stats.Disqualified) < time.Minute)
		dqTime := *stats.Disqualified

		// should not update once already disqualified
		_, err = cache.BatchUpdateStats(ctx, []*overlay.UpdateRequest{{
			NodeID:       valid2ID,
			IsUp:         false,
			AuditSuccess: true,
		}})
		require.NoError(t, err)
		dossier, err := cache.Get(ctx, valid2ID)

		require.NoError(t, err)
		require.EqualValues(t, dossier.Reputation.AuditReputationAlpha, nodeSelectionConfig.AuditReputationAlpha0)
		require.EqualValues(t, dossier.Reputation.AuditReputationBeta, nodeSelectionConfig.AuditReputationBeta0)
		require.EqualValues(t, dossier.Reputation.UptimeReputationAlpha, newUptimeAlpha)
		require.EqualValues(t, dossier.Reputation.UptimeReputationBeta, newUptimeBeta)
		require.NotNil(t, dossier.Disqualified)
		require.Equal(t, *dossier.Disqualified, dqTime)

	}
}

func TestRandomizedSelection(t *testing.T) {
	t.Parallel()

	totalNodes := 1000
	selectIterations := 100
	numNodesToSelect := 100
	minSelectCount := 3 // TODO: compute this limit better

	satellitedbtest.Run(t, func(t *testing.T, db satellite.DB) {
		ctx := testcontext.New(t)
		defer ctx.Cleanup()

		cache := db.OverlayCache()
		allIDs := make(storj.NodeIDList, totalNodes)
		nodeCounts := make(map[storj.NodeID]int)
		defaults := overlay.NodeSelectionConfig{
			AuditReputationAlpha0:  1,
			AuditReputationBeta0:   0,
			UptimeReputationAlpha0: 1,
			UptimeReputationBeta0:  0,
		}

		// put nodes in cache
		for i := 0; i < totalNodes; i++ {
			newID := testrand.NodeID()

			err := cache.UpdateAddress(ctx, &pb.Node{Id: newID}, defaults)
			require.NoError(t, err)
			_, err = cache.UpdateNodeInfo(ctx, newID, &pb.InfoResponse{
				Type:     pb.NodeType_STORAGE,
				Capacity: &pb.NodeCapacity{},
			})
			require.NoError(t, err)

			if i%2 == 0 { // make half of nodes "new" and half "vetted"
				_, err = cache.UpdateStats(ctx, &overlay.UpdateRequest{
					NodeID:       newID,
					IsUp:         true,
					AuditSuccess: true,
					AuditLambda:  1,
					AuditWeight:  1,
					AuditDQ:      0.5,
					UptimeLambda: 1,
					UptimeWeight: 1,
					UptimeDQ:     0.5,
				})
				require.NoError(t, err)
			}

			allIDs[i] = newID
			nodeCounts[newID] = 0
		}

		// select numNodesToSelect nodes selectIterations times
		for i := 0; i < selectIterations; i++ {
			var nodes []*pb.Node
			var err error

			if i%2 == 0 {
				nodes, err = cache.SelectStorageNodes(ctx, numNodesToSelect, &overlay.NodeCriteria{
					OnlineWindow: time.Hour,
					AuditCount:   1,
				})
				require.NoError(t, err)
			} else {
				nodes, err = cache.SelectNewStorageNodes(ctx, numNodesToSelect, &overlay.NodeCriteria{
					OnlineWindow: time.Hour,
					AuditCount:   1,
				})
				require.NoError(t, err)
			}
			require.Len(t, nodes, numNodesToSelect)

			for _, node := range nodes {
				nodeCounts[node.Id]++
			}
		}

		belowThreshold := 0

		table := []int{}

		// expect that each node has been selected at least minSelectCount times
		for _, id := range allIDs {
			count := nodeCounts[id]
			if count < minSelectCount {
				belowThreshold++
			}
			if count >= len(table) {
				table = append(table, make([]int, count-len(table)+1)...)
			}
			table[count]++
		}

		if belowThreshold > totalNodes*1/100 {
			t.Errorf("%d out of %d were below threshold %d", belowThreshold, totalNodes, minSelectCount)
			for count, amount := range table {
				t.Logf("%3d = %4d", count, amount)
			}
		}
	})
}

func TestIsVetted(t *testing.T) {
	testplanet.Run(t, testplanet.Config{
		SatelliteCount: 1, StorageNodeCount: 3, UplinkCount: 0,
		Reconfigure: testplanet.Reconfigure{
			Satellite: func(log *zap.Logger, index int, config *satellite.Config) {
				config.Overlay.Node.AuditCount = 1
				config.Overlay.Node.UptimeCount = 1
			},
		},
	}, func(t *testing.T, ctx *testcontext.Context, planet *testplanet.Planet) {
		var err error
		satellitePeer := planet.Satellites[0]
		satellitePeer.Audit.Service.Loop.Pause()
		satellitePeer.Repair.Checker.Loop.Pause()
		service := satellitePeer.Overlay.Service

		_, err = satellitePeer.DB.OverlayCache().UpdateStats(ctx, &overlay.UpdateRequest{
			NodeID:       planet.StorageNodes[0].ID(),
			IsUp:         true,
			AuditSuccess: true,
			AuditLambda:  1,
			AuditWeight:  1,
			AuditDQ:      0.5,
			UptimeLambda: 1,
			UptimeWeight: 1,
			UptimeDQ:     0.5,
		})
		require.NoError(t, err)

		_, err = satellitePeer.DB.OverlayCache().UpdateStats(ctx, &overlay.UpdateRequest{
			NodeID:       planet.StorageNodes[1].ID(),
			IsUp:         true,
			AuditSuccess: true,
			AuditLambda:  1,
			AuditWeight:  1,
			AuditDQ:      0.5,
			UptimeLambda: 1,
			UptimeWeight: 1,
			UptimeDQ:     0.5,
		})
		require.NoError(t, err)

		reputable, err := service.IsVetted(ctx, planet.StorageNodes[0].ID())
		require.NoError(t, err)
		require.True(t, reputable)

		reputable, err = service.IsVetted(ctx, planet.StorageNodes[1].ID())
		require.NoError(t, err)
		require.True(t, reputable)

		reputable, err = service.IsVetted(ctx, planet.StorageNodes[2].ID())
		require.NoError(t, err)
		require.False(t, reputable)

		// test dq-ing for bad uptime
		_, err = satellitePeer.DB.OverlayCache().UpdateStats(ctx, &overlay.UpdateRequest{
			NodeID:       planet.StorageNodes[0].ID(),
			IsUp:         false,
			AuditSuccess: true,
			AuditLambda:  1,
			AuditWeight:  1,
			AuditDQ:      0.5,
			UptimeLambda: 0,
			UptimeWeight: 1,
			UptimeDQ:     0.5,
		})
		require.NoError(t, err)

		// test dq-ing for bad audit
		_, err = satellitePeer.DB.OverlayCache().UpdateStats(ctx, &overlay.UpdateRequest{
			NodeID:       planet.StorageNodes[1].ID(),
			IsUp:         true,
			AuditSuccess: false,
			AuditLambda:  0,
			AuditWeight:  1,
			AuditDQ:      0.5,
			UptimeLambda: 1,
			UptimeWeight: 1,
			UptimeDQ:     0.5,
		})
		require.NoError(t, err)

		reputable, err = service.IsVetted(ctx, planet.StorageNodes[0].ID())
		require.NoError(t, err)
		require.False(t, reputable)

		reputable, err = service.IsVetted(ctx, planet.StorageNodes[1].ID())
		require.NoError(t, err)
		require.False(t, reputable)
	})
}

func TestNodeInfo(t *testing.T) {
	testplanet.Run(t, testplanet.Config{
		SatelliteCount: 1, StorageNodeCount: 1, UplinkCount: 0,
	}, func(t *testing.T, ctx *testcontext.Context, planet *testplanet.Planet) {
		planet.StorageNodes[0].Storage2.Monitor.Loop.Pause()
		planet.Satellites[0].Discovery.Service.Refresh.Pause()

		node, err := planet.Satellites[0].Overlay.Service.Get(ctx, planet.StorageNodes[0].ID())
		require.NoError(t, err)

		assert.Equal(t, pb.NodeType_STORAGE, node.Type)
		assert.NotEmpty(t, node.Operator.Email)
		assert.NotEmpty(t, node.Operator.Wallet)
		assert.Equal(t, planet.StorageNodes[0].Local().Operator, node.Operator)
		assert.NotEmpty(t, node.Capacity.FreeBandwidth)
		assert.NotEmpty(t, node.Capacity.FreeDisk)
		assert.Equal(t, planet.StorageNodes[0].Local().Capacity, node.Capacity)
		assert.NotEmpty(t, node.Version.Version)
		assert.Equal(t, planet.StorageNodes[0].Local().Version.Version, node.Version.Version)
	})
}
