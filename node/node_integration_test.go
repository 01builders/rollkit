package node

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/cometbft/cometbft/libs/log"
	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"

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

	// Setup and start node
	conf := getTestConfig(1)
	conf.BlockTime = 500 * time.Millisecond
	conf.Aggregator = true // Enable aggregator mode for block production

	genesis, signingKey := types.GetGenesisWithPrivkey(types.DefaultSigningKeyType, "test-chain")
	key, err := types.PrivKeyToSigningKey(signingKey)
	require.NoError(s.T(), err)

	p2pKey := generateSingleKey()

	// Note: We don't need to set up the executor here as it's already running from TestMain

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
	s.node = node

	err = s.node.Start()
	require.NoError(s.T(), err)
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

func (s *NodeIntegrationTestSuite) TestEmptyBlockProduction() {
	time.Sleep(500 * time.Millisecond)

	height := s.node.(*FullNode).Store.Height()
	s.Greater(height, uint64(0))

	// Verify empty blocks
	for h := uint64(1); h <= height; h++ {
		header, data, err := s.node.(*FullNode).Store.GetBlockData(s.ctx, h)
		s.NoError(err)
		s.NotNil(header)
		s.Empty(data.Txs)
	}
}

func (s *NodeIntegrationTestSuite) TestDAInteraction() {
	// No need to inject transactions as TestMain's executor is already doing it
	time.Sleep(500 * time.Millisecond)

	// Verify blocks are submitted to DA layer
	height := s.node.(*FullNode).Store.Height()
	s.Greater(height, uint64(0))

	// Check DA included height
	daHeight := s.node.(*FullNode).blockManager.GetDAIncludedHeight()
	s.Greater(daHeight, uint64(0))
	s.LessOrEqual(daHeight, height)

	// Get block from DA layer
	lastBlock, lastData, err := s.node.(*FullNode).Store.GetBlockData(s.ctx, daHeight)
	s.NoError(err)
	s.NotNil(lastBlock)
	s.NotEmpty(lastData.Txs)
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
