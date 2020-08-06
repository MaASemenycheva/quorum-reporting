package elasticsearch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/elastic/go-elasticsearch/v7/esapi"
	"github.com/elastic/go-elasticsearch/v7/esutil"
	"github.com/mitchellh/mapstructure"
	"math/big"
	"quorumengineering/quorum-report/database"
	"quorumengineering/quorum-report/log"
	"quorumengineering/quorum-report/types"
	"strconv"
	"strings"
	"sync"
)

type ElasticsearchDB struct {
	apiClient APIClient
}

func New(client APIClient) (*ElasticsearchDB, error) {
	db := &ElasticsearchDB{
		apiClient: client,
	}

	initialized, err := db.checkIsInitialized()
	if err != nil {
		log.Error("Error communicating with ElasticSearch", "err", err)
		log.Error("Please check all ElasticSearch settings, including certificates, URL and username/password.")
		return nil, err
	}
	if !initialized {
		if err := db.init(); err != nil {
			return nil, err
		}
	}
	return db, nil
}

func (es *ElasticsearchDB) init() error {
	mapping := `{"mappings":{"properties": {"internalCalls": {"type": "nested" }}}}`
	createRequest := esapi.IndicesCreateRequest{
		Index: TransactionIndex,
		Body:  strings.NewReader(mapping),
	}

	//TODO: check error scenarios
	es.apiClient.DoRequest(createRequest)

	es.apiClient.DoRequest(esapi.IndicesCreateRequest{Index: ContractIndex})
	es.apiClient.DoRequest(esapi.IndicesCreateRequest{Index: TemplateIndex})
	es.apiClient.DoRequest(esapi.IndicesCreateRequest{Index: StorageIndex})
	es.apiClient.DoRequest(esapi.IndicesCreateRequest{Index: EventIndex})
	es.apiClient.DoRequest(esapi.IndicesCreateRequest{Index: MetaIndex})
	es.apiClient.DoRequest(esapi.IndicesCreateRequest{Index: TokenIndex})

	req := esapi.IndexRequest{
		Index:      MetaIndex,
		DocumentID: "lastPersisted",
		Body:       strings.NewReader(`{"lastPersisted": 0}`),
		Refresh:    "true",
		OpType:     "create",
	}
	es.apiClient.DoRequest(req)

	return nil
}

//AddressDB
func (es *ElasticsearchDB) AddAddresses(addresses []types.Address) error {
	if len(addresses) == 0 {
		return nil
	}
	// Only use bulk update if more than one address is given
	if len(addresses) > 1 {
		bi := es.apiClient.GetBulkHandler(ContractIndex)

		var (
			wg        sync.WaitGroup
			returnErr error
		)
		for _, address := range addresses {
			contract := Contract{
				Address:             address,
				TemplateName:        address.String(),
				CreationTransaction: "",
				LastFiltered:        0,
			}
			wg.Add(1)
			bi.Add(
				context.Background(),
				esutil.BulkIndexerItem{
					Action:     "create",
					DocumentID: address.String(),
					Body:       esutil.NewJSONReader(contract),
					OnSuccess: func(ctx context.Context, item esutil.BulkIndexerItem, item2 esutil.BulkIndexerResponseItem) {
						wg.Done()
					},
					OnFailure: func(ctx context.Context, item esutil.BulkIndexerItem, item2 esutil.BulkIndexerResponseItem, err error) {
						returnErr = err
						wg.Done()
					},
				},
			)
		}

		wg.Wait()
		return returnErr
	}
	// add single address
	contract := Contract{
		Address:             addresses[0],
		TemplateName:        addresses[0].String(),
		CreationTransaction: "",
		LastFiltered:        0,
	}

	req := esapi.IndexRequest{
		Index:      ContractIndex,
		DocumentID: addresses[0].String(),
		Body:       esutil.NewJSONReader(contract),
		Refresh:    "true",
		OpType:     "create", //This will only create if the contract does not exist
	}
	_, err := es.apiClient.DoRequest(req)
	return err
}

func (es *ElasticsearchDB) AddAddressFrom(address types.Address, from uint64) error {
	contract := Contract{
		Address:             address,
		TemplateName:        address.String(),
		CreationTransaction: "",
		LastFiltered:        from - 1,
	}

	req := esapi.IndexRequest{
		Index:      ContractIndex,
		DocumentID: address.String(),
		Body:       esutil.NewJSONReader(contract),
		Refresh:    "true",
		OpType:     "create", //This will only create if the contract does not exist
	}
	_, err := es.apiClient.DoRequest(req)
	return err
}

