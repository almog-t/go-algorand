// Copyright (C) 2019-2022 Algorand, Inc.
// This file is part of go-algorand
//
// go-algorand is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as
// published by the Free Software Foundation, either version 3 of the
// License, or (at your option) any later version.
//
// go-algorand is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with go-algorand.  If not, see <https://www.gnu.org/licenses/>.

package ledger

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/algorand/go-algorand/config"
	"github.com/algorand/go-algorand/crypto"
	"github.com/algorand/go-algorand/crypto/merkletrie"
	"github.com/algorand/go-algorand/data/basics"
	"github.com/algorand/go-algorand/data/bookkeeping"
	"github.com/algorand/go-algorand/ledger/ledgercore"
	"github.com/algorand/go-algorand/logging"
	"github.com/algorand/go-algorand/protocol"
	"github.com/algorand/go-algorand/util/db"
	"github.com/algorand/go-algorand/util/metrics"
)

// CatchpointCatchupAccessor is an interface for the accessor wrapping the database storage for the catchpoint catchup functionality.
type CatchpointCatchupAccessor interface {
	// GetState returns the current state of the catchpoint catchup
	GetState(ctx context.Context) (state CatchpointCatchupState, err error)

	// SetState set the state of the catchpoint catchup
	SetState(ctx context.Context, state CatchpointCatchupState) (err error)

	// GetLabel returns the current catchpoint catchup label
	GetLabel(ctx context.Context) (label string, err error)

	// SetLabel set the catchpoint catchup label
	SetLabel(ctx context.Context, label string) (err error)

	// ResetStagingBalances resets the current staging balances, preparing for a new set of balances to be added
	ResetStagingBalances(ctx context.Context, newCatchup bool) (err error)

	// ProcessStagingBalances deserialize the given bytes as a temporary staging balances
	ProcessStagingBalances(ctx context.Context, sectionName string, bytes []byte, progress *CatchpointCatchupAccessorProgress) (err error)

	// BuildMerkleTrie inserts the account hashes into the merkle trie
	BuildMerkleTrie(ctx context.Context, progressUpdates func(uint64)) (err error)

	// GetCatchupBlockRound returns the latest block round matching the current catchpoint
	GetCatchupBlockRound(ctx context.Context) (round basics.Round, err error)

	// VerifyCatchpoint verifies that the catchpoint is valid by reconstructing the label.
	VerifyCatchpoint(ctx context.Context, blk *bookkeeping.Block) (err error)

	// StoreBalancesRound calculates the balances round based on the first block and the associated consensus parameters, and
	// store that to the database
	StoreBalancesRound(ctx context.Context, blk *bookkeeping.Block) (err error)

	// StoreFirstBlock stores a single block to the blocks database.
	StoreFirstBlock(ctx context.Context, blk *bookkeeping.Block) (err error)

	// StoreBlock stores a single block to the blocks database.
	StoreBlock(ctx context.Context, blk *bookkeeping.Block) (err error)

	// FinishBlocks concludes the catchup of the blocks database.
	FinishBlocks(ctx context.Context, applyChanges bool) (err error)

	// EnsureFirstBlock ensure that we have a single block in the staging block table, and returns that block
	EnsureFirstBlock(ctx context.Context) (blk bookkeeping.Block, err error)

	// CompleteCatchup completes the catchpoint catchup process by switching the databases tables around
	// and reloading the ledger.
	CompleteCatchup(ctx context.Context) (err error)

	// Ledger returns a narrow subset of Ledger methods needed by CatchpointCatchupAccessor clients
	Ledger() (l CatchupAccessorClientLedger)
}

type stagingWriter interface {
	writeBalances(context.Context, []normalizedAccountBalance) error
	writeCreatables(context.Context, []normalizedAccountBalance) error
	writeHashes(context.Context, []normalizedAccountBalance) error
	writeKVs(context.Context, []encodedKVRecordV6) error
	isShared() bool
}

type stagingWriterImpl struct {
	wdb db.Accessor
}

func (w *stagingWriterImpl) writeBalances(ctx context.Context, balances []normalizedAccountBalance) error {
	return w.wdb.Atomic(func(ctx context.Context, tx *sql.Tx) (err error) {
		return writeCatchpointStagingBalances(ctx, tx, balances)
	})
}

func (w *stagingWriterImpl) writeKVs(ctx context.Context, kvrs []encodedKVRecordV6) error {
	return w.wdb.Atomic(func(ctx context.Context, tx *sql.Tx) (err error) {
		return writeCatchpointStagingKVs(ctx, tx, kvrs)
	})
}

func (w *stagingWriterImpl) writeCreatables(ctx context.Context, balances []normalizedAccountBalance) error {
	return w.wdb.Atomic(func(ctx context.Context, tx *sql.Tx) error {
		return writeCatchpointStagingCreatable(ctx, tx, balances)
	})
}

func (w *stagingWriterImpl) writeHashes(ctx context.Context, balances []normalizedAccountBalance) error {
	return w.wdb.Atomic(func(ctx context.Context, tx *sql.Tx) error {
		err := writeCatchpointStagingHashes(ctx, tx, balances)
		return err
	})
}

func (w *stagingWriterImpl) isShared() bool {
	return w.wdb.IsSharedCacheConnection()
}

// catchpointCatchupAccessorImpl is the concrete implementation of the CatchpointCatchupAccessor interface
type catchpointCatchupAccessorImpl struct {
	ledger *Ledger

	stagingWriter stagingWriter

	// log copied from ledger
	log logging.Logger

	acctResCnt catchpointAccountResourceCounter

	// expecting next account to be a specific account
	expectingSpecificAccount bool
	// next expected balance account, empty address if not expecting specific account
	nextExpectedAccount basics.Address
}

