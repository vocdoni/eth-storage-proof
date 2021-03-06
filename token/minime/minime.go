package minime

import (
	"context"
	"errors"
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/vocdoni/storage-proofs-eth-go/ethstorageproof"
	"github.com/vocdoni/storage-proofs-eth-go/helpers"
	"github.com/vocdoni/storage-proofs-eth-go/token/erc20"
)

// ErrSlotNotFound represents the storage slot not found error
var ErrSlotNotFound = errors.New("storage slot not found")

const maxIterationsForDiscover = 20

// Minime token stores the whole list of balances an address has had.
// To this end we need to generate two proofs, one for proving the balance
// on a specific block and the following proving the next balance stored
// is either nil (0x0) or a bigger block number.
type Minime struct {
	erc20 *erc20.ERC20Token
}

// New creates a new Minime to get and verify Minime token proofs
func New(ctx context.Context, rpcCli *rpc.Client, tokenAddress common.Address) (*Minime, error) {
	erc20, err := erc20.New(ctx, rpcCli, tokenAddress)
	return &Minime{erc20: erc20}, err
}

// DiscoverSlot tries to find the map index slot for the minime balances
func (m *Minime) DiscoverSlot(ctx context.Context, holder common.Address) (int, *big.Rat, error) {
	balance, err := m.erc20.Balance(ctx, holder)
	if err != nil {
		return -1, nil, err
	}

	addr := common.Address{}
	copy(addr[:], m.erc20.TokenAddr[:20])
	var amount *big.Rat
	var block *big.Int
	index := -1

	for i := 0; i < maxIterationsForDiscover; i++ {
		checkPointsSize, err := m.getMinimeArraySize(ctx, holder, i)
		if err != nil {
			return 0, nil, err
		}
		if checkPointsSize <= 0 {
			continue
		}

		if amount, block, _, err = m.getMinimeAtPosition(
			ctx,
			holder,
			i,
			checkPointsSize,
			nil,
		); err != nil {
			continue
		}
		if block.Uint64() == 0 {
			continue
		}

		// Check if balance matches
		if amount.Cmp(balance) == 0 {
			index = i
			break
		}
	}
	if index == -1 {
		return index, nil, ErrSlotNotFound
	}
	return index, amount, nil
}

// GetProof returns a storage proof for a token holder and a block number.
// The MiniMe proof consists of two storage proofs in order to prove the
// block number is within a range of checkpoints.
// Examples (checkpoints are block numbers)
//
// Minime checkpoints: [100]
// For block 105, we need to provide checkpoint 100 and proof-of-nil (>100)
//
// Minime checkpoints: [70],[80],[90],[100]
// For block 87, we need to provide checkpoint 80 and 90
func (m *Minime) GetProof(ctx context.Context, holder common.Address, block *big.Int,
	islot int) (*ethstorageproof.StorageProof, error) {
	checkPointsSize, err := m.getMinimeArraySize(ctx, holder, islot)
	if err != nil {
		return nil, fmt.Errorf("cannot fetch minime array size: %w", err)
	}
	var keys [][]byte

	// Firstly, check the last checkpoint block, if smaller than the current block number
	// the proof will include the last checkpoint and a proof-of-nil for the next position.
	_, mblock, slot, err := m.getMinimeAtPosition(ctx, holder, islot, checkPointsSize, block)
	if err != nil {
		return nil, fmt.Errorf("cannot get minime: %w", err)
	}
	if block.Uint64() >= mblock.Uint64() {
		_, _, slot2, err := m.getMinimeAtPosition(ctx, holder, islot, checkPointsSize+1, block)
		if err != nil {
			return nil, err
		}
		keys = append(keys, slot[:], slot2[:])
	}

	// Secondly walk through all checkpoints starting from the last.
	if len(keys) == 0 {
		for i := checkPointsSize - 1; i > 0; i-- {
			_, checkpointBlock, prevSlot, err := m.getMinimeAtPosition(ctx, holder, islot, i-1, block)
			if err != nil {
				return nil, fmt.Errorf("cannot get minime: %w", err)
			}

			// If minime checkpoint block -1 is equal or greather than the block we
			// are looking for, that's the one we need (the previous and the current)
			if checkpointBlock.Uint64() >= block.Uint64() {
				balance, block, currSlot, err := m.getMinimeAtPosition(ctx, holder, islot, i, block)
				if err != nil {
					return nil, err
				}
				if balance.Cmp(big.NewRat(0, 1)) == 1 {
					return nil, fmt.Errorf("proof of nil has a balance value")
				}
				if block.Uint64() > 0 {
					return nil, fmt.Errorf("proof of nil has a block value")
				}
				keys = append(keys, prevSlot[:], currSlot[:])
				break
			}
		}
	}
	if len(keys) == 0 {
		return nil, fmt.Errorf("checkpoint not found")
	}

	return m.erc20.GetProof(ctx, keys, block)
}

// VerifyProof verifies a minime storage proof
func (m *Minime) VerifyProof(holder common.Address, storageRoot common.Hash,
	proofs []ethstorageproof.StorageResult, mapIndexSlot int, targetBalance,
	targetBlock *big.Int) error {
	return VerifyProof(holder, storageRoot, proofs, mapIndexSlot, targetBalance, targetBlock)
}

// getMinimeAtPosition returns the data contained in a specific checkpoint array position,
// returns the balance, the checkpoint block and the merkle tree key slot
func (m *Minime) getMinimeAtPosition(ctx context.Context, holder common.Address, mapIndexSlot,
	position int, block *big.Int) (*big.Rat, *big.Int, *common.Hash, error) {
	token, err := m.erc20.GetTokenData(ctx)
	if err != nil {
		return nil, nil, nil, err
	}
	contractAddr := common.Address{}
	copy(contractAddr[:], m.erc20.TokenAddr[:20])

	mapSlot := helpers.GetMapSlot(holder, mapIndexSlot)
	vf := helpers.HashFromPosition(mapSlot)

	offset := new(big.Int).SetInt64(int64(position - 1))
	v := new(big.Int).SetBytes(vf[:])
	v.Add(v, offset)

	arraySlot := common.BytesToHash(v.Bytes())
	value, err := m.erc20.EthCli.StorageAt(ctx, contractAddr, arraySlot, block)
	if err != nil {
		return nil, nil, nil, err
	}

	balance, _, mblock := ParseMinimeValue(value, int(token.Decimals))

	return balance, mblock, &arraySlot, nil
}

func (m *Minime) getMinimeArraySize(ctx context.Context, holder common.Address,
	islot int) (int, error) {
	// In this slot we should find the array size
	mapSlot := helpers.GetMapSlot(holder, islot)

	addr := common.Address{}
	copy(addr[:], m.erc20.TokenAddr[:20])

	value, err := m.erc20.EthCli.StorageAt(ctx, addr, mapSlot, nil)
	if err != nil {
		return 0, err
	}
	return int(new(big.Int).SetBytes(common.TrimLeftZeroes(value)).Uint64()), nil
}
