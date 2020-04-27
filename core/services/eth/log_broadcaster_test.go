package eth_test

import (
	"errors"
	"math/big"
	"testing"
	"time"

	"github.com/onsi/gomega"
	"github.com/smartcontractkit/chainlink/core/eth"
	"github.com/smartcontractkit/chainlink/core/internal/cltest"
	"github.com/smartcontractkit/chainlink/core/internal/mocks"
	ethsvc "github.com/smartcontractkit/chainlink/core/services/eth"
	"github.com/smartcontractkit/chainlink/core/store"
	"github.com/smartcontractkit/chainlink/core/store/models"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func createJob(t *testing.T, store *store.Store) models.JobSpec {
	job := cltest.NewJob()
	err := store.ORM.CreateJob(&job)
	require.NoError(t, err)
	return job
}

func requireLogConsumptionCount(t *testing.T, store *store.Store, expectedCount int) {
	comparisonFunc := func() bool {
		observedCount, err := store.ORM.CountOf(&models.LogConsumption{})
		require.NoError(t, err)
		return observedCount == expectedCount
	}

	require.Eventually(t, comparisonFunc, 5*time.Second, 10*time.Millisecond)
}

func handleLogBroadcast(t *testing.T, lb ethsvc.LogBroadcast) {
	consumed, err := lb.WasAlreadyConsumed()
	require.NoError(t, err)
	require.False(t, consumed)
	err = lb.MarkConsumed()
	require.NoError(t, err)
}

func TestLogBroadcaster_AwaitsInitialSubscribersOnStartup(t *testing.T) {
	g := gomega.NewGomegaWithT(t)

	store, cleanup := cltest.NewStore(t)
	defer cleanup()

	const (
		blockHeight uint64 = 123
	)

	ethClient := new(mocks.Client)
	sub := new(mocks.Subscription)
	listener := new(mocks.LogListener)

	chOkayToAssert := make(chan struct{}) // avoid flaky tests

	listener.On("OnConnect").Return()
	listener.On("OnDisconnect").Return().Run(func(mock.Arguments) { close(chOkayToAssert) })

	sub.On("Unsubscribe").Return()
	sub.On("Err").Return(nil)

	chSubscribe := make(chan struct{}, 10)
	ethClient.On("SubscribeToLogs", mock.Anything, mock.Anything, mock.Anything).
		Return(sub, nil).
		Run(func(mock.Arguments) { chSubscribe <- struct{}{} })
	ethClient.On("GetLatestBlock").Return(eth.Block{Number: hexutil.Uint64(blockHeight)}, nil)
	ethClient.On("GetLogs", mock.Anything).Return([]eth.Log{}, nil)

	lb := ethsvc.NewLogBroadcaster(ethClient, store.ORM, 10)
	lb.AddDependents(2)
	lb.Start()

	lb.Register(common.Address{}, listener)

	g.Consistently(func() int { return len(chSubscribe) }).Should(gomega.Equal(0))
	lb.DependentReady()
	g.Consistently(func() int { return len(chSubscribe) }).Should(gomega.Equal(0))
	lb.DependentReady()
	g.Eventually(func() int { return len(chSubscribe) }).Should(gomega.Equal(1))
	g.Consistently(func() int { return len(chSubscribe) }).Should(gomega.Equal(1))

	lb.Stop()

	<-chOkayToAssert

	ethClient.AssertExpectations(t)
	sub.AssertExpectations(t)
}

func TestLogBroadcaster_ResubscribesOnAddOrRemoveContract(t *testing.T) {
	t.Parallel()

	store, cleanup := cltest.NewStore(t)
	defer cleanup()

	const (
		numContracts        = 3
		blockHeight  uint64 = 123
	)

	ethClient := new(mocks.Client)
	sub := new(mocks.Subscription)

	var subscribeCalls int
	var unsubscribeCalls int
	ethClient.On("SubscribeToLogs", mock.Anything, mock.Anything, mock.Anything).
		Return(sub, nil).
		Run(func(args mock.Arguments) {
			subscribeCalls++
		})
	ethClient.On("GetLatestBlock").
		Return(eth.Block{Number: hexutil.Uint64(blockHeight)}, nil)
	ethClient.On("GetLogs", mock.Anything).
		Return(nil, nil)
	sub.On("Unsubscribe").
		Return().
		Run(func(mock.Arguments) { unsubscribeCalls++ })
	sub.On("Err").Return(nil)

	lb := ethsvc.NewLogBroadcaster(ethClient, store.ORM, 10)
	lb.Start()

	type registration struct {
		common.Address
		ethsvc.LogListener
	}
	registrations := make([]registration, numContracts)
	for i := 0; i < numContracts; i++ {
		listener := new(mocks.LogListener)
		listener.On("OnConnect").Return()
		listener.On("OnDisconnect").Return()
		registrations[i] = registration{cltest.NewAddress(), listener}
		lb.Register(registrations[i].Address, registrations[i].LogListener)
	}

	require.Eventually(t, func() bool { return subscribeCalls == 1 }, 5*time.Second, 10*time.Millisecond)
	gomega.NewGomegaWithT(t).Consistently(subscribeCalls).Should(gomega.Equal(1))
	gomega.NewGomegaWithT(t).Consistently(unsubscribeCalls).Should(gomega.Equal(0))

	for _, r := range registrations {
		lb.Unregister(r.Address, r.LogListener)
	}
	require.Eventually(t, func() bool { return unsubscribeCalls == 1 }, 5*time.Second, 10*time.Millisecond)
	gomega.NewGomegaWithT(t).Consistently(subscribeCalls).Should(gomega.Equal(1))

	lb.Stop()
	gomega.NewGomegaWithT(t).Consistently(unsubscribeCalls).Should(gomega.Equal(1))

	ethClient.AssertExpectations(t)
	sub.AssertExpectations(t)
}

type simpleLogListner struct {
	handler func(lb ethsvc.LogBroadcast, err error)
	id      models.ID
}

func (listner simpleLogListner) HandleLog(lb ethsvc.LogBroadcast, err error) {
	listner.handler(lb, err)
}
func (listner simpleLogListner) OnConnect()    {}
func (listner simpleLogListner) OnDisconnect() {}
func (listner simpleLogListner) Consumer() models.LogConsumer {
	return models.LogConsumer{
		Type: models.LogConsumerTypeJob,
		ID:   &listner.id,
	}
}

func TestLogBroadcaster_BroadcastsToCorrectRecipients(t *testing.T) {
	t.Parallel()

	store, cleanup := cltest.NewStore(t)
	defer cleanup()

	const blockHeight uint64 = 0

	ethClient := new(mocks.Client)
	sub := new(mocks.Subscription)

	chchRawLogs := make(chan chan<- eth.Log, 1)
	ethClient.On("SubscribeToLogs", mock.Anything, mock.Anything, mock.Anything).
		Run(func(args mock.Arguments) {
			chchRawLogs <- args.Get(1).(chan<- eth.Log)
		}).
		Return(sub, nil).
		Once()
	ethClient.On("GetLatestBlock").
		Return(eth.Block{Number: hexutil.Uint64(blockHeight)}, nil)
	ethClient.On("GetLogs", mock.Anything).
		Return(nil, nil)
	sub.On("Err").Return(nil)
	sub.On("Unsubscribe").Return()

	lb := ethsvc.NewLogBroadcaster(ethClient, store.ORM, 10)
	lb.Start()

	addr1 := cltest.NewAddress()
	addr2 := cltest.NewAddress()
	addr1SentLogs := []eth.Log{
		{Address: addr1, BlockNumber: 1, BlockHash: cltest.NewHash()},
		{Address: addr1, BlockNumber: 2, BlockHash: cltest.NewHash()},
		{Address: addr1, BlockNumber: 3, BlockHash: cltest.NewHash()},
	}
	addr2SentLogs := []eth.Log{
		{Address: addr2, BlockNumber: 4, BlockHash: cltest.NewHash()},
		{Address: addr2, BlockNumber: 5, BlockHash: cltest.NewHash()},
		{Address: addr2, BlockNumber: 6, BlockHash: cltest.NewHash()},
	}

	var addr1Logs1, addr1Logs2, addr2Logs1, addr2Logs2 []interface{}

	listener1 := simpleLogListner{
		func(lb ethsvc.LogBroadcast, err error) {
			require.NoError(t, err)
			addr1Logs1 = append(addr1Logs1, lb.Log())
			handleLogBroadcast(t, lb)
		},
		*createJob(t, store).ID,
	}
	listener2 := simpleLogListner{
		func(lb ethsvc.LogBroadcast, err error) {
			require.NoError(t, err)
			addr1Logs2 = append(addr1Logs2, lb.Log())
			handleLogBroadcast(t, lb)
		},
		*createJob(t, store).ID,
	}
	listener3 := simpleLogListner{
		func(lb ethsvc.LogBroadcast, err error) {
			require.NoError(t, err)
			addr2Logs1 = append(addr2Logs1, lb.Log())
			handleLogBroadcast(t, lb)
		},
		*createJob(t, store).ID,
	}
	listener4 := simpleLogListner{
		func(lb ethsvc.LogBroadcast, err error) {
			require.NoError(t, err)
			addr2Logs2 = append(addr2Logs2, lb.Log())
			handleLogBroadcast(t, lb)
		},
		*createJob(t, store).ID,
	}

	lb.Register(addr1, &listener1)
	lb.Register(addr1, &listener2)
	lb.Register(addr2, &listener3)
	lb.Register(addr2, &listener4)

	chRawLogs := <-chchRawLogs

	for _, log := range addr1SentLogs {
		chRawLogs <- log
	}
	for _, log := range addr2SentLogs {
		chRawLogs <- log
	}

	require.Eventually(t, func() bool { return len(addr1Logs1) == len(addr1SentLogs) }, time.Second, 10*time.Millisecond)
	require.Eventually(t, func() bool { return len(addr1Logs2) == len(addr1SentLogs) }, time.Second, 10*time.Millisecond)
	require.Eventually(t, func() bool { return len(addr2Logs1) == len(addr2SentLogs) }, time.Second, 10*time.Millisecond)
	require.Eventually(t, func() bool { return len(addr2Logs2) == len(addr2SentLogs) }, time.Second, 10*time.Millisecond)
	requireLogConsumptionCount(t, store, 12)

	lb.Stop()

	for i := range addr1SentLogs {
		require.Equal(t, &addr1SentLogs[i], addr1Logs1[i])
		require.Equal(t, &addr1SentLogs[i], addr1Logs2[i])
	}
	for i := range addr2SentLogs {
		require.Equal(t, &addr2SentLogs[i], addr2Logs1[i])
		require.Equal(t, &addr2SentLogs[i], addr2Logs2[i])
	}

	ethClient.AssertExpectations(t)
	sub.AssertExpectations(t)
}

func TestLogBroadcaster_Register_ResubscribesToMostRecentlySeenBlock(t *testing.T) {
	t.Parallel()

	store, cleanup := cltest.NewStore(t)
	defer cleanup()

	const (
		blockHeight   = 15
		expectedBlock = 5
	)

	ethClient := new(mocks.Client)
	sub := new(mocks.Subscription)

	addr1 := cltest.NewAddress()
	addr2 := cltest.NewAddress()

	chchRawLogs := make(chan chan<- eth.Log, 1)
	ethClient.On("SubscribeToLogs", mock.Anything, mock.Anything, mock.Anything).
		Run(func(args mock.Arguments) {
			chchRawLogs <- args.Get(1).(chan<- eth.Log)
		}).
		Return(sub, nil).
		Twice()

	ethClient.On("GetLatestBlock").
		Return(eth.Block{Number: hexutil.Uint64(blockHeight)}, nil)
	ethClient.On("GetLogs", mock.Anything).
		Run(func(args mock.Arguments) {
			query := args.Get(0).(ethereum.FilterQuery)
			require.Equal(t, big.NewInt(expectedBlock), query.FromBlock)
			require.Contains(t, query.Addresses, addr1)
			require.Len(t, query.Addresses, 1)
		}).
		Return(nil, nil).
		Once()
	ethClient.On("GetLogs", mock.Anything).
		Run(func(args mock.Arguments) {
			query := args.Get(0).(ethereum.FilterQuery)
			require.Equal(t, big.NewInt(expectedBlock), query.FromBlock)
			require.Contains(t, query.Addresses, addr1)
			require.Contains(t, query.Addresses, addr2)
			require.Len(t, query.Addresses, 2)
		}).
		Return(nil, nil).
		Once()

	sub.On("Unsubscribe").Return()
	sub.On("Err").Return(nil)

	listener1 := new(mocks.LogListener)
	listener2 := new(mocks.LogListener)
	listener1.On("OnConnect").Return()
	listener2.On("OnConnect").Return()
	listener1.On("OnDisconnect").Return()
	listener2.On("OnDisconnect").Return()

	lb := ethsvc.NewLogBroadcaster(ethClient, store.ORM, 10)
	lb.Start()                    // Subscribe #1
	lb.Register(addr1, listener1) // Subscribe #2
	chRawLogs := <-chchRawLogs
	chRawLogs <- eth.Log{BlockNumber: expectedBlock}
	lb.Register(addr2, listener2) // Subscribe #3
	<-chchRawLogs

	lb.Stop()

	ethClient.AssertExpectations(t)
	listener1.AssertExpectations(t)
	listener2.AssertExpectations(t)
	sub.AssertExpectations(t)
}

func TestDecodingLogListener(t *testing.T) {
	store, cleanup := cltest.NewStore(t)
	defer cleanup()

	contract, err := eth.GetV6ContractCodec("FluxAggregator")
	require.NoError(t, err)

	type LogNewRound struct {
		eth.Log
		RoundId   *big.Int
		StartedBy common.Address
		StartedAt *big.Int
	}

	logTypes := map[common.Hash]interface{}{
		eth.MustGetV6ContractEventID("FluxAggregator", "NewRound"): LogNewRound{},
	}

	var decodedLog interface{}

	job := createJob(t, store)
	listener := simpleLogListner{
		func(lb ethsvc.LogBroadcast, innerErr error) {
			err = innerErr
			decodedLog = lb.Log()
		},
		*job.ID,
	}

	decodingListener := ethsvc.NewDecodingLogListener(contract, logTypes, &listener)
	rawLog := cltest.LogFromFixture(t, "../testdata/new_round_log.json")
	logBroadcast := new(mocks.LogBroadcast)

	logBroadcast.On("Log").Return(&rawLog).Once()
	logBroadcast.On("UpdateLog", mock.Anything).Run(func(args mock.Arguments) {
		logBroadcast.On("Log").Return(args.Get(0))
	})

	decodingListener.HandleLog(logBroadcast, nil)
	require.NoError(t, err)
	newRoundLog := decodedLog.(*LogNewRound)

	require.Equal(t, newRoundLog.Log, rawLog)
	require.True(t, newRoundLog.RoundId.Cmp(big.NewInt(1)) == 0)
	require.Equal(t, newRoundLog.StartedBy, common.HexToAddress("f17f52151ebef6c7334fad080c5704d77216b732"))
	require.True(t, newRoundLog.StartedAt.Cmp(big.NewInt(15)) == 0)

	expectedErr := errors.New("oh no!")
	nilLb := new(mocks.LogBroadcast)

	logBroadcast.On("Log").Return(nil).Once()
	decodingListener.HandleLog(nilLb, expectedErr)
	require.Equal(t, err, expectedErr)
}

func TestLogBroadcaster_ReceivesAllLogsWhenResubscribing(t *testing.T) {
	t.Parallel()

	logs := make(map[uint]eth.Log)
	for n := 1; n < 18; n++ {
		logs[uint(n)] = eth.Log{
			BlockNumber: uint64(n),
			BlockHash:   cltest.NewHash(),
			Index:       0,
		}
	}

	tests := []struct {
		name             string
		blockHeight1     uint64
		blockHeight2     uint64
		batch1           []uint
		backfillableLogs []uint
		batch2           []uint
		expectedFinal    []uint
	}{
		{
			name:             "no backfilled logs, no overlap",
			blockHeight1:     0,
			blockHeight2:     2,
			batch1:           []uint{1, 2},
			backfillableLogs: nil,
			batch2:           []uint{3, 4},
			expectedFinal:    []uint{1, 2, 3, 4},
		},
		{
			name:             "no backfilled logs, overlap",
			blockHeight1:     0,
			blockHeight2:     2,
			batch1:           []uint{1, 2},
			backfillableLogs: nil,
			batch2:           []uint{2, 3},
			expectedFinal:    []uint{1, 2, 3},
		},
		{
			name:             "backfilled logs, no overlap",
			blockHeight1:     0,
			blockHeight2:     15,
			batch1:           []uint{1, 2},
			backfillableLogs: []uint{6, 7, 12, 15},
			batch2:           []uint{16, 17},
			expectedFinal:    []uint{1, 2, 6, 7, 12, 15, 16, 17},
		},
		{
			name:             "backfilled logs, overlap",
			blockHeight1:     0,
			blockHeight2:     15,
			batch1:           []uint{1, 9},
			backfillableLogs: []uint{9, 12, 15},
			batch2:           []uint{16, 17},
			expectedFinal:    []uint{1, 9, 12, 15, 16, 17},
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			store, cleanup := cltest.NewStore(t)
			defer cleanup()

			sub := new(mocks.Subscription)
			ethClient := new(mocks.Client)

			chchRawLogs := make(chan chan<- eth.Log, 1)

			ethClient.On("SubscribeToLogs", mock.Anything, mock.Anything, mock.Anything).
				Run(func(args mock.Arguments) {
					chRawLogs := args.Get(1).(chan<- eth.Log)
					chchRawLogs <- chRawLogs
				}).
				Return(sub, nil).
				Twice()

			ethClient.On("GetLatestBlock").Return(eth.Block{Number: hexutil.Uint64(test.blockHeight1)}, nil).Twice()
			ethClient.On("GetLogs", mock.Anything).Return(nil, nil).Once()

			sub.On("Err").Return(nil)
			sub.On("Unsubscribe").Return()

			lb := ethsvc.NewLogBroadcaster(ethClient, store.ORM, 10)
			lb.Start()

			var recvd []*eth.Log

			handleLog := func(lb ethsvc.LogBroadcast, err error) {
				consumed, err := lb.WasAlreadyConsumed()
				require.NoError(t, err)
				if !consumed {
					recvd = append(recvd, lb.Log().(*eth.Log))
					err = lb.MarkConsumed()
					require.NoError(t, err)
				}
			}

			logListener := &simpleLogListner{
				handler: handleLog,
			}

			// Send initial logs
			lb.Register(common.Address{0}, logListener)
			chRawLogs1 := <-chchRawLogs
			for _, logNum := range test.batch1 {
				chRawLogs1 <- logs[logNum]
			}
			require.Eventually(t, func() bool { return len(recvd) == len(test.batch1) }, 5*time.Second, 10*time.Millisecond)
			requireLogConsumptionCount(t, store, len(test.batch1))
			for i, logNum := range test.batch1 {
				require.Equal(t, *recvd[i], logs[logNum])
			}

			var backfillableLogs []eth.Log
			for _, logNum := range test.backfillableLogs {
				backfillableLogs = append(backfillableLogs, logs[logNum])
			}
			ethClient.On("GetLatestBlock").Return(eth.Block{Number: hexutil.Uint64(test.blockHeight2)}, nil).Once()
			ethClient.On("GetLogs", mock.Anything).Return(backfillableLogs, nil).Once()
			// Trigger resubscription
			lb.Register(common.Address{1}, &simpleLogListner{})
			chRawLogs2 := <-chchRawLogs
			for _, logNum := range test.batch2 {
				chRawLogs2 <- logs[logNum]
			}

			require.Eventually(t, func() bool { return len(recvd) == len(test.expectedFinal) }, 5*time.Second, 10*time.Millisecond)
			requireLogConsumptionCount(t, store, len(test.expectedFinal))
			for i, logNum := range test.expectedFinal {
				require.Equal(t, *recvd[i], logs[logNum])
			}

			lb.Stop()
		})
	}
}

