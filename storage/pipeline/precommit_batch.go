package sealing

import (
	"bytes"
	"context"
	"sort"
	"sync"
	"time"

	"github.com/ipfs/go-cid"
	"golang.org/x/xerrors"

	"github.com/filecoin-project/go-address"
	"github.com/filecoin-project/go-state-types/abi"
	"github.com/filecoin-project/go-state-types/big"
	"github.com/filecoin-project/go-state-types/builtin"
	"github.com/filecoin-project/go-state-types/builtin/v9/miner"
	verifregtypes "github.com/filecoin-project/go-state-types/builtin/v9/verifreg"
	"github.com/filecoin-project/go-state-types/network"

	"github.com/filecoin-project/lotus/api"
	"github.com/filecoin-project/lotus/build"
	"github.com/filecoin-project/lotus/chain/actors/policy"
	"github.com/filecoin-project/lotus/chain/types"
	"github.com/filecoin-project/lotus/node/config"
	"github.com/filecoin-project/lotus/node/modules/dtypes"
	"github.com/filecoin-project/lotus/storage/pipeline/sealiface"
)

//go:generate go run github.com/golang/mock/mockgen -destination=mocks/mock_precommit_batcher.go -package=mocks . PreCommitBatcherApi

type PreCommitBatcherApi interface {
	MpoolPushMessage(context.Context, *types.Message, *api.MessageSendSpec) (*types.SignedMessage, error)
	GasEstimateMessageGas(context.Context, *types.Message, *api.MessageSendSpec, types.TipSetKey) (*types.Message, error)
	StateMinerInfo(context.Context, address.Address, types.TipSetKey) (api.MinerInfo, error)
	StateMinerAvailableBalance(context.Context, address.Address, types.TipSetKey) (big.Int, error)
	ChainHead(ctx context.Context) (*types.TipSet, error)
	StateNetworkVersion(ctx context.Context, tsk types.TipSetKey) (network.Version, error)
	StateGetAllocationForPendingDeal(ctx context.Context, dealId abi.DealID, tsk types.TipSetKey) (*verifregtypes.Allocation, error)

	// Address selector
	WalletBalance(context.Context, address.Address) (types.BigInt, error)
	WalletHas(context.Context, address.Address) (bool, error)
	StateAccountKey(context.Context, address.Address, types.TipSetKey) (address.Address, error)
	StateLookupID(context.Context, address.Address, types.TipSetKey) (address.Address, error)
}

type preCommitEntry struct {
	deposit abi.TokenAmount
	pci     *miner.SectorPreCommitInfo
}

type PreCommitBatcher struct {
	api       PreCommitBatcherApi
	maddr     address.Address
	mctx      context.Context
	addrSel   AddressSelector
	feeCfg    config.MinerFeeConfig
	getConfig dtypes.GetSealingConfigFunc

	cutoffs map[abi.SectorNumber]time.Time
	todo    map[abi.SectorNumber]*preCommitEntry
	waiting map[abi.SectorNumber][]chan sealiface.PreCommitBatchRes

	notify, stop, stopped chan struct{}
	force                 chan chan []sealiface.PreCommitBatchRes
	lk                    sync.Mutex
}

func NewPreCommitBatcher(mctx context.Context, maddr address.Address, api PreCommitBatcherApi, addrSel AddressSelector, feeCfg config.MinerFeeConfig, getConfig dtypes.GetSealingConfigFunc) *PreCommitBatcher {
	b := &PreCommitBatcher{
		api:       api,
		maddr:     maddr,
		mctx:      mctx,
		addrSel:   addrSel,
		feeCfg:    feeCfg,
		getConfig: getConfig,

		cutoffs: map[abi.SectorNumber]time.Time{},
		todo:    map[abi.SectorNumber]*preCommitEntry{},
		waiting: map[abi.SectorNumber][]chan sealiface.PreCommitBatchRes{},

		notify:  make(chan struct{}, 1),
		force:   make(chan chan []sealiface.PreCommitBatchRes),
		stop:    make(chan struct{}),
		stopped: make(chan struct{}),
	}

	go b.run()

	return b
}

