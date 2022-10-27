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
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"database/sql"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/algorand/go-algorand/config"
	"github.com/algorand/go-algorand/crypto"
	"github.com/algorand/go-algorand/crypto/merklesignature"
	"github.com/algorand/go-algorand/data/basics"
	"github.com/algorand/go-algorand/ledger/ledgercore"
	ledgertesting "github.com/algorand/go-algorand/ledger/testing"
	"github.com/algorand/go-algorand/protocol"
	"github.com/algorand/go-algorand/test/partitiontest"
)

func makeString(len int) string {
	s := ""
	for i := 0; i < len; i++ {
		s += string(byte(i))
	}
	return s
}

type decodedCatchpointChunkData struct {
	headerName string
	data       []byte
}

func makeTestEncodedBalanceRecordV5(t *testing.T) encodedBalanceRecordV5 {
	er := encodedBalanceRecordV5{}
	hash := crypto.Hash([]byte{1, 2, 3})
	copy(er.Address[:], hash[:])
	oneTimeSecrets := crypto.GenerateOneTimeSignatureSecrets(0, 1)
	vrfSecrets := crypto.GenerateVRFSecrets()
	var stateProofID merklesignature.Verifier
	crypto.RandBytes(stateProofID.Commitment[:])

	ad := basics.AccountData{
		Status:             basics.NotParticipating,
		MicroAlgos:         basics.MicroAlgos{},
		RewardsBase:        0x1234123412341234,
		RewardedMicroAlgos: basics.MicroAlgos{},
		VoteID:             oneTimeSecrets.OneTimeSignatureVerifier,
		SelectionID:        vrfSecrets.PK,
		StateProofID:       stateProofID.Commitment,
		VoteFirstValid:     basics.Round(0x1234123412341234),
		VoteLastValid:      basics.Round(0x1234123412341234),
		VoteKeyDilution:    0x1234123412341234,
		AssetParams:        make(map[basics.AssetIndex]basics.AssetParams),
		Assets:             make(map[basics.AssetIndex]basics.AssetHolding),
		AuthAddr:           basics.Address(crypto.Hash([]byte{1, 2, 3, 4})),
	}
	currentConsensusParams := config.Consensus[protocol.ConsensusCurrentVersion]
	maxAssetsPerAccount := currentConsensusParams.MaxAssetsPerAccount
	// if the number of supported assets is unlimited, create only 1000 for the purpose of this unit test.
	if maxAssetsPerAccount == 0 {
		maxAssetsPerAccount = config.Consensus[protocol.ConsensusV30].MaxAssetsPerAccount
	}
	for assetCreatorAssets := 0; assetCreatorAssets < maxAssetsPerAccount; assetCreatorAssets++ {
		ap := basics.AssetParams{
			Total:         0x1234123412341234,
			Decimals:      0x12341234,
			DefaultFrozen: true,
			UnitName:      makeString(currentConsensusParams.MaxAssetUnitNameBytes),
			AssetName:     makeString(currentConsensusParams.MaxAssetNameBytes),
			URL:           makeString(currentConsensusParams.MaxAssetURLBytes),
			Manager:       basics.Address(crypto.Hash([]byte{1, byte(assetCreatorAssets)})),
			Reserve:       basics.Address(crypto.Hash([]byte{2, byte(assetCreatorAssets)})),
			Freeze:        basics.Address(crypto.Hash([]byte{3, byte(assetCreatorAssets)})),
			Clawback:      basics.Address(crypto.Hash([]byte{4, byte(assetCreatorAssets)})),
		}
		copy(ap.MetadataHash[:], makeString(32))
		ad.AssetParams[basics.AssetIndex(0x1234123412341234-assetCreatorAssets)] = ap
	}

	for assetHolderAssets := 0; assetHolderAssets < maxAssetsPerAccount; assetHolderAssets++ {
		ah := basics.AssetHolding{
			Amount: 0x1234123412341234,
			Frozen: true,
		}
		ad.Assets[basics.AssetIndex(0x1234123412341234-assetHolderAssets)] = ah
	}

	maxApps := currentConsensusParams.MaxAppsCreated
	maxOptIns := currentConsensusParams.MaxAppsOptedIn
	if maxApps == 0 {
		maxApps = config.Consensus[protocol.ConsensusV30].MaxAppsCreated
	}
	if maxOptIns == 0 {
		maxOptIns = config.Consensus[protocol.ConsensusV30].MaxAppsOptedIn
	}
	maxKeyBytesLen := currentConsensusParams.MaxAppKeyLen
	maxSumBytesLen := currentConsensusParams.MaxAppSumKeyValueLens

	genKey := func() (string, basics.TealValue) {
		len := int(crypto.RandUint64() % uint64(maxKeyBytesLen))
		if len == 0 {
			return "k", basics.TealValue{Type: basics.TealUintType, Uint: 0}
		}
		key := make([]byte, maxSumBytesLen-len)
		crypto.RandBytes(key)
		return string(key), basics.TealValue{Type: basics.TealUintType, Bytes: string(key)}
	}
	startIndex := crypto.RandUint64() % 100000
	ad.AppParams = make(map[basics.AppIndex]basics.AppParams, maxApps)
	for aidx := startIndex; aidx < startIndex+uint64(maxApps); aidx++ {
		ap := basics.AppParams{}
		ap.GlobalState = make(basics.TealKeyValue)
		for i := uint64(0); i < currentConsensusParams.MaxGlobalSchemaEntries/4; i++ {
			k, v := genKey()
			ap.GlobalState[k] = v
		}
		ad.AppParams[basics.AppIndex(aidx)] = ap
		optins := maxApps
		if maxApps > maxOptIns {
			optins = maxOptIns
		}
		ad.AppLocalStates = make(map[basics.AppIndex]basics.AppLocalState, optins)
		keys := currentConsensusParams.MaxLocalSchemaEntries / 4
		lkv := make(basics.TealKeyValue, keys)
		for i := 0; i < optins; i++ {
			for j := uint64(0); j < keys; j++ {
				k, v := genKey()
				lkv[k] = v
			}
		}
		ad.AppLocalStates[basics.AppIndex(aidx)] = basics.AppLocalState{KeyValue: lkv}
	}

	encodedAd := ad.MarshalMsg(nil)
	er.AccountData = encodedAd
	return er
}

