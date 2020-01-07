// Copyright 2019 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

package statediff

import (
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/event"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/node"
	"github.com/ethereum/go-ethereum/p2p"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/ethereum/go-ethereum/rpc"
)

const stateChangeEventChanSize = 20000

type blockChain interface {
	SubscribeStateChangeEvents(ch chan<- core.StateChangeEvent) event.Subscription
	GetBlockByHash(hash common.Hash) *types.Block
	GetReceiptsByHash(hash common.Hash) types.Receipts
}

// IService is the state-diffing service interface
type IService interface {
	// APIs(), Protocols(), Start() and Stop()
	node.Service
	// Main event loop for processing state diffs
	Loop(stateChangeEventCh chan core.StateChangeEvent)
	// Method to subscribe to receive state diff processing output
	Subscribe(id rpc.ID, sub chan<- Payload, quitChan chan<- bool)
	// Method to unsubscribe from state diff processing
	Unsubscribe(id rpc.ID) error
}

// Service is the underlying struct for the state diffing service
type Service struct {
	// Used to sync access to the Subscriptions
	sync.Mutex
	// Used to build the state diff objects
	Builder Builder
	// Used to subscribe to chain events (blocks)
	BlockChain blockChain
	// Used to signal shutdown of the service
	QuitChan chan bool
	// A mapping of rpc.IDs to their subscription channels
	Subscriptions map[rpc.ID]Subscription
	// Cache the last block so that we can avoid having to lookup the next block's parent
	lastBlock *types.Block
	// Whether or not we have any subscribers; only if we do, do we processes state diffs
	subscribers int32
}

// NewStateDiffService creates a new statediff.Service
func NewStateDiffService(db ethdb.Database, blockChain *core.BlockChain, config Config) (*Service, error) {
	return &Service{
		Mutex:         sync.Mutex{},
		BlockChain:    blockChain,
		Builder:       NewBuilder(db, blockChain, config),
		QuitChan:      make(chan bool),
		Subscriptions: make(map[rpc.ID]Subscription),
	}, nil
}

// Protocols exports the services p2p protocols, this service has none
func (sds *Service) Protocols() []p2p.Protocol {
	return []p2p.Protocol{}
}

// APIs returns the RPC descriptors the statediff.Service offers
func (sds *Service) APIs() []rpc.API {
	return []rpc.API{
		{
			Namespace: APIName,
			Version:   APIVersion,
			Service:   NewPublicStateDiffAPI(sds),
			Public:    true,
		},
	}
}

// Loop is the main processing method
func (sds *Service) Loop(stateChangeEventCh chan core.StateChangeEvent) {
	stateChangeEventsSub := sds.BlockChain.SubscribeStateChangeEvents(stateChangeEventCh)
	defer stateChangeEventsSub.Unsubscribe()

	errCh := stateChangeEventsSub.Err()
	for {
		select {
		//Notify stateChangeEvent channel of events
		case stateChangeEvent := <-stateChangeEventCh:
			log.Info("Event received from stateChangeEventCh", "event", stateChangeEvent)
			processingErr := sds.processStateChanges(stateChangeEvent)
			if processingErr != nil {
				// The service loop continues even if processing one StateChangeEvent fails
				log.Error(fmt.Sprintf("Error processing state for block %d; error: %s ",
					stateChangeEvent.Block.Number(), processingErr.Error()))
			}
		case err := <-errCh:
			log.Warn("Error from state change event subscription, breaking loop", "error", err)
			sds.close()
			return
		case <-sds.QuitChan:
			log.Info("Quitting the statediffing process")
			sds.close()
			return
		}
	}
}

