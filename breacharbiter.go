package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"

	"github.com/boltdb/bolt"
	"github.com/davecgh/go-spew/spew"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/htlcswitch"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/roasbeef/btcd/btcec"
	"github.com/roasbeef/btcd/chaincfg/chainhash"
	"github.com/roasbeef/btcd/txscript"
	"github.com/roasbeef/btcd/wire"
	"github.com/roasbeef/btcutil"
)

// retributionBucket stores retribution state on disk between detecting a
// contract breach, broadcasting a justice transaction that sweeps the channel,
// and finally witnessing the justice transaction confirm on the blockchain. It
// is critical that such state is persisted on disk, so that if our node
// restarts at any point during the retribution procedure, we can recover and
// continue from the persisted state.
var retributionBucket = []byte("retribution")

// breachArbiter is a special subsystem which is responsible for watching and
// acting on the detection of any attempted uncooperative channel breaches by
// channel counterparties. This file essentially acts as deterrence code for
// those attempting to launch attacks against the daemon. In practice it's
// expected that the logic in this file never gets executed, but it is
// important to have it in place just in case we encounter cheating channel
// counterparties.
// TODO(roasbeef): closures in config for subsystem pointers to decouple?
type breachArbiter struct {
	wallet     *lnwallet.LightningWallet
	db         *channeldb.DB
	notifier   chainntnfs.ChainNotifier
	chainIO    lnwallet.BlockChainIO
	estimator  lnwallet.FeeEstimator
	htlcSwitch *htlcswitch.Switch

	retributionStore RetributionStore

	// breachObservers is a map which tracks all the active breach
	// observers we're currently managing. The key of the map is the
	// funding outpoint of the channel, and the value is a channel which
	// will be closed once we detect that the channel has been
	// cooperatively closed, thereby killing the goroutine and freeing up
	// resources.
	breachObservers map[wire.OutPoint]chan struct{}

	// breachedContracts is a channel which is used internally within the
	// struct to send the necessary information required to punish a
	// counterparty once a channel breach is detected. Breach observers
	// use this to communicate with the main contractObserver goroutine.
	breachedContracts chan *retributionInfo

	// newContracts is a channel which is used by outside subsystems to
	// notify the breachArbiter of a new contract (a channel) that should
	// be watched.
	newContracts chan *lnwallet.LightningChannel

	// settledContracts is a channel by outside subsystems to notify
	// the breachArbiter that a channel has peacefully been closed. Once a
	// channel has been closed the arbiter no longer needs to watch for
	// breach closes.
	settledContracts chan *wire.OutPoint

	started uint32
	stopped uint32
	quit    chan struct{}
	wg      sync.WaitGroup
}

// newBreachArbiter creates a new instance of a breachArbiter initialized with
// its dependent objects.
func newBreachArbiter(wallet *lnwallet.LightningWallet, db *channeldb.DB,
	notifier chainntnfs.ChainNotifier, h *htlcswitch.Switch,
	chain lnwallet.BlockChainIO, fe lnwallet.FeeEstimator) *breachArbiter {

	return &breachArbiter{
		wallet:     wallet,
		db:         db,
		notifier:   notifier,
		chainIO:    chain,
		htlcSwitch: h,
		estimator:  fe,

		retributionStore: newRetributionStore(db),

		breachObservers:   make(map[wire.OutPoint]chan struct{}),
		breachedContracts: make(chan *retributionInfo),
		newContracts:      make(chan *lnwallet.LightningChannel),
		settledContracts:  make(chan *wire.OutPoint),
		quit:              make(chan struct{}),
	}
}

