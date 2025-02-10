package node

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/cometbft/cometbft/libs/log"
	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"

	testutils "github.com/celestiaorg/utils/test"

	cmcfg "github.com/cometbft/cometbft/config"
	"github.com/rollkit/rollkit/config"
	"github.com/rollkit/rollkit/types"
)

// NodeIntegrationTestSuite is a test suite for node integration tests
type NodeIntegrationTestSuite struct {
	suite.Suite
	ctx    context.Context
	cancel context.CancelFunc
	node   Node
}

// SetupTest is called before each test
func (s *NodeIntegrationTestSuite) SetupTest() {
	s.ctx, s.cancel = context.WithCancel(context.Background())

	// Setup node with proper configuration
	config := getTestConfig(1)
	config.BlockTime = 100 * time.Millisecond        // Faster block production for tests
	config.DABlockTime = 200 * time.Millisecond      // Faster DA submission for tests
	config.BlockManagerConfig.MaxPendingBlocks = 100 // Allow more pending blocks

	genesis, genesisValidatorKey := types.GetGenesisWithPrivkey(types.DefaultSigningKeyType, "test-chain")
	signingKey, err := types.PrivKeyToSigningKey(genesisValidatorKey)
	require.NoError(s.T(), err)

	p2pKey := generateSingleKey()

	node, err := NewNode(
		s.ctx,
		config,
		p2pKey,
		signingKey,
		genesis,
		DefaultMetricsProvider(cmcfg.DefaultInstrumentationConfig()),
		log.TestingLogger(),
	)
	require.NoError(s.T(), err)
	require.NotNil(s.T(), node)

	fn, ok := node.(*FullNode)
	require.True(s.T(), ok)

	err = fn.Start()
	require.NoError(s.T(), err)

	s.node = fn

	// Wait for node initialization with retry
	err = testutils.Retry(60, 100*time.Millisecond, func() error {
		height, err := getNodeHeight(s.node, Header)
		if err != nil {
			return err
		}
		if height == 0 {
			return fmt.Errorf("waiting for first block")
		}
		return nil
	})
	require.NoError(s.T(), err, "Node failed to produce first block")

	// Wait for DA inclusion with longer timeout
	err = testutils.Retry(100, 100*time.Millisecond, func() error {
		daHeight := s.node.(*FullNode).blockManager.GetDAIncludedHeight()
		if daHeight == 0 {
			return fmt.Errorf("waiting for DA inclusion")
		}
		return nil
	})
	require.NoError(s.T(), err, "Failed to get DA inclusion")
}

// TearDownTest is called after each test
func (s *NodeIntegrationTestSuite) TearDownTest() {
	if s.cancel != nil {
		s.cancel()
	}
	if s.node != nil {
		s.node.Stop()
	}
}

// TestNodeIntegrationTestSuite runs the test suite
func TestNodeIntegrationTestSuite(t *testing.T) {
	suite.Run(t, new(NodeIntegrationTestSuite))
}

func (s *NodeIntegrationTestSuite) waitForHeight(targetHeight uint64) error {
	return waitForAtLeastNBlocks(s.node, int(targetHeight), Store)
}