// CatchpointCatchupState is the state of the current catchpoint catchup process
type CatchpointCatchupState int32

const (
	// CatchpointCatchupStateInactive is the common state for the catchpoint catchup - not active.
	CatchpointCatchupStateInactive = iota
	// CatchpointCatchupStateLedgerDownload indicates that we're downloading the ledger
	CatchpointCatchupStateLedgerDownload
	// CatchpointCatchupStateLatestBlockDownload indicates that we're download the latest block
	CatchpointCatchupStateLatestBlockDownload
	// CatchpointCatchupStateBlocksDownload indicates that we're downloading the blocks prior to the latest one ( total of CatchpointLookback blocks )
	CatchpointCatchupStateBlocksDownload
	// CatchpointCatchupStateSwitch indicates that we're switching to use the downloaded ledger/blocks content
	CatchpointCatchupStateSwitch

	// catchpointCatchupStateLast is the last entry in the CatchpointCatchupState enumeration.
	catchpointCatchupStateLast = CatchpointCatchupStateSwitch
)

// CatchupAccessorClientLedger represents ledger interface needed for catchpoint accessor clients
type CatchupAccessorClientLedger interface {
	Block(rnd basics.Round) (blk bookkeeping.Block, err error)
	GenesisHash() crypto.Digest
	BlockHdr(rnd basics.Round) (blk bookkeeping.BlockHeader, err error)
	Latest() (rnd basics.Round)
}

// MakeCatchpointCatchupAccessor creates a CatchpointCatchupAccessor given a ledger
func MakeCatchpointCatchupAccessor(ledger *Ledger, log logging.Logger) CatchpointCatchupAccessor {
	return &catchpointCatchupAccessorImpl{
		ledger:        ledger,
		stagingWriter: &stagingWriterImpl{wdb: ledger.trackerDB().Wdb},
		log:           log,
	}
}

// GetState returns the current state of the catchpoint catchup
func (c *catchpointCatchupAccessorImpl) GetState(ctx context.Context) (state CatchpointCatchupState, err error) {
	var istate uint64
	istate, err = readCatchpointStateUint64(ctx, c.ledger.trackerDB().Rdb.Handle, catchpointStateCatchupState)
	if err != nil {
		return 0, fmt.Errorf("unable to read catchpoint catchup state '%s': %v", catchpointStateCatchupState, err)
	}
	state = CatchpointCatchupState(istate)
	return
}

// SetState set the state of the catchpoint catchup
func (c *catchpointCatchupAccessorImpl) SetState(ctx context.Context, state CatchpointCatchupState) (err error) {
	if state < CatchpointCatchupStateInactive || state > catchpointCatchupStateLast {
		return fmt.Errorf("invalid catchpoint catchup state provided : %d", state)
	}
	err = writeCatchpointStateUint64(ctx, c.ledger.trackerDB().Wdb.Handle, catchpointStateCatchupState, uint64(state))
	if err != nil {
		return fmt.Errorf("unable to write catchpoint catchup state '%s': %v", catchpointStateCatchupState, err)
	}
	return
}

// GetLabel returns the current catchpoint catchup label
func (c *catchpointCatchupAccessorImpl) GetLabel(ctx context.Context) (label string, err error) {
	label, err = readCatchpointStateString(ctx, c.ledger.trackerDB().Rdb.Handle, catchpointStateCatchupLabel)
	if err != nil {
		return "", fmt.Errorf("unable to read catchpoint catchup state '%s': %v", catchpointStateCatchupLabel, err)
	}
	return
}

// SetLabel set the catchpoint catchup label
func (c *catchpointCatchupAccessorImpl) SetLabel(ctx context.Context, label string) (err error) {
	// verify it's parsable :
	_, _, err = ledgercore.ParseCatchpointLabel(label)
	if err != nil {
		return
	}

	err = writeCatchpointStateString(ctx, c.ledger.trackerDB().Wdb.Handle, catchpointStateCatchupLabel, label)
	if err != nil {
		return fmt.Errorf("unable to write catchpoint catchup state '%s': %v", catchpointStateCatchupLabel, err)
	}
	return
}

// ResetStagingBalances resets the current staging balances, preparing for a new set of balances to be added
func (c *catchpointCatchupAccessorImpl) ResetStagingBalances(ctx context.Context, newCatchup bool) (err error) {
	wdb := c.ledger.trackerDB().Wdb
	if !newCatchup {
		c.ledger.setSynchronousMode(ctx, c.ledger.synchronousMode)
	}
	start := time.Now()
	ledgerResetstagingbalancesCount.Inc(nil)
	err = wdb.Atomic(func(ctx context.Context, tx *sql.Tx) (err error) {
		err = resetCatchpointStagingBalances(ctx, tx, newCatchup)
		if err != nil {
			return fmt.Errorf("unable to reset catchpoint catchup balances : %v", err)
		}
		if !newCatchup {
			err = writeCatchpointStateUint64(ctx, tx, catchpointStateCatchupBalancesRound, 0)
			if err != nil {
				return err
			}

			err = writeCatchpointStateUint64(ctx, tx, catchpointStateCatchupBlockRound, 0)
			if err != nil {
				return err
			}

			err = writeCatchpointStateString(ctx, tx, catchpointStateCatchupLabel, "")
			if err != nil {
				return err
			}
			err = writeCatchpointStateUint64(ctx, tx, catchpointStateCatchupState, 0)
			if err != nil {
				return fmt.Errorf("unable to write catchpoint catchup state '%s': %v", catchpointStateCatchupState, err)
			}
		}
		return
	})
	ledgerResetstagingbalancesMicros.AddMicrosecondsSince(start, nil)
	return
}