// Start is an idempotent method that officially starts the breachArbiter along
// with all other goroutines it needs to perform its functions.
func (b *breachArbiter) Start() error {
	if !atomic.CompareAndSwapUint32(&b.started, 0, 1) {
		return nil
	}

	brarLog.Tracef("Starting breach arbiter")

	// We load all pending retributions from the database and
	// deterministically reconstruct a channel close summary for each. In
	// the event that a channel is still open after being breached, we can
	// use the close summary to reinitiate a channel close so that the
	// breach is reflected in channeldb.
	breachRetInfos := make(map[wire.OutPoint]retributionInfo)
	closeSummaries := make(map[wire.OutPoint]channeldb.ChannelCloseSummary)
	err := b.retributionStore.ForAll(func(ret *retributionInfo) error {
		// Extract emitted retribution information.
		breachRetInfos[ret.chanPoint] = *ret

		// Deterministically reconstruct channel close summary from
		// persisted retribution information and record in breach close
		// summaries map under the corresponding channel point.
		closeSummary := channeldb.ChannelCloseSummary{
			ChanPoint:      ret.chanPoint,
			ClosingTXID:    ret.commitHash,
			RemotePub:      &ret.remoteIdentity,
			Capacity:       ret.capacity,
			SettledBalance: ret.settledBalance,
			CloseType:      channeldb.BreachClose,
			IsPending:      true,
		}
		closeSummaries[ret.chanPoint] = closeSummary

		return nil
	})
	if err != nil {
		return err
	}

	// We need to query that database state for all currently active
	// channels, each of these channels will need a goroutine assigned to
	// it to watch for channel breaches.
	activeChannels, err := b.db.FetchAllChannels()
	if err != nil && err != channeldb.ErrNoActiveChannels {
		brarLog.Errorf("unable to fetch active channels: %v", err)
		return err
	}

	nActive := len(activeChannels)
	if nActive > 0 {
		brarLog.Infof("Retrieved %v channels from database, watching "+
			"with vigilance!", nActive)
	}

	// Here we will determine a set of channels that will need to be managed
	// by the contractObserver.  For each of the open channels read from
	// disk, we will create a channel state machine that can be used to
	// watch for any potential channel closures.  We must first exclude any
	// channel whose retribution process has been initiated, and proceed to
	// mark them as closed.  The state machines generated for these filtered
	// channels can be discarded, as their fate will be placed in the hands
	// of an exactRetribution task spawned later.
	//
	// NOTE Spawning of the exactRetribution task is intentionally postponed
	// until after this step in order to ensure that the all breached
	// channels are reflected as closed in channeldb and consistent with
	// what is checkpointed by the breach arbiter. Instead of treating the
	// breached-and-closed and breached-but-still-active channels as
	// separate sets of channels, we first ensure that all
	// breached-but-still-active channels are promoted to
	// breached-and-closed during restart, allowing us to treat them as a
	// single set from here on out. This approach also has the added benefit
	// of minimizing the likelihood that the wrong number of tasks are
	// spawned per breached channel, and prevents us from being in a
	// position where retribution has completed but the channel is still
	// marked as open in channeldb.
	channelsToWatch := make([]*lnwallet.LightningChannel, 0, nActive)
	for _, chanState := range activeChannels {
		// Initialize active channel from persisted channel state.
		channel, err := lnwallet.NewLightningChannel(nil, b.notifier,
			b.estimator, chanState)
		if err != nil {
			brarLog.Errorf("unable to load channel from "+
				"disk: %v", err)
			return err
		}

		// Before marking this as an active channel that the breach
		// arbiter should watch, check to see if this channel was
		// previously breached. If so, we attempt to reflect this in the
		// channeldb by closing the channel. Upon success, we continue
		// because the channel is no longer open, and thus does not need
		// to be managed by the contractObserver.
		chanPoint := chanState.FundingOutpoint
		if closeSummary, ok := closeSummaries[chanPoint]; ok {
			// Since this channel should not be open, we immediately
			// notify the HTLC switch that this link should be
			// closed, and that all activity on the link should
			// cease.
			b.htlcSwitch.CloseLink(
				&chanState.FundingOutpoint,
				htlcswitch.CloseBreach,
			)

			// Ensure channeldb is consistent with the persisted
			// breach.
			err := channel.DeleteState(&closeSummary)
			if err != nil {
				brarLog.Errorf("unable to delete channel "+
					"state: %v", err)
				return err
			}

			// Now that this channel is both breached _and_ closed,
			// we can skip adding it to the `channelsToWatch` since
			// we can begin the retribution process immediately.
			continue
		}

		// Finally, add this channel to breach arbiter's list of
		// channels to watch.
		channelsToWatch = append(channelsToWatch, channel)
	}

	// TODO(roasbeef): instead use closure height of channel
	_, currentHeight, err := b.chainIO.GetBestBlock()
	if err != nil {
		return err
	}

	// Spawn the exactRetribution tasks to monitor and resolve any breaches
	// that were loaded from the retribution store.
	for chanPoint, closeSummary := range closeSummaries {
		// Register for a notification when the breach transaction is
		// confirmed on chain.
		breachTXID := closeSummary.ClosingTXID
		confChan, err := b.notifier.RegisterConfirmationsNtfn(
			&breachTXID, 1, uint32(currentHeight))
		if err != nil {
			brarLog.Errorf("unable to register for conf updates "+
				"for txid: %v, err: %v", breachTXID, err)
			return err
		}

		// Launch a new goroutine which to finalize the channel
		// retribution after the breach transaction confirms.
		retInfo := breachRetInfos[chanPoint]
		b.wg.Add(1)
		go b.exactRetribution(confChan, &retInfo)
	}

	// Start watching the remaining active channels!
	b.wg.Add(1)
	go b.contractObserver(channelsToWatch)

	// Additionally, we'll also want to retrieve any pending close or force
	// close transactions to we can properly mark them as resolved in the
	// database.
	pendingCloseChans, err := b.db.FetchClosedChannels(true)
	if err != nil {
		brarLog.Errorf("unable to fetch closing channels: %v", err)
		return err
	}
	for _, pendingClose := range pendingCloseChans {
		// If this channel was force closed, and we have a non-zero
		// time-locked balance, then the utxoNursery is currently
		// watching over it.  As a result we don't need to watch over
		// it.
		if pendingClose.CloseType == channeldb.ForceClose &&
			pendingClose.TimeLockedBalance != 0 {
			continue
		}

		brarLog.Infof("Watching for the closure of ChannelPoint(%v)",
			pendingClose.ChanPoint)

		closeTXID := pendingClose.ClosingTXID
		confNtfn, err := b.notifier.RegisterConfirmationsNtfn(
			&closeTXID, 1, uint32(currentHeight),
		)
		if err != nil {
			return err
		}

		b.wg.Add(1)
		go func(chanPoint wire.OutPoint) {
			defer b.wg.Done()

			// In the case that the ChainNotifier is shutting down,
			// all subscriber notification channels will be closed,
			// generating a nil receive.
			select {
			case confInfo, ok := <-confNtfn.Confirmed:
				if !ok {
					return
				}

				brarLog.Infof("ChannelPoint(%v) is "+
					"fully closed, at height: %v",
					chanPoint, confInfo.BlockHeight)

				// TODO(roasbeef): need to store
				// UnilateralCloseSummary on disk so can
				// possibly sweep output here

				err := b.db.MarkChanFullyClosed(&chanPoint)
				if err != nil {
					brarLog.Errorf("unable to mark chan "+
						"as closed: %v", err)
				}

			case <-b.quit:
				return
			}
		}(pendingClose.ChanPoint)
	}

	return nil
}