func (es *ElasticsearchDB) DeleteAddress(address types.Address) error {
	deleteRequest := esapi.DeleteRequest{
		Index:      ContractIndex,
		DocumentID: address.String(),
		Refresh:    "true",
	}

	_, err := es.apiClient.DoRequest(deleteRequest)
	if err != nil {
		return errors.New("error deleting address: " + err.Error())
	}

	//TODO: delete data from other indices (event + storage)
	return nil
}

func (es *ElasticsearchDB) GetAddresses() ([]types.Address, error) {
	results, err := es.apiClient.ScrollAllResults(ContractIndex, QueryAllAddressesTemplate)
	if err != nil {
		return nil, errors.New("error fetching addresses: " + err.Error())
	}
	converted := make([]types.Address, len(results))
	for i, result := range results {
		data := result.(map[string]interface{})["_source"].(map[string]interface{})
		addr := data["address"].(string)
		converted[i] = types.NewAddress(addr)
	}

	return converted, nil
}

func (es *ElasticsearchDB) GetContractTemplate(address types.Address) (string, error) {
	contract, err := es.getContractByAddress(address)
	if err != nil {
		return "", err
	}
	return contract.TemplateName, nil
}

//TemplateDB
func (es *ElasticsearchDB) GetContractABI(address types.Address) (string, error) {

	contract, err := es.getContractByAddress(address)
	if err != nil && err != database.ErrNotFound {
		return "", err
	}

	if contract != nil {
		template, err := es.getTemplateByName(contract.TemplateName)
		if err != nil && err != database.ErrNotFound {
			return "", err
		}
		if template != nil {
			return template.ABI, nil
		}
	}
	return "", nil
}

func (es *ElasticsearchDB) GetStorageLayout(address types.Address) (string, error) {
	contract, err := es.getContractByAddress(address)
	if err != nil && err != database.ErrNotFound {
		return "", err
	}
	if contract != nil {
		template, err := es.getTemplateByName(contract.TemplateName)
		if err != nil && err != database.ErrNotFound {
			return "", err
		}
		if template != nil {
			return template.StorageABI, nil
		}
	}
	return "", nil
}

func (es *ElasticsearchDB) AddTemplate(name string, abi string, layout string) error {
	template := Template{
		TemplateName: name,
		ABI:          abi,
		StorageABI:   layout,
	}

	req := esapi.IndexRequest{
		Index:      TemplateIndex,
		DocumentID: name,
		Body:       esutil.NewJSONReader(template),
		Refresh:    "true",
	}
	_, err := es.apiClient.DoRequest(req)
	return err
}

func (es *ElasticsearchDB) AssignTemplate(address types.Address, name string) error {
	return es.updateContract(address, "templateName", name)
}

func (es *ElasticsearchDB) GetTemplates() ([]string, error) {
	results, err := es.apiClient.ScrollAllResults(TemplateIndex, QueryAllTemplateNamesTemplate)
	if err != nil {
		return nil, errors.New("error fetching templates: " + err.Error())
	}
	converted := make([]string, len(results))
	for i, result := range results {
		data := result.(map[string]interface{})["_source"].(map[string]interface{})
		converted[i] = data["templateName"].(string)
	}
	return converted, nil
}

func (es *ElasticsearchDB) GetTemplateDetails(templateName string) (*types.Template, error) {
	template, err := es.getTemplateByName(templateName)
	if err != nil {
		return nil, err
	}
	return &types.Template{
		TemplateName:  templateName,
		ABI:           template.ABI,
		StorageLayout: template.StorageABI,
	}, nil
}

// BlockDB
func (es *ElasticsearchDB) WriteBlock(block *types.Block) error {
	req := esapi.IndexRequest{
		Index:      BlockIndex,
		DocumentID: strconv.FormatUint(block.Number, 10),
		Body:       esutil.NewJSONReader(block),
		Refresh:    "true",
	}

	if _, err := es.apiClient.DoRequest(req); err != nil {
		return err
	}
	return es.updateLastPersisted(block.Number)
}