func readCatchpointContent(t *testing.T, tarReader *tar.Reader) []decodedCatchpointChunkData {
	result := make([]decodedCatchpointChunkData, 0)
	for {
		header, err := tarReader.Next()
		if err != nil {
			if err == io.EOF {
				break
			}
			require.NoError(t, err)
			break
		}
		data := make([]byte, header.Size)
		readComplete := int64(0)

		for readComplete < header.Size {
			bytesRead, err := tarReader.Read(data[readComplete:])
			readComplete += int64(bytesRead)
			if err != nil {
				if err == io.EOF {
					if readComplete == header.Size {
						break
					}
					require.NoError(t, err)
				}
				break
			}
		}

		result = append(result, decodedCatchpointChunkData{headerName: header.Name, data: data})
	}

	return result
}

func readCatchpointDataFile(t *testing.T, catchpointDataPath string) []decodedCatchpointChunkData {
	fileContent, err := os.ReadFile(catchpointDataPath)
	require.NoError(t, err)

	compressorReader, err := catchpointStage1Decoder(bytes.NewBuffer(fileContent))
	require.NoError(t, err)

	tarReader := tar.NewReader(compressorReader)
	return readCatchpointContent(t, tarReader)
}

func readCatchpointFile(t *testing.T, catchpointPath string) []decodedCatchpointChunkData {
	fileContent, err := os.ReadFile(catchpointPath)
	require.NoError(t, err)

	gzipReader, err := gzip.NewReader(bytes.NewBuffer(fileContent))
	require.NoError(t, err)
	defer gzipReader.Close()

	tarReader := tar.NewReader(gzipReader)
	return readCatchpointContent(t, tarReader)
}