// Stop is an idempotent method that signals the breachArbiter to execute a
// graceful shutdown. This function will block until all goroutines spawned by
// the breachArbiter have gracefully exited.
func (b *breachArbiter) Stop() error {
	if !atomic.CompareAndSwapUint32(&b.stopped, 0, 1) {
		return nil
	}

	brarLog.Infof("Breach arbiter shutting down")

	close(b.quit)
	b.wg.Wait()

	return nil
}

// contractObserver is the primary goroutine for the breachArbiter. This
// goroutine is responsible for managing goroutines that watch for breaches for
// all current active and newly created channels. If a channel breach is
// detected by a spawned child goroutine, then the contractObserver will
// execute the retribution logic required to sweep ALL outputs from a contested
// channel into the daemon's wallet.
//
// NOTE: This MUST be run as a goroutine.
func (b *breachArbiter) contractObserver(
	activeChannels []*lnwallet.LightningChannel) {

	defer b.wg.Done()

	// For each active channel found within the database, we launch a
	// detected breachObserver goroutine for that channel and also track
	// the new goroutine within the breachObservers map so we can cancel it
	// later if necessary.
	for _, channel := range activeChannels {
		settleSignal := make(chan struct{})
		chanPoint := channel.ChannelPoint()
		b.breachObservers[*chanPoint] = settleSignal

		b.wg.Add(1)
		go b.breachObserver(channel, settleSignal)
	}

	// TODO(roasbeef): need to ensure currentHeight passed in doesn't
	// result in lost notification

out:
	for {
		select {
		case breachInfo := <-b.breachedContracts:
			_, currentHeight, err := b.chainIO.GetBestBlock()
			if err != nil {
				brarLog.Errorf(
					"unable to get best height: %v", err)
			}

			// A new channel contract has just been breached! We
			// first register for a notification to be dispatched
			// once the breach transaction (the revoked commitment
			// transaction) has been confirmed in the chain to
			// ensure we're not dealing with a moving target.
			breachTXID := &breachInfo.commitHash
			confChan, err := b.notifier.RegisterConfirmationsNtfn(
				breachTXID, 1, uint32(currentHeight),
			)
			if err != nil {
				brarLog.Errorf("unable to register for conf "+
					"updates for txid: %v, err: %v",
					breachTXID, err)
				continue
			}

			brarLog.Warnf("A channel has been breached with "+
				"txid: %v. Waiting for confirmation, then "+
				"justice will be served!", breachTXID)

			// With the retribution state persisted, channel close
			// persisted, and notification registered, we launch a
			// new goroutine which will finalize the channel
			// retribution after the breach transaction has been
			// confirmed.
			b.wg.Add(1)
			go b.exactRetribution(confChan, breachInfo)

			delete(b.breachObservers, breachInfo.chanPoint)

		case contract := <-b.newContracts:
			// A new channel has just been opened within the
			// daemon, so we launch a new breachObserver to handle
			// the detection of attempted contract breaches.
			settleSignal := make(chan struct{})
			chanPoint := contract.ChannelPoint()

			// If the contract is already being watched, then an
			// additional send indicates we have a stale version of
			// the contract. So we'll cancel active watcher
			// goroutine to create a new instance with the latest
			// contract reference.
			if oldSignal, ok := b.breachObservers[*chanPoint]; ok {
				brarLog.Infof("ChannelPoint(%v) is now live, "+
					"abandoning state contract for live "+
					"version", chanPoint)
				close(oldSignal)
			}

			b.breachObservers[*chanPoint] = settleSignal

			brarLog.Debugf("New contract detected, launching " +
				"breachObserver")

			b.wg.Add(1)
			go b.breachObserver(contract, settleSignal)

			// TODO(roasbeef): add doneChan to signal to peer
			// continue * peer send over to us on
			// loadActiveChanenls, sync until we're aware so no
			// state transitions
		case chanPoint := <-b.settledContracts:
			// A new channel has been closed either unilaterally or
			// cooperatively, as a result we no longer need a
			// breachObserver detected to the channel.
			killSignal, ok := b.breachObservers[*chanPoint]
			if !ok {
				brarLog.Errorf("Unable to find contract: %v",
					chanPoint)
				continue
			}

			brarLog.Debugf("ChannelPoint(%v) has been settled, "+
				"cancelling breachObserver", chanPoint)

			// If we had a breachObserver active, then we signal it
			// for exit and also delete its state from our tracking
			// map.
			close(killSignal)
			delete(b.breachObservers, *chanPoint)
		case <-b.quit:
			break out
		}
	}

	return
}