func (es *ElasticsearchDB) WriteBlocks(blocks []*types.Block) error {
	if len(blocks) == 0 {
		return nil
	}
	if len(blocks) == 1 {
		return es.WriteBlock(blocks[0])
	}

	bi := es.apiClient.GetBulkHandler(BlockIndex)
	var (
		wg        sync.WaitGroup
		returnErr error
	)
	wg.Add(len(blocks))
	for _, block := range blocks {
		_ = bi.Add(context.Background(), esutil.BulkIndexerItem{
			Action:     "create",
			DocumentID: strconv.FormatUint(block.Number, 10),
			Body:       esutil.NewJSONReader(block),
			OnSuccess: func(ctx context.Context, item esutil.BulkIndexerItem, item2 esutil.BulkIndexerResponseItem) {
				wg.Done()
			},
			OnFailure: func(ctx context.Context, item esutil.BulkIndexerItem, item2 esutil.BulkIndexerResponseItem, err error) {
				returnErr = err
				wg.Done()
			},
		})
	}
	wg.Wait()
	if returnErr != nil {
		return returnErr
	}

	//find lowest block number
	lowest := blocks[0].Number
	for _, block := range blocks {
		if block.Number < lowest {
			lowest = block.Number
		}
	}
	return es.updateLastPersisted(lowest)
}

func (es *ElasticsearchDB) ReadBlock(number uint64) (*types.Block, error) {
	fetchReq := esapi.GetRequest{
		Index:      BlockIndex,
		DocumentID: strconv.FormatUint(number, 10),
	}

	body, err := es.apiClient.DoRequest(fetchReq)
	if err != nil {
		return nil, err
	}

	var blockResult BlockQueryResult
	if err = json.Unmarshal(body, &blockResult); err != nil {
		return nil, err
	}
	return blockResult.Source, nil
}

func (es *ElasticsearchDB) GetLastPersistedBlockNumber() (uint64, error) {
	fetchReq := esapi.GetRequest{
		Index:      MetaIndex,
		DocumentID: "lastPersisted",
	}

	body, err := es.apiClient.DoRequest(fetchReq)
	if err != nil {
		return 0, err
	}

	var lastPersisted LastPersistedResult
	if err = json.Unmarshal(body, &lastPersisted); err != nil {
		return 0, err
	}
	return lastPersisted.Source.LastPersisted, nil
}

// TransactionDB
func (es *ElasticsearchDB) WriteTransaction(transaction *types.Transaction) error {
	req := esapi.IndexRequest{
		Index:      TransactionIndex,
		DocumentID: transaction.Hash.String(),
		Body:       esutil.NewJSONReader(transaction),
		Refresh:    "true",
	}

	_, err := es.apiClient.DoRequest(req)
	return err
}

func (es *ElasticsearchDB) WriteTransactions(transactions []*types.Transaction) error {
	if len(transactions) == 0 {
		return nil
	}
	if len(transactions) == 1 {
		return es.WriteTransaction(transactions[0])
	}

	bi := es.apiClient.GetBulkHandler(TransactionIndex)

	var (
		wg        sync.WaitGroup
		returnErr error
	)
	wg.Add(len(transactions))
	for _, transaction := range transactions {
		_ = bi.Add(
			context.Background(),
			esutil.BulkIndexerItem{
				Action:     "create",
				DocumentID: transaction.Hash.String(),
				Body:       esutil.NewJSONReader(transaction),
				OnSuccess: func(ctx context.Context, item esutil.BulkIndexerItem, item2 esutil.BulkIndexerResponseItem) {
					wg.Done()
				},
				OnFailure: func(ctx context.Context, item esutil.BulkIndexerItem, item2 esutil.BulkIndexerResponseItem, err error) {
					returnErr = err
					wg.Done()
				},
			})
	}

	wg.Wait()
	return returnErr
}

func (es *ElasticsearchDB) ReadTransaction(hash types.Hash) (*types.Transaction, error) {
	fetchReq := esapi.GetRequest{
		Index:      TransactionIndex,
		DocumentID: hash.String(),
	}

	body, err := es.apiClient.DoRequest(fetchReq)
	if err != nil {
		return nil, err
	}

	var transactionResult TransactionQueryResult
	if err = json.Unmarshal(body, &transactionResult); err != nil {
		return nil, err
	}
	return transactionResult.Source, nil
}

// IndexDB

func (es *ElasticsearchDB) IndexBlocks(addresses []types.Address, blocks []*types.Block) error {
	indexer := NewBlockIndexer(addresses, blocks, es)
	if err := indexer.Index(); err != nil {
		return err
	}
	return es.updateAllLastFiltered(addresses, blocks[len(blocks)-1].Number)
}