func verifyStateProofVerificationDataWrite(t *testing.T, data []ledgercore.StateProofVerificationData) {
	// create new protocol version, which has lower lookback
	testProtocolVersion := protocol.ConsensusVersion("test-protocol-TestBasicCatchpointWriter")
	protoParams := config.Consensus[protocol.ConsensusCurrentVersion]
	protoParams.CatchpointLookback = 32
	config.Consensus[testProtocolVersion] = protoParams
	temporaryDirectory := t.TempDir()
	defer func() {
		delete(config.Consensus, testProtocolVersion)
	}()
	accts := ledgertesting.RandomAccounts(300, false)

	ml := makeMockLedgerForTracker(t, true, 10, testProtocolVersion, []map[basics.Address]basics.AccountData{accts})
	defer ml.Close()

	conf := config.GetDefaultLocal()
	conf.CatchpointInterval = 1
	conf.Archival = true
	au, _ := newAcctUpdates(t, ml, conf)
	err := au.loadFromDisk(ml, 0)
	require.NoError(t, err)
	au.close()
	fileName := filepath.Join(temporaryDirectory, "15.data")

	mockCommitData := make([]verificationCommitData, 0)
	for _, element := range data {
		mockCommitData = append(mockCommitData, verificationCommitData{verificationData: element})
	}

	err = ml.dbs.Wdb.Atomic(func(ctx context.Context, tx *sql.Tx) error {
		return insertStateProofVerificationData(ctx, tx, mockCommitData)
	})

	require.NoError(t, err)

	readDb := ml.trackerDB().Rdb
	err = readDb.Atomic(func(ctx context.Context, tx *sql.Tx) (err error) {
		writer, err := makeCatchpointWriter(context.Background(), fileName, tx, DefaultMaxResourcesPerChunk)
		if err != nil {
			return err
		}
		_, err = writer.WriteStateProofVerificationData()
		if err != nil {
			return err
		}
		for {
			more, err := writer.WriteStep(context.Background())
			require.NoError(t, err)
			if !more {
				break
			}
		}
		return
	})

	catchpointData := readCatchpointDataFile(t, fileName)
	require.Equal(t, "stateProofVerificationData.msgpack", catchpointData[0].headerName)
	var wrappedData catchpointStateProofVerificationData
	err = protocol.Decode(catchpointData[0].data, &wrappedData)
	require.NoError(t, err)

	for index, verificationData := range wrappedData.Data {
		require.Equal(t, data[index], verificationData)
	}
}

func TestEncodedBalanceRecordEncoding(t *testing.T) {
	partitiontest.PartitionTest(t)

	er := makeTestEncodedBalanceRecordV5(t)
	encodedBr := er.MarshalMsg(nil)

	var er2 encodedBalanceRecordV5
	_, err := er2.UnmarshalMsg(encodedBr)
	require.NoError(t, err)

	require.Equal(t, er, er2)
}

func TestCatchpointFileBalancesChunkEncoding(t *testing.T) {
	partitiontest.PartitionTest(t)

	// The next operations are heavy on the memory.
	// Garbage collection helps prevent trashing
	runtime.GC()

	fbc := catchpointFileBalancesChunkV5{}
	for i := 0; i < 512; i++ {
		fbc.Balances = append(fbc.Balances, makeTestEncodedBalanceRecordV5(t))
	}
	encodedFbc := fbc.MarshalMsg(nil)

	var fbc2 catchpointFileBalancesChunkV5
	_, err := fbc2.UnmarshalMsg(encodedFbc)
	require.NoError(t, err)

	require.Equal(t, fbc, fbc2)
	// Garbage collection helps prevent trashing
	// for next tests
	runtime.GC()
}

func TestBasicCatchpointWriter(t *testing.T) {
	partitiontest.PartitionTest(t)

	// create new protocol version, which has lower lookback
	testProtocolVersion := protocol.ConsensusVersion("test-protocol-TestBasicCatchpointWriter")
	protoParams := config.Consensus[protocol.ConsensusCurrentVersion]
	protoParams.CatchpointLookback = 32
	config.Consensus[testProtocolVersion] = protoParams
	temporaryDirectory := t.TempDir()
	defer func() {
		delete(config.Consensus, testProtocolVersion)
	}()
	accts := ledgertesting.RandomAccounts(300, false)

	ml := makeMockLedgerForTracker(t, true, 10, testProtocolVersion, []map[basics.Address]basics.AccountData{accts})
	defer ml.Close()

	conf := config.GetDefaultLocal()
	conf.CatchpointInterval = 1
	conf.Archival = true
	au, _ := newAcctUpdates(t, ml, conf)
	err := au.loadFromDisk(ml, 0)
	require.NoError(t, err)
	au.close()
	fileName := filepath.Join(temporaryDirectory, "15.data")

	readDb := ml.trackerDB().Rdb
	err = readDb.Atomic(func(ctx context.Context, tx *sql.Tx) (err error) {
		writer, err := makeCatchpointWriter(context.Background(), fileName, tx, DefaultMaxResourcesPerChunk)
		if err != nil {
			return err
		}
		_, err = writer.WriteStateProofVerificationData()
		if err != nil {
			return err
		}
		for {
			more, err := writer.WriteStep(context.Background())
			require.NoError(t, err)
			if !more {
				break
			}
		}
		return
	})

	catchpointContent := readCatchpointDataFile(t, fileName)
	require.Equal(t, "balances.1.1.msgpack", catchpointContent[1].headerName)

	var balances catchpointFileBalancesChunkV6
	err = protocol.Decode(catchpointContent[1].data, &balances)
	require.NoError(t, err)
	require.Equal(t, uint64(len(accts)), uint64(len(balances.Balances)))
}