// exactRetribution is a goroutine which is executed once a contract breach has
// been detected by a breachObserver. This function is responsible for
// punishing a counterparty for violating the channel contract by sweeping ALL
// the lingering funds within the channel into the daemon's wallet.
//
// NOTE: This MUST be run as a goroutine.
func (b *breachArbiter) exactRetribution(
	confChan *chainntnfs.ConfirmationEvent,
	breachInfo *retributionInfo) {

	defer b.wg.Done()

	// TODO(roasbeef): state needs to be checkpointed here

	select {
	case _, ok := <-confChan.Confirmed:
		// If the second value is !ok, then the channel has been closed
		// signifying a daemon shutdown, so we exit.
		if !ok {
			return
		}

		// Otherwise, if this is a real confirmation notification, then
		// we fall through to complete our duty.
	case <-b.quit:
		return
	}

	brarLog.Debugf("Breach transaction %v has been confirmed, sweeping "+
		"revoked funds", breachInfo.commitHash)

	// With the breach transaction confirmed, we now create the justice tx
	// which will claim ALL the funds within the channel.
	justiceTx, err := b.createJusticeTx(breachInfo)
	if err != nil {
		brarLog.Errorf("unable to create justice tx: %v", err)
		return
	}

	brarLog.Debugf("Broadcasting justice tx: %v",
		newLogClosure(func() string {
			return spew.Sdump(justiceTx)
		}))

	_, currentHeight, err := b.chainIO.GetBestBlock()
	if err != nil {
		brarLog.Errorf("unable to get current height: %v", err)
		return
	}

	// Finally, broadcast the transaction, finalizing the channels'
	// retribution against the cheating counterparty.
	if err := b.wallet.PublishTransaction(justiceTx); err != nil {
		brarLog.Errorf("unable to broadcast "+
			"justice tx: %v", err)
		return
	}

	// As a conclusionary step, we register for a notification to be
	// dispatched once the justice tx is confirmed. After confirmation we
	// notify the caller that initiated the retribution workflow that the
	// deed has been done.
	justiceTXID := justiceTx.TxHash()
	confChan, err = b.notifier.RegisterConfirmationsNtfn(&justiceTXID, 1,
		uint32(currentHeight))
	if err != nil {
		brarLog.Errorf("unable to register for conf for txid: %v",
			justiceTXID)
		return
	}

	select {
	case _, ok := <-confChan.Confirmed:
		if !ok {
			return
		}

		// TODO(roasbeef): factor in HTLCs
		revokedFunds := breachInfo.revokedOutput.amt
		totalFunds := revokedFunds + breachInfo.selfOutput.amt

		brarLog.Infof("Justice for ChannelPoint(%v) has "+
			"been served, %v revoked funds (%v total) "+
			"have been claimed", breachInfo.chanPoint,
			revokedFunds, totalFunds)

		// With the channel closed, mark it in the database as such.
		err := b.db.MarkChanFullyClosed(&breachInfo.chanPoint)
		if err != nil {
			brarLog.Errorf("unable to mark chan as closed: %v", err)
		}

		// Justice has been carried out; we can safely delete the
		// retribution info from the database.
		err = b.retributionStore.Remove(&breachInfo.chanPoint)
		if err != nil {
			brarLog.Errorf("unable to remove retribution "+
				"from the db: %v", err)
		}

		// TODO(roasbeef): add peer to blacklist?

		// TODO(roasbeef): close other active channels with offending
		// peer

		close(breachInfo.doneChan)

		return
	case <-b.quit:
		return
	}
}

