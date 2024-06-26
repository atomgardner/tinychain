package nakamoto

import (
	"database/sql"
	"fmt"
	_ "github.com/mattn/go-sqlite3"
	"github.com/liamzebedee/tinychain-go/core"
	"encoding/hex"
)


func OpenDB(dbPath string) (*sql.DB, error) {
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		return nil, err
	}

	// Check to perform migrations.
	_, err = db.Exec("create table if not exists tinychain_version (version int)")
	if err != nil {
		return nil, fmt.Errorf("error checking database version: %s", err)
	}
	// Check the database version.
	rows, err := db.Query("select version from tinychain_version limit 1")
	if err != nil {
		return nil, fmt.Errorf("error checking database version: %s", err)
	}
	databaseVersion := 0
	if rows.Next() {
		rows.Scan(&databaseVersion)
	}

	// Log version.
	fmt.Printf("Database version: %d\n", databaseVersion)

	// Migration: v0.
	if databaseVersion == 0 {
		// Perform migrations.
		
		// Create tables.
		_, err = db.Exec("create table epochs (id TEXT PRIMARY KEY)")
		if err != nil {
			return nil, fmt.Errorf("error creating 'epochs' table: %s", err)
		}
		_, err = db.Exec("create table blocks (hash blob primary key, parent_hash blob, timestamp integer, num_transactions integer, transactions_merkle_root blob, nonce blob, height integer, foreign key (epoch) REFERENCES epochs (id), size_bytes integer)")
		if err != nil {
			return nil, fmt.Errorf("error creating 'blocks' table: %s", err)
		}
		_, err = db.Exec("create table transactions (hash blob primary key, block_hash blob, sig blob, from_pubkey blob, data blob)")
		if err != nil {
			return nil, fmt.Errorf("error creating 'transactions' table: %s", err)
		}

		// Create indexes.
		_, err = db.Exec("create index blocks_parent_hash on blocks (parent_hash)")
		if err != nil {
			return nil, fmt.Errorf("error creating 'blocks_parent_hash' index: %s", err)
		}

		// Update version.
		dbVersion := 1
		_, err = db.Exec("insert into tinychain_version (version) values (?)", dbVersion)
		if err != nil {
			return nil, fmt.Errorf("error updating database version: %s", err)
		}
	}

	return db, err
}

func NewBlockDAGFromDB(db *sql.DB, stateMachine StateMachine, consensus ConsensusConfig) (BlockDAG, error) {
	dag := BlockDAG{
		db: db,
		stateMachine: stateMachine,
		consensus: consensus,
	}
	return dag, nil
}

func (dag *BlockDAG) initialiseBlockDAG() (error) {
	genesisBlockHash := dag.consensus.GenesisBlockHash
	genesisBlock := RawBlock{
		ParentHash: [32]byte{},
		Timestamp: 0,
		NumTransactions: 0,
		TransactionsMerkleRoot: [32]byte{},
		Nonce: [32]byte{},
		Transactions: []RawTransaction{},
	}
	genesisHeight := 0
	
	// Insert the genesis epoch.
	epoch0 := Epoch{
		Number: 0,
		StartBlockHash: genesisBlockHash,
		StartTime: 0,
		StartHeight: genesisHeight,
		EndBlockHash: [32]byte{},
		EndTime: 0,
		Difficulty: dag.consensus.GenesisDifficulty,
	}
	_, err := db.Exec(
		"insert into epochs (id) values (?)", 
		epoch.Id(),
	)
	if err != nil {
		return err
	}

	// Insert the genesis block.
	_, err = db.Exec(
		"insert into blocks (hash, parent_hash, timestamp, num_transactions, transactions_merkle_root, nonce, height, epoch, size_bytes) values (?, ?, ?, ?, ?, ?)", 
		genesisBlockHash[:],
		genesisBlock.ParentHash[:],
		genesisBlock.Timestamp, 
		genesisBlock.NumTransactions, 
		genesisBlock.TransactionsMerkleRoot[:], 
		genesisBlock.Nonce[:],
		height,
		epoch.Id(),
		raw.SizeBytes(),
	)
	if err != nil {
		return err
	}
}