func TestFullCatchpointWriter(t *testing.T) {
	partitiontest.PartitionTest(t)

	// create new protocol version, which has lower lookback
	testProtocolVersion := protocol.ConsensusVersion("test-protocol-TestFullCatchpointWriter")
	protoParams := config.Consensus[protocol.ConsensusCurrentVersion]
	protoParams.CatchpointLookback = 32
	config.Consensus[testProtocolVersion] = protoParams
	temporaryDirectory := t.TempDir()
	defer func() {
		delete(config.Consensus, testProtocolVersion)
	}()

	accts := ledgertesting.RandomAccounts(BalancesPerCatchpointFileChunk*3, false)
	ml := makeMockLedgerForTracker(t, true, 10, testProtocolVersion, []map[basics.Address]basics.AccountData{accts})
	defer ml.Close()

	conf := config.GetDefaultLocal()
	conf.CatchpointInterval = 1
	conf.Archival = true
	au, _ := newAcctUpdates(t, ml, conf)
	err := au.loadFromDisk(ml, 0)
	require.NoError(t, err)
	au.close()
	catchpointDataFilePath := filepath.Join(temporaryDirectory, "15.data")
	catchpointFilePath := filepath.Join(temporaryDirectory, "15.catchpoint")
	readDb := ml.trackerDB().Rdb
	var totalAccounts uint64
	var totalChunks uint64
	var biggestChunkLen uint64
	var accountsRnd basics.Round
	var totals ledgercore.AccountTotals
	err = readDb.Atomic(func(ctx context.Context, tx *sql.Tx) (err error) {
		writer, err := makeCatchpointWriter(context.Background(), catchpointDataFilePath, tx, DefaultMaxResourcesPerChunk)
		if err != nil {
			return err
		}
		_, err = writer.WriteStateProofVerificationData()
		if err != nil {
			return err
		}
		for {
			more, err := writer.WriteStep(context.Background())
			require.NoError(t, err)
			if !more {
				break
			}
		}
		totalAccounts = writer.GetTotalAccounts()
		totalChunks = writer.GetTotalChunks()
		biggestChunkLen = writer.GetBiggestChunkLen()
		accountsRnd, err = accountsRound(tx)
		if err != nil {
			return
		}
		totals, err = accountsTotals(ctx, tx, false)
		return
	})
	require.NoError(t, err)
	blocksRound := accountsRnd + 1
	blockHeaderDigest := crypto.Hash([]byte{1, 2, 3})
	catchpointLabel := fmt.Sprintf("%d#%v", blocksRound, blockHeaderDigest) // this is not a correct way to create a label, but it's good enough for this unit test
	catchpointFileHeader := CatchpointFileHeader{
		Version:           CatchpointFileVersionV6,
		BalancesRound:     accountsRnd,
		BlocksRound:       blocksRound,
		Totals:            totals,
		TotalAccounts:     totalAccounts,
		TotalChunks:       totalChunks,
		Catchpoint:        catchpointLabel,
		BlockHeaderDigest: blockHeaderDigest,
	}
	err = repackCatchpoint(
		context.Background(), catchpointFileHeader, biggestChunkLen,
		catchpointDataFilePath, catchpointFilePath)
	require.NoError(t, err)

	// create a ledger.
	var initState ledgercore.InitState
	initState.Block.CurrentProtocol = protocol.ConsensusCurrentVersion
	l, err := OpenLedger(ml.log, "TestFullCatchpointWriter", true, initState, conf)
	require.NoError(t, err)
	defer l.Close()
	accessor := MakeCatchpointCatchupAccessor(l, l.log)

	err = accessor.ResetStagingBalances(context.Background(), true)
	require.NoError(t, err)

	var catchupProgress CatchpointCatchupAccessorProgress
	catchpointContent := readCatchpointFile(t, catchpointFilePath)
	for _, catchpointData := range catchpointContent {
		err = accessor.ProgressStagingBalances(context.Background(), catchpointData.headerName, catchpointData.data, &catchupProgress)
		require.NoError(t, err)
	}

	err = l.trackerDBs.Wdb.Atomic(func(ctx context.Context, tx *sql.Tx) error {
		err := applyCatchpointStagingBalances(ctx, tx, 0, 0)
		return err
	})
	require.NoError(t, err)

	// verify that the account data aligns with what we originally stored :
	for addr, acct := range accts {
		acctData, validThrough, _, err := l.LookupLatest(addr)
		require.NoErrorf(t, err, "failed to lookup for account %v after restoring from catchpoint", addr)
		require.Equal(t, acct, acctData)
		require.Equal(t, basics.Round(0), validThrough)
	}
}

