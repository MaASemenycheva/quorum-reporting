package monitor

import (
	"context"
	"log"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/mitchellh/mapstructure"

	"quorumengineering/quorum-report/client"
	"quorumengineering/quorum-report/database"
	"quorumengineering/quorum-report/graphql"
	"quorumengineering/quorum-report/types"
)

type TransactionMonitor struct {
	db           database.Database
	quorumClient client.Client
}

func NewTransactionMonitor(db database.Database, quorumClient client.Client) *TransactionMonitor {
	return &TransactionMonitor{db, quorumClient}
}

func (tm *TransactionMonitor) PullTransactions(block *types.Block) error {
	log.Printf("Pull all transactions for block %v.\n", block.Number)

	for _, txHash := range block.Transactions {
		// 1. Query transaction details by graphql.
		tx, err := tm.createTransaction(txHash)
		if err != nil {
			return err
		}
		log.Println(tx.Hash.Hex())
		// 2. Write transactions to DB.
		err = tm.db.WriteTransaction(tx)
		if err != nil {
			return err
		}
	}
	return nil
}

func (tm *TransactionMonitor) createTransaction(hash common.Hash) (*types.Transaction, error) {
	var (
		resp     map[string]interface{}
		txOrigin graphql.Transaction
	)
	err := tm.quorumClient.ExecuteGraphQLQuery(context.Background(), &resp, graphql.TransactionDetailQuery(hash))
	if err != nil {
		// TODO: if quorum node is down, reconnect?
		return nil, err
	}
	err = mapstructure.Decode(resp["transaction"].(map[string]interface{}), &txOrigin)
	if err != nil {
		return nil, err
	}

	// Create reporting transaction struct fields.
	blockNumber, err := hexutil.DecodeUint64(txOrigin.Block.Number)
	if err != nil {
		return nil, err
	}
	nonce, err := hexutil.DecodeUint64(txOrigin.Nonce)
	if err != nil {
		return nil, err
	}
	value, err := hexutil.DecodeUint64(txOrigin.Value)
	if err != nil {
		return nil, err
	}
	gas, err := hexutil.DecodeUint64(txOrigin.Gas)
	if err != nil {
		return nil, err
	}
	gasUsed, err := hexutil.DecodeUint64(txOrigin.GasUsed)
	if err != nil {
		return nil, err
	}
	cumulativeGasUsed, err := hexutil.DecodeUint64(txOrigin.CumulativeGasUsed)
	if err != nil {
		return nil, err
	}

	tx := &types.Transaction{
		Hash:              common.HexToHash(txOrigin.Hash),
		Status:            txOrigin.Status == "0x1",
		BlockNumber:       blockNumber,
		Index:             txOrigin.Index,
		Nonce:             nonce,
		From:              common.HexToAddress(txOrigin.From.Address),
		To:                common.HexToAddress(txOrigin.To.Address),
		Value:             value,
		Gas:               gas,
		GasUsed:           gasUsed,
		CumulativeGasUsed: cumulativeGasUsed,
		CreatedContract:   common.HexToAddress(txOrigin.CreatedContract.Address),
		Data:              hexutil.MustDecode(txOrigin.InputData),
		PrivateData:       hexutil.MustDecode(txOrigin.PrivateInputData),
		IsPrivate:         txOrigin.IsPrivate,
	}
	events := []*types.Event{}
	for _, l := range txOrigin.Logs {
		topics := []common.Hash{}
		for _, t := range l.Topics {
			topics = append(topics, common.HexToHash(t))
		}
		e := &types.Event{
			Index:           l.Index,
			Address:         common.HexToAddress(l.Account.Address),
			Topics:          topics,
			Data:            hexutil.MustDecode(l.Data),
			BlockNumber:     tx.BlockNumber,
			TransactionHash: tx.Hash,
		}
		events = append(events, e)
	}
	tx.Events = events

	// Trace internal calls of the transaction
	// Reference: https://github.com/ethereum/go-ethereum/issues/3128
	type TraceConfig struct {
		Tracer string
	}
	err = tm.quorumClient.RPCCall(context.Background(), &resp, "debug_traceTransaction", tx.Hash, &TraceConfig{Tracer: "callTracer"})
	if err != nil {
		return nil, err
	}
	if resp["calls"] != nil {
		respCalls := resp["calls"].([]interface{})
		tx.InternalCalls = make([]*types.InternalCall, len(respCalls))
		for i, respCall := range respCalls {
			respCallMap := respCall.(map[string]interface{})
			gas, err := hexutil.DecodeUint64(respCallMap["gas"].(string))
			if err != nil {
				return nil, err
			}
			gasUsed, err := hexutil.DecodeUint64(respCallMap["gasUsed"].(string))
			if err != nil {
				return nil, err
			}
			value = uint64(0)
			if val, ok := respCallMap["value"].(string); ok {
				value, err = hexutil.DecodeUint64(val)
				if err != nil {
					return nil, err
				}
			}
			tx.InternalCalls[i] = &types.InternalCall{
				From:    common.HexToAddress(respCallMap["from"].(string)),
				To:      common.HexToAddress(respCallMap["to"].(string)),
				Gas:     gas,
				GasUsed: gasUsed,
				Value:   value,
				Input:   hexutil.MustDecode(respCallMap["input"].(string)),
				Output:  hexutil.MustDecode(respCallMap["output"].(string)),
				Type:    respCallMap["type"].(string),
			}
		}
	}
	return tx, nil
}