package monitor

import (
	"context"
	"log"
	"runtime"
	"sync"
	"time"

	ethTypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/event"

	"quorumengineering/quorum-report/client"
	"quorumengineering/quorum-report/database"
	"quorumengineering/quorum-report/types"
)

// MonitorService starts all monitors. It pulls data from Quorum node and update the database.
type MonitorService struct {
	db           database.Database
	quorumClient client.Client
	blockMonitor *BlockMonitor
	batchWriter  *BatchWriter
	stopFeed     event.Feed
	totalWorkers int
}

func NewMonitorService(db database.Database, quorumClient client.Client, consensus string, tuningConfig types.TuningConfig) *MonitorService {
	batchWriteChan := make(chan *BlockAndTransactions, tuningConfig.BlockProcessingQueueSize)
	return &MonitorService{
		db:           db,
		quorumClient: quorumClient,
		blockMonitor: NewBlockMonitor(db, quorumClient, consensus, batchWriteChan),
		batchWriter:  NewBatchWriter(batchWriteChan, db),
		totalWorkers: 3 * runtime.NumCPU(),
	}
}

func (m *MonitorService) Start() error {
	log.Println("Start monitor service...")

	// Pulling historical blocks since the last persisted while continuously listening to ChainHeadEvent.
	// For every block received, pull transactions/ events related to the registered contracts.

	log.Println("Start to sync blocks...")

	// Start batch writer and workers
	m.startBatchWriter()
	m.startWorkers()

	go m.run()

	return nil
}

func (m *MonitorService) Stop() {
	m.stopFeed.Send(types.StopEvent{})
	log.Println("Monitor service stopped.")
}

func (m *MonitorService) subscribeStopEvent() (chan types.StopEvent, event.Subscription) {
	c := make(chan types.StopEvent)
	s := m.stopFeed.Subscribe(c)
	return c, s
}

func (m *MonitorService) startBatchWriter() {
	go func() {
		stopChan, stopSubscription := m.subscribeStopEvent()
		defer stopSubscription.Unsubscribe()
		m.batchWriter.Run(stopChan)
	}()
}

func (m *MonitorService) startWorkers() {
	for i := 0; i < m.totalWorkers; i++ {
		go func() {
			stopChan, stopSubscription := m.subscribeStopEvent()
			defer stopSubscription.Unsubscribe()
			m.blockMonitor.startWorker(stopChan)
		}()
	}
}

func (m *MonitorService) run() {
	stopChan, stopSubscription := m.subscribeStopEvent()
	defer stopSubscription.Unsubscribe()

	/*
		We want to sync historical blocks as well as listen to the chain head simultaneously,
		whilst also being able to abort if we are shutting down and retry later if the connection
		to Quorum is lost.

		1. This loop will kick off the processing of historical/chain head syncing.
			a) if an error occurs setting up the chain head subscription, wait for a timeout period and try again
			b) if an error occurs setting up the historical block sync, cancel the chain head sub, wait and try again
		2. If we receive a shutdown message, cancel the chain head listener, wait for the historical block sync to finish and return
		3. If the chain head sub has an error, close the "cancelChan" which will stop the historical sync

		Note: 	errors in the historical sync *after* it is set up will not propagate up to here, but instead be
				handled internally. If the historical sync is cancelled, it returns without giving an error, allowing
				the function to break its internal loop.

				we need to set up the historical sync every time since we may have missed some blocks whilst the
				chain head subscription was down
	*/

	for {
		chStopChan := make(chan bool)
		cancelChan := make(chan bool)
		var wg sync.WaitGroup
		wg.Add(1)

		if err := m.listenToChainHead(cancelChan, chStopChan); err != nil {
			log.Printf("Subscribe to chain head event error: %v. Retry in 1 second...\n", err)
			time.Sleep(time.Second)
			continue
		}
		if err := m.syncHistoricBlocks(cancelChan, &wg); err != nil {
			log.Printf("Sync historic blocks error: %v. Retry in 1 second...\n", err)
			close(chStopChan)
			time.Sleep(time.Second)
			continue
		}

		select {
		case <-stopChan:
			close(chStopChan)
			<-cancelChan
			wg.Wait()
			return
		case <-cancelChan:
			wg.Wait()
			log.Println("Retry in 1 second...")
			time.Sleep(time.Second)
		}
	}
}

func (m *MonitorService) syncHistoricBlocks(cancelChan chan bool, wg *sync.WaitGroup) error {
	currentBlockNumber, err := m.blockMonitor.currentBlockNumber()
	if err != nil {
		return err
	}
	log.Printf("Current block head is: %v.\n", currentBlockNumber)
	lastPersisted, err := m.db.GetLastPersistedBlockNumber()
	if err != nil {
		return err
	}

	// Sync is called in a go routine so that it doesn't block main process.
	go func() {
		defer log.Println("Returning from historical block processing.")
		defer wg.Done()
		err := m.blockMonitor.syncBlocks(lastPersisted+1, currentBlockNumber, cancelChan)
		for err != nil {
			log.Printf("Sync historic blocks up to %v failed: %v.\n", currentBlockNumber, err)
			time.Sleep(time.Second)
			err = m.blockMonitor.syncBlocks(err.EndBlockNumber(), currentBlockNumber, cancelChan)
		}
	}()

	return nil
}

func (m *MonitorService) listenToChainHead(cancelChan chan bool, stopChan chan bool) error {
	headers := make(chan *ethTypes.Header)
	sub, err := m.quorumClient.SubscribeNewHead(context.Background(), headers)
	if err != nil {
		return err
	}

	go func() {
		defer close(cancelChan)
		log.Println("Starting chain head listener.")
		for {
			select {
			case err := <-sub.Err():
				log.Printf("Chain head event subscription error: %v.\n", err)
				return
			case header := <-headers:
				m.blockMonitor.processChainHead(header)
			case <-stopChan:
				log.Println("Stopping chain head listener.")
				return
			}
		}
	}()

	return nil
}
