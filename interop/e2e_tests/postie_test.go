package e2e_tests

import (
	"context"
	"math/big"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/params"

	"github.com/ethereum-optimism/optimism/indexer/node"
	"github.com/ethereum-optimism/optimism/op-bindings/bindings"
	"github.com/ethereum-optimism/optimism/op-bindings/predeploys"
	"github.com/ethereum-optimism/optimism/op-e2e/e2eutils/wait"
	"github.com/ethereum-optimism/optimism/op-service/eth"
	"github.com/ethereum-optimism/optimism/op-service/metrics"
)

func TestPostieStorageRootUpdates(t *testing.T) {
	testSuite := createE2ETestSuite(t)

	// wait for the first storage root of chain B to change
	var oldStorageRoot common.Hash
	require.NoError(t, wait.For(context.Background(), time.Second/2, func() (bool, error) {
		oldStorageRoot = testSuite.PostieA.OutboxStorageRoot(testSuite.ChainIdB)
		return oldStorageRoot != common.Hash{}, nil
	}))

	// initiate an message on chain B
	// NOTE: the destination chain does not matter for now as postie will update for any change
	outbox, err := bindings.NewCrossL2Outbox(predeploys.CrossL2OutboxAddr, testSuite.OpSysB.Clients["sequencer"])
	require.NoError(t, err)

	sender, senderAddr := testSuite.OpCfg.Secrets.Bob, testSuite.OpCfg.Secrets.Addresses().Bob
	senderOpts, err := bind.NewKeyedTransactorWithChainID(sender, big.NewInt(int64(testSuite.ChainIdB)))
	require.NoError(t, err)
	senderOpts.Value = big.NewInt(params.Ether / 2)

	tx, err := outbox.InitiateMessage(senderOpts, common.BigToHash(big.NewInt(int64(testSuite.ChainIdA))), senderAddr, big.NewInt(25_000), []byte{})
	require.NoError(t, err)

	_, err = wait.ForReceiptOK(context.Background(), testSuite.OpSysB.Clients["sequencer"], tx.Hash())
	require.NoError(t, err)

	// wait for a changed root
	require.NoError(t, wait.For(context.Background(), time.Second/2, func() (bool, error) {
		return testSuite.PostieA.OutboxStorageRoot(testSuite.ChainIdB) != oldStorageRoot, nil
	}))

	clnt := node.FromRPCClient(testSuite.OpSysB.RawClients["sequencer"], node.NewMetrics(metrics.NewRegistry(), ""))
	root, err := clnt.StorageHash(predeploys.CrossL2OutboxAddr, nil)
	require.NoError(t, err)
	require.Equal(t, root, testSuite.PostieA.OutboxStorageRoot(testSuite.ChainIdB))

	inbox, err := bindings.NewCrossL2Inbox(predeploys.CrossL2InboxAddr, testSuite.OpSysA.Clients["sequencer"])
	require.NoError(t, err)

	includedRoot, err := inbox.Roots(&bind.CallOpts{}, common.BigToHash(big.NewInt(int64(testSuite.ChainIdB))), root)
	require.NoError(t, err)
	require.True(t, includedRoot)
}

