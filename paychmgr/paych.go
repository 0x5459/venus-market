package paychmgr

import (
	"context"
	"errors"
	"fmt"

	"github.com/ipfs/go-cid"

	"github.com/filecoin-project/go-address"
	cborutil "github.com/filecoin-project/go-cbor-util"
	"github.com/filecoin-project/go-state-types/actors"
	"github.com/filecoin-project/go-state-types/big"

	"github.com/filecoin-project/venus-market/v2/models/repo"

	"github.com/filecoin-project/venus/pkg/crypto"
	lpaych "github.com/filecoin-project/venus/venus-shared/actors/builtin/paych"
	types2 "github.com/filecoin-project/venus/venus-shared/types"
	types "github.com/filecoin-project/venus/venus-shared/types/market"
)

// insufficientFundsErr indicates that there are not enough funds in the
// channel to create a voucher
type insufficientFundsErr interface {
	Shortfall() big.Int
}

type ErrInsufficientFunds struct {
	shortfall big.Int
}

func newErrInsufficientFunds(shortfall big.Int) *ErrInsufficientFunds {
	return &ErrInsufficientFunds{shortfall: shortfall}
}

func (e *ErrInsufficientFunds) Error() string {
	return fmt.Sprintf("not enough funds in channel to cover voucher - shortfall: %d", e.shortfall)
}

func (e *ErrInsufficientFunds) Shortfall() big.Int {
	return e.shortfall
}

type laneState struct {
	redeemed big.Int
	nonce    uint64
}

func (ls laneState) Redeemed() (big.Int, error) {
	return ls.redeemed, nil
}

func (ls laneState) Nonce() (uint64, error) {
	return ls.nonce, nil
}

// channelAccessor is used to simplify locking when accessing a channel
type channelAccessor struct {
	from address.Address
	to   address.Address

	// chctx is used by background processes (eg when waiting for things to be
	// confirmed on chain)
	chctx           context.Context
	sa              *stateAccessor
	api             managerAPI
	channelInfoRepo repo.PaychChannelInfoRepo
	msgInfoRepo     repo.PaychMsgInfoRepo
	lk              *channelLock
	fundsReqQueue   []*fundsReq
	msgListeners    msgListeners
}

func newChannelAccessor(pm *Manager, from address.Address, to address.Address) *channelAccessor {
	return &channelAccessor{
		from:            from,
		to:              to,
		chctx:           pm.ctx,
		sa:              pm.sa,
		api:             pm.pchapi,
		channelInfoRepo: pm.channelInfoRepo,
		msgInfoRepo:     pm.msgInfoRepo,
		lk:              &channelLock{globalLock: &pm.lk},
		msgListeners:    newMsgListeners(),
	}
}

func (ca *channelAccessor) messageBuilder(ctx context.Context, from address.Address) (lpaych.MessageBuilder, error) {
	nwVersion, err := ca.api.StateNetworkVersion(ctx, types2.EmptyTSK)
	if err != nil {
		return nil, err
	}

	ver, err := actors.VersionForNetwork(nwVersion)
	if err != nil {
		return nil, err
	}
	return lpaych.Message(ver, from), nil
}

func (ca *channelAccessor) getChannelInfo(ctx context.Context, addr address.Address) (*types.ChannelInfo, error) {
	ca.lk.Lock()
	defer ca.lk.Unlock()

	return ca.channelInfoRepo.GetChannelByAddress(ctx, addr)
}

func (ca *channelAccessor) outboundActiveByFromTo(ctx context.Context, from, to address.Address) (*types.ChannelInfo, error) {
	ca.lk.Lock()
	defer ca.lk.Unlock()

	return ca.channelInfoRepo.OutboundActiveByFromTo(ctx, from, to)
}