func (b *PreCommitBatcher) run() {
	var forceRes chan []sealiface.PreCommitBatchRes
	var lastRes []sealiface.PreCommitBatchRes

	cfg, err := b.getConfig()
	if err != nil {
		panic(err)
	}

	timer := time.NewTimer(b.batchWait(cfg.PreCommitBatchWait, cfg.PreCommitBatchSlack))
	for {
		if forceRes != nil {
			forceRes <- lastRes
			forceRes = nil
		}
		lastRes = nil

		var sendAboveMax bool
		select {
		case <-b.stop:
			close(b.stopped)
			return
		case <-b.notify:
			sendAboveMax = true
		case <-timer.C:
			// do nothing
		case fr := <-b.force: // user triggered
			forceRes = fr
		}

		var err error
		lastRes, err = b.maybeStartBatch(sendAboveMax)
		if err != nil {
			log.Warnw("PreCommitBatcher processBatch error", "error", err)
		}

		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}

		timer.Reset(b.batchWait(cfg.PreCommitBatchWait, cfg.PreCommitBatchSlack))
	}
}

func (b *PreCommitBatcher) batchWait(maxWait, slack time.Duration) time.Duration {
	now := time.Now()

	b.lk.Lock()
	defer b.lk.Unlock()

	if len(b.todo) == 0 {
		return maxWait
	}

	var cutoff time.Time
	for sn := range b.todo {
		sectorCutoff := b.cutoffs[sn]
		if cutoff.IsZero() || (!sectorCutoff.IsZero() && sectorCutoff.Before(cutoff)) {
			cutoff = sectorCutoff
		}
	}
	for sn := range b.waiting {
		sectorCutoff := b.cutoffs[sn]
		if cutoff.IsZero() || (!sectorCutoff.IsZero() && sectorCutoff.Before(cutoff)) {
			cutoff = sectorCutoff
		}
	}

	if cutoff.IsZero() {
		return maxWait
	}

	cutoff = cutoff.Add(-slack)
	if cutoff.Before(now) {
		return time.Nanosecond // can't return 0
	}

	wait := cutoff.Sub(now)
	if wait > maxWait {
		wait = maxWait
	}

	return wait
}

func (b *PreCommitBatcher) maybeStartBatch(notif bool) ([]sealiface.PreCommitBatchRes, error) {
	b.lk.Lock()
	defer b.lk.Unlock()

	total := len(b.todo)
	if total == 0 {
		return nil, nil // nothing to do
	}

	cfg, err := b.getConfig()
	if err != nil {
		return nil, xerrors.Errorf("getting config: %w", err)
	}

	if notif && total < cfg.MaxPreCommitBatch {
		return nil, nil
	}

	ts, err := b.api.ChainHead(b.mctx)
	if err != nil {
		return nil, err
	}

	// TODO: Drop this once nv14 has come and gone
	nv, err := b.api.StateNetworkVersion(b.mctx, ts.Key())
	if err != nil {
		return nil, xerrors.Errorf("couldn't get network version: %w", err)
	}

	individual := false
	if !cfg.BatchPreCommitAboveBaseFee.Equals(big.Zero()) && ts.MinTicketBlock().ParentBaseFee.LessThan(cfg.BatchPreCommitAboveBaseFee) && nv >= network.Version14 {
		individual = true
	}

	// todo support multiple batches
	var res []sealiface.PreCommitBatchRes
	if !individual {
		res, err = b.processBatch(cfg, ts.Key(), ts.MinTicketBlock().ParentBaseFee, nv)
	} else {
		res, err = b.processIndividually(cfg)
	}
	if err != nil && len(res) == 0 {
		return nil, err
	}

	for _, r := range res {
		if err != nil {
			r.Error = err.Error()
		}

		for _, sn := range r.Sectors {
			for _, ch := range b.waiting[sn] {
				ch <- r // buffered
			}

			delete(b.waiting, sn)
			delete(b.todo, sn)
			delete(b.cutoffs, sn)
		}
	}

	return res, nil
}