// CatchpointCatchupAccessorProgress is used by the caller of ProcessStagingBalances to obtain progress information
type CatchpointCatchupAccessorProgress struct {
	TotalAccounts      uint64
	ProcessedAccounts  uint64
	ProcessedBytes     uint64
	TotalChunks        uint64
	SeenHeader         bool
	Version            uint64
	TotalAccountHashes uint64

	// Having the cachedTrie here would help to accelerate the catchup process since the trie maintain an internal cache of nodes.
	// While rebuilding the trie, we don't want to force and reload (some) of these nodes into the cache for each catchpoint file chunk.
	cachedTrie *merkletrie.Trie

	BalancesWriteDuration   time.Duration
	CreatablesWriteDuration time.Duration
	HashesWriteDuration     time.Duration
	KVWriteDuration         time.Duration
}

// ProcessStagingBalances deserialize the given bytes as a temporary staging balances
func (c *catchpointCatchupAccessorImpl) ProcessStagingBalances(ctx context.Context, sectionName string, bytes []byte, progress *CatchpointCatchupAccessorProgress) (err error) {
	// content.msgpack comes first, followed by stateProofVerificationData.msgpack and then by balances.x.msgpack.
	if sectionName == "content.msgpack" {
		return c.processStagingContent(ctx, bytes, progress)
	}
	if sectionName == "stateProofVerificationData.msgpack" {
		return c.processStagingStateProofVerificationData(ctx, bytes, progress)
	}
	if strings.HasPrefix(sectionName, "balances.") && strings.HasSuffix(sectionName, ".msgpack") {
		return c.processStagingBalances(ctx, bytes, progress)
	}
	// we want to allow undefined sections to support backward compatibility.
	c.log.Warnf("CatchpointCatchupAccessorImpl::ProcessStagingBalances encountered unexpected section name '%s' of length %d, which would be ignored", sectionName, len(bytes))
	return nil
}

// processStagingStateProofVerificationData deserialize the given bytes as a temporary staging state proof verification data
func (c *catchpointCatchupAccessorImpl) processStagingStateProofVerificationData(_ context.Context, bytes []byte, _ *CatchpointCatchupAccessorProgress) (err error) {
	var decodedData catchpointStateProofVerificationData
	err = protocol.Decode(bytes, &decodedData)
	if err != nil {
		return err
	}

	wdb := c.ledger.trackerDB().Wdb

	// 6 months of stuck state proofs should lead to about 1.5 MB of data, so we avoid redundant timers
	// and progress reports.

	err = wdb.Atomic(func(ctx context.Context, tx *sql.Tx) (err error) {
		for _, data := range decodedData.Data {
			err = writeCatchpointStateProofVerificationData(ctx, tx, &data)
			if err != nil {
				return err
			}
		}
		return nil
	})

	return err
}

// processStagingContent deserialize the given bytes as a temporary staging balances content
func (c *catchpointCatchupAccessorImpl) processStagingContent(ctx context.Context, bytes []byte, progress *CatchpointCatchupAccessorProgress) (err error) {
	if progress.SeenHeader {
		return fmt.Errorf("CatchpointCatchupAccessorImpl::processStagingContent: content chunk already seen")
	}
	var fileHeader CatchpointFileHeader
	err = protocol.Decode(bytes, &fileHeader)
	if err != nil {
		return err
	}
	switch fileHeader.Version {
	case CatchpointFileVersionV5:
	case CatchpointFileVersionV6:
	case CatchpointFileVersionV7:
	default:
		return fmt.Errorf("CatchpointCatchupAccessorImpl::processStagingContent: unable to process catchpoint - version %d is not supported", fileHeader.Version)
	}

	// the following fields are now going to be ignored. We could add these to the database and validate these
	// later on:
	// TotalAccounts, TotalAccounts, Catchpoint, BlockHeaderDigest, BalancesRound
	wdb := c.ledger.trackerDB().Wdb
	start := time.Now()
	ledgerProcessstagingcontentCount.Inc(nil)
	err = writeCatchpointStateUint64(ctx, wdb.Handle, catchpointStateCatchupVersion, fileHeader.Version)
	if err != nil {
		return fmt.Errorf("CatchpointCatchupAccessorImpl::processStagingContent: unable to write catchpoint catchup state '%s': %v", catchpointStateCatchupVersion, err)
	}

	err = wdb.Atomic(func(ctx context.Context, tx *sql.Tx) (err error) {
		err = writeCatchpointStateUint64(ctx, tx, catchpointStateCatchupBlockRound, uint64(fileHeader.BlocksRound))
		if err != nil {
			return fmt.Errorf("CatchpointCatchupAccessorImpl::processStagingContent: unable to write catchpoint catchup state '%s': %v", catchpointStateCatchupBlockRound, err)
		}
		if fileHeader.Version == CatchpointFileVersionV6 || fileHeader.Version == CatchpointFileVersionV7 {
			err = writeCatchpointStateUint64(ctx, tx, catchpointStateCatchupHashRound, uint64(fileHeader.BlocksRound))
			if err != nil {
				return fmt.Errorf("CatchpointCatchupAccessorImpl::processStagingContent: unable to write catchpoint catchup state '%s': %v", catchpointStateCatchupHashRound, err)
			}
		}
		err = accountsPutTotals(tx, fileHeader.Totals, true)
		return
	})
	ledgerProcessstagingcontentMicros.AddMicrosecondsSince(start, nil)
	if err == nil {
		progress.SeenHeader = true
		progress.TotalAccounts = fileHeader.TotalAccounts
		progress.TotalChunks = fileHeader.TotalChunks
		progress.Version = fileHeader.Version
		c.ledger.setSynchronousMode(ctx, c.ledger.accountsRebuildSynchronousMode)
	}

	return err
}

