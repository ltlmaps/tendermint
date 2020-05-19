package state_test

import (
	"fmt"
	"github.com/stretchr/testify/assert"
	"github.com/tendermint/tendermint/crypto"
	"github.com/tendermint/tendermint/libs/bytes"
	"github.com/tendermint/tendermint/version"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/tendermint/tendermint/crypto/ed25519"
	"github.com/tendermint/tendermint/crypto/tmhash"
	"github.com/tendermint/tendermint/libs/log"
	memmock "github.com/tendermint/tendermint/mempool/mock"
	sm "github.com/tendermint/tendermint/state"
	"github.com/tendermint/tendermint/state/mocks"
	"github.com/tendermint/tendermint/types"
	tmtime "github.com/tendermint/tendermint/types/time"
)

const validationTestsStopHeight int64 = 10

var defaultTestTime = time.Date(2019, 1, 1, 0, 0, 0, 0, time.UTC)

func TestValidateBlockHeader(t *testing.T) {
	proxyApp := newTestApp()
	require.NoError(t, proxyApp.Start())
	defer proxyApp.Stop()

	state, stateDB, privVals := makeState(3, 1)
	blockExec := sm.NewBlockExecutor(
		stateDB,
		log.TestingLogger(),
		proxyApp.Consensus(),
		memmock.Mempool{},
		sm.MockEvidencePool{},
	)
	lastCommit := types.NewCommit(0, 0, types.BlockID{}, nil)

	// some bad values
	wrongHash := tmhash.Sum([]byte("this hash is wrong"))
	wrongVersion1 := state.Version.Consensus
	wrongVersion1.Block++
	wrongVersion2 := state.Version.Consensus
	wrongVersion2.App++

	// Manipulation of any header field causes failure.
	testCases := []struct {
		name          string
		malleateBlock func(block *types.Block)
	}{
		{"Version wrong1", func(block *types.Block) { block.Version = wrongVersion1 }},
		{"Version wrong2", func(block *types.Block) { block.Version = wrongVersion2 }},
		{"ChainID wrong", func(block *types.Block) { block.ChainID = "not-the-real-one" }},
		{"Height wrong", func(block *types.Block) { block.Height += 10 }},
		{"Time wrong", func(block *types.Block) { block.Time = block.Time.Add(-time.Second * 1) }},

		{"LastBlockID wrong", func(block *types.Block) { block.LastBlockID.PartsHeader.Total += 10 }},
		{"LastCommitHash wrong", func(block *types.Block) { block.LastCommitHash = wrongHash }},
		{"DataHash wrong", func(block *types.Block) { block.DataHash = wrongHash }},

		{"ValidatorsHash wrong", func(block *types.Block) { block.ValidatorsHash = wrongHash }},
		{"NextValidatorsHash wrong", func(block *types.Block) { block.NextValidatorsHash = wrongHash }},
		{"ConsensusHash wrong", func(block *types.Block) { block.ConsensusHash = wrongHash }},
		{"AppHash wrong", func(block *types.Block) { block.AppHash = wrongHash }},
		{"LastResultsHash wrong", func(block *types.Block) { block.LastResultsHash = wrongHash }},

		{"EvidenceHash wrong", func(block *types.Block) { block.EvidenceHash = wrongHash }},
		{"Proposer wrong", func(block *types.Block) { block.ProposerAddress = ed25519.GenPrivKey().PubKey().Address() }},
		{"Proposer invalid", func(block *types.Block) { block.ProposerAddress = []byte("wrong size") }},
	}

	// Build up state for multiple heights
	for height := int64(1); height < validationTestsStopHeight; height++ {
		proposerAddr := state.Validators.GetProposer().Address
		/*
			Invalid blocks don't pass
		*/
		for _, tc := range testCases {
			block, _ := state.MakeBlock(height, makeTxs(height), lastCommit, nil, proposerAddr)
			tc.malleateBlock(block)
			err := blockExec.ValidateBlock(state, block)
			require.Error(t, err, tc.name)
		}

		/*
			A good block passes
		*/
		var err error
		state, _, lastCommit, err = makeAndCommitGoodBlock(state, height, lastCommit, proposerAddr, blockExec, privVals, nil)
		require.NoError(t, err, "height %d", height)
	}
}