func (es *ElasticsearchDB) IndexStorage(rawStorage map[types.Address]*types.AccountState, blockNumber uint64) error {
	biState := es.apiClient.GetBulkHandler(StateIndex)
	biStorage := es.apiClient.GetBulkHandler(StorageIndex)

	var (
		wg        sync.WaitGroup
		returnErr error
	)
	for address, dumpAccount := range rawStorage {
		wg.Add(2)
		stateObj := State{
			Address:     address,
			BlockNumber: blockNumber,
			StorageRoot: dumpAccount.Root,
		}
		converted := make([]StorageEntry, 0, len(dumpAccount.Storage))
		for slot, val := range dumpAccount.Storage {
			converted = append(converted, StorageEntry{slot, val})
		}
		storageMap := Storage{
			StorageRoot: dumpAccount.Root,
			StorageMap:  converted,
		}

		_ = biState.Add(
			context.Background(),
			esutil.BulkIndexerItem{
				Action:     "create",
				DocumentID: address.String() + "-" + strconv.FormatUint(blockNumber, 10),
				Body:       esutil.NewJSONReader(stateObj),
				OnSuccess: func(ctx context.Context, item esutil.BulkIndexerItem, item2 esutil.BulkIndexerResponseItem) {
					wg.Done()
				},
				OnFailure: func(ctx context.Context, item esutil.BulkIndexerItem, item2 esutil.BulkIndexerResponseItem, err error) {
					returnErr = err
					wg.Done()
				},
			},
		)
		_ = biStorage.Add(
			context.Background(),
			esutil.BulkIndexerItem{
				Action:     "create",
				DocumentID: dumpAccount.Root.String(),
				Body:       esutil.NewJSONReader(storageMap),
				OnSuccess: func(ctx context.Context, item esutil.BulkIndexerItem, item2 esutil.BulkIndexerResponseItem) {
					wg.Done()
				},
				OnFailure: func(ctx context.Context, item esutil.BulkIndexerItem, item2 esutil.BulkIndexerResponseItem, err error) {
					returnErr = err
					wg.Done()
				},
			},
		)
	}
	wg.Wait()
	return returnErr
}

func (es *ElasticsearchDB) GetContractCreationTransaction(address types.Address) (types.Hash, error) {
	contract, err := es.getContractByAddress(address)
	if err != nil {
		return "", err
	}
	return contract.CreationTransaction, nil
}

func (es *ElasticsearchDB) GetAllTransactionsToAddress(address types.Address, options *types.QueryOptions) ([]types.Hash, error) {
	queryString := fmt.Sprintf(QueryByToAddressWithOptionsTemplate(options), address.String())

	from := options.PageSize * options.PageNumber
	if from+options.PageSize > 1000 {
		return nil, ErrPaginationLimitExceeded
	}
	req := esapi.SearchRequest{
		Index: []string{TransactionIndex},
		Body:  strings.NewReader(queryString),
		From:  &from,
		Size:  &options.PageSize,
		Sort:  []string{"blockNumber:desc", "index:asc"},
	}
	results, err := es.doSearchRequest(req)
	if err != nil {
		return nil, err
	}

	converted := make([]types.Hash, len(results.Hits.Hits))
	for i, result := range results.Hits.Hits {
		hsh := result.Source["hash"].(string)
		converted[i] = types.NewHash(hsh)
	}

	return converted, nil
}

func (es *ElasticsearchDB) GetTransactionsToAddressTotal(address types.Address, options *types.QueryOptions) (uint64, error) {
	queryString := fmt.Sprintf(QueryByToAddressWithOptionsTemplate(options), address.String())

	req := esapi.CountRequest{
		Index: []string{TransactionIndex},
		Body:  strings.NewReader(queryString),
	}
	results, err := es.doCountRequest(req)
	if err != nil {
		return 0, err
	}
	return results.Count, nil
}

func (es *ElasticsearchDB) GetAllTransactionsInternalToAddress(address types.Address, options *types.QueryOptions) ([]types.Hash, error) {
	queryString := fmt.Sprintf(QueryInternalTransactionsWithOptionsTemplate(options), address.String())

	from := options.PageSize * options.PageNumber
	if from+options.PageSize > 1000 {
		return nil, ErrPaginationLimitExceeded
	}
	req := esapi.SearchRequest{
		Index: []string{TransactionIndex},
		Body:  strings.NewReader(queryString),
		From:  &from,
		Size:  &options.PageSize,
		Sort:  []string{"blockNumber:desc", "index:asc"},
	}
	results, err := es.doSearchRequest(req)
	if err != nil {
		return nil, err
	}

	converted := make([]types.Hash, len(results.Hits.Hits))
	for i, result := range results.Hits.Hits {
		hsh := result.Source["hash"].(string)
		converted[i] = types.NewHash(hsh)
	}

	return converted, nil
}