func TestStateProofVerificationDataWrite(t *testing.T) {
	partitiontest.PartitionTest(t)

	verificationData := ledgercore.StateProofVerificationData{
		TargetStateProofRound: 120,
		VotersCommitment:      nil,
		ProvenWeight:          basics.MicroAlgos{Raw: 100},
	}

	verifyStateProofVerificationDataWrite(t, []ledgercore.StateProofVerificationData{verificationData})
}

func TestEmptyStateProofVerificationDataWrite(t *testing.T) {
	partitiontest.PartitionTest(t)

	verifyStateProofVerificationDataWrite(t, []ledgercore.StateProofVerificationData{})
}

func TestCatchpointReadDatabaseOverflowSingleAccount(t *testing.T) {
	partitiontest.PartitionTest(t)

	// create new protocol version, which has lower lookback
	testProtocolVersion := protocol.ConsensusVersion("test-protocol-TestFullCatchpointWriter")
	protoParams := config.Consensus[protocol.ConsensusCurrentVersion]
	protoParams.CatchpointLookback = 32
	config.Consensus[testProtocolVersion] = protoParams
	temporaryDirectory := t.TempDir()
	defer func() {
		delete(config.Consensus, testProtocolVersion)
	}()

	maxResourcesPerChunk := 5

	accts := ledgertesting.RandomAccounts(1, false)
	// force acct to have overflowing number of resources
	for addr, acct := range accts {
		if acct.AssetParams == nil {
			acct.AssetParams = make(map[basics.AssetIndex]basics.AssetParams, 0)
			accts[addr] = acct
		}
		for i := uint64(0); i < 20; i++ {
			ap := ledgertesting.RandomAssetParams()
			acct.AssetParams[basics.AssetIndex(i+100)] = ap
		}
	}

	ml := makeMockLedgerForTracker(t, true, 10, testProtocolVersion, []map[basics.Address]basics.AccountData{accts})
	defer ml.Close()

	conf := config.GetDefaultLocal()
	conf.CatchpointInterval = 1
	conf.Archival = true
	au, _ := newAcctUpdates(t, ml, conf)
	err := au.loadFromDisk(ml, 0)
	require.NoError(t, err)
	au.close()
	catchpointDataFilePath := filepath.Join(temporaryDirectory, "15.data")
	readDb := ml.trackerDB().Rdb

	err = readDb.Atomic(func(ctx context.Context, tx *sql.Tx) (err error) {
		expectedTotalAccounts := uint64(1)
		totalAccountsWritten := uint64(0)
		totalResources := 0
		totalChunks := 0
		var expectedTotalResources int
		cw, err := makeCatchpointWriter(context.Background(), catchpointDataFilePath, tx, maxResourcesPerChunk)
		err = cw.tx.QueryRowContext(cw.ctx, "SELECT count(1) FROM resources").Scan(&expectedTotalResources)
		if err != nil {
			return err
		}
		// repeat this until read all accts
		for totalAccountsWritten < expectedTotalAccounts {
			cw.balancesChunk.Balances = nil
			err := cw.readDatabaseStep(cw.ctx, cw.tx)
			if err != nil {
				return err
			}
			totalAccountsWritten += cw.balancesChunk.numAccounts
			numResources := 0
			for _, balance := range cw.balancesChunk.Balances {
				numResources += len(balance.Resources)
			}
			if numResources > maxResourcesPerChunk {
				return fmt.Errorf("too many resources in this chunk: found %d resources, maximum %d resources", numResources, maxResourcesPerChunk)
			}
			totalResources += numResources
			totalChunks++
		}

		if totalChunks <= 1 {
			return fmt.Errorf("expected more than one chunk due to overflow")
		}

		if expectedTotalResources != totalResources {
			return fmt.Errorf("total resources did not match: expected %d, actual %d", expectedTotalResources, totalResources)
		}

		return
	})

	require.NoError(t, err)
}