// processStagingBalances deserialize the given bytes as a temporary staging balances
func (c *catchpointCatchupAccessorImpl) processStagingBalances(ctx context.Context, bytes []byte, progress *CatchpointCatchupAccessorProgress) (err error) {
	if !progress.SeenHeader {
		return fmt.Errorf("CatchpointCatchupAccessorImpl::processStagingBalances: content chunk was missing")
	}

	start := time.Now()
	ledgerProcessstagingbalancesCount.Inc(nil)

	var normalizedAccountBalances []normalizedAccountBalance
	var expectingMoreEntries []bool
	var chunkKVs []encodedKVRecordV6

	switch progress.Version {
	default:
		// unsupported version.
		// we won't get to this point, since we've already verified the version in processStagingContent
		return errors.New("unsupported version")
	case CatchpointFileVersionV5:
		var balances catchpointFileBalancesChunkV5
		err = protocol.Decode(bytes, &balances)
		if err != nil {
			return err
		}

		if len(balances.Balances) == 0 {
			return fmt.Errorf("processStagingBalances received a chunk with no accounts")
		}

		normalizedAccountBalances, err = prepareNormalizedBalancesV5(balances.Balances, c.ledger.GenesisProto())
		expectingMoreEntries = make([]bool, len(balances.Balances))

	case CatchpointFileVersionV6:
	case CatchpointFileVersionV7:
		var chunk catchpointFileChunkV6
		err = protocol.Decode(bytes, &chunk)
		if err != nil {
			return err
		}

		if len(chunk.Balances) == 0 && len(chunk.KVs) == 0 {
			return fmt.Errorf("processStagingBalances received a chunk with no accounts or KVs")
		}

		normalizedAccountBalances, err = prepareNormalizedBalancesV6(chunk.Balances, c.ledger.GenesisProto())
		expectingMoreEntries = make([]bool, len(chunk.Balances))
		for i, balance := range chunk.Balances {
			expectingMoreEntries[i] = balance.ExpectingMoreEntries
		}
		chunkKVs = chunk.KVs
	}

	if err != nil {
		return fmt.Errorf("processStagingBalances failed to prepare normalized balances : %w", err)
	}

	expectingSpecificAccount := c.expectingSpecificAccount
	nextExpectedAccount := c.nextExpectedAccount

	// keep track of number of resources processed for each account
	for i, balance := range normalizedAccountBalances {
		// missing resources for this account
		if expectingSpecificAccount && balance.address != nextExpectedAccount {
			return fmt.Errorf("processStagingBalances received incomplete chunks for account %v", nextExpectedAccount)
		}

		for _, resData := range balance.resources {
			if resData.IsApp() && resData.IsOwning() {
				c.acctResCnt.totalAppParams++
			}
			if resData.IsApp() && resData.IsHolding() {
				c.acctResCnt.totalAppLocalStates++
			}
			if resData.IsAsset() && resData.IsOwning() {
				c.acctResCnt.totalAssetParams++
			}
			if resData.IsAsset() && resData.IsHolding() {
				c.acctResCnt.totalAssets++
			}
		}
		// check that counted resources adds up for this account
		if !expectingMoreEntries[i] {
			if c.acctResCnt.totalAppParams != balance.accountData.TotalAppParams {
				return fmt.Errorf(
					"processStagingBalances received %d appParams for account %v, expected %d",
					c.acctResCnt.totalAppParams,
					balance.address,
					balance.accountData.TotalAppParams,
				)
			}
			if c.acctResCnt.totalAppLocalStates != balance.accountData.TotalAppLocalStates {
				return fmt.Errorf(
					"processStagingBalances received %d appLocalStates for account %v, expected %d",
					c.acctResCnt.totalAppParams,
					balance.address,
					balance.accountData.TotalAppLocalStates,
				)
			}
			if c.acctResCnt.totalAssetParams != balance.accountData.TotalAssetParams {
				return fmt.Errorf(
					"processStagingBalances received %d assetParams for account %v, expected %d",
					c.acctResCnt.totalAppParams,
					balance.address,
					balance.accountData.TotalAssetParams,
				)
			}
			if c.acctResCnt.totalAssets != balance.accountData.TotalAssets {
				return fmt.Errorf(
					"processStagingBalances received %d assets for account %v, expected %d",
					c.acctResCnt.totalAppParams,
					balance.address,
					balance.accountData.TotalAssets,
				)
			}
			c.acctResCnt = catchpointAccountResourceCounter{}
			nextExpectedAccount = basics.Address{}
			expectingSpecificAccount = false
		} else {
			nextExpectedAccount = balance.address
			expectingSpecificAccount = true
		}
	}

	wg := sync.WaitGroup{}

	var errBalances error
	var errCreatables error
	var errHashes error
	var errKVs error
	var durBalances time.Duration
	var durCreatables time.Duration
	var durHashes time.Duration
	var durKVs time.Duration

	// start the balances writer
	wg.Add(1)
	go func() {
		defer wg.Done()
		start := time.Now()
		errBalances = c.stagingWriter.writeBalances(ctx, normalizedAccountBalances)
		durBalances = time.Since(start)
	}()

	// on a in-memory database, wait for the writer to finish before starting the new writer
	if c.stagingWriter.isShared() {
		wg.Wait()
	}

	// starts the creatables writer
	wg.Add(1)
	go func() {
		defer wg.Done()
		hasCreatables := false
		for _, accBal := range normalizedAccountBalances {
			for _, res := range accBal.resources {
				if res.IsOwning() {
					hasCreatables = true
					break
				}
			}
		}
		if hasCreatables {
			start := time.Now()
			errCreatables = c.stagingWriter.writeCreatables(ctx, normalizedAccountBalances)
			durCreatables = time.Since(start)
		}
	}()

	// on a in-memory database, wait for the writer to finish before starting the new writer
	if c.stagingWriter.isShared() {
		wg.Wait()
	}

	// start the accounts pending hashes writer
	wg.Add(1)
	go func() {
		defer wg.Done()
		start := time.Now()
		errHashes = c.stagingWriter.writeHashes(ctx, normalizedAccountBalances)
		durHashes = time.Since(start)
	}()

	// on a in-memory database, wait for the writer to finish before starting the new writer
	if c.stagingWriter.isShared() {
		wg.Wait()
	}

	// start the kv store writer
	wg.Add(1)
	go func() {
		defer wg.Done()

		start := time.Now()
		errKVs = c.stagingWriter.writeKVs(ctx, chunkKVs)
		durKVs = time.Since(start)
	}()

	wg.Wait()

	if errBalances != nil {
		return errBalances
	}
	if errCreatables != nil {
		return errCreatables
	}
	if errHashes != nil {
		return errHashes
	}
	if errKVs != nil {
		return errKVs
	}

	progress.BalancesWriteDuration += durBalances
	progress.CreatablesWriteDuration += durCreatables
	progress.HashesWriteDuration += durHashes
	progress.KVWriteDuration += durKVs

	ledgerProcessstagingbalancesMicros.AddMicrosecondsSince(start, nil)
	progress.ProcessedBytes += uint64(len(bytes))
	for _, acctBal := range normalizedAccountBalances {
		progress.TotalAccountHashes += uint64(len(acctBal.accountHashes))
		if !acctBal.partialBalance {
			progress.ProcessedAccounts++
		}
	}

	// not strictly required, but clean up the pointer when we're done.
	if progress.ProcessedAccounts == progress.TotalAccounts {
		progress.cachedTrie = nil
		// restore "normal" synchronous mode
		c.ledger.setSynchronousMode(ctx, c.ledger.synchronousMode)
	}

	c.expectingSpecificAccount = expectingSpecificAccount
	c.nextExpectedAccount = nextExpectedAccount
	return err
}