func (dag *BlockDAG) IngestBlock(raw RawBlock) error {
	// 1. Verify parent is known.
	if raw.ParentHash != dag.consensus.GenesisBlockHash {
		if parent_block := dag.GetBlockByHash(raw.ParentHash); parent_block == nil {
			return fmt.Errorf("Unknown parent block.")
		}
	}

	// 2. Verify timestamp is within bounds.
	// TODO: subjectivity.

	// 3. Verify num transactions is the same as the length of the transactions list.
	if int(raw.NumTransactions) != len(raw.Transactions) {
		return fmt.Errorf("Num transactions does not match length of transactions list.")
	}

	// 4. Verify transactions are valid.
	for i, block_tx := range raw.Transactions {
		// TODO: Verify signature.
		// This depends on where exactly we are verifying the sig.
		err := dag.stateMachine.VerifyTx(block_tx)

		if err != nil {
			return fmt.Errorf("Transaction %d is invalid.", i)
		}
	}

	// 5. Verify transaction merkle root is valid.
	txlist := make([][]byte, len(raw.Transactions))
	for i, block_tx := range raw.Transactions {
		txlist[i] = block_tx.Envelope()
	}
	expectedMerkleRoot := core.ComputeMerkleHash(txlist)
	if expectedMerkleRoot != raw.TransactionsMerkleRoot {
		return fmt.Errorf("Merkle root does not match computed merkle root.")
	}

	// 6. Verify POW solution is valid.
	epoch, err := dag.GetEpochForBlockHash(raw.ParentHash)
	if epoch == nil {
		return fmt.Errorf("Parent block epoch not found.")
	}
	if err != nil {
		return fmt.Errorf("Failed to get parent block epoch: %s.", err)
	}
	if !VerifyPOW(raw.Hash(), epoch.Difficulty) {
		return fmt.Errorf("POW solution is invalid.")
	}

	// 7. Verify block size is correctly computed.


	// Annotations:
	// 1. Add block height.
	// 2. Compute block epoch.
	var height uint64 = 0
	if raw.ParentHash == dag.consensus.GenesisBlockHash {
		height = 1
	} else {
		parent_block := dag.GetBlockByHash(raw.ParentHash)
		height = parent_block.Height + 1
	}
	
	// Ingest into database store.
	tx, err := dag.db.Begin()
	if err != nil {
		return err
	}
	
	blockhash := raw.Hash()
	_, err = tx.Exec(
		"insert into blocks (hash, parent_hash, timestamp, num_transactions, transactions_merkle_root, nonce, height, epoch, size_bytes) values (?, ?, ?, ?, ?, ?)", 
		blockhash[:],
		raw.ParentHash[:], 
		raw.Timestamp, 
		raw.NumTransactions, 
		raw.TransactionsMerkleRoot[:], 
		raw.Nonce[:],
		height,
		epoch.Number,
		raw.SizeBytes(),
	)
	if err != nil {
		tx.Rollback()
		return err
	}
	for _, block_tx := range raw.Transactions {
		txhash := block_tx.Hash()
		_, err = tx.Exec(
			"insert into transactions (hash, block_hash, sig, from_pubkey, data) values (?, ?, ?, ?, ?)", 
			txhash[:],
			blockhash[:], 
			block_tx.Sig[:], 
			block_tx.FromPubkey[:], 
			block_tx.Data[:],
		)
		if err != nil {
			tx.Rollback()
			return err
		}
	}
	tx.Commit()

	return nil
}

func (dag *BlockDAG) GetEpochForBlockHash(parentBlockHash [32]byte) (*Epoch, error) {
	// Special case: genesis block.
	if parentBlockHash == dag.consensus.GenesisBlockHash {
		
	}

	// Get height of block.
	rows, err := dag.db.Query("select height from blocks where hash = ? limit 1", parentBlockHash)
	if err != nil {
		return nil, err
	}
	height := uint64(0)
	if rows.Next() {
		rows.Scan(&height)
	} else {
		return nil, fmt.Errorf("Block not found: %s.", hex.EncodeToString(parentBlockHash[:]))
	}

	// Recompute the difficulty.
	if height % int(dag.consensus.EpochLengthBlocks) == 0 {
		// Load the chain of blocks.
		chain := make([]Block, dag.consensus.EpochLengthBlocks)
		rows, err := dag.db.Query("select hash, parent_hash, timestamp, num_transactions, transactions_merkle_root, nonce from blocks where height >= ? order by height asc limit ?", height - dag.consensus.EpochLengthBlocks, dag.consensus.EpochLengthBlocks)
		if err != nil {
			return nil, err
		}
		for i := 0; rows.Next(); i++ {
			block := Block{}
			rows.Scan(&block.Hash, &block.ParentHash, &block.Timestamp, &block.NumTransactions, &block.TransactionsMerkleRoot, &block.Nonce)
			chain[i] = block
		}

		// Compute the time taken to mine the last epoch.
		epoch_start := chain[len(chain) - int(dag.consensus.EpochLengthBlocks)].Timestamp
		epoch_end := chain[len(chain) - 1].Timestamp
		epoch_duration := epoch_end - epoch_start
		if epoch_duration == 0 {
			epoch_duration = 1
		}
		epoch_index := len(chain) / int(dag.consensus.EpochLengthBlocks)
		fmt.Printf("epoch i=%d start_time=%d end_time=%d duration=%d \n", epoch_index, epoch_start, epoch_end, epoch_duration)

		// Compute the target epoch length.
		target_epoch_length := dag.consensus.TargetEpochLengthMillis * dag.consensus.EpochLengthBlocks

		// Compute the new difficulty.
		// difficulty = difficulty * (epoch_duration / target_epoch_length)
		new_difficulty := new(big.Int)
		new_difficulty.Mul(&dag.consensus.InitialDifficulty, big.NewInt(int64(epoch_duration)))
		new_difficulty.Div(new_difficulty, big.NewInt(int64(target_epoch_length)))

		fmt.Printf("New difficulty: %x\n", new_difficulty.String())

		// Finalise prev epoch.
		// Create new epoch.
		// new_epoch := Epoch{
		// 	Number: epoch_index,
		// 	StartBlockHash: chain[0].Hash,
		// 	StartTime: epoch_start,
		// 	StartHeight: chain[0].Height,
		// 	EndBlockHash: chain[len(chain) - 1].Hash,
		// }
	}


	return nil, nil
}

func (dag *BlockDAG) GetBlockByHash(hash [32]byte) (*Block) {
	block := Block{}

	// Query database.
	rows, err := dag.db.Query("select parent_hash, timestamp, num_transactions, transactions_merkle_root, nonce from blocks where hash = ? limit 1", hash)
	if err != nil {
		return nil
	}

	if rows.Next() {
		rows.Scan(&block.ParentHash, &block.Timestamp, &block.NumTransactions, &block.TransactionsMerkleRoot, &block.Nonce)
		return &block
	} else {
		return nil
	}
}