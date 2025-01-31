package node

import (
	"bytes"
	"context"
	"fmt"
	"testing"
	"time"

	cmcfg "github.com/cometbft/cometbft/config"
	"github.com/cometbft/cometbft/libs/log"
	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"

	"github.com/rollkit/rollkit/mempool"

	testutils "github.com/celestiaorg/utils/test"
	"github.com/rollkit/rollkit/types"
)

// FullNodeTestSuite is a test suite for full node integration tests
type FullNodeTestSuite struct {
	suite.Suite
	ctx    context.Context
	cancel context.CancelFunc
	node   *FullNode
	config *cmcfg.Config
}

func (s *FullNodeTestSuite) SetupTest() {
	s.ctx, s.cancel = context.WithCancel(context.Background())

	// Setup node with proper configuration
	config := getTestConfig(1)
	config.BlockTime = 500 * time.Millisecond
	config.Aggregator = true // Enable aggregator mode for block production

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
}

func (s *FullNodeTestSuite) TearDownTest() {
	if s.cancel != nil {
		s.cancel()
	}
	if s.node != nil {
		s.node.Stop()
	}
}

// TestFullNodeTestSuite runs the test suite
func TestFullNodeTestSuite(t *testing.T) {
	suite.Run(t, new(FullNodeTestSuite))
}

func (s *FullNodeTestSuite) TestSubmitBlocksToDA() {
	require := require.New(s.T())

	// Wait for the first block to be produced and submitted to DA
	err := waitForFirstBlock(s.node, Header)
	require.NoError(err)

	// Verify that block was submitted to DA
	height, err := getNodeHeight(s.node, Header)
	require.NoError(err)
	require.Greater(height, uint64(0))

	// Store the current height
	initialHeight := height

	// Wait for a few more blocks to ensure continuous DA submission
	// Wait longer to ensure we have enough time for new blocks
	time.Sleep(5 * s.node.nodeConfig.BlockTime)

	// Get new height and verify it increased
	newHeight, err := getNodeHeight(s.node, Header)
	require.NoError(err)
	require.Greater(newHeight, initialHeight, "Expected new blocks to be produced")

	// Additional verification of DA inclusion
	submitted := s.node.blockManager.GetDAIncludedHeight()
	require.Greater(submitted, uint64(0), "Expected blocks to be submitted to DA")
}

func (s *FullNodeTestSuite) TestMaxPending() {
	require := require.New(s.T())

	// Reconfigure node with low max pending
	s.node.Stop()
	config := getTestConfig(1)
	config.BlockManagerConfig.MaxPendingBlocks = 2
	config.BlockTime = 500 * time.Millisecond
	config.Aggregator = true

	genesis, genesisValidatorKey := types.GetGenesisWithPrivkey(types.DefaultSigningKeyType, "test-chain")
	signingKey, err := types.PrivKeyToSigningKey(genesisValidatorKey)
	require.NoError(err)

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
	require.NoError(err)
	require.NotNil(node)

	fn, ok := node.(*FullNode)
	require.True(ok)

	err = fn.Start()
	require.NoError(err)
	s.node = fn

	// Wait for blocks to be produced up to max pending
	time.Sleep(time.Duration(config.BlockManagerConfig.MaxPendingBlocks+1) * config.BlockTime)

	// Verify that number of pending blocks doesn't exceed max
	height, err := getNodeHeight(s.node, Header)
	require.NoError(err)
	require.LessOrEqual(height, config.BlockManagerConfig.MaxPendingBlocks)
}

func (s *FullNodeTestSuite) TestGenesisInitialization() {
	require := require.New(s.T())

	// Verify genesis state
	state := s.node.blockManager.GetLastState()
	require.Equal(s.node.genesis.InitialHeight, int64(state.InitialHeight))
	require.Equal(s.node.genesis.ChainID, state.ChainID)
}

func (s *FullNodeTestSuite) TestDAInclusion() {
	require := require.New(s.T())

	// Wait for several blocks
	err := waitForAtLeastNBlocks(s.node, 3, Header)
	require.NoError(err)

	// Verify DA submissions
	submitted := s.node.blockManager.GetDAIncludedHeight()
	require.Greater(submitted, uint64(0))

	// Verify DA height tracking
	daHeight := s.node.blockManager.GetLastState().DAHeight
	require.GreaterOrEqual(daHeight, submitted)
}

func (s *FullNodeTestSuite) TestStateRecovery() {
	require := require.New(s.T())

	// Get current state
	originalHeight, err := getNodeHeight(s.node, Store)
	require.NoError(err)

	// Wait for some blocks
	time.Sleep(2 * s.node.nodeConfig.BlockTime)

	// Restart node
	err = s.node.Stop()
	require.NoError(err)
	err = s.node.Start()
	require.NoError(err)

	// Wait a bit after restart
	time.Sleep(s.node.nodeConfig.BlockTime)

	// Verify state persistence
	recoveredHeight, err := getNodeHeight(s.node, Store)
	require.NoError(err)
	require.GreaterOrEqual(recoveredHeight, originalHeight)
}

func (s *FullNodeTestSuite) TestInvalidDAConfig() {
	require := require.New(s.T())

	invalidConfig := getTestConfig(1)
	invalidConfig.DAAddress = "invalid"

	_, _, err := newTestNode(s.ctx, s.T(), Full, "test-chain")
	require.Error(err)
	require.Contains(err.Error(), "failed to initialize DA client")
}

func (s *FullNodeTestSuite) TestTransactionProcessing() {
	require := require.New(s.T())

	// Submit test transaction
	tx := []byte{0x01, 0x02, 0x03}
	err := s.node.Mempool.CheckTx(tx, nil, mempool.TxInfo{})
	require.NoError(err)

	// Wait for tx inclusion
	err = waitForTxInclusion(s.node, tx)
	require.NoError(err)
}

func waitForTxInclusion(node *FullNode, tx []byte) error {
	return testutils.Retry(300, 100*time.Millisecond, func() error {
		height, err := getNodeHeight(node, Header)
		if err != nil {
			return err
		}

		// Check all blocks up to current height
		for h := uint64(1); h <= height; h++ {
			_, data, err := node.Store.GetBlockData(context.Background(), h)
			if err != nil {
				continue
			}

			for _, blockTx := range data.Txs {
				if bytes.Equal(blockTx, tx) {
					return nil
				}
			}
		}
		return fmt.Errorf("tx not found in any blocks")
	})
}