// BuildMerkleTrie would process the catchpointpendinghashes and insert all the items in it into the merkle trie
func (c *catchpointCatchupAccessorImpl) BuildMerkleTrie(ctx context.Context, progressUpdates func(uint64)) (err error) {
	wdb := c.ledger.trackerDB().Wdb
	rdb := c.ledger.trackerDB().Rdb
	err = wdb.Atomic(func(ctx context.Context, tx *sql.Tx) (err error) {
		// creating the index can take a while, so ensure we don't generate false alerts for no good reason.
		db.ResetTransactionWarnDeadline(ctx, tx, time.Now().Add(120*time.Second))
		return createCatchpointStagingHashesIndex(ctx, tx)
	})
	if err != nil {
		return
	}

	wg := sync.WaitGroup{}
	wg.Add(2)
	errChan := make(chan error, 2)

	writerQueue := make(chan [][]byte, 16)
	c.ledger.setSynchronousMode(ctx, c.ledger.accountsRebuildSynchronousMode)
	defer c.ledger.setSynchronousMode(ctx, c.ledger.synchronousMode)

	// starts the hashes reader
	go func() {
		defer wg.Done()
		defer close(writerQueue)

		err := rdb.Atomic(func(transactionCtx context.Context, tx *sql.Tx) (err error) {
			it := makeCatchpointPendingHashesIterator(trieRebuildAccountChunkSize, tx)
			var hashes [][]byte
			for {
				hashes, err = it.Next(transactionCtx)
				if err != nil {
					break
				}
				if len(hashes) > 0 {
					writerQueue <- hashes
				}
				if len(hashes) != trieRebuildAccountChunkSize {
					break
				}
				if ctx.Err() != nil {
					it.Close()
					break
				}
			}
			// disable the warning for over-long atomic operation execution. It's meaningless here since it's
			// co-dependent on the other go-routine.
			db.ResetTransactionWarnDeadline(transactionCtx, tx, time.Now().Add(5*time.Second))
			return err
		})
		if err != nil {
			errChan <- err
		}
	}()

	// starts the merkle trie writer
	go func() {
		defer wg.Done()
		var trie *merkletrie.Trie
		uncommitedHashesCount := 0
		keepWriting := true
		hashesWritten := uint64(0)
		var mc *MerkleCommitter
		if progressUpdates != nil {
			progressUpdates(hashesWritten)
		}

		err := wdb.Atomic(func(transactionCtx context.Context, tx *sql.Tx) (err error) {
			// create the merkle trie for the balances
			mc, err = MakeMerkleCommitter(tx, true)
			if err != nil {
				return
			}

			trie, err = merkletrie.MakeTrie(mc, TrieMemoryConfig)
			return err
		})
		if err != nil {
			errChan <- err
			return
		}

		for keepWriting {
			var hashesToWrite [][]byte
			select {
			case hashesToWrite = <-writerQueue:
				if hashesToWrite == nil {
					// i.e. the writerQueue is closed.
					keepWriting = false
					continue
				}
			case <-ctx.Done():
				keepWriting = false
				continue
			}

			err = rdb.Atomic(func(transactionCtx context.Context, tx *sql.Tx) (err error) {
				mc, err = MakeMerkleCommitter(tx, true)
				if err != nil {
					return
				}
				trie.SetCommitter(mc)
				for _, accountHash := range hashesToWrite {
					var added bool
					added, err = trie.Add(accountHash)
					if !added {
						return fmt.Errorf("CatchpointCatchupAccessorImpl::BuildMerkleTrie: The provided catchpoint file contained the same account more than once. hash '%s'", hex.EncodeToString(accountHash))
					}
					if err != nil {
						return
					}
				}
				uncommitedHashesCount += len(hashesToWrite)
				hashesWritten += uint64(len(hashesToWrite))
				return nil
			})
			if err != nil {
				break
			}

			if uncommitedHashesCount >= trieRebuildCommitFrequency {
				err = wdb.Atomic(func(transactionCtx context.Context, tx *sql.Tx) (err error) {
					// set a long 30-second window for the evict before warning is generated.
					db.ResetTransactionWarnDeadline(transactionCtx, tx, time.Now().Add(30*time.Second))
					mc, err = MakeMerkleCommitter(tx, true)
					if err != nil {
						return
					}
					trie.SetCommitter(mc)
					_, err = trie.Evict(true)
					if err != nil {
						return
					}
					uncommitedHashesCount = 0
					return nil
				})
				if err != nil {
					keepWriting = false
					continue
				}
			}
			if progressUpdates != nil {
				progressUpdates(hashesWritten)
			}
		}
		if err != nil {
			errChan <- err
			return
		}
		if uncommitedHashesCount > 0 {
			err = wdb.Atomic(func(transactionCtx context.Context, tx *sql.Tx) (err error) {
				// set a long 30-second window for the evict before warning is generated.
				db.ResetTransactionWarnDeadline(transactionCtx, tx, time.Now().Add(30*time.Second))
				mc, err = MakeMerkleCommitter(tx, true)
				if err != nil {
					return
				}
				trie.SetCommitter(mc)
				_, err = trie.Evict(true)
				return
			})
		}

		if err != nil {
			errChan <- err
		}
	}()

	wg.Wait()

	select {
	case err := <-errChan:
		return err
	default:
	}

	return err
}

