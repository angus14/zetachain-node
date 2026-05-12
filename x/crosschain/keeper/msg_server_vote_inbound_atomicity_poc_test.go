package keeper_test

import (
	"testing"

	sdkmath "cosmossdk.io/math"
	"github.com/stretchr/testify/require"

	"github.com/zeta-chain/node/pkg/coin"
	keepertest "github.com/zeta-chain/node/testutil/keeper"
	"github.com/zeta-chain/node/testutil/sample"
	"github.com/zeta-chain/node/x/crosschain/keeper"
	"github.com/zeta-chain/node/x/crosschain/types"
	observertypes "github.com/zeta-chain/node/x/observer/types"
)

func TestPOC_VoteInbound_FinalizedBallotButNoCCTX_WhenValidateInboundFails(t *testing.T) {
	k, ctx, _, zk := keepertest.CrosschainKeeper(t)
	msgServer := keeper.NewMsgServerImpl(*k)

	validatorList := setObservers(t, k, ctx, zk)
	zk.ObserverKeeper.SetTSS(ctx, sample.Tss())

	receiverChain := int64(0)
	for _, ch := range zk.ObserverKeeper.GetSupportedChains(ctx) {
		if ch.IsExternalChain() && ch.ChainId != 1337 {
			receiverChain = ch.ChainId
			break
		}
	}
	require.NotZero(t, receiverChain)

	msg := &types.MsgVoteInbound{
		Creator:       "",
		Sender:        "0x954598965C2aCdA2885B037561526260764095B8",
		SenderChainId: 1337,
		TxOrigin:      "0x954598965C2aCdA2885B037561526260764095B8",
		Receiver:      "0x954598965C2aCdA2885B037561526260764095B8",
		ReceiverChain: receiverChain,

		Amount: sdkmath.NewUint(1),

		Message: "",

		InboundHash:        "0x1111111111111111111111111111111111111111111111111111111111111111",
		InboundBlockHeight: 1,
		CallOptions: &types.CallOptions{
			GasLimit: 1000000000,
		},
		CoinType:                coin.CoinType_ERC20,
		Asset:                   "",
		EventIndex:              1,
		ProtocolContractVersion: types.ProtocolContractVersion_V2,
		RevertOptions:           types.NewEmptyRevertOptions(),
		Status:                  types.InboundStatus_SUCCESS,
		ConfirmationMode:        types.ConfirmationMode_SAFE,
	}

	k.SetInboundTracker(ctx, types.InboundTracker{
		ChainId:  msg.SenderChainId,
		TxHash:   msg.InboundHash,
		CoinType: msg.CoinType,
	})

	var finalErr error
	for _, validatorAddr := range validatorList {
		msg.Creator = validatorAddr
		_, err := msgServer.VoteInbound(ctx, msg)
		if err != nil {
			finalErr = err
			break
		}
	}

	require.Error(t, finalErr)
	require.Contains(t, finalErr.Error(), "failed to validate inbound")

	ballot, found := zk.ObserverKeeper.GetBallot(ctx, msg.Digest())
	require.True(t, found)
	require.Equal(t, observertypes.BallotStatus_BallotFinalized_SuccessObservation, ballot.BallotStatus)

	_, found = k.GetCrossChainTx(ctx, msg.Digest())
	require.False(t, found)

	require.False(t, k.IsFinalizedInbound(ctx, msg.InboundHash, msg.SenderChainId, msg.EventIndex))

	_, trackerFound := k.GetInboundTracker(ctx, msg.SenderChainId, msg.InboundHash)
	require.True(t, trackerFound)

	// Retry same inbound after the failed finalization.
	// The ballot is already finalized, so VoteInbound will not re-enter ValidateInbound.
	msg.Creator = validatorList[0]
	_, err := msgServer.VoteInbound(ctx, msg)
	require.Error(t, err)

	_, found = k.GetCrossChainTx(ctx, msg.Digest())
	require.False(t, found)
}