func TestAppendLogChannel(t *testing.T) {
	t.Parallel()

	logs1 := []eth.Log{
		{BlockNumber: 1},
		{BlockNumber: 2},
		{BlockNumber: 3},
		{BlockNumber: 4},
		{BlockNumber: 5},
	}

	logs2 := []eth.Log{
		{BlockNumber: 6},
		{BlockNumber: 7},
		{BlockNumber: 8},
		{BlockNumber: 9},
		{BlockNumber: 10},
	}

	logs3 := []eth.Log{
		{BlockNumber: 11},
		{BlockNumber: 12},
		{BlockNumber: 13},
		{BlockNumber: 14},
		{BlockNumber: 15},
	}

	ch1 := make(chan eth.Log)
	ch2 := make(chan eth.Log)
	ch3 := make(chan eth.Log)

	chCombined := ethsvc.ExposedAppendLogChannel(ch1, ch2)
	chCombined = ethsvc.ExposedAppendLogChannel(chCombined, ch3)

	go func() {
		defer close(ch1)
		for _, log := range logs1 {
			ch1 <- log
		}
	}()
	go func() {
		defer close(ch2)
		for _, log := range logs2 {
			ch2 <- log
		}
	}()
	go func() {
		defer close(ch3)
		for _, log := range logs3 {
			ch3 <- log
		}
	}()

	expected := append(logs1, logs2...)
	expected = append(expected, logs3...)

	var i int
	for log := range chCombined {
		require.Equal(t, expected[i], log)
		i++
	}
}