// GetCatchupBlockRound returns the latest block round matching the current catchpoint
func (c *catchpointCatchupAccessorImpl) GetCatchupBlockRound(ctx context.Context) (round basics.Round, err error) {
	var iRound uint64
	iRound, err = readCatchpointStateUint64(ctx, c.ledger.trackerDB().Rdb.Handle, catchpointStateCatchupBlockRound)
	if err != nil {
		return 0, fmt.Errorf("unable to read catchpoint catchup state '%s': %v", catchpointStateCatchupBlockRound, err)
	}
	return basics.Round(iRound), nil
}

// VerifyCatchpoint verifies that the catchpoint is valid by reconstructing the label.
func (c *catchpointCatchupAccessorImpl) VerifyCatchpoint(ctx context.Context, blk *bookkeeping.Block) (err error) {
	rdb := c.ledger.trackerDB().Rdb
	var balancesHash crypto.Digest
	var rawStateProofVerificationData []ledgercore.StateProofVerificationData
	var blockRound basics.Round
	var totals ledgercore.AccountTotals
	var catchpointLabel string
	var version uint64

	catchpointLabel, err = readCatchpointStateString(ctx, rdb.Handle, catchpointStateCatchupLabel)
	if err != nil {
		return fmt.Errorf("unable to read catchpoint catchup state '%s': %v", catchpointStateCatchupLabel, err)
	}

	version, err = readCatchpointStateUint64(ctx, rdb.Handle, catchpointStateCatchupVersion)
	if err != nil {
		return fmt.Errorf("unable to retrieve catchpoint version: %v", err)
	}

	var iRound uint64
	iRound, err = readCatchpointStateUint64(ctx, rdb.Handle, catchpointStateCatchupBlockRound)
	if err != nil {
		return fmt.Errorf("unable to read catchpoint catchup state '%s': %v", catchpointStateCatchupBlockRound, err)
	}
	blockRound = basics.Round(iRound)

	start := time.Now()
	ledgerVerifycatchpointCount.Inc(nil)

	err = rdb.Atomic(func(ctx context.Context, tx *sql.Tx) (err error) {
		// create the merkle trie for the balances
		mc, err0 := MakeMerkleCommitter(tx, true)
		if err0 != nil {
			return fmt.Errorf("unable to make MerkleCommitter: %v", err0)
		}
		var trie *merkletrie.Trie
		trie, err = merkletrie.MakeTrie(mc, TrieMemoryConfig)
		if err != nil {
			return fmt.Errorf("unable to make trie: %v", err)
		}

		balancesHash, err = trie.RootHash()
		if err != nil {
			return fmt.Errorf("unable to get trie root hash: %v", err)
		}

		totals, err = accountsTotals(ctx, tx, true)
		if err != nil {
			return fmt.Errorf("unable to get accounts totals: %v", err)
		}

		rawStateProofVerificationData, err = CatchpointStateProofVerification(ctx, tx)
		if err != nil {
			return fmt.Errorf("unable to get state proof verification data: %v", err)
		}

		return
	})
	ledgerVerifycatchpointMicros.AddMicrosecondsSince(start, nil)
	if err != nil {
		return err
	}
	if blockRound != blk.Round() {
		return fmt.Errorf("block round in block header doesn't match block round in catchpoint")
	}

	wrappedData := catchpointStateProofVerificationData{Data: rawStateProofVerificationData}
	stateProofVerificationDataHash := crypto.HashObj(wrappedData)

	var catchpointLabelMaker ledgercore.CatchpointLabelMaker
	if version <= CatchpointFileVersionV6 {
		catchpointLabelMaker = ledgercore.MakeCatchpointLabelMakerV6(blockRound, blk.Digest(), balancesHash, totals)
	} else {
		catchpointLabelMaker = ledgercore.MakeCatchpointLabelMakerCurrent(blockRound, blk.Digest(), balancesHash, totals, stateProofVerificationDataHash)
	}
	generatedLabel := ledgercore.MakeLabel(catchpointLabelMaker)

	if catchpointLabel != generatedLabel {
		return fmt.Errorf("catchpoint hash mismatch; expected %s, calculated %s", catchpointLabel, generatedLabel)
	}
	return nil
}

