// Copyright (C) 2019-2023 Algorand, Inc.
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

package catchup

import (
	"fmt"
	"net/http"
	"os/exec"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/algorand/go-algorand/config"
	"github.com/algorand/go-algorand/data/basics"
	"github.com/algorand/go-algorand/ledger/ledgercore"
	"github.com/algorand/go-algorand/logging"
	"github.com/algorand/go-algorand/nodecontrol"
	"github.com/algorand/go-algorand/protocol"
	"github.com/algorand/go-algorand/test/framework/fixtures"
	"github.com/algorand/go-algorand/test/partitiontest"
	"github.com/algorand/go-deadlock"
)

const basicTestCatchpointInterval = 4

func denyRoundRequestsWebProxy(a *require.Assertions, listeningAddress string, round basics.Round) *fixtures.WebProxy {
	log := logging.NewLogger()
	log.SetLevel(logging.Info)

	wp, err := fixtures.MakeWebProxy(listeningAddress, log, func(response http.ResponseWriter, request *http.Request, next http.HandlerFunc) {
		// prevent requests for the given block to go through.
		if request.URL.String() == fmt.Sprintf("/v1/test-v1/block/%d", round) {
			response.WriteHeader(http.StatusBadRequest)
			response.Write([]byte(fmt.Sprintf("webProxy prevents block %d from serving", round)))
			return
		}
		next(response, request)
	})
	a.NoError(err)
	log.Infof("web proxy listens at %s\n", wp.GetListenAddress())
	return wp
}

func getFirstCatchpointRound(consensusParams *config.ConsensusParams) basics.Round {
	// fast catchup downloads some blocks back from catchpoint round - CatchpointLookback
	expectedBlocksToDownload := consensusParams.MaxTxnLife + consensusParams.DeeperBlockHeaderHistory
	const restrictedBlockRound = 2 // block number that is rejected to be downloaded to ensure fast catchup and not regular catchup is running
	// calculate the target round: this is the next round after catchpoint
	// that is greater than expectedBlocksToDownload before the restrictedBlock block number
	minRound := restrictedBlockRound + consensusParams.CatchpointLookback
	return basics.Round(((expectedBlocksToDownload+minRound)/basicTestCatchpointInterval + 1) * basicTestCatchpointInterval)
}

func applyCatchpointConsensusChanges(consensusParams *config.ConsensusParams) {
	// MaxBalLookback  =  2 x SeedRefreshInterval x SeedLookback
	// ref. https://github.com/algorandfoundation/specs/blob/master/dev/abft.md
	consensusParams.SeedLookback = 2
	consensusParams.SeedRefreshInterval = 2
	consensusParams.MaxBalLookback = 2 * consensusParams.SeedLookback * consensusParams.SeedRefreshInterval // 8
	consensusParams.MaxTxnLife = 13
	consensusParams.CatchpointLookback = consensusParams.MaxBalLookback
	consensusParams.EnableOnlineAccountCatchpoints = true
	if runtime.GOARCH == "amd64" || runtime.GOARCH == "arm64" {
		// amd64 and arm64 platforms are generally quite capable, so accelerate the round times to make the test run faster.
		consensusParams.AgreementFilterTimeoutPeriod0 = 1 * time.Second
		consensusParams.AgreementFilterTimeout = 1 * time.Second
	}
	consensusParams.ApprovedUpgrades = map[protocol.ConsensusVersion]uint64{}
}

func configureCatchpointGeneration(a *require.Assertions, nodeController *nodecontrol.NodeController) {
	cfg, err := config.LoadConfigFromDisk(nodeController.GetDataDir())
	a.NoError(err)

	cfg.CatchpointInterval = basicTestCatchpointInterval
	cfg.MaxAcctLookback = 2
	err = cfg.SaveToDisk(nodeController.GetDataDir())
	a.NoError(err)
}

func configureCatchpointUsage(a *require.Assertions, nodeController *nodecontrol.NodeController) {
	cfg, err := config.LoadConfigFromDisk(nodeController.GetDataDir())
	a.NoError(err)

	cfg.MaxAcctLookback = 2
	cfg.Archival = false
	cfg.CatchpointInterval = 0
	cfg.NetAddress = ""
	cfg.EnableLedgerService = false
	cfg.EnableBlockService = false
	cfg.BaseLoggerDebugLevel = uint32(logging.Debug)
	cfg.CatchupBlockValidateMode = 12
	err = cfg.SaveToDisk(nodeController.GetDataDir())
	a.NoError(err)
}