func TestLogBroadcaster_InjectsLogConsumptionRecordFunctions(t *testing.T) {
	store, cleanup := cltest.NewStore(t)
	defer cleanup()

	const blockHeight uint64 = 0

	ethClient := new(mocks.Client)
	sub := new(mocks.Subscription)

	chchRawLogs := make(chan chan<- eth.Log, 1)

	ethClient.On("SubscribeToLogs", mock.Anything, mock.Anything, mock.Anything).
		Run(func(args mock.Arguments) {
			chRawLogs := args.Get(1).(chan<- eth.Log)
			chchRawLogs <- chRawLogs
		}).
		Return(sub, nil).
		Once()

	ethClient.On("GetLatestBlock").Return(eth.Block{Number: hexutil.Uint64(blockHeight)}, nil)
	ethClient.On("GetLogs", mock.Anything).Return([]eth.Log{}, nil).Once()

	sub.On("Err").Return(nil)
	sub.On("Unsubscribe").Return()

	lb := ethsvc.NewLogBroadcaster(ethClient, store.ORM, 10)
	lb.Start()

	listenerCount := 0

	job := createJob(t, store)
	logListener := simpleLogListner{
		func(lb ethsvc.LogBroadcast, err error) {
			consumed, err := lb.WasAlreadyConsumed()
			require.NoError(t, err)
			require.False(t, consumed)
			err = lb.MarkConsumed()
			require.NoError(t, err)
			consumed, err = lb.WasAlreadyConsumed()
			require.NoError(t, err)
			require.True(t, consumed)
			listenerCount++
		},
		*job.ID,
	}
	addr := common.Address{1}

	lb.Register(addr, &logListener)

	chRawLogs := <-chchRawLogs
	chRawLogs <- eth.Log{Address: addr, BlockHash: cltest.NewHash(), BlockNumber: 0, Index: 0}
	chRawLogs <- eth.Log{Address: addr, BlockHash: cltest.NewHash(), BlockNumber: 1, Index: 0}

	require.Eventually(t, func() bool { return listenerCount == 2 }, 5*time.Second, 10*time.Millisecond)
	requireLogConsumptionCount(t, store, 2)
}

