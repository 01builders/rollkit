package node

import (
	"context"
	"testing"
	"time"

	execTest "github.com/rollkit/go-execution/test"
	"github.com/stretchr/testify/require"
)

func TestBasicExecutionFlow(t *testing.T) {
	require := require.New(t)

	// Setup node
	node, cleanup := setupTestNodeWithCleanup(t)
	defer cleanup()

	//startNodeWithCleanup(t, node)

	ctx := context.Background()

	// Give node time to initialize
	time.Sleep(1 * time.Second)

	// Test InitChain - we don't need to call this explicitly as it's done during node start
	executor := node.blockManager.GetExecutor()
	require.NotNil(executor)

	// Test GetTxs
	txs, err := executor.GetTxs(ctx)
	require.NoError(err)

	// Create a mock executor and initialize chain
	mockExec := execTest.NewDummyExecutor()
	stateRoot, maxBytes, err := mockExec.InitChain(ctx, time.Now(), 1, "test-chain")
	require.NoError(err)
	require.Greater(maxBytes, uint64(0))
	executor = mockExec

	// Test ExecuteTxs
	newStateRoot, newMaxBytes, err := executor.ExecuteTxs(ctx, txs, 1, time.Now(), stateRoot)
	require.NoError(err)
	require.Greater(newMaxBytes, uint64(0))
	require.Equal(maxBytes, newMaxBytes)

	require.NotEmpty(newStateRoot)

	// Test SetFinal
	err = executor.SetFinal(ctx, 1)
	require.NoError(err)
}

func TestExecutionWithDASync(t *testing.T) {
	t.Run("basic DA sync with transactions", func(t *testing.T) {
		require := require.New(t)
		ctx := context.Background()

		// Setup node with mock DA
		node, cleanup := setupTestNodeWithCleanup(t)
		defer cleanup()

		// Start node
		err := node.Start()
		require.NoError(err)
		defer func() {
			err := node.Stop()
			require.NoError(err)
		}()

		// Give node time to initialize and submit blocks to DA
		time.Sleep(2 * time.Second)

		// Verify DA client is working
		require.NotNil(node.dalc)

		// Get the executor from the node
		executor := node.blockManager.GetExecutor()
		require.NotNil(executor)

		// Wait for first block to be produced with a shorter timeout
		err = waitForFirstBlock(node, Header)
		require.NoError(err)

		// Get height and verify it's greater than 0
		height, err := getNodeHeight(node, Header)
		require.NoError(err)
		require.Greater(height, uint64(0))

		// Get the block data and verify transactions were included
		header, data, err := node.Store.GetBlockData(ctx, height)
		require.NoError(err)
		require.NotNil(header)
		require.NotNil(data)
		require.NotEmpty(data.Txs, "Expected block to contain transactions")
	})
}