func TestValidateBlockCommit(t *testing.T) {
	proxyApp := newTestApp()
	require.NoError(t, proxyApp.Start())
	defer proxyApp.Stop()

	state, stateDB, privVals := makeState(1, 1)
	blockExec := sm.NewBlockExecutor(
		stateDB,
		log.TestingLogger(),
		proxyApp.Consensus(),
		memmock.Mempool{},
		sm.MockEvidencePool{},
	)
	lastCommit := types.NewCommit(0, 0, types.BlockID{}, nil)
	wrongSigsCommit := types.NewCommit(1, 0, types.BlockID{}, nil)
	badPrivVal := types.NewMockPV()

	for height := int64(1); height < validationTestsStopHeight; height++ {
		proposerAddr := state.Validators.GetProposer().Address
		if height > 1 {
			/*
				#2589: ensure state.LastValidators.VerifyCommit fails here
			*/
			// should be height-1 instead of height
			wrongHeightVote, err := types.MakeVote(
				height,
				state.LastBlockID,
				state.Validators,
				privVals[proposerAddr.String()],
				chainID,
				time.Now(),
			)
			require.NoError(t, err, "height %d", height)
			wrongHeightCommit := types.NewCommit(
				wrongHeightVote.Height,
				wrongHeightVote.Round,
				state.LastBlockID,
				[]types.CommitSig{wrongHeightVote.CommitSig()},
			)
			block, _ := state.MakeBlock(height, makeTxs(height), wrongHeightCommit, nil, proposerAddr)
			err = blockExec.ValidateBlock(state, block)
			_, isErrInvalidCommitHeight := err.(types.ErrInvalidCommitHeight)
			require.True(t, isErrInvalidCommitHeight, "expected ErrInvalidCommitHeight at height %d but got: %v", height, err)

			/*
				#2589: test len(block.LastCommit.Signatures) == state.LastValidators.Size()
			*/
			block, _ = state.MakeBlock(height, makeTxs(height), wrongSigsCommit, nil, proposerAddr)
			err = blockExec.ValidateBlock(state, block)
			_, isErrInvalidCommitSignatures := err.(types.ErrInvalidCommitSignatures)
			require.True(t, isErrInvalidCommitSignatures,
				"expected ErrInvalidCommitSignatures at height %d, but got: %v",
				height,
				err,
			)
		}

		/*
			A good block passes
		*/
		var err error
		var blockID types.BlockID
		state, blockID, lastCommit, err = makeAndCommitGoodBlock(
			state,
			height,
			lastCommit,
			proposerAddr,
			blockExec,
			privVals,
			nil,
		)
		require.NoError(t, err, "height %d", height)

		/*
			wrongSigsCommit is fine except for the extra bad precommit
		*/
		goodVote, err := types.MakeVote(height,
			blockID,
			state.Validators,
			privVals[proposerAddr.String()],
			chainID,
			time.Now(),
		)
		require.NoError(t, err, "height %d", height)

		bpvPubKey, err := badPrivVal.GetPubKey()
		require.NoError(t, err)

		badVote := &types.Vote{
			ValidatorAddress: bpvPubKey.Address(),
			ValidatorIndex:   0,
			Height:           height,
			Round:            0,
			Timestamp:        tmtime.Now(),
			Type:             types.PrecommitType,
			BlockID:          blockID,
		}
		err = badPrivVal.SignVote(chainID, goodVote)
		require.NoError(t, err, "height %d", height)
		err = badPrivVal.SignVote(chainID, badVote)
		require.NoError(t, err, "height %d", height)

		wrongSigsCommit = types.NewCommit(goodVote.Height, goodVote.Round,
			blockID, []types.CommitSig{goodVote.CommitSig(), badVote.CommitSig()})
	}
}