func (b *PreCommitBatcher) processIndividually(cfg sealiface.Config) ([]sealiface.PreCommitBatchRes, error) {
	mi, err := b.api.StateMinerInfo(b.mctx, b.maddr, types.EmptyTSK)
	if err != nil {
		return nil, xerrors.Errorf("couldn't get miner info: %w", err)
	}

	avail := types.TotalFilecoinInt

	if cfg.CollateralFromMinerBalance && !cfg.DisableCollateralFallback {
		avail, err = b.api.StateMinerAvailableBalance(b.mctx, b.maddr, types.EmptyTSK)
		if err != nil {
			return nil, xerrors.Errorf("getting available miner balance: %w", err)
		}

		avail = big.Sub(avail, cfg.AvailableBalanceBuffer)
		if avail.LessThan(big.Zero()) {
			avail = big.Zero()
		}
	}

	var res []sealiface.PreCommitBatchRes

	for sn, info := range b.todo {
		r := sealiface.PreCommitBatchRes{
			Sectors: []abi.SectorNumber{sn},
		}

		mcid, err := b.processSingle(cfg, mi, &avail, info)
		if err != nil {
			r.Error = err.Error()
		} else {
			r.Msg = &mcid
		}

		res = append(res, r)
	}

	return res, nil
}

func (b *PreCommitBatcher) processSingle(cfg sealiface.Config, mi api.MinerInfo, avail *abi.TokenAmount, entry *preCommitEntry) (cid.Cid, error) {
	msgParams := infoToPreCommitSectorParams(entry.pci)
	enc := new(bytes.Buffer)

	if err := msgParams.MarshalCBOR(enc); err != nil {
		return cid.Undef, xerrors.Errorf("marshaling precommit params: %w", err)
	}

	deposit := entry.deposit
	if cfg.CollateralFromMinerBalance {
		c := big.Sub(deposit, *avail)
		*avail = big.Sub(*avail, deposit)
		deposit = c

		if deposit.LessThan(big.Zero()) {
			deposit = big.Zero()
		}
		if (*avail).LessThan(big.Zero()) {
			*avail = big.Zero()
		}
	}

	goodFunds := big.Add(deposit, big.Int(b.feeCfg.MaxPreCommitGasFee))

	from, _, err := b.addrSel.AddressFor(b.mctx, b.api, mi, api.PreCommitAddr, goodFunds, deposit)
	if err != nil {
		return cid.Undef, xerrors.Errorf("no good address to send precommit message from: %w", err)
	}

	mcid, err := sendMsg(b.mctx, b.api, from, b.maddr, builtin.MethodsMiner.PreCommitSector, deposit, big.Int(b.feeCfg.MaxPreCommitGasFee), enc.Bytes())
	if err != nil {
		return cid.Undef, xerrors.Errorf("pushing message to mpool: %w", err)
	}

	return mcid, nil
}