// createVoucher creates a voucher with the given specification, setting its
// nonce, signing the voucher and storing it in the local datastore.
// If there are not enough funds in the channel to create the voucher, returns
// the shortfall in funds.
func (ca *channelAccessor) createVoucher(ctx context.Context, ch address.Address, voucher types2.SignedVoucher) (*VoucherCreateResult, error) {
	ca.lk.Lock()
	defer ca.lk.Unlock()

	// Find the channel for the voucher
	ci, err := ca.channelInfoRepo.GetChannelByAddress(ctx, ch)
	if err != nil {
		return nil, fmt.Errorf("failed to get channel info by address: %w", err)
	}

	// Set the voucher channel
	sv := &voucher
	sv.ChannelAddr = ch

	// Get the next nonce on the given lane
	sv.Nonce = ca.nextNonceForLane(ci, voucher.Lane)

	// Sign the voucher
	vb, err := sv.SigningBytes()
	if err != nil {
		return nil, fmt.Errorf("failed to get voucher signing bytes: %w", err)
	}

	sig, err := ca.api.WalletSign(ctx, ci.Control, vb)
	if err != nil {
		return nil, fmt.Errorf("failed to sign voucher: %w", err)
	}
	sv.Signature = sig

	// Store the voucher
	if _, err := ca.addVoucherUnlocked(ctx, ch, sv, big.NewInt(0)); err != nil {
		// If there are not enough funds in the channel to cover the voucher,
		// return a voucher create result with the shortfall
		var ife insufficientFundsErr
		if errors.As(err, &ife) {
			return &VoucherCreateResult{
				Shortfall: ife.Shortfall(),
			}, nil
		}

		return nil, fmt.Errorf("failed to persist voucher: %w", err)
	}

	return &VoucherCreateResult{Voucher: sv, Shortfall: big.NewInt(0)}, nil
}

func (ca *channelAccessor) nextNonceForLane(ci *types.ChannelInfo, lane uint64) uint64 {
	var maxnonce uint64
	for _, v := range ci.Vouchers {
		if v.Voucher.Lane == lane {
			if v.Voucher.Nonce > maxnonce {
				maxnonce = v.Voucher.Nonce
			}
		}
	}

	return maxnonce + 1
}

func (ca *channelAccessor) checkVoucherValid(ctx context.Context, ch address.Address, sv *types2.SignedVoucher) (map[uint64]lpaych.LaneState, error) {
	ca.lk.Lock()
	defer ca.lk.Unlock()

	return ca.checkVoucherValidUnlocked(ctx, ch, sv)
}

func (ca *channelAccessor) checkVoucherValidUnlocked(ctx context.Context, ch address.Address, sv *types2.SignedVoucher) (map[uint64]lpaych.LaneState, error) {
	if sv.ChannelAddr != ch {
		return nil, fmt.Errorf("voucher ChannelAddr doesn't match channel address, got %s, expected %s", sv.ChannelAddr, ch)
	}

	// check voucher is unlocked
	if sv.Extra != nil {
		return nil, fmt.Errorf("voucher is Message Locked")
	}
	if sv.TimeLockMax != 0 {
		return nil, fmt.Errorf("voucher is Max Time Locked")
	}
	if sv.TimeLockMin != 0 {
		return nil, fmt.Errorf("voucher is Min Time Locked")
	}
	if len(sv.SecretHash) != 0 {
		return nil, fmt.Errorf("voucher is Hash Locked")
	}

	// Load payment channel actor state
	act, pchState, err := ca.sa.loadPaychActorState(ctx, ch)
	if err != nil {
		return nil, err
	}

	// Load channel "From" account actor state
	f, err := pchState.From()
	if err != nil {
		return nil, err
	}

	from, err := ca.api.resolveToKeyAddress(ctx, f, nil)
	if err != nil {
		return nil, err
	}
	// verify voucher signature
	vb, err := sv.SigningBytes()
	if err != nil {
		return nil, err
	}

	// TODO: technically, either party may create and sign a voucher.
	// However, for now, we only accept them from the channel creator.
	// More complex handling logic can be added later
	if err := crypto.Verify(sv.Signature, from, vb); err != nil {
		return nil, err
	}

	// Check the voucher against the highest known voucher nonce / value
	laneStates, err := ca.laneState(ctx, pchState, ch)
	if err != nil {
		return nil, err
	}

	// If the new voucher nonce value is less than the highest known
	// nonce for the lane
	ls, lsExists := laneStates[sv.Lane]
	if lsExists {
		n, err := ls.Nonce()
		if err != nil {
			return nil, err
		}

		if sv.Nonce <= n {
			return nil, fmt.Errorf("nonce too low")
		}

		// If the voucher amount is less than the highest known voucher amount
		r, err := ls.Redeemed()
		if err != nil {
			return nil, err
		}
		if sv.Amount.LessThanEqual(r) {
			return nil, fmt.Errorf("voucher amount is lower than amount for voucher with lower nonce")
		}
	}

	// Total redeemed is the total redeemed amount for all lanes, including
	// the new voucher
	// eg
	//
	// lane 1 redeemed:            3
	// lane 2 redeemed:            2
	// voucher for lane 1:         5
	//
	// Voucher supersedes lane 1 redeemed, therefore
	// effective lane 1 redeemed:  5
	//
	// lane 1:  5
	// lane 2:  2
	//          -
	// total:   7
	totalRedeemed, err := ca.totalRedeemedWithVoucher(laneStates, sv)
	if err != nil {
		return nil, err
	}

	// Total required balance must not exceed actor balance
	if act.Balance.LessThan(totalRedeemed) {
		return nil, newErrInsufficientFunds(big.Sub(totalRedeemed, act.Balance))
	}

	if len(sv.Merges) != 0 {
		return nil, fmt.Errorf("dont currently support paych lane merges")
	}

	return laneStates, nil
}

