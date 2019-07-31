// Copyright (C) 2019 Storj Labs, Inc.
// See LICENSE for copying information.

package storagenodedb

import (
	_ "github.com/mattn/go-sqlite3" // used indirectly
	"github.com/zeebo/errs"
	"go.uber.org/zap"
	monkit "gopkg.in/spacemonkeygo/monkit.v2"

	"storj.io/storj/pkg/kademlia"
	"storj.io/storj/storage"
	"storj.io/storj/storage/boltdb"
	"storj.io/storj/storage/filestore"
	"storj.io/storj/storage/teststore"
	"storj.io/storj/storagenode"
)

var (
	mon = monkit.Package()
)

var _ storagenode.DB = (*DB)(nil)

// Config configures storage node database
type Config struct {
	// TODO: figure out better names
	Storage  string
	Info     string
	Info2    string
	Kademlia string

	Pieces string
}

// DB contains access to different database tables
type DB struct {
	log *zap.Logger

	pieces interface {
		storage.Blobs
		Close() error
	}

	info *InfoDB

	kdb, ndb, adb storage.KeyValueStore
}

// New creates a new master database for storage node
func New(log *zap.Logger, config Config) (*DB, error) {
	piecesDir, err := filestore.NewDir(config.Pieces)
	if err != nil {
		return nil, err
	}
	pieces := filestore.New(piecesDir)

	infodb, err := newInfo(config.Info2)
	if err != nil {
		return nil, err
	}

	dbs, err := boltdb.NewShared(config.Kademlia, kademlia.KademliaBucket, kademlia.NodeBucket, kademlia.AntechamberBucket)
	if err != nil {
		return nil, err
	}

	return &DB{
		log: log,

		pieces: pieces,

		info: infodb,

		kdb: dbs[0],
		ndb: dbs[1],
		adb: dbs[2],
	}, nil
}

// NewTest creates new test database for storage node.
func NewTest(log *zap.Logger, storageDir string) (*DB, error) {
	piecesDir, err := filestore.NewDir(storageDir)
	if err != nil {
		return nil, err
	}
	pieces := filestore.New(piecesDir)

	infodb, err := NewInfoTest()
	if err != nil {
		return nil, err
	}

	return &DB{
		log: log,

		pieces: pieces,
		info:   infodb,

		kdb: teststore.New(),
		ndb: teststore.New(),
		adb: teststore.New(),
	}, nil
}

// CreateTables creates any necessary tables.
func (db *DB) CreateTables() error {
	return db.info.CreateTables(db.log)
}

// Close closes any resources.
func (db *DB) Close() error {
	return errs.Combine(
		db.kdb.Close(),
		db.ndb.Close(),
		db.adb.Close(),

		db.pieces.Close(),
		db.info.Close(),
	)
}

// Pieces returns blob storage for pieces
func (db *DB) Pieces() storage.Blobs {
	return db.pieces
}

// RoutingTable returns kademlia routing table
func (db *DB) RoutingTable() (kdb, ndb, adb storage.KeyValueStore) {
	return db.kdb, db.ndb, db.adb
}