func TestCatchpointReadDatabaseOverflowAccounts(t *testing.T) {
	partitiontest.PartitionTest(t)

	// create new protocol version, which has lower lookback
	testProtocolVersion := protocol.ConsensusVersion("test-protocol-TestFullCatchpointWriter")
	protoParams := config.Consensus[protocol.ConsensusCurrentVersion]
	protoParams.CatchpointLookback = 32
	config.Consensus[testProtocolVersion] = protoParams
	temporaryDirectory := t.TempDir()
	defer func() {
		delete(config.Consensus, testProtocolVersion)
	}()

	maxResourcesPerChunk := 5

	accts := ledgertesting.RandomAccounts(5, false)
	// force each acct to have overflowing number of resources
	assetIndex := 1000
	for addr, acct := range accts {
		if acct.AssetParams == nil {
			acct.AssetParams = make(map[basics.AssetIndex]basics.AssetParams, 0)
			accts[addr] = acct
		}
		for i := uint64(0); i < 20; i++ {
			ap := ledgertesting.RandomAssetParams()
			acct.AssetParams[basics.AssetIndex(assetIndex)] = ap
			assetIndex++
		}
	}

	ml := makeMockLedgerForTracker(t, true, 10, testProtocolVersion, []map[basics.Address]basics.AccountData{accts})
	defer ml.Close()

	conf := config.GetDefaultLocal()
	conf.CatchpointInterval = 1
	conf.Archival = true
	au, _ := newAcctUpdates(t, ml, conf)
	err := au.loadFromDisk(ml, 0)
	require.NoError(t, err)
	au.close()
	catchpointDataFilePath := filepath.Join(temporaryDirectory, "15.data")
	readDb := ml.trackerDB().Rdb

	err = readDb.Atomic(func(ctx context.Context, tx *sql.Tx) (err error) {
		expectedTotalAccounts, err := totalAccounts(ctx, tx)
		if err != nil {
			return err
		}
		totalAccountsWritten := uint64(0)
		totalResources := 0
		var expectedTotalResources int
		cw, err := makeCatchpointWriter(context.Background(), catchpointDataFilePath, tx, maxResourcesPerChunk)
		err = cw.tx.QueryRowContext(cw.ctx, "SELECT count(1) FROM resources").Scan(&expectedTotalResources)
		if err != nil {
			return err
		}
		// repeat this until read all accts
		for totalAccountsWritten < expectedTotalAccounts {
			cw.balancesChunk.Balances = nil
			err := cw.readDatabaseStep(cw.ctx, cw.tx)
			if err != nil {
				return err
			}
			totalAccountsWritten += cw.balancesChunk.numAccounts
			numResources := 0
			for _, balance := range cw.balancesChunk.Balances {
				numResources += len(balance.Resources)
			}
			if numResources > maxResourcesPerChunk {
				return fmt.Errorf("too many resources in this chunk: found %d resources, maximum %d resources", numResources, maxResourcesPerChunk)
			}
			totalResources += numResources
		}

		if expectedTotalResources != totalResources {
			return fmt.Errorf("total resources did not match: expected %d, actual %d", expectedTotalResources, totalResources)
		}

		return
	})

	require.NoError(t, err)
}