func TestValidateBlockEvidence(t *testing.T) {
	proxyApp := newTestApp()
	require.NoError(t, proxyApp.Start())
	defer proxyApp.Stop()

	state, stateDB, privVals := makeState(4, 1)
	state.ConsensusParams.Evidence.MaxNum = 3
	blockExec := sm.NewBlockExecutor(
		stateDB,
		log.TestingLogger(),
		proxyApp.Consensus(),
		memmock.Mempool{},
		sm.MockEvidencePool{},
	)
	lastCommit := types.NewCommit(0, 0, types.BlockID{}, nil)

	for height := int64(1); height < validationTestsStopHeight; height++ {
		proposerAddr := state.Validators.GetProposer().Address
		maxNumEvidence := state.ConsensusParams.Evidence.MaxNum
		t.Log(maxNumEvidence)
		if height > 1 {
			/*
				A block with too much evidence fails
			*/
			require.True(t, maxNumEvidence > 2)
			evidence := make([]types.Evidence, 0)
			// one more than the maximum allowed evidence
			for i := uint32(0); i <= maxNumEvidence; i++ {
				evidence = append(evidence, types.NewMockEvidence(height, time.Now(), proposerAddr))
			}
			block, _ := state.MakeBlock(height, makeTxs(height), lastCommit, evidence, proposerAddr)
			err := blockExec.ValidateBlock(state, block)
			_, ok := err.(*types.ErrEvidenceOverflow)
			require.True(t, ok, "expected error to be of type ErrEvidenceOverflow at height %d", height)
		}

		/*
			A good block with several pieces of good evidence passes
		*/
		require.True(t, maxNumEvidence > 2)
		evidence := make([]types.Evidence, 0)
		// precisely the amount of allowed evidence
		for i := uint32(0); i < maxNumEvidence; i++ {
			// make different evidence for each validator
			addr, _ := state.Validators.GetByIndex(int(i))
			evidence = append(evidence, types.NewMockEvidence(height, time.Now(), addr))
		}

		var err error
		state, _, lastCommit, err = makeAndCommitGoodBlock(
			state,
			height,
			lastCommit,
			proposerAddr,
			blockExec,
			privVals,
			evidence,
		)
		require.NoError(t, err, "height %d", height)
	}
}

func TestValidateFailBlockOnCommittedEvidence(t *testing.T) {
	var height int64 = 1
	state, stateDB, _ := makeState(2, int(height))
	addr, _ := state.Validators.GetByIndex(0)
	addr2, _ := state.Validators.GetByIndex(1)
	ev := types.NewMockEvidence(height, defaultTestTime, addr)
	ev2 := types.NewMockEvidence(height, defaultTestTime, addr2)

	evpool := &mocks.EvidencePool{}
	evpool.On("IsPending", mock.AnythingOfType("types.MockEvidence")).Return(false)
	evpool.On("IsCommitted", ev).Return(false)
	evpool.On("IsCommitted", ev2).Return(true)

	blockExec := sm.NewBlockExecutor(
		stateDB, log.TestingLogger(),
		nil,
		nil,
		evpool)
	// A block with a couple pieces of evidence passes.
	block := makeBlock(state, height)
	block.Evidence.Evidence = []types.Evidence{ev, ev2}
	block.EvidenceHash = block.Evidence.Hash()
	err := blockExec.ValidateBlock(state, block)

	assert.Error(t, err)
	assert.IsType(t, err, &types.ErrEvidenceInvalid{})
}

func TestValidateAlreadyPendingEvidence(t *testing.T) {
	var height int64 = 1
	state, stateDB, _ := makeState(2, int(height))
	addr, _ := state.Validators.GetByIndex(0)
	addr2, _ := state.Validators.GetByIndex(1)
	ev := types.NewMockEvidence(height, defaultTestTime, addr)
	ev2 := types.NewMockEvidence(height, defaultTestTime, addr2)

	evpool := &mocks.EvidencePool{}
	evpool.On("IsPending", ev).Return(false)
	evpool.On("IsPending", ev2).Return(true)
	evpool.On("IsCommitted", mock.AnythingOfType("types.MockEvidence")).Return(false)

	blockExec := sm.NewBlockExecutor(
		stateDB, log.TestingLogger(),
		nil,
		nil,
		evpool)
	// A block with a couple pieces of evidence passes.
	block := makeBlock(state, height)
	// add one evidence seen before and one evidence that hasn't
	block.Evidence.Evidence = []types.Evidence{ev, ev2}
	block.EvidenceHash = block.Evidence.Hash()
	err := blockExec.ValidateBlock(state, block)

	assert.NoError(t, err)
}

func TestValidateDuplicateEvidenceShouldFail(t *testing.T) {
	var height int64 = 1
	state, stateDB, _ := makeState(1, int(height))
	addr, _ := state.Validators.GetByIndex(0)
	addr2, _ := state.Validators.GetByIndex(1)
	ev := types.NewMockEvidence(height, defaultTestTime, addr)
	ev2 := types.NewMockEvidence(height, defaultTestTime, addr2)

	blockExec := sm.NewBlockExecutor(
		stateDB, log.TestingLogger(),
		nil,
		nil,
		sm.MockEvidencePool{})
	// A block with a couple pieces of evidence passes.
	block := makeBlock(state, height)
	block.Evidence.Evidence = []types.Evidence{ev, ev2, ev2}
	block.EvidenceHash = block.Evidence.Hash()
	err := blockExec.ValidateBlock(state, block)

	assert.Error(t, err)
}