func (es *ElasticsearchDB) GetTransactionsInternalToAddressTotal(address types.Address, options *types.QueryOptions) (uint64, error) {
	queryString := fmt.Sprintf(QueryInternalTransactionsWithOptionsTemplate(options), address.String())

	req := esapi.CountRequest{
		Index: []string{TransactionIndex},
		Body:  strings.NewReader(queryString),
	}
	results, err := es.doCountRequest(req)
	if err != nil {
		return 0, err
	}
	return results.Count, nil
}

func (es *ElasticsearchDB) GetAllEventsFromAddress(address types.Address, options *types.QueryOptions) ([]*types.Event, error) {
	queryString := fmt.Sprintf(QueryByAddressWithOptionsTemplate(options), address.String())

	from := options.PageSize * options.PageNumber
	if from+options.PageSize > 1000 {
		return nil, ErrPaginationLimitExceeded
	}
	req := esapi.SearchRequest{
		Index: []string{EventIndex},
		Body:  strings.NewReader(queryString),
		From:  &from,
		Size:  &options.PageSize,
		Sort:  []string{"blockNumber:desc", "index:asc"},
	}
	results, err := es.doSearchRequest(req)
	if err != nil {
		return nil, err
	}

	convertedList := make([]*types.Event, len(results.Hits.Hits))
	for i, result := range results.Hits.Hits {
		marshalled, _ := json.Marshal(result.Source)
		var event types.Event
		if err = json.Unmarshal(marshalled, &event); err != nil {
			return nil, err
		}
		convertedList[i] = &event
	}

	return convertedList, nil
}

func (es *ElasticsearchDB) GetEventsFromAddressTotal(address types.Address, options *types.QueryOptions) (uint64, error) {
	queryString := fmt.Sprintf(QueryByAddressWithOptionsTemplate(options), address.String())

	req := esapi.CountRequest{
		Index: []string{EventIndex},
		Body:  strings.NewReader(queryString),
	}
	results, err := es.doCountRequest(req)
	if err != nil {
		return 0, err
	}
	return results.Count, nil
}

func (es *ElasticsearchDB) GetStorage(address types.Address, blockNumber uint64) (map[types.Hash]string, error) {
	fetchReq := esapi.GetRequest{
		Index:      StateIndex,
		DocumentID: address.String() + "-" + strconv.FormatUint(blockNumber, 10),
	}
	body, err := es.apiClient.DoRequest(fetchReq)
	if err != nil && err != database.ErrNotFound {
		return nil, err
	}
	if err == database.ErrNotFound {
		return nil, nil
	}
	var stateResult StateQueryResult
	if err = json.Unmarshal(body, &stateResult); err != nil {
		return nil, err
	}

	storageFetchReq := esapi.GetRequest{
		Index:      StorageIndex,
		DocumentID: stateResult.Source.StorageRoot.String(),
	}
	body, err = es.apiClient.DoRequest(storageFetchReq)
	if err != nil && err != database.ErrNotFound {
		return nil, err
	}
	if err == database.ErrNotFound {
		return nil, nil
	}
	var storageResult StorageQueryResult
	if err = json.Unmarshal(body, &storageResult); err != nil {
		return nil, err
	}
	converted := make(map[types.Hash]string)
	for _, storageEntry := range storageResult.Source.StorageMap {
		converted[storageEntry.Key] = storageEntry.Value
	}
	return converted, nil
}

func (es *ElasticsearchDB) GetLastFiltered(address types.Address) (uint64, error) {
	contract, err := es.getContractByAddress(address)
	if err != nil {
		return 0, err
	}
	return contract.LastFiltered, nil
}

// Token DB
func (es *ElasticsearchDB) RecordNewBalance(contract types.Address, holder types.Address, block uint64, amount *big.Int) error {
	tokenInfo := TokenHolder{
		Contract:    contract,
		Holder:      holder,
		BlockNumber: block,
		Amount:      amount.String(),
	}

	req := esapi.IndexRequest{
		Index:      TokenIndex,
		DocumentID: fmt.Sprintf("%s-%s-%d", contract.String(), holder.String(), block),
		Body:       esutil.NewJSONReader(tokenInfo),
		Refresh:    "true",
		OpType:     "create", //This will only create if the contract does not exist
	}

	_, err := es.apiClient.DoRequest(req)
	return err
}