func TestFullCatchpointWriterOverflowAccounts(t *testing.T) {
	partitiontest.PartitionTest(t)

	// create new protocol version, which has lower lookback
	testProtocolVersion := protocol.ConsensusVersion("test-protocol-TestFullCatchpointWriter")
	protoParams := config.Consensus[protocol.ConsensusCurrentVersion]
	protoParams.CatchpointLookback = 32
	config.Consensus[testProtocolVersion] = protoParams
	temporaryDirectory := t.TempDir()
	defer func() {
		delete(config.Consensus, testProtocolVersion)
	}()

	accts := ledgertesting.RandomAccounts(BalancesPerCatchpointFileChunk*3, false)
	ml := makeMockLedgerForTracker(t, true, 10, testProtocolVersion, []map[basics.Address]basics.AccountData{accts})
	defer ml.Close()

	conf := config.GetDefaultLocal()
	conf.CatchpointInterval = 1
	conf.Archival = true
	au, _ := newAcctUpdates(t, ml, conf)
	err := au.loadFromDisk(ml, 0)
	require.NoError(t, err)
	au.close()
	catchpointDataFilePath := filepath.Join(temporaryDirectory, "15.data")
	catchpointFilePath := filepath.Join(temporaryDirectory, "15.catchpoint")
	readDb := ml.trackerDB().Rdb
	var totalAccounts uint64
	var totalChunks uint64
	var biggestChunkLen uint64
	var accountsRnd basics.Round
	var totals ledgercore.AccountTotals
	err = readDb.Atomic(func(ctx context.Context, tx *sql.Tx) (err error) {
		writer, err := makeCatchpointWriter(context.Background(), catchpointDataFilePath, tx, 5)
		if err != nil {
			return err
		}
		for {
			more, err := writer.WriteStep(context.Background())
			require.NoError(t, err)
			if !more {
				break
			}
		}
		totalAccounts = writer.GetTotalAccounts()
		totalChunks = writer.GetTotalChunks()
		biggestChunkLen = writer.GetBiggestChunkLen()
		accountsRnd, err = accountsRound(tx)
		if err != nil {
			return
		}
		totals, err = accountsTotals(ctx, tx, false)
		return
	})
	require.NoError(t, err)
	blocksRound := accountsRnd + 1
	blockHeaderDigest := crypto.Hash([]byte{1, 2, 3})
	catchpointLabel := fmt.Sprintf("%d#%v", blocksRound, blockHeaderDigest) // this is not a correct way to create a label, but it's good enough for this unit test
	catchpointFileHeader := CatchpointFileHeader{
		Version:           CatchpointFileVersionV6,
		BalancesRound:     accountsRnd,
		BlocksRound:       blocksRound,
		Totals:            totals,
		TotalAccounts:     totalAccounts,
		TotalChunks:       totalChunks,
		Catchpoint:        catchpointLabel,
		BlockHeaderDigest: blockHeaderDigest,
	}
	err = repackCatchpoint(
		context.Background(), catchpointFileHeader, biggestChunkLen,
		catchpointDataFilePath, catchpointFilePath)
	require.NoError(t, err)

	// create a ledger.
	var initState ledgercore.InitState
	initState.Block.CurrentProtocol = protocol.ConsensusCurrentVersion
	l, err := OpenLedger(ml.log, "TestFullCatchpointWriter", true, initState, conf)
	require.NoError(t, err)
	defer l.Close()
	accessor := MakeCatchpointCatchupAccessor(l, l.log)

	err = accessor.ResetStagingBalances(context.Background(), true)
	require.NoError(t, err)

	var catchupProgress CatchpointCatchupAccessorProgress
	catchpointContent := readCatchpointFile(t, catchpointFilePath)
	for _, catchpointData := range catchpointContent {
		err = accessor.ProgressStagingBalances(context.Background(), catchpointData.headerName, catchpointData.data, &catchupProgress)
		require.NoError(t, err)
	}

	err = l.trackerDBs.Wdb.Atomic(func(ctx context.Context, tx *sql.Tx) error {
		err := applyCatchpointStagingBalances(ctx, tx, 0, 0)
		return err
	})
	require.NoError(t, err)

	// verify that the account data aligns with what we originally stored :
	for addr, acct := range accts {
		acctData, validThrough, _, err := l.LookupLatest(addr)
		require.NoErrorf(t, err, "failed to lookup for account %v after restoring from catchpoint", addr)
		require.Equal(t, acct, acctData)
		require.Equal(t, basics.Round(0), validThrough)
	}
}