type nodeExitErrorCollector struct {
	errors   []error
	messages []string
	mu       deadlock.Mutex
	a        *require.Assertions
}

func (ec *nodeExitErrorCollector) nodeExitWithError(nc *nodecontrol.NodeController, err error) {
	if err == nil {
		return
	}

	exitError, ok := err.(*exec.ExitError)
	if !ok {
		if err != nil {
			ec.mu.Lock()
			ec.errors = append(ec.errors, err)
			ec.messages = append(ec.messages, "Node at %s has terminated with an error", nc.GetDataDir())
			ec.mu.Unlock()
		}
		return
	}
	ws := exitError.Sys().(syscall.WaitStatus)
	exitCode := ws.ExitStatus()

	if err != nil {
		ec.mu.Lock()
		ec.errors = append(ec.errors, err)
		ec.messages = append(ec.messages, fmt.Sprintf("Node at %s has terminated with error code %d", nc.GetDataDir(), exitCode))
		ec.mu.Unlock()
	}
}

func (ec *nodeExitErrorCollector) Print() {
	ec.mu.Lock()
	defer ec.mu.Unlock()
	for i, err := range ec.errors {
		ec.a.NoError(err, ec.messages[i])
	}
}

func startCatchpointGeneratingNode(a *require.Assertions, fixture *fixtures.RestClientFixture, nodeName string) (nodecontrol.NodeController, *nodeExitErrorCollector) {
	nodeController, err := fixture.GetNodeController(nodeName)
	a.NoError(err)

	configureCatchpointGeneration(a, &nodeController)

	errorsCollector := nodeExitErrorCollector{a: a}
	_, err = nodeController.StartAlgod(nodecontrol.AlgodStartArgs{
		PeerAddress:       "",
		ListenIP:          "",
		RedirectOutput:    true,
		RunUnderHost:      false,
		TelemetryOverride: "",
		ExitErrorCallback: errorsCollector.nodeExitWithError,
	})
	a.NoError(err)
	return nodeController, &errorsCollector
}

func startCatchpointUsingNode(a *require.Assertions, fixture *fixtures.RestClientFixture, nodeName string, peerAddress string) (nodecontrol.NodeController, *fixtures.WebProxy, *nodeExitErrorCollector) {
	nodeController, err := fixture.GetNodeController(nodeName)
	a.NoError(err)

	configureCatchpointUsage(a, &nodeController)

	wp := denyRoundRequestsWebProxy(a, peerAddress, 2)
	errorsCollector := nodeExitErrorCollector{a: a}
	_, err = nodeController.StartAlgod(nodecontrol.AlgodStartArgs{
		PeerAddress:       wp.GetListenAddress(),
		ListenIP:          "",
		RedirectOutput:    true,
		RunUnderHost:      false,
		TelemetryOverride: "",
		ExitErrorCallback: errorsCollector.nodeExitWithError,
	})
	a.NoError(err)
	return nodeController, wp, &errorsCollector
}

func startCatchpointNormalNode(a *require.Assertions, fixture *fixtures.RestClientFixture, nodeName string, peerAddress string) (nodecontrol.NodeController, *nodeExitErrorCollector) {
	nodeController, err := fixture.GetNodeController(nodeName)
	a.NoError(err)

	errorsCollector := nodeExitErrorCollector{a: a}
	_, err = nodeController.StartAlgod(nodecontrol.AlgodStartArgs{
		PeerAddress:       peerAddress,
		ListenIP:          "",
		RedirectOutput:    true,
		RunUnderHost:      false,
		TelemetryOverride: "",
		ExitErrorCallback: errorsCollector.nodeExitWithError,
	})
	a.NoError(err)
	return nodeController, &errorsCollector
}