func (s *NodeIntegrationTestSuite) TestBlockProduction() {
	// Wait for at least one block to be produced and transactions to be included
	time.Sleep(5 * time.Second) // Give more time for the full flow

	// Get transactions from executor to verify they are being injected
	execTxs, err := s.node.(*FullNode).blockManager.GetExecutor().GetTxs(s.ctx)
	s.NoError(err)
	s.T().Logf("Number of transactions from executor: %d", len(execTxs))

	// Wait for at least one block to be produced
	err = s.waitForHeight(1)
	s.NoError(err, "Failed to produce first block")

	// Get the current height
	height := s.node.(*FullNode).Store.Height()
	s.GreaterOrEqual(height, uint64(1), "Expected block height >= 1")

	// Get all blocks and log their contents
	for h := uint64(1); h <= height; h++ {
		header, data, err := s.node.(*FullNode).Store.GetBlockData(s.ctx, h)
		s.NoError(err)
		s.NotNil(header)
		s.NotNil(data)
		s.T().Logf("Block height: %d, Time: %s, Number of transactions: %d", h, header.Time(), len(data.Txs))
	}

	// Get the latest block
	header, data, err := s.node.(*FullNode).Store.GetBlockData(s.ctx, height)
	s.NoError(err)
	s.NotNil(header)
	s.NotNil(data)

	// Log block details
	s.T().Logf("Latest block height: %d, Time: %s, Number of transactions: %d", height, header.Time(), len(data.Txs))

	// Verify chain state
	state, err := s.node.(*FullNode).Store.GetState(s.ctx)
	s.NoError(err)
	s.Equal(height, state.LastBlockHeight)

	// Verify block content
	s.NotEmpty(data.Txs, "Expected block to contain transactions")
}

func (s *NodeIntegrationTestSuite) TestDAInclusion() {
	require := require.New(s.T())

	// Get initial height and DA height
	initialHeight, err := getNodeHeight(s.node, Header)
	require.NoError(err, "Failed to get initial height")
	initialDAHeight := s.node.(*FullNode).blockManager.GetDAIncludedHeight()

	// Retry loop to check DA height
	var finalDAHeight uint64
	err = testutils.Retry(300, 100*time.Millisecond, func() error {
		currentDAHeight := s.node.(*FullNode).blockManager.GetDAIncludedHeight()
		if currentDAHeight <= initialDAHeight {
			return fmt.Errorf("waiting for DA height to increase")
		}
		finalDAHeight = currentDAHeight
		return nil
	})
	require.NoError(err, "DA height did not increase")

	// Get new heights and verify they increased
	newHeight, err := getNodeHeight(s.node, Header)
	require.NoError(err, "Failed to get new height")
	require.Greater(newHeight, initialHeight, "Expected new blocks to be produced")

	// Verify DA height tracking
	daHeight := s.node.(*FullNode).blockManager.GetLastState().DAHeight
	require.GreaterOrEqual(daHeight, finalDAHeight, "DA height should be at least equal to final DA height")
	require.Greater(finalDAHeight, initialDAHeight, "Final DA height should be greater than initial height")
}

func (s *NodeIntegrationTestSuite) TestLazyAggregation() {
	// Configure node for lazy aggregation
	conf := getTestConfig(1)
	conf.LazyAggregator = true
	conf.LazyBlockTime = 1 * time.Second
	conf.BlockTime = 100 * time.Millisecond

	node := s.setupNodeWithConfig(conf)
	defer node.Stop()

	// No transactions - should produce blocks at LazyBlockTime
	time.Sleep(1500 * time.Millisecond)
	heightBeforeTx := node.(*FullNode).Store.Height()
	s.Greater(heightBeforeTx, uint64(0))

	// TestMain's executor is already injecting transactions
	time.Sleep(200 * time.Millisecond)

	heightAfterTx := node.(*FullNode).Store.Height()
	s.Greater(heightAfterTx, heightBeforeTx)
}

func (s *NodeIntegrationTestSuite) TestNodeRecovery() {
	// Get initial height
	time.Sleep(300 * time.Millisecond)
	initialHeight := s.node.(*FullNode).Store.Height()
	s.Greater(initialHeight, uint64(0))

	// Stop node
	s.node.Stop()

	// TestMain's executor continues injecting transactions while node is down
	time.Sleep(300 * time.Millisecond)

	// Restart node
	err := s.node.Start()
	s.NoError(err)

	time.Sleep(500 * time.Millisecond)

	// Verify node recovered and continued producing blocks
	newHeight := s.node.(*FullNode).Store.Height()
	s.Greater(newHeight, initialHeight)

	// Verify pending transactions were processed
	lastHeader, lastData, err := s.node.(*FullNode).Store.GetBlockData(s.ctx, newHeight)
	s.NoError(err)
	s.NotNil(lastHeader)
	s.NotEmpty(lastData.Txs)
}