func (ca *channelAccessor) checkVoucherSpendable(ctx context.Context, ch address.Address, sv *types2.SignedVoucher, secret []byte) (bool, error) {
	ca.lk.Lock()
	defer ca.lk.Unlock()

	recipient, err := ca.getPaychRecipient(ctx, ch)
	if err != nil {
		return false, err
	}

	ci, err := ca.channelInfoRepo.GetChannelByAddress(ctx, ch)
	if err != nil {
		return false, err
	}

	// Check if voucher has already been submitted
	submitted, err := ci.WasVoucherSubmitted(sv)
	if err != nil {
		return false, err
	}
	if submitted {
		return false, nil
	}

	mb, err := ca.messageBuilder(ctx, recipient)
	if err != nil {
		return false, err
	}

	mes, err := mb.Update(ch, sv, secret)
	if err != nil {
		return false, err
	}

	ret, err := ca.api.call(ctx, mes, nil)
	if err != nil {
		return false, err
	}

	if ret.MsgRct.ExitCode != 0 {
		return false, nil
	}

	return true, nil
}

func (ca *channelAccessor) getPaychRecipient(ctx context.Context, ch address.Address) (address.Address, error) {
	_, state, err := ca.api.getPaychState(ctx, ch, nil)
	if err != nil {
		return address.Address{}, err
	}

	return state.To()
}

func (ca *channelAccessor) addVoucher(ctx context.Context, ch address.Address, sv *types2.SignedVoucher, minDelta big.Int) (big.Int, error) {
	ca.lk.Lock()
	defer ca.lk.Unlock()

	return ca.addVoucherUnlocked(ctx, ch, sv, minDelta)
}

func (ca *channelAccessor) addVoucherUnlocked(ctx context.Context, ch address.Address, sv *types2.SignedVoucher, minDelta big.Int) (big.Int, error) {
	ci, err := ca.channelInfoRepo.GetChannelByAddress(ctx, ch)
	if err != nil {
		return big.Int{}, err
	}

	// Check if the voucher has already been added
	for _, v := range ci.Vouchers {
		eq, err := cborutil.Equals(sv, v.Voucher)
		if err != nil {
			return big.Int{}, err
		}
		if eq {
			// Ignore the duplicate voucher.
			log.Warnf("AddVoucher: voucher re-added")
			return big.NewInt(0), nil
		}

	}

	// Check voucher validity
	laneStates, err := ca.checkVoucherValidUnlocked(ctx, ch, sv)
	if err != nil {
		return big.NewInt(0), err
	}

	// The change in value is the delta between the voucher amount and
	// the highest previous voucher amount for the lane
	laneState, exists := laneStates[sv.Lane]
	redeemed := big.NewInt(0)
	if exists {
		redeemed, err = laneState.Redeemed()
		if err != nil {
			return big.NewInt(0), err
		}
	}

	delta := big.Sub(sv.Amount, redeemed)
	if minDelta.GreaterThan(delta) {
		return delta, fmt.Errorf("addVoucher: supplied token amount too low; minD=%s, D=%s; laneAmt=%s; v.Amt=%s", minDelta, delta, redeemed, sv.Amount)
	}

	ci.Vouchers = append(ci.Vouchers, &types.VoucherInfo{
		Voucher: sv,
	})

	if ci.NextLane <= sv.Lane {
		ci.NextLane = sv.Lane + 1
	}

	return delta, ca.channelInfoRepo.SaveChannel(ctx, ci)
}

