package storageprovider

// this file implements storagemarket.StorageProviderNode

import (
	"context"
	"fmt"

	"github.com/ipfs/go-cid"
	logging "github.com/ipfs/go-log/v2"

	"github.com/filecoin-project/go-address"
	cborutil "github.com/filecoin-project/go-cbor-util"
	"github.com/filecoin-project/go-fil-markets/shared"
	"github.com/filecoin-project/go-fil-markets/storagemarket"
	"github.com/filecoin-project/go-fil-markets/storagemarket/network"
	"github.com/filecoin-project/go-state-types/abi"
	"github.com/filecoin-project/go-state-types/builtin"
	"github.com/filecoin-project/go-state-types/crypto"
	"github.com/filecoin-project/go-state-types/exitcode"

	"github.com/filecoin-project/venus-market/v2/api/clients"
	"github.com/filecoin-project/venus-market/v2/api/clients/signer"
	"github.com/filecoin-project/venus-market/v2/config"
	"github.com/filecoin-project/venus-market/v2/fundmgr"
	"github.com/filecoin-project/venus-market/v2/utils"

	"github.com/filecoin-project/venus/pkg/constants"
	vCrypto "github.com/filecoin-project/venus/pkg/crypto"
	"github.com/filecoin-project/venus/pkg/events/state"
	marketactor "github.com/filecoin-project/venus/venus-shared/actors/builtin/market"
	"github.com/filecoin-project/venus/venus-shared/actors/builtin/miner"
	v1api "github.com/filecoin-project/venus/venus-shared/api/chain/v1"
	"github.com/filecoin-project/venus/venus-shared/types"
	types2 "github.com/filecoin-project/venus/venus-shared/types/market"
)

var log = logging.Logger("storageadapter")

type ProviderNodeAdapter struct {
	v1api.FullNode

	cfg *config.MarketConfig

	fundMgr   *fundmgr.FundManager
	msgClient clients.IMixMessage

	dealPublisher *DealPublisher

	extendPieceMeta DealAssiger
	dsMatcher       *dealStateMatcher
	dealInfo        *CurrentDealInfoManager

	signer signer.ISigner
}

func NewProviderNodeAdapter(cfg *config.MarketConfig) func(
	dealPublisher *DealPublisher,
	extendPieceMeta DealAssiger,
	fundMgr *fundmgr.FundManager,
	fullNode v1api.FullNode,
	msgClient clients.IMixMessage,
	signer signer.ISigner,
) StorageProviderNode {
	return func(
		dealPublisher *DealPublisher,
		extendPieceMeta DealAssiger,
		fundMgr *fundmgr.FundManager,
		fullNode v1api.FullNode,
		msgClient clients.IMixMessage,
		signer signer.ISigner,
	) StorageProviderNode {
		na := &ProviderNodeAdapter{
			FullNode:        fullNode,
			msgClient:       msgClient,
			dealPublisher:   dealPublisher,
			dsMatcher:       newDealStateMatcher(state.NewStatePredicates(state.WrapFastAPI(fullNode))),
			extendPieceMeta: extendPieceMeta,
			fundMgr:         fundMgr,
			cfg:             cfg,
			signer:          signer,
		}
		na.dealInfo = &CurrentDealInfoManager{
			CDAPI: &CurrentDealInfoAPIAdapter{CurrentDealInfoTskAPI: na},
		}
		return na
	}
}

func (pna *ProviderNodeAdapter) PublishDeals(ctx context.Context, deal types2.MinerDeal) (cid.Cid, error) {
	return pna.dealPublisher.Publish(ctx, deal.ClientDealProposal)
}

func (pna *ProviderNodeAdapter) VerifySignature(ctx context.Context, sig crypto.Signature, addr address.Address, input []byte, _ shared.TipSetToken) (bool, error) {
	addr, err := pna.StateAccountKey(ctx, addr, types.EmptyTSK)
	if err != nil {
		return false, err
	}

	err = vCrypto.Verify(&sig, addr, input)
	return err == nil, err
}