func (sds *Service) processStateChanges(stateChangeEvent core.StateChangeEvent) error {
	var accountDiffs []AccountDiff
	modifiedAccounts := stateChangeEvent.StateChanges.ModifiedAccounts
	for addr, modifiedAccount := range modifiedAccounts {
		//TODO: perhaps the AccountDiff struct should change such that the Value is
		// actually an Account instead of changing it to a byte array here and then
		// needing to change it back to an Account later


		//TODO: Also change AccountDiff.Storage to just a map instead of an array of StorageDiffs?
		accountBytes, err := rlp.EncodeToBytes(modifiedAccount.Account)
		if err != nil {
			return err
		}

		var storageDiffs []StorageDiff
		for k, v := range modifiedAccount.Storage {
			diff := StorageDiff{
				Key:   k[:],
				Value: v[:],
			}
			storageDiffs = append(storageDiffs, diff)
		}

		accountDiff := AccountDiff{
			Key:     addr[:],
			Value:   accountBytes,
			Storage: storageDiffs,
		}

		accountDiffs = append(accountDiffs, accountDiff)
	}

	stateDiff := StateDiff{
		BlockNumber:     stateChangeEvent.Block.Number(),
		BlockHash:       stateChangeEvent.Block.Hash(),
		UpdatedAccounts: accountDiffs,
		encoded:         nil,
		err:             nil,
	}

	stateDiffRlp, err := rlp.EncodeToBytes(stateDiff)
	if err != nil {
		return err
	}
	payload := Payload{
		StateDiffRlp: stateDiffRlp,
	}

	sds.send(payload)
	return nil
}

// Subscribe is used by the API to subscribe to the service loop
func (sds *Service) Subscribe(id rpc.ID, sub chan<- Payload, quitChan chan<- bool) {
	log.Info("Subscribing to the statediff service")
	if atomic.CompareAndSwapInt32(&sds.subscribers, 0, 1) {
		log.Info("State diffing subscription received; beginning statediff processing")
	}
	sds.Lock()
	sds.Subscriptions[id] = Subscription{
		PayloadChan: sub,
		QuitChan:    quitChan,
	}
	sds.Unlock()
}

// Unsubscribe is used to unsubscribe from the service loop
func (sds *Service) Unsubscribe(id rpc.ID) error {
	log.Info("Unsubscribing from the statediff service")
	sds.Lock()
	_, ok := sds.Subscriptions[id]
	if !ok {
		return fmt.Errorf("cannot unsubscribe; subscription for id %s does not exist", id)
	}
	delete(sds.Subscriptions, id)
	if len(sds.Subscriptions) == 0 {
		if atomic.CompareAndSwapInt32(&sds.subscribers, 1, 0) {
			log.Info("No more subscriptions; halting statediff processing")
		}
	}
	sds.Unlock()
	return nil
}

// Start is used to begin the service
func (sds *Service) Start(*p2p.Server) error {
	log.Info("Starting statediff service")

	stateChangeEventCh := make(chan core.StateChangeEvent, stateChangeEventChanSize)
	go sds.Loop(stateChangeEventCh)

	return nil
}

// Stop is used to close down the service
func (sds *Service) Stop() error {
	log.Info("Stopping statediff service")
	close(sds.QuitChan)
	return nil
}

// send is used to fan out and serve the payloads to all subscriptions
func (sds *Service) send(payload Payload) {
	sds.Lock()
	for id, sub := range sds.Subscriptions {
		select {
		case sub.PayloadChan <- payload:
			log.Info(fmt.Sprintf("sending state diff payload to subscription %s", id))
		default:
			log.Info(fmt.Sprintf("unable to send payload to subscription %s; channel has no receiver", id))
			// in this case, try to close the bad subscription and remove it
			select {
			case sub.QuitChan <- true:
				log.Info(fmt.Sprintf("closing subscription %s", id))
			default:
				log.Info(fmt.Sprintf("unable to close subscription %s; channel has no receiver", id))
			}
			delete(sds.Subscriptions, id)
		}
	}
	// If after removing all bad subscriptions we have none left, halt processing
	if len(sds.Subscriptions) == 0 {
		if atomic.CompareAndSwapInt32(&sds.subscribers, 1, 0) {
			log.Info("No more subscriptions; halting statediff processing")
		}
	}
	sds.Unlock()
}

// close is used to close all listening subscriptions
func (sds *Service) close() {
	sds.Lock()
	for id, sub := range sds.Subscriptions {
		select {
		case sub.QuitChan <- true:
			log.Info(fmt.Sprintf("closing subscription %s", id))
		default:
			log.Info(fmt.Sprintf("unable to close subscription %s; channel has no receiver", id))
		}
		delete(sds.Subscriptions, id)
	}
	sds.Unlock()
}