var blockId = types.BlockID{
	Hash: []byte("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"),
	PartsHeader: types.PartSetHeader{
		Total: 1,
		Hash:  []byte("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"),
	},
}

func TestValidateAmnesiaEvidence(t *testing.T) {
	var height int64 = 1
	state, stateDB, vals := makeState(1, int(height))
	addr, val := state.Validators.GetByIndex(0)
	voteA := makeVote(height, 1, 0, addr, blockId)
	err := vals[val.Address.String()].SignVote(chainID, voteA)
	require.NoError(t, err)
	voteB := makeVote(height, 2, 0, addr, types.BlockID{})
	err = vals[val.Address.String()].SignVote(chainID, voteB)
	require.NoError(t, err)
	var ae types.Evidence
	ae = types.AmnesiaEvidence{
		PotentialAmnesiaEvidence: types.PotentialAmnesiaEvidence{
			VoteA: voteA,
			VoteB: voteB,
		},
		Polc: types.EmptyPOLC(),
	}

	evpool := &mocks.EvidencePool{}
	evpool.On("IsPending", ae).Return(false)
	evpool.On("IsCommitted", ae).Return(false)
	evpool.On("AddEvidence", ae).Return(fmt.Errorf("test error"))

	blockExec := sm.NewBlockExecutor(
		stateDB, log.TestingLogger(),
		nil,
		nil,
		evpool)
	// A block with a couple pieces of evidence passes.
	block := makeBlock(state, height)
	block.Evidence.Evidence = []types.Evidence{ae}
	block.EvidenceHash = block.Evidence.Hash()
	err = blockExec.ValidateBlock(state, block)

	errMsg := "Invalid evidence: unknown amnesia evidence, trying to add to evidence pool, err: test error"
	if assert.Error(t, err) {
		assert.Equal(t, err.Error()[:len(errMsg)], errMsg)
	}
}

func TestVerifyEvidenceWrongAddress(t *testing.T) {
	var height int64 = 1
	state, stateDB, _ := makeState(1, int(height))
	randomAddr := []byte("wrong address")
	ev := types.NewMockEvidence(height, defaultTestTime, randomAddr)

	blockExec := sm.NewBlockExecutor(
		stateDB, log.TestingLogger(),
		nil,
		nil,
		sm.MockEvidencePool{})
	// A block with a couple pieces of evidence passes.
	block := makeBlock(state, height)
	block.Evidence.Evidence = []types.Evidence{ev}
	block.EvidenceHash = block.Evidence.Hash()
	err := blockExec.ValidateBlock(state, block)
	errMsg := "Invalid evidence: address 77726F6E672061646472657373 was not a validator at height 1"
	if assert.Error(t, err) {
		assert.Equal(t, err.Error()[:len(errMsg)], errMsg)
	}
}

func TestVerifyEvidenceExpiredEvidence(t *testing.T) {
	var height int64 = 4
	state, stateDB, _ := makeState(1, int(height))
	state.ConsensusParams.Evidence.MaxAgeNumBlocks = 1
	addr, _ := state.Validators.GetByIndex(0)
	ev := types.NewMockEvidence(1, defaultTestTime, addr)
	err := sm.VerifyEvidence(stateDB, state, ev, nil)
	errMsg := "evidence from height 1 (created at: 2019-01-01 00:00:00 +0000 UTC) is too old"
	if assert.Error(t, err) {
		assert.Equal(t, err.Error()[:len(errMsg)], errMsg)
	}
}