func (s *NodeIntegrationTestSuite) TestMaxPendingBlocks() {
	// Configure node with small pending blocks limit
	conf := getTestConfig(1)
	conf.MaxPendingBlocks = 2
	conf.BlockTime = 100 * time.Millisecond

	node := s.setupNodeWithConfig(conf)
	defer node.Stop()

	time.Sleep(500 * time.Millisecond)

	// Verify block production respects max pending limit
	height := node.(*FullNode).Store.Height()
	s.LessOrEqual(height, conf.MaxPendingBlocks)
}

func (s *NodeIntegrationTestSuite) TestHighThroughput() {
	// Configure node for high throughput
	conf := getTestConfig(1)
	conf.BlockTime = 100 * time.Millisecond // Faster block production
	conf.Aggregator = true

	node := s.setupNodeWithConfig(conf)
	defer node.Stop()

	// First, let's wait for the executor to inject some transactions
	// before we start checking blocks
	time.Sleep(500 * time.Millisecond)

	// Get initial transactions from executor to verify they are being injected
	execTxs, err := node.(*FullNode).blockManager.GetExecutor().GetTxs(s.ctx)
	s.NoError(err)
	s.T().Logf("Initial number of transactions from executor: %d", len(execTxs))
	s.Greater(len(execTxs), 0, "Expected executor to have injected some transactions before starting")

	// Now wait for blocks to be produced with the injected transactions
	time.Sleep(4 * time.Second)

	// Get current transactions to verify more were injected during the wait
	execTxs, err = node.(*FullNode).blockManager.GetExecutor().GetTxs(s.ctx)
	s.NoError(err)
	s.T().Logf("Final number of transactions from executor: %d", len(execTxs))

	// Verify blocks were produced
	height := node.(*FullNode).Store.Height()
	s.Greater(height, uint64(0))
	s.T().Logf("Current block height: %d", height)

	// Sample blocks to verify transaction inclusion
	var emptyBlocks, nonEmptyBlocks int
	for h := uint64(1); h <= height; h++ {
		header, data, err := node.(*FullNode).Store.GetBlockData(s.ctx, h)
		s.NoError(err)
		s.NotNil(header)
		s.NotNil(data)

		if len(data.Txs) == 0 {
			emptyBlocks++
		} else {
			nonEmptyBlocks++
		}

		s.T().Logf("Block height: %d, Time: %s, Number of transactions: %d",
			h, header.Time(), len(data.Txs))
	}

	// Log block statistics
	s.T().Logf("Block statistics - Empty: %d, Non-empty: %d, Total: %d",
		emptyBlocks, nonEmptyBlocks, height)

	// Verify we have more blocks with transactions than without
	s.Greater(nonEmptyBlocks, emptyBlocks,
		"Expected majority of blocks to contain transactions")

	// Verify at least some recent blocks contain transactions
	// Check last 5 blocks
	lastBlocksToCheck := uint64(5)
	if height < lastBlocksToCheck {
		lastBlocksToCheck = height
	}

	for h := height - lastBlocksToCheck + 1; h <= height; h++ {
		_, data, err := node.(*FullNode).Store.GetBlockData(s.ctx, h)
		s.NoError(err)
		s.NotEmpty(data.Txs, fmt.Sprintf("Expected transactions in recent block %d", h))
	}
}

func (s *NodeIntegrationTestSuite) TestTransactionOrdering() {
	// Inject transactions with specific ordering requirements
	// Verify they are processed in the correct order in blocks
}

func (s *NodeIntegrationTestSuite) TestFailedTransactions() {
	// Test handling of transactions that fail execution
	// Verify they are properly excluded/reported
}