func (pna *ProviderNodeAdapter) GetMinerWorkerAddress(ctx context.Context, maddr address.Address, tok shared.TipSetToken) (address.Address, error) {
	tsk, err := types.TipSetKeyFromBytes(tok)
	if err != nil {
		return address.Undef, err
	}

	mi, err := pna.StateMinerInfo(ctx, maddr, tsk)
	if err != nil {
		return address.Address{}, err
	}
	return mi.Worker, nil
}

func (pna *ProviderNodeAdapter) GetProofType(ctx context.Context, maddr address.Address, tok shared.TipSetToken) (abi.RegisteredSealProof, error) {
	tsk, err := types.TipSetKeyFromBytes(tok)
	if err != nil {
		return 0, err
	}

	mi, err := pna.StateMinerInfo(ctx, maddr, tsk)
	if err != nil {
		return 0, err
	}

	nver, err := pna.StateNetworkVersion(ctx, tsk)
	if err != nil {
		return 0, err
	}

	return miner.PreferredSealProofTypeFromWindowPoStType(nver, mi.WindowPoStProofType)
}

func (pna *ProviderNodeAdapter) Sign(ctx context.Context, data interface{}) (*crypto.Signature, error) {
	switch data.(type) {
	case *types2.SignInfo:

	default:
		return nil, fmt.Errorf("data type is not SignInfo")
	}

	info := data.(*types2.SignInfo)
	msgBytes, err := cborutil.Dump(info.Data)
	if err != nil {
		return nil, fmt.Errorf("serializing: %w", err)
	}

	tok, _, err := pna.GetChainHead(ctx)
	if err != nil {
		return nil, fmt.Errorf("couldn't get chain head: %w", err)
	}

	worker, err := pna.GetMinerWorkerAddress(ctx, info.Addr, tok)
	if err != nil {
		return nil, err
	}

	signerAddr, err := pna.StateAccountKey(ctx, worker, types.EmptyTSK)
	if err != nil {
		return nil, err
	}
	localSignature, err := pna.signer.WalletSign(ctx, signerAddr, msgBytes, types.MsgMeta{Type: info.Type})
	if err != nil {
		return nil, err
	}
	return localSignature, nil
}

func (pna *ProviderNodeAdapter) SignWithGivenMiner(mAddr address.Address) network.ResigningFunc {
	return func(ctx context.Context, data interface{}) (*crypto.Signature, error) {
		msgBytes, err := cborutil.Dump(data)
		if err != nil {
			return nil, fmt.Errorf("serializing: %w", err)
		}

		tok, _, err := pna.GetChainHead(ctx)
		if err != nil {
			return nil, fmt.Errorf("couldn't get chain head: %w", err)
		}

		worker, err := pna.GetMinerWorkerAddress(ctx, mAddr, tok)
		if err != nil {
			return nil, err
		}

		signerAddr, err := pna.StateAccountKey(ctx, worker, types.EmptyTSK)
		if err != nil {
			return nil, err
		}
		localSignature, err := pna.signer.WalletSign(ctx, signerAddr, msgBytes, types.MsgMeta{
			Type: types.MTUnknown,
		})
		if err != nil {
			return nil, err
		}
		return localSignature, nil
	}
}

func (pna *ProviderNodeAdapter) ReserveFunds(ctx context.Context, wallet, addr address.Address, amt abi.TokenAmount) (cid.Cid, error) {
	return pna.fundMgr.Reserve(ctx, wallet, addr, amt)
}

func (pna *ProviderNodeAdapter) ReleaseFunds(ctx context.Context, addr address.Address, amt abi.TokenAmount) error {
	return pna.fundMgr.Release(addr, amt)
}