// breachObserver notifies the breachArbiter contract observer goroutine that a
// channel's contract has been breached by the prior counterparty. Once
// notified the breachArbiter will attempt to sweep ALL funds within the
// channel using the information provided within the BreachRetribution
// generated due to the breach of channel contract. The funds will be swept
// only after the breaching transaction receives a necessary number of
// confirmations.
func (b *breachArbiter) breachObserver(contract *lnwallet.LightningChannel,
	settleSignal chan struct{}) {

	defer b.wg.Done()

	chanPoint := contract.ChannelPoint()

	brarLog.Debugf("Breach observer for ChannelPoint(%v) started",
		chanPoint)

	select {
	// A read from this channel indicates that the contract has been
	// settled cooperatively so we exit as our duties are no longer needed.
	case <-settleSignal:
		contract.Stop()
		return

	// The channel has been closed by a normal means: force closing with
	// the latest commitment transaction.
	case closeInfo := <-contract.UnilateralClose:
		// Launch a goroutine to cancel out this contract within the
		// breachArbiter's main goroutine.
		b.wg.Add(1)
		go func() {
			defer b.wg.Done()

			select {
			case b.settledContracts <- chanPoint:
			case <-b.quit:
			}
		}()

		// Next, we'll launch a goroutine to wait until the closing
		// transaction has been confirmed so we can mark the contract
		// as resolved in the database. This go routine is _not_
		// tracked by the breach aribter's wait group since the callback
		// may not be executed before shutdown, potentially leading to
		// a deadlock.
		//
		// TODO(roasbeef): also notify utxoNursery, might've had
		// outbound HTLC's in flight
		go waitForChanToClose(uint32(closeInfo.SpendingHeight),
			b.notifier, nil, chanPoint, closeInfo.SpenderTxHash,
			func() {
				// As we just detected a channel was closed via
				// a unilateral commitment broadcast by the
				// remote party, we'll need to sweep our main
				// commitment output, and any outstanding
				// outgoing HTLC we had as well.
				//
				// TODO(roasbeef): actually sweep HTLC's *
				// ensure reliable confirmation
				if closeInfo.SelfOutPoint != nil {
					sweepTx, err := b.craftCommitSweepTx(
						closeInfo,
					)
					if err != nil {
						brarLog.Errorf("unable to "+
							"generate sweep tx: %v",
							err)
						goto close
					}

					err = b.wallet.PublishTransaction(
						sweepTx,
					)
					if err != nil {
						brarLog.Errorf("unable to "+
							"broadcast tx: %v", err)
					}
				}

			close:
				brarLog.Infof("Force closed ChannelPoint(%v) "+
					"is fully closed, updating DB",
					chanPoint)

				err := b.db.MarkChanFullyClosed(chanPoint)
				if err != nil {
					brarLog.Errorf("unable to mark chan "+
						"as closed: %v", err)
				}
			})

	// A read from this channel indicates that a channel breach has been
	// detected! So we notify the main coordination goroutine with the
	// information needed to bring the counterparty to justice.
	case breachInfo := <-contract.ContractBreach:
		brarLog.Warnf("REVOKED STATE #%v FOR ChannelPoint(%v) "+
			"broadcast, REMOTE PEER IS DOING SOMETHING "+
			"SKETCHY!!!", breachInfo.RevokedStateNum,
			chanPoint)

		// Immediately notify the HTLC switch that this link has been
		// breached in order to ensure any incoming or outgoing
		// multi-hop HTLCs aren't sent over this link, nor any other
		// links associated with this peer.
		b.htlcSwitch.CloseLink(chanPoint, htlcswitch.CloseBreach)
		chanInfo := contract.StateSnapshot()

		// TODO(roasbeef): need to handle case of remote broadcast
		// mid-local initiated state-transition, possible
		// false-positive?

		// First we generate the witness generation function which will
		// be used to sweep the output only we can satisfy on the
		// commitment transaction. This output is just a regular p2wkh
		// output.
		localSignDesc := breachInfo.LocalOutputSignDesc
		localWitness := func(tx *wire.MsgTx, hc *txscript.TxSigHashes,
			inputIndex int) ([][]byte, error) {

			desc := localSignDesc
			desc.SigHashes = hc
			desc.InputIndex = inputIndex

			return lnwallet.CommitSpendNoDelay(
				b.wallet.Cfg.Signer, &desc, tx)
		}

		// Next we create the witness generation function that will be
		// used to sweep the cheating counterparty's output by taking
		// advantage of the revocation clause within the output's
		// witness script.
		remoteSignDesc := breachInfo.RemoteOutputSignDesc
		remoteWitness := func(tx *wire.MsgTx, hc *txscript.TxSigHashes,
			inputIndex int) ([][]byte, error) {

			desc := breachInfo.RemoteOutputSignDesc
			desc.SigHashes = hc
			desc.InputIndex = inputIndex

			return lnwallet.CommitSpendRevoke(
				b.wallet.Cfg.Signer, &desc, tx)
		}

		// Assemble the retribution information that parameterizes the
		// construction of transactions required to correct the breach.
		// TODO(roasbeef): populate htlc breaches
		retInfo := &retributionInfo{
			commitHash: breachInfo.BreachTransaction.TxHash(),
			chanPoint:  *chanPoint,

			remoteIdentity: chanInfo.RemoteIdentity,
			capacity:       chanInfo.Capacity,
			settledBalance: chanInfo.LocalBalance.ToSatoshis(),

			selfOutput: &breachedOutput{
				amt:            btcutil.Amount(localSignDesc.Output.Value),
				outpoint:       breachInfo.LocalOutpoint,
				signDescriptor: localSignDesc,
				witnessType:    lnwallet.CommitmentNoDelay,
				witnessFunc:    localWitness,
			},

			revokedOutput: &breachedOutput{
				amt:            btcutil.Amount(remoteSignDesc.Output.Value),
				outpoint:       breachInfo.RemoteOutpoint,
				signDescriptor: remoteSignDesc,
				witnessType:    lnwallet.CommitmentRevoke,
				witnessFunc:    remoteWitness,
			},

			htlcOutputs: []*breachedOutput{},

			doneChan: make(chan struct{}),
		}

		// Persist the pending retribution state to disk.
		if err := b.retributionStore.Add(retInfo); err != nil {
			brarLog.Errorf("unable to persist "+
				"retribution info to db: %v", err)
		}

		closeInfo := &channeldb.ChannelCloseSummary{
			ChanPoint:      *chanPoint,
			ClosingTXID:    breachInfo.BreachTransaction.TxHash(),
			RemotePub:      &chanInfo.RemoteIdentity,
			Capacity:       chanInfo.Capacity,
			SettledBalance: chanInfo.LocalBalance.ToSatoshis(),
			CloseType:      channeldb.BreachClose,
			IsPending:      true,
		}
		if err := contract.DeleteState(closeInfo); err != nil {
			brarLog.Errorf("unable to delete channel state: %v",
				err)
		}

		// Finally, we send the retribution information into the
		// breachArbiter event loop to deal swift justice.
		select {
		case b.breachedContracts <- retInfo:
		case <-b.quit:
		}

	case <-b.quit:
		return
	}
}