// StoreBalancesRound calculates the balances round based on the first block and the associated consensus parameters, and
// store that to the database
func (c *catchpointCatchupAccessorImpl) StoreBalancesRound(ctx context.Context, blk *bookkeeping.Block) (err error) {
	// calculate the balances round and store it. It *should* be identical to the one in the catchpoint file header, but we don't want to
	// trust the one in the catchpoint file header, so we'll calculate it ourselves.
	catchpointLookback := config.Consensus[blk.CurrentProtocol].CatchpointLookback
	if catchpointLookback == 0 {
		catchpointLookback = config.Consensus[blk.CurrentProtocol].MaxBalLookback
	}
	balancesRound := blk.Round() - basics.Round(catchpointLookback)
	wdb := c.ledger.trackerDB().Wdb
	start := time.Now()
	ledgerStorebalancesroundCount.Inc(nil)
	err = wdb.Atomic(func(ctx context.Context, tx *sql.Tx) (err error) {
		err = writeCatchpointStateUint64(ctx, tx, catchpointStateCatchupBalancesRound, uint64(balancesRound))
		if err != nil {
			return fmt.Errorf("CatchpointCatchupAccessorImpl::StoreBalancesRound: unable to write catchpoint catchup state '%s': %v", catchpointStateCatchupBalancesRound, err)
		}
		return
	})
	ledgerStorebalancesroundMicros.AddMicrosecondsSince(start, nil)
	return
}

// StoreFirstBlock stores a single block to the blocks database.
func (c *catchpointCatchupAccessorImpl) StoreFirstBlock(ctx context.Context, blk *bookkeeping.Block) (err error) {
	blockDbs := c.ledger.blockDB()
	start := time.Now()
	ledgerStorefirstblockCount.Inc(nil)
	err = blockDbs.Wdb.Atomic(func(ctx context.Context, tx *sql.Tx) (err error) {
		return blockStartCatchupStaging(tx, *blk)
	})
	ledgerStorefirstblockMicros.AddMicrosecondsSince(start, nil)
	if err != nil {
		return err
	}
	return nil
}

// StoreBlock stores a single block to the blocks database.
func (c *catchpointCatchupAccessorImpl) StoreBlock(ctx context.Context, blk *bookkeeping.Block) (err error) {
	blockDbs := c.ledger.blockDB()
	start := time.Now()
	ledgerCatchpointStoreblockCount.Inc(nil)
	err = blockDbs.Wdb.Atomic(func(ctx context.Context, tx *sql.Tx) (err error) {
		return blockPutStaging(tx, *blk)
	})
	ledgerCatchpointStoreblockMicros.AddMicrosecondsSince(start, nil)
	if err != nil {
		return err
	}
	return nil
}

// FinishBlocks concludes the catchup of the blocks database.
func (c *catchpointCatchupAccessorImpl) FinishBlocks(ctx context.Context, applyChanges bool) (err error) {
	blockDbs := c.ledger.blockDB()
	start := time.Now()
	ledgerCatchpointFinishblocksCount.Inc(nil)
	err = blockDbs.Wdb.Atomic(func(ctx context.Context, tx *sql.Tx) (err error) {
		if applyChanges {
			return blockCompleteCatchup(tx)
		}
		// TODO: unused, either actually implement cleanup on catchpoint failure, or delete this
		return blockAbortCatchup(tx)
	})
	ledgerCatchpointFinishblocksMicros.AddMicrosecondsSince(start, nil)
	if err != nil {
		return err
	}
	return nil
}

// EnsureFirstBlock ensure that we have a single block in the staging block table, and returns that block
func (c *catchpointCatchupAccessorImpl) EnsureFirstBlock(ctx context.Context) (blk bookkeeping.Block, err error) {
	blockDbs := c.ledger.blockDB()
	start := time.Now()
	ledgerCatchpointEnsureblock1Count.Inc(nil)
	err = blockDbs.Wdb.Atomic(func(ctx context.Context, tx *sql.Tx) (err error) {
		blk, err = blockEnsureSingleBlock(tx)
		return
	})
	ledgerCatchpointEnsureblock1Micros.AddMicrosecondsSince(start, nil)
	if err != nil {
		return blk, err
	}
	return blk, nil
}

// CompleteCatchup completes the catchpoint catchup process by switching the databases tables around
// and reloading the ledger.
func (c *catchpointCatchupAccessorImpl) CompleteCatchup(ctx context.Context) (err error) {
	err = c.FinishBlocks(ctx, true)
	if err != nil {
		return err
	}
	err = c.finishBalances(ctx)
	if err != nil {
		return err
	}

	return c.ledger.reloadLedger()
}