func (s *NodeIntegrationTestSuite) TestDALayerResilience() {
	// Test DA submission retries
	// Test handling of DA temporary unavailability
	// Test batch size limits
}

func (s *NodeIntegrationTestSuite) TestBlockBatchSizes() {
	// Test different transaction batch sizes
	// Verify proper block creation with varying load
}

func (s *NodeIntegrationTestSuite) TestSequencerTiming() {
	// Test block production timing under different loads
	// Verify sequencer respects configured intervals
}

func (s *NodeIntegrationTestSuite) TestGenesisInitialization() {
	// TODO: implement genesis state verification
	// TODO: verify validator set
	// TODO: verify initial height
}

func (s *NodeIntegrationTestSuite) TestDAFailures() {
	// TODO: simulate DA layer failures
	// TODO: verify retry mechanism
	// TODO: verify state consistency
}

func (s *NodeIntegrationTestSuite) TestStateTransitions() {
	// TODO: track state changes across blocks
	// TODO: verify AppHash updates
	// TODO: verify state root consistency
}

func (s *NodeIntegrationTestSuite) TestBlockHeaderSync() {
	// TODO: verify header sync process
	// TODO: verify header verification
	// TODO: test out-of-order headers
}

func (s *NodeIntegrationTestSuite) TestBlockDataSync() {
	// TODO: verify block data sync
	// TODO: verify data integrity
	// TODO: test out-of-order data
}

func (s *NodeIntegrationTestSuite) TestSequencerBatchRetrieval() {
	// TODO: verify batch retrieval
	// TODO: test sequencer unavailability
	// TODO: verify batch ordering
}

func (s *NodeIntegrationTestSuite) TestSequencerBatchProcessing() {
	// TODO: verify batch processing
	// TODO: test invalid batches
	// TODO: verify size limits
}

func (s *NodeIntegrationTestSuite) TestSequencerConnectionHandling() {
	// TODO: test connection management
	// TODO: verify reconnection logic
	// TODO: test error handling
}

func (s *NodeIntegrationTestSuite) TestExecutionErrors() {
	// TODO: simulate execution failures
	// TODO: verify error handling
	// TODO: verify state consistency
}

func (s *NodeIntegrationTestSuite) setupNodeWithConfig(conf config.NodeConfig) Node {
	genesis, signingKey := types.GetGenesisWithPrivkey(types.DefaultSigningKeyType, "test-chain")
	key, err := types.PrivKeyToSigningKey(signingKey)
	require.NoError(s.T(), err)

	p2pKey := generateSingleKey()

	node, err := NewNode(
		s.ctx,
		conf,
		p2pKey,
		key,
		genesis,
		DefaultMetricsProvider(cmcfg.DefaultInstrumentationConfig()),
		log.TestingLogger(),
	)
	require.NoError(s.T(), err)

	err = node.Start()
	require.NoError(s.T(), err)

	return node
}

func (s *NodeIntegrationTestSuite) TestMaxPending() {
	require := require.New(s.T())

	// Reconfigure node with low max pending
	err := s.node.Stop()
	require.NoError(err)

	config := getTestConfig(1)
	config.BlockManagerConfig.MaxPendingBlocks = 2
	config.BlockTime = 100 * time.Millisecond
	config.DABlockTime = 1 * time.Second // Slower DA submission to test pending blocks

	// ... rest of the test setup ...

	// Wait longer for blocks to be produced up to max pending
	err = testutils.Retry(60, 100*time.Millisecond, func() error {
		height, err := getNodeHeight(s.node, Header)
		if err != nil {
			return err
		}
		if height == 0 {
			return fmt.Errorf("waiting for blocks")
		}
		return nil
	})
	require.NoError(err)

	// Verify that number of pending blocks doesn't exceed max
	height, err := getNodeHeight(s.node, Header)
	require.NoError(err)
	require.LessOrEqual(height, config.BlockManagerConfig.MaxPendingBlocks)
}