// breachedOutput contains all the information needed to sweep a breached
// output. A breached output is an output that we are now entitled to due to a
// revoked commitment transaction being broadcast.
type breachedOutput struct {
	amt      btcutil.Amount
	outpoint wire.OutPoint

	signDescriptor lnwallet.SignDescriptor
	witnessType    lnwallet.WitnessType
	witnessFunc    lnwallet.WitnessGenerator

	twoStageClaim bool
}

// retributionInfo encapsulates all the data needed to sweep all the contested
// funds within a channel whose contract has been breached by the prior
// counterparty. This struct is used to create the justice transaction which
// spends all outputs of the commitment transaction into an output controlled
// by the wallet.
type retributionInfo struct {
	commitHash chainhash.Hash
	chanPoint  wire.OutPoint

	// Fields copied from channel snapshot when a breach is detected. This
	// is necessary for deterministically constructing the channel close
	// summary in the event that the breach arbiter crashes before closing
	// the channel.
	remoteIdentity btcec.PublicKey
	capacity       btcutil.Amount
	settledBalance btcutil.Amount

	selfOutput *breachedOutput

	revokedOutput *breachedOutput

	htlcOutputs []*breachedOutput

	doneChan chan struct{}
}

// createJusticeTx creates a transaction which exacts "justice" by sweeping ALL
// the funds within the channel which we are now entitled to due to a breach of
// the channel's contract by the counterparty. This function returns a *fully*
// signed transaction with the witness for each input fully in place.
func (b *breachArbiter) createJusticeTx(
	r *retributionInfo) (*wire.MsgTx, error) {

	// First, we obtain a new public key script from the wallet which we'll
	// sweep the funds to.
	// TODO(roasbeef): possibly create many outputs to minimize change in
	// the future?
	pkScriptOfJustice, err := newSweepPkScript(b.wallet)
	if err != nil {
		return nil, err
	}

	r.selfOutput.witnessFunc = r.selfOutput.witnessType.GenWitnessFunc(
		&b.wallet.Cfg.Signer, &r.selfOutput.signDescriptor)

	r.revokedOutput.witnessFunc = r.revokedOutput.witnessType.GenWitnessFunc(
		&b.wallet.Cfg.Signer, &r.revokedOutput.signDescriptor)

	for i := range r.htlcOutputs {
		r.htlcOutputs[i].witnessFunc = r.htlcOutputs[i].witnessType.GenWitnessFunc(
			&b.wallet.Cfg.Signer, &r.htlcOutputs[i].signDescriptor)
	}

	// Before creating the actual TxOut, we'll need to calculate the proper
	// fee to attach to the transaction to ensure a timely confirmation.
	// TODO(roasbeef): remove hard-coded fee
	totalAmt := r.selfOutput.amt + r.revokedOutput.amt
	sweepedAmt := int64(totalAmt - 5000)

	// With the fee calculated, we can now create the justice transaction
	// using the information gathered above.
	justiceTx := wire.NewMsgTx(2)
	justiceTx.AddTxOut(&wire.TxOut{
		PkScript: pkScriptOfJustice,
		Value:    sweepedAmt,
	})
	justiceTx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: r.selfOutput.outpoint,
	})
	justiceTx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: r.revokedOutput.outpoint,
	})

	hashCache := txscript.NewTxSigHashes(justiceTx)

	// Finally, using the witness generation functions attached to the
	// retribution information, we'll populate the inputs with fully valid
	// witnesses for both commitment outputs, and all the pending HTLCs at
	// this state in the channel's history.
	// TODO(roasbeef): handle the 2-layer HTLCs
	localWitness, err := r.selfOutput.witnessFunc(justiceTx, hashCache, 0)
	if err != nil {
		return nil, err
	}
	justiceTx.TxIn[0].Witness = localWitness

	remoteWitness, err := r.revokedOutput.witnessFunc(justiceTx, hashCache, 1)
	if err != nil {
		return nil, err
	}
	justiceTx.TxIn[1].Witness = remoteWitness

	return justiceTx, nil
}