func (ca *channelAccessor) submitVoucher(ctx context.Context, ch address.Address, sv *types2.SignedVoucher, secret []byte) (cid.Cid, error) {
	ca.lk.Lock()
	defer ca.lk.Unlock()

	ci, err := ca.channelInfoRepo.GetChannelByAddress(ctx, ch)
	if err != nil {
		return cid.Undef, err
	}

	has, err := ci.HasVoucher(sv)
	if err != nil {
		return cid.Undef, err
	}

	// If the channel has the voucher
	if has {
		// Check that the voucher hasn't already been submitted
		submitted, err := ci.WasVoucherSubmitted(sv)
		if err != nil {
			return cid.Undef, err
		}
		if submitted {
			return cid.Undef, fmt.Errorf("cannot submit voucher that has already been submitted")
		}
	}

	mb, err := ca.messageBuilder(ctx, ci.Control)
	if err != nil {
		return cid.Undef, err
	}

	msg, err := mb.Update(ch, sv, secret)
	if err != nil {
		return cid.Undef, err
	}

	msgId, err := ca.api.PushMessage(ctx, msg, nil)
	if err != nil {
		return cid.Undef, err
	}

	// If the channel didn't already have the voucher
	if !has {
		// Add the voucher to the channel
		ci.Vouchers = append(ci.Vouchers, &types.VoucherInfo{
			Voucher: sv,
		})
	}

	// Mark the voucher and any lower-nonce vouchers as having been submitted
	err = ci.MarkVoucherSubmitted(sv)
	if err != nil {
		return cid.Undef, err
	}
	err = ca.channelInfoRepo.SaveChannel(ctx, ci)
	if err != nil {
		return cid.Undef, err
	}

	return msgId, nil
}

func (ca *channelAccessor) allocateLane(ctx context.Context, ch address.Address) (uint64, error) {
	ca.lk.Lock()
	defer ca.lk.Unlock()

	ci, err := ca.channelInfoRepo.GetChannelByAddress(ctx, ch)
	if err != nil {
		return 0, err
	}

	out := ci.NextLane
	ci.NextLane++

	return out, ca.channelInfoRepo.SaveChannel(ctx, ci)
}

// vouchersForPaych gets the vouchers for the given channel
func (ca *channelAccessor) vouchersForPaych(ctx context.Context, ch address.Address) ([]*types.VoucherInfo, error) {
	ci, err := ca.channelInfoRepo.GetChannelByAddress(ctx, ch)
	if err != nil {
		return nil, err
	}

	return ci.Vouchers, nil
}

func (ca *channelAccessor) listVouchers(ctx context.Context, ch address.Address) ([]*types.VoucherInfo, error) {
	ca.lk.Lock()
	defer ca.lk.Unlock()

	// TODO: just having a passthrough method like this feels odd. Seems like
	// there should be some filtering we're doing here
	return ca.vouchersForPaych(ctx, ch)
}

