package fluxmonitor

import (
	"github.com/ethereum/go-ethereum/common"
	"github.com/smartcontractkit/chainlink/core/services/eth"
	"github.com/smartcontractkit/chainlink/core/utils"
)

func (fm *ConcreteFluxMonitor) MockLogBroadcaster() *mockLogBroadcaster {
	mock := mockLogBroadcaster{}
	fm.logBroadcaster = &mock
	return &mock
}

type mockLogBroadcaster struct {
	Started bool
	utils.DependentAwaiter
}

func (mlb *mockLogBroadcaster) Start() {
	mlb.Started = true
}
func (mlb *mockLogBroadcaster) Register(common.Address, eth.LogListener) bool {
	return false
}
func (mlb *mockLogBroadcaster) Unregister(common.Address, eth.LogListener) {}
func (mlb *mockLogBroadcaster) Stop()                                      {}

type MockableLogBroadcaster interface {
	MockLogBroadcaster() *mockLogBroadcaster
}