// craftCommitmentSweepTx creates a transaction to sweep the non-delayed output
// within the commitment transaction that pays to us. We must manually sweep
// this output as it uses a tweaked public key in its pkScript, so the wallet
// won't immediacy be aware of it.
//
// TODO(roasbeef): alternative options
//  * leave the output in the chain, use as input to future funding tx
//  * leave output in the chain, extend wallet to add knowledge of how to claim
func (b *breachArbiter) craftCommitSweepTx(
	closeInfo *lnwallet.UnilateralCloseSummary) (*wire.MsgTx, error) {

	// First, we'll fetch a fresh script that we can use to sweep the funds
	// under the control of the wallet.
	sweepPkScript, err := newSweepPkScript(b.wallet)
	if err != nil {
		return nil, err
	}

	// TODO(roasbeef): use proper fees
	outputAmt := closeInfo.SelfOutputSignDesc.Output.Value
	sweepAmt := int64(outputAmt - 5000)

	if sweepAmt <= 0 {
		// TODO(roasbeef): add output to special pool, can be swept
		// when: funding a channel, sweeping time locked outputs, or
		// delivering
		// justice after a channel breach
		return nil, fmt.Errorf("output to small to sweep in isolation")
	}

	// With the amount we're sweeping computed, we can now creating the
	// sweep transaction itself.
	sweepTx := wire.NewMsgTx(1)
	sweepTx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: *closeInfo.SelfOutPoint,
	})
	sweepTx.AddTxOut(&wire.TxOut{
		PkScript: sweepPkScript,
		Value:    int64(sweepAmt),
	})

	// Next, we'll generate the signature required to satisfy the p2wkh
	// witness program.
	signDesc := closeInfo.SelfOutputSignDesc
	signDesc.SigHashes = txscript.NewTxSigHashes(sweepTx)
	signDesc.InputIndex = 0
	sweepSig, err := b.wallet.Cfg.Signer.SignOutputRaw(sweepTx, signDesc)
	if err != nil {
		return nil, err
	}

	// Finally, we'll manually craft the witness. The witness here is the
	// exact same as a regular p2wkh witness, but we'll need to ensure that
	// we use the tweaked public key as the last item in the witness stack
	// which was originally used to created the pkScript we're spending.
	witness := make([][]byte, 2)
	witness[0] = append(sweepSig, byte(txscript.SigHashAll))
	witness[1] = lnwallet.TweakPubKeyWithTweak(
		signDesc.PubKey, signDesc.SingleTweak,
	).SerializeCompressed()

	sweepTx.TxIn[0].Witness = witness

	brarLog.Infof("Sweeping commitment output with: %v", spew.Sdump(sweepTx))

	return sweepTx, nil
}

// RetributionStore provides an interface for managing a persistent map from
// wire.OutPoint -> retributionInfo. Upon learning of a breach, a BreachArbiter
// should record the retributionInfo for the breached channel, which serves a
// checkpoint in the event that retribution needs to be resumed after failure.
// A RetributionStore provides an interface for managing the persisted set, as
// well as mapping user defined functions over the entire on-disk contents.
//
// Calls to RetributionStore may occur concurrently. A concrete instance of
// RetributionStore should use appropriate synchronization primitives, or
// be otherwise safe for concurrent access.
type RetributionStore interface {
	// Add persists the retributionInfo to disk, using the information's
	// chanPoint as the key. This method should overwrite any existing
	// entires found under the same key, and an error should be raised if
	// the addition fails.
	Add(retInfo *retributionInfo) error

	// Remove deletes the retributionInfo from disk, if any exists, under
	// the given key. An error should be re raised if the removal fails.
	Remove(key *wire.OutPoint) error

	// ForAll iterates over the existing on-disk contents and applies a
	// chosen, read-only callback to each. This method should ensure that it
	// immediately propagate any errors generated by the callback.
	ForAll(cb func(*retributionInfo) error) error
}

// retributionStore handles persistence of retribution states to disk and is
// backed by a boltdb bucket. The primary responsibility of the retribution
// store is to ensure that we can recover from a restart in the middle of a
// breached contract retribution.
type retributionStore struct {
	db *channeldb.DB
}

// newRetributionStore creates a new instance of a retributionStore.
func newRetributionStore(db *channeldb.DB) *retributionStore {
	return &retributionStore{
		db: db,
	}
}

// Add adds a retribution state to the retributionStore, which is then persisted
// to disk.
func (rs *retributionStore) Add(ret *retributionInfo) error {
	return rs.db.Update(func(tx *bolt.Tx) error {
		// If this is our first contract breach, the retributionBucket
		// won't exist, in which case, we just create a new bucket.
		retBucket, err := tx.CreateBucketIfNotExists(retributionBucket)
		if err != nil {
			return err
		}

		var outBuf bytes.Buffer
		if err := writeOutpoint(&outBuf, &ret.chanPoint); err != nil {
			return err
		}

		var retBuf bytes.Buffer
		if err := ret.Encode(&retBuf); err != nil {
			return err
		}

		return retBucket.Put(outBuf.Bytes(), retBuf.Bytes())
	})
}

// Remove removes a retribution state from the retributionStore database.
func (rs *retributionStore) Remove(key *wire.OutPoint) error {
	return rs.db.Update(func(tx *bolt.Tx) error {
		retBucket := tx.Bucket(retributionBucket)

		// We return an error if the bucket is not already created,
		// since normal operation of the breach arbiter should never try
		// to remove a finalized retribution state that is not already
		// stored in the db.
		if retBucket == nil {
			return errors.New("unable to remove retribution " +
				"because the db bucket doesn't exist.")
		}

		var outBuf bytes.Buffer
		if err := writeOutpoint(&outBuf, key); err != nil {
			return err
		}

		return retBucket.Delete(outBuf.Bytes())
	})
}

// ForAll iterates through all stored retributions and executes the passed
// callback function on each retribution.
func (rs *retributionStore) ForAll(cb func(*retributionInfo) error) error {
	return rs.db.View(func(tx *bolt.Tx) error {
		// If the bucket does not exist, then there are no pending
		// retributions.
		retBucket := tx.Bucket(retributionBucket)
		if retBucket == nil {
			return nil
		}

		// Otherwise, we fetch each serialized retribution info,
		// deserialize it, and execute the passed in callback function
		// on it.
		return retBucket.ForEach(func(outBytes, retBytes []byte) error {
			ret := &retributionInfo{}
			if err := ret.Decode(
				bytes.NewBuffer(retBytes),
			); err != nil {
				return err
			}

			return cb(ret)
		})
	})
}