func runBasicCatchpointCatchup(t *testing.T, consensusParams *config.ConsensusParams, roundsAfterCatchpoint basics.Round) {
	// Overview of this function:
	// Start a two-node network (primary has 100%, secondary has 0%)
	// create a web proxy, have the secondary node use it as a peer, blocking all requests for round #2. ( and allowing everything else )
	// Let it run until the first usable catchpoint, as computed in getFirstCatchpointRound, is generated.
	// instruct the catchpoint using node to catchpoint catchup from the proxy.
	// wait until the using node is caught up to catchpointRound + roundsAfterCatchpoint, skipping the "impossible" hole of round #2.
	a := require.New(fixtures.SynchronizedTest(t))

	consensus := make(config.ConsensusProtocols)
	const consensusCatchpointCatchupTestProtocol = protocol.ConsensusVersion("catchpointtestingprotocol")
	consensus[consensusCatchpointCatchupTestProtocol] = *consensusParams

	var fixture fixtures.RestClientFixture
	fixture.SetConsensus(consensus)
	fixture.SetupNoStart(t, filepath.Join("nettemplates", "CatchpointCatchupTestNetwork.json"))

	primaryNode, primaryErrorsCollector := startCatchpointGeneratingNode(a, &fixture, "Primary")
	defer primaryErrorsCollector.Print()
	defer primaryNode.StopAlgod()

	primaryNodeAddr, err := primaryNode.GetListeningAddress()
	a.NoError(err)

	usingNode, wp, usingNodeErrorsCollector := startCatchpointUsingNode(a, &fixture, "Node", primaryNodeAddr)
	defer usingNodeErrorsCollector.Print()
	defer wp.Close()
	defer usingNode.StopAlgod()

	targetCatchpointRound := getFirstCatchpointRound(consensusParams)

	primaryNodeRestClient := fixture.GetAlgodClientForController(primaryNode)
	catchpointLabel, err := fixture.ClientWaitForCatchpoint(primaryNodeRestClient, targetCatchpointRound)
	a.NoError(err)
	fmt.Printf("%s\n", catchpointLabel)

	usingNodeRestClient := fixture.GetAlgodClientForController(usingNode)
	_, err = usingNodeRestClient.Catchup(catchpointLabel)
	a.NoError(err)

	err = fixture.ClientWaitForRoundWithTimeout(usingNodeRestClient, uint64(targetCatchpointRound+roundsAfterCatchpoint))
	a.NoError(err)
}

func TestBasicCatchpointCatchup(t *testing.T) {
	partitiontest.PartitionTest(t)
	defer fixtures.ShutdownSynchronizedTest(t)

	// TODO: Reenable short
	//if testing.Short() {
	//	t.Skip()
	//}

	catchpointCatchupProtocol := config.Consensus[protocol.ConsensusCurrentVersion]
	applyCatchpointConsensusChanges(&catchpointCatchupProtocol)
	runBasicCatchpointCatchup(t, &catchpointCatchupProtocol, 1)
}