func (es *ElasticsearchDB) GetBalance(contract types.Address, holder types.Address, options *types.QueryOptions) (map[uint64]*big.Int, error) {
	queryString := fmt.Sprintf(QueryTokenBalanceAtBlockRange(options), contract.String(), holder.String())

	from := options.PageSize * options.PageNumber
	if from+options.PageSize > 1000 {
		return nil, ErrPaginationLimitExceeded
	}
	req := esapi.SearchRequest{
		Index: []string{TokenIndex},
		Body:  strings.NewReader(queryString),
		From:  &from,
		Size:  &options.PageSize,
		Sort:  []string{"blockNumber:desc"},
	}
	results, err := es.doSearchRequest(req)
	if err != nil {
		return nil, err
	}

	balanceMap := make(map[uint64]*big.Int)
	for _, result := range results.Hits.Hits {
		blockNumber := uint64(result.Source["blockNumber"].(float64))
		tokenAmount, success := new(big.Int).SetString(result.Source["amount"].(string), 10)
		if !success {
			return nil, errors.New("some error")
		}
		//block, _ := strconv.ParseUint(blockNumber, 10, 64)
		balanceMap[blockNumber] = tokenAmount
	}

	return balanceMap, nil
}

func (es *ElasticsearchDB) RecordERC721Token(contract types.Address, holder types.Address, block uint64, tokenId *big.Int) error {
	//find old entry
	existingTokenEntry, errExisting := es.ERC721TokenByTokenID(contract, block-1, tokenId)
	if errExisting != nil && errExisting != database.ErrNotFound {
		return errExisting
	}

	paddedTokenId := fmt.Sprintf("%085d", tokenId)
	first, _ := strconv.ParseUint(paddedTokenId[0:17], 10, 64)
	second, _ := strconv.ParseUint(paddedTokenId[17:34], 10, 64)
	third, _ := strconv.ParseUint(paddedTokenId[34:51], 10, 64)
	fourth, _ := strconv.ParseUint(paddedTokenId[51:68], 10, 64)
	fifth, _ := strconv.ParseUint(paddedTokenId[68:85], 10, 64)

	//add new entry
	tokenHolderInfo := SortableERC721Token{
		types.ERC721Token{
			Contract:  contract,
			Holder:    holder,
			Token:     tokenId.String(),
			HeldFrom:  block,
			HeldUntil: nil,
		},
		first, second, third, fourth, fifth,
	}

	req := esapi.IndexRequest{
		Index:      TokenIndex,
		DocumentID: fmt.Sprintf("%s-%s-%d", contract.String(), tokenId.String(), block),
		Body:       esutil.NewJSONReader(tokenHolderInfo),
		Refresh:    "true",
		OpType:     "create", //This will only create if the contract does not exist
	}

	if _, err := es.apiClient.DoRequest(req); err != nil {
		return err
	}

	/////

	if errExisting == database.ErrNotFound {
		return nil
	}

	//update the older entry
	query := map[string]interface{}{
		"doc": map[string]interface{}{
			"heldUntil": block - 1,
		},
	}

	updateRequest := esapi.UpdateRequest{
		Index:      TokenIndex,
		DocumentID: fmt.Sprintf("%s-%s-%d", contract.String(), tokenId.String(), existingTokenEntry.HeldFrom),
		Body:       esutil.NewJSONReader(query),
		Refresh:    "true",
	}

	_, err := es.apiClient.DoRequest(updateRequest)
	return err
}

func (es *ElasticsearchDB) ERC721TokenByTokenID(contract types.Address, block uint64, tokenId *big.Int) (types.ERC721Token, error) {
	formattedQuery := fmt.Sprintf(QueryERC721TokenAtBlock(), contract.String(), tokenId.String(), block)

	pageSize := 1
	searchReq := esapi.SearchRequest{
		Index: []string{TokenIndex},
		Body:  strings.NewReader(formattedQuery),
		Size:  &pageSize,
	}

	results, err := es.doSearchRequest(searchReq)
	if err != nil {
		return types.ERC721Token{}, err
	}

	if len(results.Hits.Hits) == 0 {
		return types.ERC721Token{}, database.ErrNotFound
	}

	var tokenResult types.ERC721Token
	err = mapstructure.Decode(results.Hits.Hits[0].Source, &tokenResult)
	return tokenResult, err
}