// Adds funds with the StorageMinerActor for a piecestorage participant.  Used by both providers and clients.
func (pna *ProviderNodeAdapter) AddFunds(ctx context.Context, addr address.Address, amount abi.TokenAmount) (cid.Cid, error) {
	// (Provider Node API)
	pCfg := pna.cfg.MinerProviderConfig(addr, true)
	msgId, err := pna.msgClient.PushMessage(ctx,
		&types.Message{
			To:     marketactor.Address,
			From:   addr,
			Value:  amount,
			Method: builtin.MethodsMarket.AddBalance,
		},
		&types.MessageSendSpec{MaxFee: abi.TokenAmount(pCfg.MaxMarketBalanceAddFee)},
	)
	if err != nil {
		return cid.Undef, err
	}

	return msgId, nil
}

func (pna *ProviderNodeAdapter) GetBalance(ctx context.Context, addr address.Address, encodedTs shared.TipSetToken) (storagemarket.Balance, error) {
	tsk, err := types.TipSetKeyFromBytes(encodedTs)
	if err != nil {
		return storagemarket.Balance{}, err
	}

	bal, err := pna.StateMarketBalance(ctx, addr, tsk)
	if err != nil {
		return storagemarket.Balance{}, err
	}

	return utils.ToSharedBalance(bal), nil
}

func (pna *ProviderNodeAdapter) DealProviderCollateralBounds(ctx context.Context, provider address.Address, size abi.PaddedPieceSize, isVerified bool) (abi.TokenAmount, abi.TokenAmount, error) {
	bounds, err := pna.StateDealProviderCollateralBounds(ctx, size, isVerified, types.EmptyTSK)
	if err != nil {
		return abi.TokenAmount{}, abi.TokenAmount{}, err
	}

	// The maximum amount of collateral that the provider will put into escrow
	// for a deal is calculated as a multiple of the minimum bounded amount
	pCfg := pna.cfg.MinerProviderConfig(provider, true)
	max := types.BigMul(bounds.Min, types.NewInt(pCfg.MaxProviderCollateralMultiplier))

	return bounds.Min, max, nil
}

func (pna *ProviderNodeAdapter) GetChainHead(ctx context.Context) (shared.TipSetToken, abi.ChainEpoch, error) {
	head, err := pna.ChainHead(ctx)
	if err != nil {
		return nil, 0, err
	}

	return head.Key().Bytes(), head.Height(), nil
}

func (pna *ProviderNodeAdapter) WaitForMessage(ctx context.Context, mcid cid.Cid, cb func(code exitcode.ExitCode, bytes []byte, finalCid cid.Cid, err error) error) error {
	receipt, err := pna.msgClient.WaitMsg(ctx, mcid, 2*constants.MessageConfidence, constants.LookbackNoLimit, true)
	if err != nil {
		return cb(0, nil, cid.Undef, err)
	}
	ctx.Done()
	return cb(receipt.Receipt.ExitCode, receipt.Receipt.Return, receipt.Message, nil)
}

func (pna *ProviderNodeAdapter) WaitForPublishDeals(ctx context.Context, publishCid cid.Cid, proposal types.DealProposal) (*storagemarket.PublishDealsWaitResult, error) {
	// Wait for deal to be published (plus additional time for confidence)
	receipt, err := pna.msgClient.WaitMsg(ctx, publishCid, 2*constants.MessageConfidence, constants.LookbackNoLimit, true)
	if err != nil {
		return nil, fmt.Errorf("WaitForPublishDeals errored: %w", err)
	}
	if receipt.Receipt.ExitCode != exitcode.Ok {
		return nil, fmt.Errorf("WaitForPublishDeals exit code: %s", receipt.Receipt.ExitCode)
	}

	// The deal ID may have changed since publish if there was a reorg, so
	// get the current deal ID
	head, err := pna.ChainHead(ctx)
	if err != nil {
		return nil, fmt.Errorf("WaitForPublishDeals failed to get chain head: %w", err)
	}

	res, err := pna.dealInfo.GetCurrentDealInfo(ctx, head.Key(), &proposal, publishCid)
	if err != nil {
		return nil, fmt.Errorf("WaitForPublishDeals getting deal info errored: %w", err)
	}

	return &storagemarket.PublishDealsWaitResult{DealID: res.DealID, FinalCid: receipt.Message}, nil
}