func TestCatchpointLabelGeneration(t *testing.T) {
	partitiontest.PartitionTest(t)
	defer fixtures.ShutdownSynchronizedTest(t)

	if testing.Short() {
		t.Skip()
	}

	testCases := []struct {
		catchpointInterval uint64
		archival           bool
		expectLabels       bool
	}{
		{4, true, true},
		{4, false, true},
		{0, true, false},
	}

	for _, tc := range testCases {
		t.Run(fmt.Sprintf("CatchpointInterval_%v/Archival_%v", tc.catchpointInterval, tc.archival), func(t *testing.T) {
			a := require.New(fixtures.SynchronizedTest(t))
			log := logging.TestingLog(t)

			consensus := make(config.ConsensusProtocols)
			const consensusCatchpointCatchupTestProtocol = protocol.ConsensusVersion("catchpointtestingprotocol")
			catchpointCatchupProtocol := config.Consensus[protocol.ConsensusCurrentVersion]
			applyCatchpointConsensusChanges(&catchpointCatchupProtocol)
			consensus[consensusCatchpointCatchupTestProtocol] = catchpointCatchupProtocol

			var fixture fixtures.RestClientFixture
			fixture.SetConsensus(consensus)

			errorsCollector := nodeExitErrorCollector{a: a}
			defer errorsCollector.Print()

			fixture.SetupNoStart(t, filepath.Join("nettemplates", "CatchpointCatchupTestNetwork.json"))

			// Get primary node
			primaryNode, err := fixture.GetNodeController("Primary")
			a.NoError(err)

			cfg, err := config.LoadConfigFromDisk(primaryNode.GetDataDir())
			a.NoError(err)
			cfg.CatchpointInterval = tc.catchpointInterval
			cfg.Archival = tc.archival
			cfg.MaxAcctLookback = 2
			cfg.SaveToDisk(primaryNode.GetDataDir())

			// start the primary node
			_, err = primaryNode.StartAlgod(nodecontrol.AlgodStartArgs{
				PeerAddress:       "",
				ListenIP:          "",
				RedirectOutput:    true,
				RunUnderHost:      false,
				TelemetryOverride: "",
				ExitErrorCallback: errorsCollector.nodeExitWithError,
			})
			a.NoError(err)
			defer primaryNode.StopAlgod()

			// Let the network make some progress
			currentRound := uint64(1)
			targetRound := uint64(21)
			primaryNodeRestClient := fixture.GetAlgodClientForController(primaryNode)
			log.Infof("Building ledger history..")
			for {
				err = fixture.ClientWaitForRound(primaryNodeRestClient, currentRound, 45*time.Second)
				a.NoError(err)
				if targetRound <= currentRound {
					break
				}
				currentRound++
			}
			log.Infof("done building!\n")

			primaryNodeStatus, err := primaryNodeRestClient.Status()
			a.NoError(err)
			a.NotNil(primaryNodeStatus.LastCatchpoint)
			if tc.expectLabels {
				a.NotEmpty(*primaryNodeStatus.LastCatchpoint)
			} else {
				a.Empty(*primaryNodeStatus.LastCatchpoint)
			}
		})
	}
}