func (b *PreCommitBatcher) processPreCommitBatch(cfg sealiface.Config, bf abi.TokenAmount, entries []*preCommitEntry, nv network.Version) ([]sealiface.PreCommitBatchRes, error) {
	log.Errorf("!!!!!!!!!!!!!!!!processPreCommitBatch: %d entries!!!!!!!!!!!!!!!!", len(entries))
	params := miner.PreCommitSectorBatchParams{}
	deposit := big.Zero()
	var res sealiface.PreCommitBatchRes

	for _, p := range entries {
		res.Sectors = append(res.Sectors, p.pci.SectorNumber)
		params.Sectors = append(params.Sectors, *infoToPreCommitSectorParams(p.pci))
		deposit = big.Add(deposit, p.deposit)
	}

	enc := new(bytes.Buffer)
	if err := params.MarshalCBOR(enc); err != nil {
		return []sealiface.PreCommitBatchRes{res}, xerrors.Errorf("couldn't serialize PreCommitSectorBatchParams: %w", err)
	}

	mi, err := b.api.StateMinerInfo(b.mctx, b.maddr, types.EmptyTSK)
	if err != nil {
		return []sealiface.PreCommitBatchRes{res}, xerrors.Errorf("couldn't get miner info: %w", err)
	}

	maxFee := b.feeCfg.MaxPreCommitBatchGasFee.FeeForSectors(len(params.Sectors))

	aggFeeRaw, err := policy.AggregatePreCommitNetworkFee(nv, len(params.Sectors), bf)
	if err != nil {
		log.Errorf("getting aggregate precommit network fee: %s", err)
		return []sealiface.PreCommitBatchRes{res}, xerrors.Errorf("getting aggregate precommit network fee: %s", err)
	}

	aggFee := big.Div(big.Mul(aggFeeRaw, aggFeeNum), aggFeeDen)

	needFunds := big.Add(deposit, aggFee)
	needFunds, err = collateralSendAmount(b.mctx, b.api, b.maddr, cfg, needFunds)
	if err != nil {
		return []sealiface.PreCommitBatchRes{res}, err
	}

	goodFunds := big.Add(maxFee, needFunds)

	from, _, err := b.addrSel.AddressFor(b.mctx, b.api, mi, api.PreCommitAddr, goodFunds, deposit)
	if err != nil {
		return []sealiface.PreCommitBatchRes{res}, xerrors.Errorf("no good address found: %w", err)
	}

	msg, err := simulateMsgGas(b.mctx, b.api, from, b.maddr, builtin.MethodsMiner.PreCommitSectorBatch, deposit, maxFee, enc.Bytes())

	var gasLimit int64
	if msg != nil {
		gasLimit = msg.GasLimit
	}
	log.Errorf("!!!!!!!!!!!!!!simulate Msg err: %w, gasLimit: %d !!!!!!!!!!!!!!!!!!!!", err, gasLimit)
	if err != nil && (!api.ErrorIsIn(err, []error{&api.ErrOutOfGas{}}) || len(entries) == 1) {
		res.Error = err.Error()
		return []sealiface.PreCommitBatchRes{res}, xerrors.Errorf("simulating PreCommitBatch message failed: %w", err)
	}

	// If we're out of gas, split the batch in half and try again
	if api.ErrorIsIn(err, []error{&api.ErrOutOfGas{}}) {
		mid := len(entries) / 2
		ret0, err := b.processPreCommitBatch(cfg, bf, entries[:mid], nv)
		ret1, err := b.processPreCommitBatch(cfg, bf, entries[mid:], nv)

		return append(ret0, ret1...), err
	}

	// If state call succeeds, we can send the message for real
	mcid, err := sendMsg(b.mctx, b.api, from, b.maddr, builtin.MethodsMiner.PreCommitSectorBatch, deposit, maxFee, enc.Bytes())
	log.Errorf("!!!!!!!!!!!!!!sendMsg err: %w, mcid: %v!!!!!!!!!!!!!!!!!!!!", err, mcid)
	if err != nil {
		res.Error = err.Error()
		return []sealiface.PreCommitBatchRes{res}, xerrors.Errorf("pushing message to mpool: %w", err)
	}
	res.Msg = &mcid
	return []sealiface.PreCommitBatchRes{res}, nil
}

func (b *PreCommitBatcher) processBatch(cfg sealiface.Config, tsk types.TipSetKey, bf abi.TokenAmount, nv network.Version) ([]sealiface.PreCommitBatchRes, error) {
	var pcEntries []*preCommitEntry
	for _, p := range b.todo {
		pcEntries = append(pcEntries, p)
	}

	return b.processPreCommitBatch(cfg, bf, pcEntries, nv)
}