func (es *ElasticsearchDB) ERC721TokensForAccountAtBlock(contract types.Address, holder types.Address, block uint64, options *types.TokenQueryOptions) ([]types.ERC721Token, error) {
	formattedQuery := fmt.Sprintf(QueryERC721HolderAtBlock(options.BeginTokenId, options.EndTokenId), contract.String(), holder.String(), block, block)

	from := options.PageSize * options.PageNumber
	if from+options.PageSize > 1000 {
		return nil, ErrPaginationLimitExceeded
	}

	searchReq := esapi.SearchRequest{
		Index: []string{TokenIndex},
		Body:  strings.NewReader(formattedQuery),
		From:  &from,
		Size:  &options.PageSize,
		Sort:  []string{"first:desc", "second:desc", "third:desc", "fourth:desc", "fifth:desc"},
	}

	results, err := es.doSearchRequest(searchReq)
	if err != nil {
		return nil, err
	}

	convertedResults := make([]types.ERC721Token, 0, len(results.Hits.Hits))
	for _, result := range results.Hits.Hits {
		var tokenResult types.ERC721Token
		if err := mapstructure.Decode(result.Source, &tokenResult); err != nil {
			return nil, err
		}
		convertedResults = append(convertedResults, tokenResult)
	}
	return convertedResults, nil
}

func (es *ElasticsearchDB) AllERC721TokensAtBlock(contract types.Address, block uint64, options *types.TokenQueryOptions) ([]types.ERC721Token, error) {
	formattedQuery := fmt.Sprintf(QueryERC721AllTokensAtBlock(options.BeginTokenId, options.EndTokenId), contract.String(), block, block)

	from := options.PageSize * options.PageNumber
	if from+options.PageSize > 1000 {
		return nil, ErrPaginationLimitExceeded
	}

	searchReq := esapi.SearchRequest{
		Index: []string{TokenIndex},
		Body:  strings.NewReader(formattedQuery),
		From:  &from,
		Size:  &options.PageSize,
		Sort:  []string{"first:desc", "second:desc", "third:desc", "fourth:desc", "fifth:desc"},
	}

	results, err := es.doSearchRequest(searchReq)
	if err != nil {
		return nil, err
	}

	convertedResults := make([]types.ERC721Token, 0, len(results.Hits.Hits))
	for _, result := range results.Hits.Hits {
		var tokenResult types.ERC721Token
		if err := mapstructure.Decode(result.Source, &tokenResult); err != nil {
			return nil, err
		}
		convertedResults = append(convertedResults, tokenResult)
	}
	return convertedResults, nil
}

func (es *ElasticsearchDB) AllHoldersAtBlock(contract types.Address, block uint64, options *types.TokenQueryOptions) ([]types.Address, error) {
	if options.PageSize > 1000 {
		return nil, ErrPaginationLimitExceeded
	}

	afterQuery := ""
	if options.After != "" {
		afterQuery = fmt.Sprintf(`"after": { "holder": "%s"},`, options.After)
	}

	formattedQuery := fmt.Sprintf(QueryERC721AllHoldersAtBlock(), contract.String(), block, block, options.PageSize, afterQuery)

	searchReq := esapi.SearchRequest{
		Index: []string{TokenIndex},
		Body:  strings.NewReader(formattedQuery),
	}

	results, err := es.doSearchRequest(searchReq)
	if err != nil {
		return nil, err
	}

	var aggResult ERC721HolderAggregateResult
	rawAggResult := results.Aggregations.Results
	if err := mapstructure.Decode(rawAggResult, &aggResult); err != nil {
		return nil, err
	}

	convertedResults := make([]types.Address, 0, len(aggResult.Buckets))
	for _, result := range aggResult.Buckets {
		convertedResults = append(convertedResults, types.NewAddress(result.Key.Holder))
	}
	fmt.Println(results)
	return convertedResults, nil
}

// Internal functions