func TestVerifyEvidenceWithAmnesiaEvidence(t *testing.T) {
	var height int64 = 1
	state, stateDB, vals := makeState(4, int(height))
	addr, val := state.Validators.GetByIndex(0)
	addr2, val2 := state.Validators.GetByIndex(1)
	voteA := makeVote(height, 1, 0, addr, types.BlockID{})
	err := vals[val.Address.String()].SignVote(chainID, voteA)
	require.NoError(t, err)
	voteB := makeVote(height, 2, 0, addr, blockId)
	err = vals[val.Address.String()].SignVote(chainID, voteB)
	require.NoError(t, err)
	voteC := makeVote(height, 2, 1, addr2, blockId)
	err = vals[val2.Address.String()].SignVote(chainID, voteC)
	require.NoError(t, err)
	//var ae types.Evidence
	badAe := types.AmnesiaEvidence{
		PotentialAmnesiaEvidence: types.PotentialAmnesiaEvidence{
			VoteA: voteA,
			VoteB: voteB,
		},
		Polc: types.ProofOfLockChange{
			Votes:  []types.Vote{*voteC},
			PubKey: val.PubKey,
		},
	}
	err = sm.VerifyEvidence(stateDB, state, badAe, nil)
	if assert.Error(t, err) {
		assert.Equal(t, err.Error(), "amnesia evidence contains invalid polc, err: not enough voting power to reach majority needed: 2667, got 1000")
	}
	addr3, val3 := state.Validators.GetByIndex(2)
	voteD := makeVote(height, 2, 2, addr3, blockId)
	err = vals[val3.Address.String()].SignVote(chainID, voteD)
	require.NoError(t, err)
	addr4, val4 := state.Validators.GetByIndex(3)
	voteE := makeVote(height, 2, 3, addr4, blockId)
	err = vals[val4.Address.String()].SignVote(chainID, voteE)
	require.NoError(t, err)

	goodAe := types.AmnesiaEvidence{
		PotentialAmnesiaEvidence: types.PotentialAmnesiaEvidence{
			VoteA: voteA,
			VoteB: voteB,
		},
		Polc: types.ProofOfLockChange{
			Votes:  []types.Vote{*voteC, *voteD, *voteE},
			PubKey: val.PubKey,
		},
	}
	err = sm.VerifyEvidence(stateDB, state, goodAe, nil)
	assert.NoError(t, err)

	goodAe = types.AmnesiaEvidence{
		PotentialAmnesiaEvidence: types.PotentialAmnesiaEvidence{
			VoteA: voteA,
			VoteB: voteB,
		},
		Polc: types.EmptyPOLC(),
	}
	err = sm.VerifyEvidence(stateDB, state, goodAe, nil)
	assert.NoError(t, err)

}

func TestVerifyEvidenceWithLunaticValidatorEvidence(t *testing.T) {

}

func TestVerifyEvidenceWithPhantomValidatorEvidence(t *testing.T) {
	state, stateDB, vals := makeState(4, 3)
	addr, val := state.Validators.GetByIndex(0)
	h := &types.Header{
		Version:            version.Consensus{Block: 1, App: 2},
		ChainID:            chainID,
		Height:             2,
		Time:               defaultTestTime,
		LastBlockID:        blockId,
		LastCommitHash:     tmhash.Sum([]byte("last_commit_hash")),
		DataHash:           tmhash.Sum([]byte("data_hash")),
		ValidatorsHash:     tmhash.Sum([]byte("validators_hash")),
		NextValidatorsHash: tmhash.Sum([]byte("next_validators_hash")),
		ConsensusHash:      tmhash.Sum([]byte("consensus_hash")),
		AppHash:            tmhash.Sum([]byte("app_hash")),
		LastResultsHash:    tmhash.Sum([]byte("last_results_hash")),
		EvidenceHash:       tmhash.Sum([]byte("evidence_hash")),
		ProposerAddress:    crypto.AddressHash([]byte("proposer_address")),
	}
	vote := makeVote(2, 1, 0, addr, blockId)
	err := vals[val.Address.String()].SignVote(chainID, vote)
	require.NoError(t, err)
	ev := types.PhantomValidatorEvidence{
		Header:                      h,
		Vote:                        vote,
		LastHeightValidatorWasInSet: 1,
	}
	err = ev.ValidateBasic()
	assert.NoError(t, err)
	err = sm.VerifyEvidence(stateDB, state, ev, nil)
	assert.Error(t, err)
	t.Log(err)
}

func makeVote(height int64, round, index int, addr bytes.HexBytes, blockID types.BlockID) *types.Vote {
	return &types.Vote{
		Type:             types.SignedMsgType(2),
		Height:           height,
		Round:            round,
		BlockID:          blockID,
		Timestamp:        time.Now(),
		ValidatorAddress: addr,
		ValidatorIndex:   index,
	}
}