// laneState gets the LaneStates from chain, then applies all vouchers in
// the data store over the chain state
func (ca *channelAccessor) laneState(ctx context.Context, state lpaych.State, ch address.Address) (map[uint64]lpaych.LaneState, error) {
	// TODO: we probably want to call UpdateChannelState with all vouchers to be fully correct
	//  (but technically dont't need to)

	laneCount, err := state.LaneCount()
	if err != nil {
		return nil, err
	}

	// Note: we use a map instead of an array to store laneStates because the
	// api sets the lane ID (the index) and potentially they could use a
	// very large index.
	laneStates := make(map[uint64]lpaych.LaneState, laneCount)
	err = state.ForEachLaneState(func(idx uint64, ls lpaych.LaneState) error {
		laneStates[idx] = ls
		return nil
	})
	if err != nil {
		return nil, err
	}

	// Apply locally stored vouchers
	vouchers, err := ca.vouchersForPaych(ctx, ch)
	if err != nil && !errors.Is(err, repo.ErrNotFound) {
		return nil, err
	}

	for _, v := range vouchers {
		for range v.Voucher.Merges {
			return nil, fmt.Errorf("paych merges not handled yet")
		}

		// Check if there is an existing laneState in the payment channel
		// for this voucher's lane
		ls, ok := laneStates[v.Voucher.Lane]

		// If the voucher does not have a higher nonce than the existing
		// laneState for this lane, ignore it
		if ok {
			n, err := ls.Nonce()
			if err != nil {
				return nil, err
			}
			if v.Voucher.Nonce < n {
				continue
			}
		}

		// Voucher has a higher nonce, so replace laneState with this voucher
		laneStates[v.Voucher.Lane] = laneState{v.Voucher.Amount, v.Voucher.Nonce}
	}

	return laneStates, nil
}

// Get the total redeemed amount across all lanes, after applying the voucher
func (ca *channelAccessor) totalRedeemedWithVoucher(laneStates map[uint64]lpaych.LaneState, sv *types2.SignedVoucher) (big.Int, error) {
	// TODO: merges
	if len(sv.Merges) != 0 {
		return big.Int{}, fmt.Errorf("dont currently support paych lane merges")
	}

	total := big.NewInt(0)
	for _, ls := range laneStates {
		r, err := ls.Redeemed()
		if err != nil {
			return big.Int{}, err
		}
		total = big.Add(total, r)
	}

	lane, ok := laneStates[sv.Lane]
	if ok {
		// If the voucher is for an existing lane, and the voucher nonce
		// is higher than the lane nonce
		n, err := lane.Nonce()
		if err != nil {
			return big.Int{}, err
		}

		if sv.Nonce > n {
			// Add the delta between the redeemed amount and the voucher
			// amount to the total
			r, err := lane.Redeemed()
			if err != nil {
				return big.Int{}, err
			}

			delta := big.Sub(sv.Amount, r)
			total = big.Add(total, delta)
		}
	} else {
		// If the voucher is *not* for an existing lane, just add its
		// value (implicitly a new lane will be created for the voucher)
		total = big.Add(total, sv.Amount)
	}

	return total, nil
}

func (ca *channelAccessor) settle(ctx context.Context, ch address.Address) (cid.Cid, error) {
	ca.lk.Lock()
	defer ca.lk.Unlock()

	ci, err := ca.channelInfoRepo.GetChannelByAddress(ctx, ch)
	if err != nil {
		return cid.Undef, err
	}

	mb, err := ca.messageBuilder(ctx, ci.Control)
	if err != nil {
		return cid.Undef, err
	}
	msg, err := mb.Settle(ch)
	if err != nil {
		return cid.Undef, err
	}

	msgId, err := ca.api.PushMessage(ctx, msg, nil)
	if err != nil {
		return cid.Undef, err
	}

	ci.Settling = true
	err = ca.channelInfoRepo.SaveChannel(ctx, ci)
	if err != nil {
		log.Errorf("Error marking channel as settled: %s", err)
	}
	return msgId, nil
}

func (ca *channelAccessor) collect(ctx context.Context, ch address.Address) (cid.Cid, error) {
	ca.lk.Lock()
	defer ca.lk.Unlock()

	ci, err := ca.channelInfoRepo.GetChannelByAddress(ctx, ch)
	if err != nil {
		return cid.Undef, err
	}

	mb, err := ca.messageBuilder(ctx, ci.Control)
	if err != nil {
		return cid.Undef, err
	}

	msg, err := mb.Collect(ch)
	if err != nil {
		return cid.Undef, err
	}

	msgId, err := ca.api.PushMessage(ctx, msg, nil)
	if err != nil {
		return cid.Undef, err
	}
	return msgId, nil
}