// TestNodeTxHandlerRestart starts a two-node and one relay network
// Waits until a catchpoint is created
// Lets the primary node have the majority of the stake
// Sends a transaction from the second node
// The transaction will be confirmed only if the txHandler gets the transaction
func TestNodeTxHandlerRestart(t *testing.T) {
	partitiontest.PartitionTest(t)
	defer fixtures.ShutdownSynchronizedTest(t)

	if testing.Short() {
		t.Skip()
	}
	a := require.New(fixtures.SynchronizedTest(t))

	consensus := make(config.ConsensusProtocols)
	protoVersion := protocol.ConsensusCurrentVersion
	catchpointCatchupProtocol := config.Consensus[protoVersion]
	applyCatchpointConsensusChanges(&catchpointCatchupProtocol)
	catchpointCatchupProtocol.StateProofInterval = 0
	consensus[protoVersion] = catchpointCatchupProtocol

	var fixture fixtures.RestClientFixture
	fixture.SetConsensus(consensus)
	fixture.SetupNoStart(t, filepath.Join("nettemplates", "TwoNodes50EachWithRelay.json"))

	// Get primary node
	primaryNode, err := fixture.GetNodeController("Node1")
	a.NoError(err)
	// Get secondary node
	secondNode, err := fixture.GetNodeController("Node2")
	a.NoError(err)
	// Get the relay
	relayNode, err := fixture.GetNodeController("Relay")
	a.NoError(err)

	// prepare it's configuration file to set it to generate a catchpoint every 16 rounds.
	cfg, err := config.LoadConfigFromDisk(primaryNode.GetDataDir())
	a.NoError(err)
	const catchpointInterval = 16
	cfg.CatchpointInterval = catchpointInterval
	cfg.CatchpointTracking = 2
	cfg.MaxAcctLookback = 2
	cfg.Archival = false

	cfg.TxSyncIntervalSeconds = 200000 // disable txSync

	cfg.SaveToDisk(primaryNode.GetDataDir())
	cfg.SaveToDisk(secondNode.GetDataDir())

	cfg, err = config.LoadConfigFromDisk(relayNode.GetDataDir())
	a.NoError(err)
	cfg.TxSyncIntervalSeconds = 200000 // disable txSync
	cfg.SaveToDisk(relayNode.GetDataDir())

	fixture.Start()
	defer fixture.LibGoalFixture.Shutdown()

	client1 := fixture.GetLibGoalClientFromNodeController(primaryNode)
	client2 := fixture.GetLibGoalClientFromNodeController(secondNode)
	wallet1, err := client1.GetUnencryptedWalletHandle()
	a.NoError(err)
	wallet2, err := client2.GetUnencryptedWalletHandle()
	a.NoError(err)
	addrs1, err := client1.ListAddresses(wallet1)
	a.NoError(err)
	addrs2, err := client2.ListAddresses(wallet2)
	a.NoError(err)

	// let the second node have insufficient stake for proposing a block
	tx, err := client2.SendPaymentFromUnencryptedWallet(addrs2[0], addrs1[0], 1000, 4999999999000000, nil)
	a.NoError(err)
	status, err := client1.Status()
	a.NoError(err)
	_, err = fixture.WaitForConfirmedTxn(status.LastRound+100, addrs1[0], tx.ID().String())
	a.NoError(err)
	targetCatchpointRound := status.LastRound

	// ensure the catchpoint is created for targetCatchpointRound
	timer := time.NewTimer(100 * time.Second)
outer:
	for {
		status, err = client1.Status()
		a.NoError(err)

		var round basics.Round
		if status.LastCatchpoint != nil && len(*status.LastCatchpoint) > 0 {
			round, _, err = ledgercore.ParseCatchpointLabel(*status.LastCatchpoint)
			a.NoError(err)
			if uint64(round) >= targetCatchpointRound {
				break
			}
		}
		select {
		case <-timer.C:
			a.Failf("timeout waiting a catchpoint", "target: %d, got %d", targetCatchpointRound, round)
			break outer
		default:
			time.Sleep(250 * time.Millisecond)
		}
	}

	// let the primary node catchup
	err = client1.Catchup(*status.LastCatchpoint)
	a.NoError(err)

	status1, err := client1.Status()
	a.NoError(err)
	targetRound := status1.LastRound + 5

	// Wait for the network to start making progress again
	primaryNodeRestClient := fixture.GetAlgodClientForController(primaryNode)
	err = fixture.ClientWaitForRound(primaryNodeRestClient, targetRound,
		10*catchpointCatchupProtocol.AgreementFilterTimeout)
	a.NoError(err)

	// let the 2nd client send a transaction
	tx, err = client2.SendPaymentFromUnencryptedWallet(addrs2[0], addrs1[0], 1000, 50000, nil)
	a.NoError(err)

	status, err = client2.Status()
	a.NoError(err)
	_, err = fixture.WaitForConfirmedTxn(status.LastRound+50, addrs2[0], tx.ID().String())
	a.NoError(err)
}

