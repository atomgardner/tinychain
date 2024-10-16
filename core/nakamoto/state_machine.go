package nakamoto

import (
	"database/sql"
	"errors"
	"fmt"
	"math/bits"
)

var ErrInsufficientBalance = errors.New("insufficient balance")
var ErrToBalanceOverflow = errors.New("\"to\" balance overflow")
var ErrMinerBalanceOverflow = errors.New("\"miner\" balance overflow")
var ErrAmountPlusFeeOverflow = errors.New("(amount + fee) overflow")

var stateMachineLogger = NewLogger("state-machine", "")

type StateLeaf struct {
	PubKey  [65]byte
	Balance uint64
}

// The input to the state transition function.
type StateMachineInput struct {
	// The raw transaction to be processed.
	RawTransaction RawTransaction

	// Is it the coinbase transaction.
	IsCoinbase bool

	// Miner address for fees.
	MinerPubkey [65]byte
}

// The state machine is the core of the business logic for the Nakamoto blockchain.
// It performs the state transition function, which encapsulates:
// 1. Minting coins into circulation via the coinbase transaction.
// 2. Transferring coins between accounts.
//
// It is oblivious to:
//   - the consensus algorithm, transaction sequencing.
//   - signatures. The state machine does not care about validating signatures. At Bitcoin's core, it is a sequencing/DA layer.
type StateMachine struct {
	// The current state.
	state map[[65]byte]uint64
}

func NewStateMachine(db *sql.DB) (*StateMachine, error) {
	return &StateMachine{
		state: make(map[[65]byte]uint64),
	}, nil
}

func (c *StateMachine) Apply(leafs []*StateLeaf) {
	for _, leaf := range leafs {
		c.state[leaf.PubKey] = leaf.Balance
	}
}

// Transitions the state machine to the next state.
func (c *StateMachine) Transition(input StateMachineInput) ([]*StateLeaf, error) {
	// Check transaction version.
	if input.RawTransaction.Version != 1 {
		return nil, errors.New("unsupported transaction version")
	}

	if input.IsCoinbase {
		return c.transitionCoinbase(input)
	} else {
		return c.transitionTransfer(input)
	}
}

func (c *StateMachine) transitionTransfer(input StateMachineInput) ([]*StateLeaf, error) {
	fromBalance := c.GetBalance(input.RawTransaction.FromPubkey)
	toBalance := c.GetBalance(input.RawTransaction.ToPubkey)
	minerBalance := c.GetBalance(input.MinerPubkey)
	amount := input.RawTransaction.Amount
	fee := input.RawTransaction.Fee

	// Check for overflow on 3 operations:
	// 1. toBalance += amount
	// 2. minerBalance += fee
	// 3. amount + fee
	// Check if the `to` balance will overflow.
	// The Add64 function adds two 64-bit unsigned integers along with an optional carry-in value. It returns the result of the addition and the carry-out value. The carry-out is set to 1 if the addition results in an overflow (i.e., the sum is greater than what can be represented in 64 bits), and 0 otherwise.
	if _, carry := bits.Add64(toBalance, amount, 0); carry != 0 {
		return nil, ErrToBalanceOverflow
	}
	if _, carry := bits.Add64(minerBalance, fee, 0); carry != 0 {
		return nil, ErrMinerBalanceOverflow
	}
	if _, carry := bits.Add64(amount, fee, 0); carry != 0 {
		return nil, ErrAmountPlusFeeOverflow
	}

	// Check if the `from` account has enough balance.
	if fromBalance < (amount + fee) {
		// return nil, fmt.Errorf("insufficient balance. balance=%d, amount=%d", fromBalance, amount)
		return nil, ErrInsufficientBalance
	}

	// Deduct the coins from the `from` account balance.
	fromBalance -= amount

	// Add the coins to the `to` account balance.
	toBalance += amount

	// Add the fee to the `miner` account balance.
	minerBalance += fee

	// Create the new state leaves.
	fromLeaf := &StateLeaf{
		PubKey:  input.RawTransaction.FromPubkey,
		Balance: fromBalance,
	}
	toLeaf := &StateLeaf{
		PubKey:  input.RawTransaction.ToPubkey,
		Balance: toBalance,
	}
	minerLeaf := &StateLeaf{
		PubKey:  input.MinerPubkey,
		Balance: minerBalance,
	}
	leaves := []*StateLeaf{
		fromLeaf,
		toLeaf,
		minerLeaf,
	}
	return leaves, nil
}

func (c *StateMachine) transitionCoinbase(input StateMachineInput) ([]*StateLeaf, error) {
	toBalance := c.GetBalance(input.RawTransaction.ToPubkey)
	amount := input.RawTransaction.Amount

	// Check if the `to` balance will overflow.
	// The Add64 function adds two 64-bit unsigned integers along with an optional carry-in value. It returns the result of the addition and the carry-out value. The carry-out is set to 1 if the addition results in an overflow (i.e., the sum is greater than what can be represented in 64 bits), and 0 otherwise.
	if _, carry := bits.Add64(toBalance, amount, 0); carry != 0 {
		return nil, ErrToBalanceOverflow
	}

	// Add the coins to the `to` account balance.
	toBalance += amount

	// Create the new state leaves.
	toLeaf := &StateLeaf{
		PubKey:  input.RawTransaction.ToPubkey,
		Balance: toBalance,
	}
	leaves := []*StateLeaf{
		toLeaf,
	}
	return leaves, nil
}

func (c *StateMachine) GetBalance(account [65]byte) uint64 {
	return c.state[account]
}

// Returns a list of modified accounts.
func (c *StateMachine) GetStateSnapshot() []StateLeaf {
	return nil
}

// Given a block DAG and a list of block hashes, extracts the transaction sequence, applies each transaction in order, and returns the final state.
func RebuildState(dag *BlockDAG, stateMachine StateMachine, longestChainHashList [][32]byte) (*StateMachine, error) {
	for _, blockHash := range longestChainHashList {
		// 1. Get all transactions for block.
		// TODO ignore: nonce, sig
		txs, err := dag.GetBlockTransactions(blockHash)
		if err != nil {
			return nil, err
		}

		stateMachineLogger.Printf("Processing block %x with %d transactions", blockHash, len(*txs))

		// 2. Map transactions to state leaves through state machine transition function.
		var stateMachineInput StateMachineInput
		var minerPubkey [65]byte
		isCoinbase := false

		for i, tx := range *txs {
			// Special case: coinbase tx is always the first tx in the block.
			if i == 0 {
				minerPubkey = tx.FromPubkey
				isCoinbase = true
			}

			// Construct the state machine input.
			stateMachineInput = StateMachineInput{
				RawTransaction: tx.ToRawTransaction(),
				IsCoinbase:     isCoinbase,
				MinerPubkey:    minerPubkey,
			}

			// Transition the state machine.
			effects, err := stateMachine.Transition(stateMachineInput)
			if err != nil {
				return nil, fmt.Errorf("Error transitioning state machine: block=%x txindex=%d error=\"%s\"", blockHash, i, err)
			}

			// Apply the effects.
			stateMachine.Apply(effects)

			if i == 0 {
				isCoinbase = false
			}
		}
	}

	return &stateMachine, nil
}