// finishBalances concludes the catchup of the balances(tracker) database.
func (c *catchpointCatchupAccessorImpl) finishBalances(ctx context.Context) (err error) {
	wdb := c.ledger.trackerDB().Wdb
	start := time.Now()
	ledgerCatchpointFinishBalsCount.Inc(nil)
	err = wdb.Atomic(func(ctx context.Context, tx *sql.Tx) (err error) {
		var balancesRound, hashRound uint64
		var totals ledgercore.AccountTotals

		balancesRound, err = readCatchpointStateUint64(ctx, tx, catchpointStateCatchupBalancesRound)
		if err != nil {
			return err
		}

		hashRound, err = readCatchpointStateUint64(ctx, tx, catchpointStateCatchupHashRound)
		if err != nil {
			return err
		}

		totals, err = accountsTotals(ctx, tx, true)
		if err != nil {
			return err
		}

		if hashRound == 0 {
			err = resetAccountHashes(ctx, tx)
			if err != nil {
				return err
			}
		}

		// Reset the database to version 6. For now, we create a version 6 database from
		// the catchpoint and let `reloadLedger()` run the normal database migration.
		// When implementing a new catchpoint format (e.g. adding a new table),
		// it might be necessary to restore it into the latest database version. To do that, one
		// will need to run the 6->7 migration code manually here or in a similar function to create
		// onlineaccounts and other tables.
		err = accountsReset(ctx, tx)
		if err != nil {
			return err
		}
		{
			tp := trackerDBParams{
				initAccounts:      c.ledger.GenesisAccounts(),
				initProto:         c.ledger.GenesisProtoVersion(),
				catchpointEnabled: c.ledger.catchpoint.catchpointEnabled(),
				dbPathPrefix:      c.ledger.catchpoint.dbDirectory,
				blockDb:           c.ledger.blockDBs,
			}
			_, err = runMigrations(ctx, tx, tp, c.ledger.log, 6 /*target database version*/)
			if err != nil {
				return err
			}
		}

		err = applyCatchpointStagingBalances(ctx, tx, basics.Round(balancesRound), basics.Round(hashRound))
		if err != nil {
			return err
		}

		err = accountsPutTotals(tx, totals, false)
		if err != nil {
			return err
		}

		err = resetCatchpointStagingBalances(ctx, tx, false)
		if err != nil {
			return err
		}

		err = writeCatchpointStateUint64(ctx, tx, catchpointStateCatchupBalancesRound, 0)
		if err != nil {
			return err
		}

		err = writeCatchpointStateUint64(ctx, tx, catchpointStateCatchupBlockRound, 0)
		if err != nil {
			return err
		}

		err = writeCatchpointStateString(ctx, tx, catchpointStateCatchupLabel, "")
		if err != nil {
			return err
		}

		if hashRound != 0 {
			err = writeCatchpointStateUint64(ctx, tx, catchpointStateCatchupHashRound, 0)
			if err != nil {
				return err
			}
		}

		err = writeCatchpointStateUint64(ctx, tx, catchpointStateCatchupState, 0)
		if err != nil {
			return fmt.Errorf("unable to write catchpoint catchup state '%s': %v", catchpointStateCatchupState, err)
		}

		return
	})
	ledgerCatchpointFinishBalsMicros.AddMicrosecondsSince(start, nil)
	return err
}

// Ledger returns ledger instance as CatchupAccessorClientLedger interface
func (c *catchpointCatchupAccessorImpl) Ledger() (l CatchupAccessorClientLedger) {
	return c.ledger
}

var ledgerResetstagingbalancesCount = metrics.NewCounter("ledger_catchup_resetstagingbalances_count", "calls")
var ledgerResetstagingbalancesMicros = metrics.NewCounter("ledger_catchup_resetstagingbalances_micros", "µs spent")
var ledgerProcessstagingcontentCount = metrics.NewCounter("ledger_catchup_processstagingcontent_count", "calls")
var ledgerProcessstagingcontentMicros = metrics.NewCounter("ledger_catchup_processstagingcontent_micros", "µs spent")
var ledgerProcessstagingbalancesCount = metrics.NewCounter("ledger_catchup_processstagingbalances_count", "calls")
var ledgerProcessstagingbalancesMicros = metrics.NewCounter("ledger_catchup_processstagingbalances_micros", "µs spent")
var ledgerVerifycatchpointCount = metrics.NewCounter("ledger_catchup_verifycatchpoint_count", "calls")
var ledgerVerifycatchpointMicros = metrics.NewCounter("ledger_catchup_verifycatchpoint_micros", "µs spent")
var ledgerStorebalancesroundCount = metrics.NewCounter("ledger_catchup_storebalancesround_count", "calls")
var ledgerStorebalancesroundMicros = metrics.NewCounter("ledger_catchup_storebalancesround_micros", "µs spent")
var ledgerStorefirstblockCount = metrics.NewCounter("ledger_catchup_storefirstblock_count", "calls")
var ledgerStorefirstblockMicros = metrics.NewCounter("ledger_catchup_storefirstblock_micros", "µs spent")
var ledgerCatchpointStoreblockCount = metrics.NewCounter("ledger_catchup_catchpoint_storeblock_count", "calls")
var ledgerCatchpointStoreblockMicros = metrics.NewCounter("ledger_catchup_catchpoint_storeblock_micros", "µs spent")
var ledgerCatchpointFinishblocksCount = metrics.NewCounter("ledger_catchup_catchpoint_finishblocks_count", "calls")
var ledgerCatchpointFinishblocksMicros = metrics.NewCounter("ledger_catchup_catchpoint_finishblocks_micros", "µs spent")
var ledgerCatchpointEnsureblock1Count = metrics.NewCounter("ledger_catchup_catchpoint_ensureblock1_count", "calls")
var ledgerCatchpointEnsureblock1Micros = metrics.NewCounter("ledger_catchup_catchpoint_ensureblock1_micros", "µs spent")
var ledgerCatchpointFinishBalsCount = metrics.NewCounter("ledger_catchup_catchpoint_finish_bals_count", "calls")
var ledgerCatchpointFinishBalsMicros = metrics.NewCounter("ledger_catchup_catchpoint_finish_bals_micros", "µs spent")