func (pna *ProviderNodeAdapter) GetDataCap(ctx context.Context, addr address.Address, encodedTs shared.TipSetToken) (*abi.StoragePower, error) {
	tsk, err := types.TipSetKeyFromBytes(encodedTs)
	if err != nil {
		return nil, err
	}

	sp, err := pna.StateVerifiedClientStatus(ctx, addr, tsk)
	return sp, err
}

func (pna *ProviderNodeAdapter) SearchMsg(ctx context.Context, from types.TipSetKey, msg cid.Cid, limit abi.ChainEpoch, allowReplaced bool) (*types.MsgLookup, error) {
	return pna.msgClient.WaitMsg(ctx, msg, constants.MessageConfidence, limit, allowReplaced)
}

func (pna *ProviderNodeAdapter) GetMessage(ctx context.Context, mc cid.Cid) (*types.Message, error) {
	return pna.msgClient.GetMessage(ctx, mc)
}

// StorageProviderNode are common interfaces provided by a filecoin Node to both StorageClient and StorageProvider
type StorageProviderNode interface {
	v1api.FullNode
	// Sign sign the given data with the given address's private key
	Sign(ctx context.Context, data interface{}) (*crypto.Signature, error)

	// SignWithGivenMiner sign the data with the worker address of the given miner
	SignWithGivenMiner(mAddr address.Address) network.ResigningFunc

	// GetChainHead returns a tipset token for the current chain head
	GetChainHead(ctx context.Context) (shared.TipSetToken, abi.ChainEpoch, error)

	// Adds funds with the StorageMinerActor for a storage participant.  Used by both providers and clients.
	AddFunds(ctx context.Context, addr address.Address, amount abi.TokenAmount) (cid.Cid, error)

	// ReserveFunds reserves the given amount of funds is ensures it is available for the deal
	ReserveFunds(ctx context.Context, wallet, addr address.Address, amt abi.TokenAmount) (cid.Cid, error)

	// ReleaseFunds releases funds reserved with ReserveFunds
	ReleaseFunds(ctx context.Context, addr address.Address, amt abi.TokenAmount) error

	// GetBalance returns locked/unlocked for a storage participant.  Used by both providers and clients.
	GetBalance(ctx context.Context, addr address.Address, tok shared.TipSetToken) (storagemarket.Balance, error)

	// VerifySignature verifies a given set of data was signed properly by a given address's private key
	VerifySignature(ctx context.Context, signature crypto.Signature, signer address.Address, plaintext []byte, tok shared.TipSetToken) (bool, error)

	// WaitForMessage waits until a message appears on chain. If it is already on chain, the callback is called immediately
	WaitForMessage(ctx context.Context, mcid cid.Cid, onCompletion func(exitcode.ExitCode, []byte, cid.Cid, error) error) error

	// DealProviderCollateralBounds returns the min and max collateral a storage provider can issue.
	DealProviderCollateralBounds(ctx context.Context, provider address.Address, size abi.PaddedPieceSize, isVerified bool) (abi.TokenAmount, abi.TokenAmount, error)

	// PublishDeals publishes a deal on chain, returns the message cid, but does not wait for message to appear
	PublishDeals(ctx context.Context, deal types2.MinerDeal) (cid.Cid, error)

	// WaitForPublishDeals waits for a deal publish message to land on chain.
	WaitForPublishDeals(ctx context.Context, mcid cid.Cid, proposal types.DealProposal) (*storagemarket.PublishDealsWaitResult, error)

	// GetMinerWorkerAddress returns the worker address associated with a miner
	GetMinerWorkerAddress(ctx context.Context, addr address.Address, tok shared.TipSetToken) (address.Address, error)

	// GetDataCap gets the current data cap for addr
	GetDataCap(ctx context.Context, addr address.Address, tok shared.TipSetToken) (*abi.StoragePower, error)

	// GetProofType gets the current seal proof type for the given miner.
	GetProofType(ctx context.Context, addr address.Address, tok shared.TipSetToken) (abi.RegisteredSealProof, error)
}

var _ StorageProviderNode = &ProviderNodeAdapter{}