func TestPostieInboxRelay(t *testing.T) {
	testSuite := createE2ETestSuite(t)

	// wait for the first storage root of chain B to change
	var oldStorageRoot common.Hash
	require.NoError(t, wait.For(context.Background(), time.Second/2, func() (bool, error) {
		oldStorageRoot = testSuite.PostieA.OutboxStorageRoot(testSuite.ChainIdB)
		return oldStorageRoot != common.Hash{}, nil
	}))

	outbox, err := bindings.NewCrossL2Outbox(predeploys.CrossL2OutboxAddr, testSuite.OpSysB.Clients["sequencer"])
	require.NoError(t, err)

	// Transfer 0.5 ETH from Bob's account from Chain B -> A
	sender, senderAddr := testSuite.OpCfg.Secrets.Bob, testSuite.OpCfg.Secrets.Addresses().Bob
	senderOpts, _ := bind.NewKeyedTransactorWithChainID(sender, big.NewInt(int64(testSuite.ChainIdB)))
	senderOpts.Value = big.NewInt(params.Ether / 2)
	tx, err := outbox.InitiateMessage(senderOpts, common.BigToHash(big.NewInt(int64(testSuite.ChainIdA))), senderAddr, big.NewInt(25_000), []byte{})
	require.NoError(t, err)

	msgRec, err := wait.ForReceiptOK(context.Background(), testSuite.OpSysB.Clients["sequencer"], tx.Hash())
	require.NoError(t, err)
	require.Len(t, msgRec.Logs, 1, "expecting a MessagePassed log event")

	// Get the MessagePassed event, so we can get the message-root easily,
	// without re-implementing the logic that computes it.
	num := msgRec.BlockNumber.Uint64()
	msgPassIter, err := outbox.FilterMessagePassed(&bind.FilterOpts{
		Start:   num,
		End:     &num,
		Context: context.Background(),
	}, nil, nil, nil)
	require.NoError(t, err)
	require.True(t, msgPassIter.Next())
	t.Log("passed message:", msgPassIter.Event)

	require.NoError(t, wait.For(context.Background(), time.Second/2, func() (bool, error) {
		return testSuite.PostieA.OutboxStorageRoot(testSuite.ChainIdB) != oldStorageRoot, nil
	}))

	// Relay this message onto chain A
	inbox, err := bindings.NewCrossL2Inbox(predeploys.CrossL2InboxAddr, testSuite.OpSysA.Clients["sequencer"])
	require.NoError(t, err)

	// get proof for the message we just sent into the outbox
	superchainMessageRoot := msgPassIter.Event.MessageRoot

	cl := testSuite.OpSysB.Clients["sequencer"].Client()
	sentMessagesStorageSlot := common.Hash{} // slot 0
	// solidity storage mapping combines key and slot into final storage key
	storageKey := crypto.Keccak256Hash(superchainMessageRoot[:], sentMessagesStorageSlot[:])
	t.Logf("requesting proof for storage key: %s", storageKey)

	mptKey := crypto.Keccak256Hash(storageKey[:]) // for debugging
	t.Logf("requesting proof for MPT key: %s", mptKey)

	outboxRoot := testSuite.PostieA.OutboxStorageRoot(testSuite.ChainIdB)
	t.Logf("outbox root: %s", outboxRoot)

	var getProofResponse *eth.AccountResult
	err = cl.CallContext(context.Background(), &getProofResponse, "eth_getProof", predeploys.CrossL2OutboxAddr, []common.Hash{storageKey}, "latest")
	require.NoError(t, err, "must build storage proof")
	require.Equal(t, outboxRoot, getProofResponse.StorageHash, "outbox storage hash must match what proof is generated for")
	require.Len(t, getProofResponse.StorageProof, 1, "need storage proof")
	msgProofEntry := getProofResponse.StorageProof[0]
	// Just concatenate all RLP nodes of the MPT tree, top to bottom (as the eth_getProof should return).
	// The precompile reads it as a stream based
	var proofData []byte
	for i, mptNode := range msgProofEntry.Proof {
		proofData = append(proofData, mptNode...)
		t.Logf("proof node %d: %s    - hash: %s", i, mptNode, crypto.Keccak256Hash(mptNode))
	}

	senderOpts, err = bind.NewKeyedTransactorWithChainID(sender, big.NewInt(int64(testSuite.ChainIdA)))
	require.NoError(t, err)
	tx, err = inbox.RunCrossL2Message(senderOpts,
		bindings.TypesSuperchainMessage{
			Nonce:       big.NewInt(0), // first message
			SourceChain: common.BigToHash(big.NewInt(int64(testSuite.ChainIdB))),
			TargetChain: common.BigToHash(big.NewInt(int64(testSuite.ChainIdA))),
			From:        senderAddr,
			To:          senderAddr,
			GasLimit:    big.NewInt(25_000),
			Data:        []byte{},
			Value:       big.NewInt(params.Ether / 2),
		},
		outboxRoot,
		proofData,
	)
	require.NoError(t, err)

	rec, err := wait.ForReceiptOK(context.Background(), testSuite.OpSysA.Clients["sequencer"], tx.Hash())
	require.NoError(t, err)
	t.Log("receipt", rec)
	//wait.PrintDebugTrace(context.Background(), testSuite.OpSysA.Clients["sequencer"], tx.Hash())
}