// register PreCommit, wait for batch message, return message CID
func (b *PreCommitBatcher) AddPreCommit(ctx context.Context, s SectorInfo, deposit abi.TokenAmount, in *miner.SectorPreCommitInfo) (res sealiface.PreCommitBatchRes, err error) {
	ts, err := b.api.ChainHead(b.mctx)
	if err != nil {
		log.Errorf("getting chain head: %s", err)
		return sealiface.PreCommitBatchRes{}, err
	}

	dealStartCutoff := getDealStartCutoff(s)
	if dealStartCutoff <= ts.Height() {
		return sealiface.PreCommitBatchRes{}, xerrors.Errorf("cutoff has already passed (cutoff %d <= curEpoch %d)", dealStartCutoff, ts.Height())
	}

	// Allocation cutoff is a soft deadline, so don't fail if we've passed it.
	allocationCutoff := b.getAllocationCutoff(s)

	var cutoffEpoch abi.ChainEpoch
	if dealStartCutoff < allocationCutoff {
		cutoffEpoch = dealStartCutoff
	} else {
		cutoffEpoch = allocationCutoff
	}

	sn := s.SectorNumber

	b.lk.Lock()
	b.cutoffs[sn] = time.Now().Add(time.Duration(cutoffEpoch-ts.Height()) * time.Duration(build.BlockDelaySecs) * time.Second)
	b.todo[sn] = &preCommitEntry{
		deposit: deposit,
		pci:     in,
	}

	sent := make(chan sealiface.PreCommitBatchRes, 1)
	b.waiting[sn] = append(b.waiting[sn], sent)

	select {
	case b.notify <- struct{}{}:
	default: // already have a pending notification, don't need more
	}
	b.lk.Unlock()

	select {
	case c := <-sent:
		return c, nil
	case <-ctx.Done():
		return sealiface.PreCommitBatchRes{}, ctx.Err()
	}
}

func (b *PreCommitBatcher) Flush(ctx context.Context) ([]sealiface.PreCommitBatchRes, error) {
	resCh := make(chan []sealiface.PreCommitBatchRes, 1)
	select {
	case b.force <- resCh:
		select {
		case res := <-resCh:
			return res, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (b *PreCommitBatcher) Pending(ctx context.Context) ([]abi.SectorID, error) {
	b.lk.Lock()
	defer b.lk.Unlock()

	mid, err := address.IDFromAddress(b.maddr)
	if err != nil {
		return nil, err
	}

	res := make([]abi.SectorID, 0)
	for _, s := range b.todo {
		res = append(res, abi.SectorID{
			Miner:  abi.ActorID(mid),
			Number: s.pci.SectorNumber,
		})
	}

	sort.Slice(res, func(i, j int) bool {
		if res[i].Miner != res[j].Miner {
			return res[i].Miner < res[j].Miner
		}

		return res[i].Number < res[j].Number
	})

	return res, nil
}

func (b *PreCommitBatcher) Stop(ctx context.Context) error {
	close(b.stop)

	select {
	case <-b.stopped:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func getDealStartCutoff(si SectorInfo) abi.ChainEpoch {
	cutoffEpoch := si.TicketEpoch + policy.MaxPreCommitRandomnessLookback
	for _, p := range si.Pieces {
		if p.DealInfo == nil {
			continue
		}

		startEpoch := p.DealInfo.DealSchedule.StartEpoch
		if startEpoch < cutoffEpoch {
			cutoffEpoch = startEpoch
		}
	}

	return cutoffEpoch
}

func (b *PreCommitBatcher) getAllocationCutoff(si SectorInfo) abi.ChainEpoch {
	cutoff := si.TicketEpoch + policy.MaxPreCommitRandomnessLookback
	for _, p := range si.Pieces {
		if p.DealInfo == nil {
			continue
		}

		alloc, _ := b.api.StateGetAllocationForPendingDeal(b.mctx, p.DealInfo.DealID, types.EmptyTSK)
		// alloc is nil if this is not a verified deal in nv17 or later
		if alloc == nil {
			continue
		}
		if alloc.Expiration < cutoff {
			cutoff = alloc.Expiration
		}
	}
	return cutoff
}
