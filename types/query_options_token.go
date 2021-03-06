package types

import (
	"math/big"
)

var defaultTokenQueryOptions = &TokenQueryOptions{
	BeginBlockNumber: big.NewInt(0),
	EndBlockNumber:   big.NewInt(-1),

	PageSize:   10,
	PageNumber: 0,
}

type TokenQueryOptions struct {
	BeginBlockNumber *big.Int `json:"beginBlockNumber"`
	EndBlockNumber   *big.Int `json:"endBlockNumber"`

	After string `json:"after"`

	PageSize   int `json:"pageSize"`
	PageNumber int `json:"pageNumber"`
}

func (opts *TokenQueryOptions) SetDefaults() {
	if opts.BeginBlockNumber == nil {
		opts.BeginBlockNumber = defaultQueryOptions.BeginBlockNumber
	}
	if opts.EndBlockNumber == nil {
		opts.EndBlockNumber = defaultQueryOptions.EndBlockNumber
	}
	if opts.PageSize == 0 {
		opts.PageSize = defaultTokenQueryOptions.PageSize
	}
	if opts.PageNumber == 0 {
		opts.PageNumber = defaultTokenQueryOptions.PageNumber
	}
}
