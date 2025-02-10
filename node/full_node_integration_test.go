package node

import (
	"context"
	"fmt"
	"testing"
	"time"

	testutils "github.com/celestiaorg/utils/test"
	cmcfg "github.com/cometbft/cometbft/config"
	"github.com/cometbft/cometbft/libs/log"
	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"

	"github.com/rollkit/rollkit/types"
)

// FullNodeTestSuite is a test suite for full node integration tests
type FullNodeTestSuite struct {
	suite.Suite
	ctx    context.Context
	cancel context.CancelFunc
	node   *FullNode
}

func (s *FullNodeTestSuite) SetupTest() {
	s.ctx, s.cancel = context.WithCancel(context.Background())

	// Setup node with proper configuration
	config := getTestConfig(1)
	config.BlockTime = 100 * time.Millisecond   // Faster block production for tests
	config.DABlockTime = 200 * time.Millisecond // Faster DA submission for tests

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

	// Wait for the node to start and initialize DA connection
	time.Sleep(2 * time.Second)

	// Verify that the node is running and producing blocks
	height, err := getNodeHeight(s.node, Header)
	require.NoError(s.T(), err, "Failed to get node height")
	require.Greater(s.T(), height, uint64(0), "Node should have produced at least one block")

	// Wait for DA inclusion with retry
	err = testutils.Retry(30, 100*time.Millisecond, func() error {
		daHeight := s.node.blockManager.GetDAIncludedHeight()
		if daHeight == 0 {
			return fmt.Errorf("waiting for DA inclusion")
		}
		return nil
	})
	require.NoError(s.T(), err, "Failed to get DA inclusion")
}

func (s *FullNodeTestSuite) TearDownTest() {
	if s.cancel != nil {
		s.cancel()
	}
	if s.node != nil {
		err := s.node.Stop()
		if err != nil {
			s.T().Logf("Error stopping node in teardown: %v", err)
		}
	}
}

// TestFullNodeTestSuite runs the test suite
func TestFullNodeTestSuite(t *testing.T) {
	suite.Run(t, new(FullNodeTestSuite))
}

func (s *FullNodeTestSuite) TestSubmitBlocksToDA() {
	require := require.New(s.T())

	// Get initial DA height
	initialDAHeight := s.node.blockManager.GetDAIncludedHeight()
	require.Greater(initialDAHeight, uint64(0), "Initial DA height should be greater than 0")

	// Wait for blocks to be produced and submitted to DA
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Retry loop to check DA height
	var finalDAHeight uint64
	for i := 0; i < 30; i++ {
		select {
		case <-ctx.Done():
			s.T().Fatal("timeout waiting for DA height to increase")
		default:
			currentDAHeight := s.node.blockManager.GetDAIncludedHeight()
			if currentDAHeight > initialDAHeight+30 {
				finalDAHeight = currentDAHeight
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
	}

	require.Greater(finalDAHeight, initialDAHeight+30, "DA height did not increase sufficiently")

	// Verify DA height tracking
	daHeight := s.node.blockManager.GetLastState().DAHeight
	submittedHeight := s.node.blockManager.GetDAIncludedHeight()
	require.GreaterOrEqual(daHeight, submittedHeight, "DA height should be at least equal to submitted height")
	require.Greater(submittedHeight, initialDAHeight, "Submitted height should be greater than initial height")
}

func (s *FullNodeTestSuite) TestDAInclusion() {
	require := require.New(s.T())

	// Get initial height and DA height
	initialHeight, err := getNodeHeight(s.node, Header)
	require.NoError(err, "Failed to get initial height")
	initialDAHeight := s.node.blockManager.GetDAIncludedHeight()

	// Wait for blocks to be produced and submitted to DA
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Retry loop to check DA height
	var finalDAHeight uint64
	for i := 0; i < 30; i++ {
		select {
		case <-ctx.Done():
			s.T().Fatal("timeout waiting for DA height to increase")
		default:
			currentDAHeight := s.node.blockManager.GetDAIncludedHeight()
			if currentDAHeight > initialDAHeight {
				finalDAHeight = currentDAHeight
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
	}

	// Get new heights and verify they increased
	newHeight, err := getNodeHeight(s.node, Header)
	require.NoError(err, "Failed to get new height")
	require.Greater(newHeight, initialHeight, "Expected new blocks to be produced")

	// Verify DA height tracking
	daHeight := s.node.blockManager.GetLastState().DAHeight
	require.GreaterOrEqual(daHeight, finalDAHeight, "DA height should be at least equal to final DA height")
	require.Greater(finalDAHeight, initialDAHeight, "Final DA height should be greater than initial height")
}

func (s *FullNodeTestSuite) TestMaxPending() {
	require := require.New(s.T())

	// Reconfigure node with low max pending
	err := s.node.Stop()
	require.NoError(err)

	config := getTestConfig(1)
	config.BlockManagerConfig.MaxPendingBlocks = 2

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

	// Wait blocks to be produced up to max pending
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

func (s *FullNodeTestSuite) TestStateRecovery() {
	require := require.New(s.T())

	// Get current state
	originalHeight, err := getNodeHeight(s.node, Store)
	require.NoError(err)

	// Wait for some blocks
	time.Sleep(2 * s.node.nodeConfig.BlockTime)

	// Restart node, we don't need to check for errors
	_ = s.node.Stop()
	_ = s.node.Start()

	// Wait a bit after restart
	time.Sleep(s.node.nodeConfig.BlockTime)

	// Verify state persistence
	recoveredHeight, err := getNodeHeight(s.node, Store)
	require.NoError(err)
	require.GreaterOrEqual(recoveredHeight, originalHeight)
}

func (s *FullNodeTestSuite) TestInvalidDAConfig() {
	require := require.New(s.T())

	// Create a node with invalid DA configuration
	invalidConfig := getTestConfig(1)
	invalidConfig.DAAddress = "invalid://invalid-address:1234" // Use an invalid URL scheme

	genesis, genesisValidatorKey := types.GetGenesisWithPrivkey(types.DefaultSigningKeyType, "test-chain")
	signingKey, err := types.PrivKeyToSigningKey(genesisValidatorKey)
	require.NoError(err)

	p2pKey := generateSingleKey()

	// Attempt to create a node with invalid DA config
	node, err := NewNode(
		s.ctx,
		invalidConfig,
		p2pKey,
		signingKey,
		genesis,
		DefaultMetricsProvider(cmcfg.DefaultInstrumentationConfig()),
		log.TestingLogger(),
	)

	// Verify that node creation fails with appropriate error
	require.Error(err, "Expected error when creating node with invalid DA config")
	require.Contains(err.Error(), "unknown url scheme", "Expected error related to invalid URL scheme")
	require.Nil(node, "Node should not be created with invalid DA config")
}