// Encode serializes the retribution into the passed byte stream.
func (ret *retributionInfo) Encode(w io.Writer) error {
	var scratch [8]byte

	if _, err := w.Write(ret.commitHash[:]); err != nil {
		return err
	}

	if err := writeOutpoint(w, &ret.chanPoint); err != nil {
		return err
	}

	if _, err := w.Write(
		ret.remoteIdentity.SerializeCompressed()); err != nil {
		return err
	}

	binary.BigEndian.PutUint64(scratch[:8], uint64(ret.capacity))
	if _, err := w.Write(scratch[:8]); err != nil {
		return err
	}

	binary.BigEndian.PutUint64(scratch[:8], uint64(ret.settledBalance))
	if _, err := w.Write(scratch[:8]); err != nil {
		return err
	}

	if err := ret.selfOutput.Encode(w); err != nil {
		return err
	}

	if err := ret.revokedOutput.Encode(w); err != nil {
		return err
	}

	numHtlcOutputs := len(ret.htlcOutputs)
	if err := wire.WriteVarInt(w, 0, uint64(numHtlcOutputs)); err != nil {
		return err
	}

	for i := 0; i < numHtlcOutputs; i++ {
		if err := ret.htlcOutputs[i].Encode(w); err != nil {
			return err
		}
	}

	return nil
}

// Dencode deserializes a retribution from the passed byte stream.
func (ret *retributionInfo) Decode(r io.Reader) error {
	var scratch [33]byte

	if _, err := io.ReadFull(r, scratch[:32]); err != nil {
		return err
	}
	hash, err := chainhash.NewHash(scratch[:32])
	if err != nil {
		return err
	}
	ret.commitHash = *hash

	if err := readOutpoint(r, &ret.chanPoint); err != nil {
		return err
	}

	if _, err = io.ReadFull(r, scratch[:33]); err != nil {
		return err
	}
	remoteIdentity, err := btcec.ParsePubKey(scratch[:33], btcec.S256())
	if err != nil {
		return err
	}
	ret.remoteIdentity = *remoteIdentity

	if _, err := io.ReadFull(r, scratch[:8]); err != nil {
		return err
	}
	ret.capacity = btcutil.Amount(binary.BigEndian.Uint64(scratch[:8]))

	if _, err := io.ReadFull(r, scratch[:8]); err != nil {
		return err
	}
	ret.settledBalance = btcutil.Amount(
		binary.BigEndian.Uint64(scratch[:8]))

	ret.selfOutput = &breachedOutput{}
	if err := ret.selfOutput.Decode(r); err != nil {
		return err
	}

	ret.revokedOutput = &breachedOutput{}
	if err := ret.revokedOutput.Decode(r); err != nil {
		return err
	}

	numHtlcOutputsU64, err := wire.ReadVarInt(r, 0)
	if err != nil {
		return err
	}
	numHtlcOutputs := int(numHtlcOutputsU64)

	ret.htlcOutputs = make([]*breachedOutput, numHtlcOutputs)
	for i := 0; i < numHtlcOutputs; i++ {
		ret.htlcOutputs[i] = &breachedOutput{}
		if err := ret.htlcOutputs[i].Decode(r); err != nil {
			return err
		}
	}

	return nil
}

// Encode serializes a breachedOutput into the passed byte stream.
func (bo *breachedOutput) Encode(w io.Writer) error {
	var scratch [8]byte

	binary.BigEndian.PutUint64(scratch[:8], uint64(bo.amt))
	if _, err := w.Write(scratch[:8]); err != nil {
		return err
	}

	if err := writeOutpoint(w, &bo.outpoint); err != nil {
		return err
	}

	if err := lnwallet.WriteSignDescriptor(
		w, &bo.signDescriptor); err != nil {
		return err
	}

	binary.BigEndian.PutUint16(scratch[:2], uint16(bo.witnessType))
	if _, err := w.Write(scratch[:2]); err != nil {
		return err
	}

	if bo.twoStageClaim {
		scratch[0] = 1
	} else {
		scratch[0] = 0
	}
	if _, err := w.Write(scratch[:1]); err != nil {
		return err
	}

	return nil
}

// Decode deserializes a breachedOutput from the passed byte stream.
func (bo *breachedOutput) Decode(r io.Reader) error {
	var scratch [8]byte

	if _, err := io.ReadFull(r, scratch[:8]); err != nil {
		return err
	}
	bo.amt = btcutil.Amount(binary.BigEndian.Uint64(scratch[:8]))

	if err := readOutpoint(r, &bo.outpoint); err != nil {
		return err
	}

	if err := lnwallet.ReadSignDescriptor(
		r, &bo.signDescriptor); err != nil {
		return err
	}

	if _, err := io.ReadFull(r, scratch[:2]); err != nil {
		return err
	}
	bo.witnessType = lnwallet.WitnessType(
		binary.BigEndian.Uint16(scratch[:2]))

	if _, err := io.ReadFull(r, scratch[:1]); err != nil {
		return err
	}
	if scratch[0] == 1 {
		bo.twoStageClaim = true
	} else {
		bo.twoStageClaim = false
	}

	return nil
}