func TestLogBroadcaster_ProcessesLogsFromReorgs(t *testing.T) {
	store, cleanup := cltest.NewStore(t)
	defer cleanup()

	ethClient := new(mocks.Client)
	sub := new(mocks.Subscription)

	const blockHeight uint64 = 0

	chchRawLogs := make(chan chan<- eth.Log, 1)
	ethClient.On("SubscribeToLogs", mock.Anything, mock.Anything, mock.Anything).
		Run(func(args mock.Arguments) { chchRawLogs <- args.Get(1).(chan<- eth.Log) }).
		Return(sub, nil).
		Once()
	ethClient.On("GetLatestBlock").
		Return(eth.Block{Number: hexutil.Uint64(blockHeight)}, nil)
	ethClient.On("GetLogs", mock.Anything).Return([]eth.Log{}, nil).Once()
	sub.On("Unsubscribe").Return()
	sub.On("Err").Return(nil)

	lb := ethsvc.NewLogBroadcaster(ethClient, store.ORM, 10)
	lb.Start()

	blockHash0 := cltest.NewHash()
	blockHash1 := cltest.NewHash()
	blockHash2 := cltest.NewHash()
	blockHash1R := cltest.NewHash()
	blockHash2R := cltest.NewHash()

	addr := cltest.NewAddress()
	logs := []eth.Log{
		{Address: addr, BlockHash: blockHash0, BlockNumber: 0, Index: 0},
		{Address: addr, BlockHash: blockHash1, BlockNumber: 1, Index: 0},
		{Address: addr, BlockHash: blockHash2, BlockNumber: 2, Index: 0},
		{Address: addr, BlockHash: blockHash1R, BlockNumber: 1, Index: 0},
		{Address: addr, BlockHash: blockHash2R, BlockNumber: 2, Index: 0},
	}

	var recvd []*eth.Log

	job := createJob(t, store)
	listener := simpleLogListner{
		func(lb ethsvc.LogBroadcast, err error) {
			require.NoError(t, err)
			ethLog := lb.Log().(*eth.Log)
			recvd = append(recvd, ethLog)
			handleLogBroadcast(t, lb)
		},
		*job.ID,
	}

	lb.Register(addr, &listener)

	chRawLogs := <-chchRawLogs

	for i := 0; i < len(logs); i++ {
		chRawLogs <- logs[i]
	}

	require.Eventually(t, func() bool { return len(recvd) == 5 }, 5*time.Second, 10*time.Millisecond)
	requireLogConsumptionCount(t, store, 5)

	for idx, receivedLog := range recvd {
		require.Equal(t, receivedLog, &logs[idx])
	}

	ethClient.AssertExpectations(t)
}