// TestNodeTxSyncRestart starts a two-node and one relay network
// Waits until a catchpoint is created
// Lets the primary node have the majority of the stake
// Stops the primary node to miss the next transaction
// Sends a transaction from the second node
// Starts the primary node, and immediately after start the catchup
// The transaction will be confirmed only when the TxSync of the pools passes the transaction to the primary node
func TestNodeTxSyncRestart(t *testing.T) {
	partitiontest.PartitionTest(t)
	defer fixtures.ShutdownSynchronizedTest(t)

	if testing.Short() {
		t.Skip()
	}
	a := require.New(fixtures.SynchronizedTest(t))

	consensus := make(config.ConsensusProtocols)
	protoVersion := protocol.ConsensusCurrentVersion
	catchpointCatchupProtocol := config.Consensus[protoVersion]
	prevMaxTxnLife := catchpointCatchupProtocol.MaxTxnLife
	applyCatchpointConsensusChanges(&catchpointCatchupProtocol)
	catchpointCatchupProtocol.MaxTxnLife = prevMaxTxnLife
	catchpointCatchupProtocol.StateProofInterval = 0
	consensus[protoVersion] = catchpointCatchupProtocol

	var fixture fixtures.RestClientFixture
	fixture.SetConsensus(consensus)
	fixture.SetupNoStart(t, filepath.Join("nettemplates", "TwoNodes50EachWithRelay.json"))

	// Get primary node
	primaryNode, err := fixture.GetNodeController("Node1")
	a.NoError(err)
	// Get secondary node
	secondNode, err := fixture.GetNodeController("Node2")
	a.NoError(err)
	// Get the relay
	relayNode, err := fixture.GetNodeController("Relay")
	a.NoError(err)

	// prepare it's configuration file to set it to generate a catchpoint every 16 rounds.
	cfg, err := config.LoadConfigFromDisk(primaryNode.GetDataDir())
	a.NoError(err)
	const catchpointInterval = 16
	cfg.CatchpointInterval = catchpointInterval
	cfg.CatchpointTracking = 2
	cfg.MaxAcctLookback = 2
	cfg.Archival = false

	// Shorten the txn sync interval so the test can run faster
	cfg.TxSyncIntervalSeconds = 4

	cfg.SaveToDisk(primaryNode.GetDataDir())
	cfg.SaveToDisk(secondNode.GetDataDir())

	cfg, err = config.LoadConfigFromDisk(relayNode.GetDataDir())
	a.NoError(err)
	cfg.TxSyncIntervalSeconds = 4
	cfg.SaveToDisk(relayNode.GetDataDir())

	fixture.Start()
	defer fixture.LibGoalFixture.Shutdown()

	client1 := fixture.GetLibGoalClientFromNodeController(primaryNode)
	client2 := fixture.GetLibGoalClientFromNodeController(secondNode)
	wallet1, err := client1.GetUnencryptedWalletHandle()
	a.NoError(err)
	wallet2, err := client2.GetUnencryptedWalletHandle()
	a.NoError(err)
	addrs1, err := client1.ListAddresses(wallet1)
	a.NoError(err)
	addrs2, err := client2.ListAddresses(wallet2)
	a.NoError(err)

	// let the second node have insufficient stake for proposing a block
	tx, err := client2.SendPaymentFromUnencryptedWallet(addrs2[0], addrs1[0], 1000, 4999999999000000, nil)
	a.NoError(err)
	status, err := client1.Status()
	a.NoError(err)
	_, err = fixture.WaitForConfirmedTxn(status.LastRound+100, addrs1[0], tx.ID().String())
	a.NoError(err)
	targetCatchpointRound := status.LastRound

	// ensure the catchpoint is created for targetCatchpointRound
	timer := time.NewTimer(100 * time.Second)
outer:
	for {
		status, err = client1.Status()
		a.NoError(err)

		var round basics.Round
		if status.LastCatchpoint != nil && len(*status.LastCatchpoint) > 0 {
			round, _, err = ledgercore.ParseCatchpointLabel(*status.LastCatchpoint)
			a.NoError(err)
			if uint64(round) >= targetCatchpointRound {
				break
			}
		}
		select {
		case <-timer.C:
			a.Failf("timeout waiting a catchpoint", "target: %d, got %d", targetCatchpointRound, round)
			break outer
		default:
			time.Sleep(250 * time.Millisecond)
		}
	}

	// stop the primary node
	client1.FullStop()

	// let the 2nd client send a transaction
	tx, err = client2.SendPaymentFromUnencryptedWallet(addrs2[0], addrs1[0], 1000, 50000, nil)
	a.NoError(err)

	// now that the primary missed the transaction, start it, and let it catchup
	_, err = fixture.StartNode(primaryNode.GetDataDir())
	a.NoError(err)
	// let the primary node catchup
	err = client1.Catchup(*status.LastCatchpoint)
	a.NoError(err)

	// the transaction should not be confirmed yet
	_, err = fixture.WaitForConfirmedTxn(0, addrs2[0], tx.ID().String())
	a.Error(err)

	// Wait for the catchup
	for t := 0; t < 10; t++ {
		status1, err := client1.Status()
		a.NoError(err)
		status2, err := client2.Status()
		a.NoError(err)

		if status1.LastRound+1 >= status2.LastRound {
			// if the primary node is within 1 round of the secondary node, then it has
			// caught up
			break
		}
		time.Sleep(catchpointCatchupProtocol.AgreementFilterTimeout)
	}

	status, err = client2.Status()
	a.NoError(err)
	_, err = fixture.WaitForConfirmedTxn(status.LastRound+50, addrs2[0], tx.ID().String())
	a.NoError(err)
}