func (es *ElasticsearchDB) checkIsInitialized() (bool, error) {
	fetchReq := esapi.CatIndicesRequest{
		Index: []string{MetaIndex, ContractIndex, BlockIndex, StorageIndex, TransactionIndex, EventIndex, TokenIndex},
	}

	if _, err := es.apiClient.DoRequest(fetchReq); err != nil {
		if err == ErrIndexNotFound {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func (es *ElasticsearchDB) getContractByAddress(address types.Address) (*Contract, error) {
	fetchReq := esapi.GetRequest{
		Index:      ContractIndex,
		DocumentID: address.String(),
	}

	body, err := es.apiClient.DoRequest(fetchReq)
	if err != nil {
		return nil, err
	}

	var contract ContractQueryResult
	if err = json.Unmarshal(body, &contract); err != nil {
		return nil, err
	}
	return &contract.Source, nil
}

func (es *ElasticsearchDB) getTemplateByName(name string) (*Template, error) {
	fetchReq := esapi.GetRequest{
		Index:      TemplateIndex,
		DocumentID: name,
	}

	body, err := es.apiClient.DoRequest(fetchReq)
	if err != nil {
		return nil, err
	}

	var template TemplateQueryResult
	if err = json.Unmarshal(body, &template); err != nil {
		return nil, err
	}
	return &template.Source, nil
}

func (es *ElasticsearchDB) updateAllLastFiltered(addresses []types.Address, lastFiltered uint64) error {
	bi := es.apiClient.GetBulkHandler(ContractIndex)

	for _, address := range addresses {
		_ = bi.Add(
			context.Background(),
			esutil.BulkIndexerItem{
				Action:     "update",
				DocumentID: address.String(),
				Body:       strings.NewReader(fmt.Sprintf(`{"doc":{"lastFiltered":%d}}`, lastFiltered)),
			},
		)
	}
	return nil
}

func (es *ElasticsearchDB) updateContract(address types.Address, property string, value string) error {
	//check contract exists before updating
	_, err := es.getContractByAddress(address)
	if err != nil {
		return err
	}

	query := map[string]interface{}{
		"doc": map[string]interface{}{
			property: value,
		},
	}

	updateRequest := esapi.UpdateRequest{
		Index:      ContractIndex,
		DocumentID: address.String(),
		Body:       esutil.NewJSONReader(query),
		Refresh:    "true",
	}

	_, err = es.apiClient.DoRequest(updateRequest)
	return err
}

func (es *ElasticsearchDB) createEvents(events []*types.Event) error {
	bi := es.apiClient.GetBulkHandler(EventIndex)

	var (
		wg        sync.WaitGroup
		returnErr error
	)
	for _, event := range events {
		wg.Add(1)
		_ = bi.Add(
			context.Background(),
			esutil.BulkIndexerItem{
				Action:     "create",
				DocumentID: strconv.FormatUint(event.BlockNumber, 10) + "-" + strconv.FormatUint(event.Index, 10),
				Body:       esutil.NewJSONReader(event),
				OnSuccess: func(ctx context.Context, item esutil.BulkIndexerItem, item2 esutil.BulkIndexerResponseItem) {
					wg.Done()
				},
				OnFailure: func(ctx context.Context, item esutil.BulkIndexerItem, item2 esutil.BulkIndexerResponseItem, err error) {
					returnErr = err
					wg.Done()
				},
			},
		)
	}
	wg.Wait()
	return returnErr
}

func (es *ElasticsearchDB) Stop() {
	es.apiClient.CloseIndexers()
	log.Info("Elasticsearch indexers closed")
}

func (es *ElasticsearchDB) doSearchRequest(req esapi.SearchRequest) (*SearchQueryResult, error) {
	body, err := es.apiClient.DoRequest(req)
	if err != nil {
		return nil, err
	}

	var ret SearchQueryResult
	if err = json.Unmarshal(body, &ret); err != nil {
		return nil, err
	}
	return &ret, nil
}

func (es *ElasticsearchDB) doCountRequest(req esapi.CountRequest) (*CountQueryResult, error) {
	body, err := es.apiClient.DoRequest(req)
	if err != nil {
		return nil, err
	}

	var ret CountQueryResult
	if err = json.Unmarshal(body, &ret); err != nil {
		return nil, err
	}
	return &ret, nil
}

func (es *ElasticsearchDB) updateLastPersisted(startingBlockNumber uint64) error {
	last, err := es.GetLastPersistedBlockNumber()
	if err != nil {
		return err
	}

	blockNumber := startingBlockNumber
	if blockNumber == last+1 {
		for {
			if block, _ := es.ReadBlock(blockNumber + 1); block != nil {
				blockNumber++
			} else {
				break
			}
		}
		req := esapi.IndexRequest{
			Index:      MetaIndex,
			DocumentID: "lastPersisted",
			Body:       strings.NewReader(fmt.Sprintf(`{"lastPersisted": %d}`, blockNumber)),
			Refresh:    "true",
		}
		_, err := es.apiClient.DoRequest(req)
		return err
	}
	return nil
}
