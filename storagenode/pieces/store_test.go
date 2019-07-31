// Copyright (C) 2019 Storj Labs, Inc.
// See LICENSE for copying information.

package pieces_test

import (
	"bytes"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"

	"storj.io/storj/internal/testcontext"
	"storj.io/storj/internal/testidentity"
	"storj.io/storj/internal/testrand"
	"storj.io/storj/pkg/pkcrypto"
	"storj.io/storj/pkg/storj"
	"storj.io/storj/storage/filestore"
	"storj.io/storj/storagenode/pieces"
)

func TestPieces(t *testing.T) {
	ctx := testcontext.New(t)
	defer ctx.Cleanup()

	dir, err := filestore.NewDir(ctx.Dir("pieces"))
	require.NoError(t, err)

	blobs := filestore.New(dir)
	defer ctx.Check(blobs.Close)

	store := pieces.NewStore(zaptest.NewLogger(t), blobs)

	satelliteID := testidentity.MustPregeneratedSignedIdentity(0, storj.LatestIDVersion()).ID
	pieceID := storj.NewPieceID()

	source := testrand.Bytes(8000)

	{ // write data
		writer, err := store.Writer(ctx, satelliteID, pieceID)
		require.NoError(t, err)

		n, err := io.Copy(writer, bytes.NewReader(source))
		require.NoError(t, err)
		assert.Equal(t, len(source), int(n))
		assert.Equal(t, len(source), int(writer.Size()))

		// verify hash
		hash := pkcrypto.NewHash()
		_, _ = hash.Write(source)
		assert.Equal(t, hash.Sum(nil), writer.Hash())

		// commit
		require.NoError(t, writer.Commit(ctx))
		// after commit we should be able to call cancel without an error
		require.NoError(t, writer.Cancel(ctx))
	}

	{ // valid reads
		read := func(offset, length int64) []byte {
			reader, err := store.Reader(ctx, satelliteID, pieceID)
			require.NoError(t, err)

			pos, err := reader.Seek(offset, io.SeekStart)
			require.NoError(t, err)
			require.Equal(t, offset, pos)

			data := make([]byte, length)
			n, err := io.ReadFull(reader, data)
			require.NoError(t, err)
			require.Equal(t, int(length), n)

			require.NoError(t, reader.Close())

			return data
		}

		require.Equal(t, source[10:11], read(10, 1))
		require.Equal(t, source[10:1010], read(10, 1000))
		require.Equal(t, source, read(0, int64(len(source))))
	}

	{ // reading ends with io.EOF
		reader, err := store.Reader(ctx, satelliteID, pieceID)
		require.NoError(t, err)

		data := make([]byte, 111)
		for {
			_, err := reader.Read(data)
			if err != nil {
				if err == io.EOF {
					break
				}
				require.NoError(t, err)
			}
		}

		require.NoError(t, reader.Close())
	}

	{ // test delete
		assert.NoError(t, store.Delete(ctx, satelliteID, pieceID))
		// read should now fail
		_, err := store.Reader(ctx, satelliteID, pieceID)
		assert.Error(t, err)
	}

	{ // write cancel
		cancelledPieceID := storj.NewPieceID()
		writer, err := store.Writer(ctx, satelliteID, cancelledPieceID)
		require.NoError(t, err)

		n, err := io.Copy(writer, bytes.NewReader(source))
		require.NoError(t, err)
		assert.Equal(t, len(source), int(n))
		assert.Equal(t, len(source), int(writer.Size()))

		// cancel writing
		require.NoError(t, writer.Cancel(ctx))
		// commit should not fail
		require.Error(t, writer.Commit(ctx))

		// read should fail
		_, err = store.Reader(ctx, satelliteID, cancelledPieceID)
		assert.Error(t, err)
	}
}
