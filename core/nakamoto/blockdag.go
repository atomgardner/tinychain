package nakamoto

import (
	"database/sql"
	"encoding/hex"
	"fmt"
	"math/big"

	"github.com/liamzebedee/tinychain-go/core"
	_ "github.com/mattn/go-sqlite3"
)

var logger = NewLogger("blockdag", "")

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
	defer rows.Close()
	databaseVersion := 0
	if rows.Next() {
		rows.Scan(&databaseVersion)
	}

	// Log version.
	logger.Printf("Database version: %d\n", databaseVersion)

	// Migration: v0.
	if databaseVersion == 0 {
		// Perform migrations.
		dbVersion := 1
		logger.Printf("Running migration: %d\n", dbVersion)

		// Create tables.

		// epochs
		_, err = db.Exec("create table epochs (id TEXT PRIMARY KEY, start_block_hash blob, start_time integer, start_height integer, difficulty blob)")
		if err != nil {
			return nil, fmt.Errorf("error creating 'epochs' table: %s", err)
		}

		// blocks
		_, err = db.Exec(`create table blocks (
			hash blob primary key, 
			parent_hash blob, 
			difficulty blob, 
			timestamp integer, 
			num_transactions integer, 
			transactions_merkle_root blob, 
			nonce blob, 
			graffiti blob, 
			height integer, 
			epoch TEXT, 
			size_bytes integer, 
			parent_total_work blob, 
			acc_work blob, 
			foreign key (epoch) REFERENCES epochs (id)
		)`)
		if err != nil {
			return nil, fmt.Errorf("error creating 'blocks' table: %s", err)
		}

		// transactions_blocks
		_, err = db.Exec(`
			create table transactions_blocks (
				block_hash blob, transaction_hash blob, txindex integer, 
				
				primary key (block_hash, transaction_hash, txindex),
				foreign key (block_hash) references blocks (hash), 
				foreign key (transaction_hash) references transactions (hash)
			)
		`)
		if err != nil {
			return nil, fmt.Errorf("error creating 'transactions_blocks' table: %s", err)
		}

		// transactions
		_, err = db.Exec("create table transactions (hash blob primary key, sig blob, from_pubkey blob, to_pubkey blob, amount integer, fee integer, nonce integer, version integer)")
		if err != nil {
			return nil, fmt.Errorf("error creating 'transactions' table: %s", err)
		}

		// Create indexes.
		_, err = db.Exec("create index blocks_parent_hash on blocks (parent_hash)")
		if err != nil {
			return nil, fmt.Errorf("error creating 'blocks_parent_hash' index: %s", err)
		}

		// Update version.
		_, err = db.Exec("insert into tinychain_version (version) values (?)", dbVersion)
		if err != nil {
			return nil, fmt.Errorf("error updating database version: %s", err)
		}

		logger.Printf("Database upgraded to: %d\n", dbVersion)
	}

	return db, err
}

// The block DAG is the core data structure of the Nakamoto consensus protocol.
// It is a directed acyclic graph of blocks, where each block has a parent block.
// As it is infeasible to store the entirety of the blockchain in-memory,
// the block DAG is backed by a SQL database.
type BlockDAG struct {
	// The backing SQL database store, which stores:
	// - blocks
	// - epochs
	// - transactions
	db *sql.DB

	// The state machine.
	stateMachine StateMachineInterface

	// Consensus settings.
	consensus ConsensusConfig

	// The "light client" tip. This is the tip of the heaviest chain of block headers.
	HeadersTip Block

	// The "full node" tip. This is the tip of the heaviest chain of full blocks.
	FullTip Block

	// OnNewTip handler.
	OnNewHeadersTip func(tip Block, prevTip Block)
	OnNewFullTip func(tip Block, prevTip Block)
}

func NewBlockDAGFromDB(db *sql.DB, stateMachine StateMachineInterface, consensus ConsensusConfig) (BlockDAG, error) {
	dag := BlockDAG{
		db:           db,
		stateMachine: stateMachine,
		consensus:    consensus,
	}

	err := dag.initialiseBlockDAG()
	if err != nil {
		panic(err)
	}

	dag.HeadersTip, err = dag.GetLatestTip()
	if err != nil {
		panic(err)
	}

	dag.FullTip, err = dag.GetLatestTip()
	if err != nil {
		panic(err)
	}

	return dag, nil
}

// Initalises the block DAG with the genesis block.
func (dag *BlockDAG) initialiseBlockDAG() error {
	genesisBlock := GetRawGenesisBlockFromConfig(dag.consensus)
	genesisBlockHash := genesisBlock.Hash()
	genesisHeight := uint64(0)

	// Check if we have already initialised the database.
	rows, err := dag.db.Query("select count(*) from blocks where hash = ?", genesisBlockHash[:])
	if err != nil {
		return err
	}
	count := 0
	if rows.Next() {
		rows.Scan(&count)
	}
	if count > 0 {
		return nil
	}
	rows.Close()

	// Begin initialisation.
	logger.Printf("Initialising block DAG...\n")

	// Insert the genesis epoch.
	epoch0 := Epoch{
		Number:         0,
		StartBlockHash: genesisBlockHash,
		StartTime:      genesisBlock.Timestamp,
		StartHeight:    genesisHeight,
		Difficulty:     dag.consensus.GenesisDifficulty,
	}
	_, err = dag.db.Exec(
		"insert into epochs (id, start_block_hash, start_time, start_height, difficulty) values (?, ?, ?, ?, ?)",
		epoch0.GetId(),
		epoch0.StartBlockHash[:],
		epoch0.StartTime,
		epoch0.StartHeight,
		epoch0.Difficulty.Bytes(),
	)
	if err != nil {
		return err
	}

	work := CalculateWork(Bytes32ToBigInt(genesisBlock.Hash()))
	logger.Printf("Inserted genesis epoch difficulty=%s\n", dag.consensus.GenesisDifficulty.String())
	accWorkBuf := BigIntToBytes32(*work)

	// Insert the genesis block.
	_, err = dag.db.Exec(
		"insert into blocks (hash, parent_hash, parent_total_work, difficulty, timestamp, num_transactions, transactions_merkle_root, nonce, graffiti, height, epoch, size_bytes, acc_work) values (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
		genesisBlockHash[:],
		genesisBlock.ParentHash[:],
		genesisBlock.ParentTotalWork[:],
		dag.consensus.GenesisDifficulty.Bytes(),
		genesisBlock.Timestamp,
		genesisBlock.NumTransactions,
		genesisBlock.TransactionsMerkleRoot[:],
		genesisBlock.Nonce[:],
		genesisBlock.Graffiti[:],
		genesisHeight,
		epoch0.GetId(),
		genesisBlock.SizeBytes(),
		PadBytes(accWorkBuf[:], 32),
	)
	if err != nil {
		return err
	}

	logger.Printf("Inserted genesis block hash=%s work=%s\n", hex.EncodeToString(genesisBlockHash[:]), work.String())

	return nil
}

// Ingests a block header, and recomputes the headers tip. Used by light clients / SPV sync.
func (dag *BlockDAG) IngestHeader(raw RawBlock) error {
	// 1. Verify parent is known.
	parentBlock, err := dag.GetBlockByHash(raw.ParentHash)
	if err != nil {
		return err
	}
	if parentBlock == nil {
		return fmt.Errorf("Unknown parent block.")
	}

	// 6. Verify POW solution is valid.
	height := uint64(parentBlock.Height + 1)
	var epoch *Epoch

	// 6a. Compute the current difficulty epoch.
	//

	// Are we on an epoch boundary?
	if height%dag.consensus.EpochLengthBlocks == 0 {
		// Recompute difficulty and create new epoch.
		logger.Printf("Recomputing difficulty for epoch %d\n", height/dag.consensus.EpochLengthBlocks)

		// Get current epoch.
		epoch, err = dag.GetEpochForBlockHash(raw.ParentHash)
		if err != nil {
			return err
		}
		newDifficulty := RecomputeDifficulty(epoch.StartTime, raw.Timestamp, epoch.Difficulty, dag.consensus.TargetEpochLengthMillis, dag.consensus.EpochLengthBlocks, height)

		epoch = &Epoch{
			Number:         height / dag.consensus.EpochLengthBlocks,
			StartBlockHash: raw.Hash(),
			StartTime:      raw.Timestamp,
			StartHeight:    height,
			Difficulty:     newDifficulty,
		}
		_, err := dag.db.Exec(
			"insert into epochs (id, start_block_hash, start_time, start_height, difficulty) values (?, ?, ?, ?, ?)",
			epoch.GetId(),
			epoch.StartBlockHash[:],
			epoch.StartTime,
			epoch.StartHeight,
			newDifficulty.Bytes(),
		)
		if err != nil {
			return err
		}
	} else {
		// Lookup current epoch.
		epoch, err = dag.GetEpochForBlockHash(raw.ParentHash)
		if epoch == nil {
			return fmt.Errorf("Parent block epoch not found.")
		}
		if err != nil {
			return err
		}
	}

	// 6b. Verify POW solution.
	blockHash := raw.Hash()
	if !VerifyPOW(blockHash, epoch.Difficulty) {
		return fmt.Errorf("POW solution is invalid.")
	}

	// 6c. Verify parent total work is correct.
	parentTotalWork := Bytes32ToBigInt(raw.ParentTotalWork)
	if parentBlock.AccumulatedWork.Cmp(&parentTotalWork) != 0 {
		logger.Printf("Comparing parent total work. expected=%s actual=%s\n", parentBlock.AccumulatedWork.String(), parentTotalWork.String())
		return fmt.Errorf("Parent total work is incorrect.")
	}


	// 8. Ingest block into database store.
	tx, err := dag.db.Begin()
	if err != nil {
		return err
	}

	acc_work := new(big.Int)
	work := CalculateWork(Bytes32ToBigInt(blockHash))
	acc_work.Add(&parentBlock.AccumulatedWork, work)
	acc_work_buf := BigIntToBytes32(*acc_work)

	// Insert block.
	blockhash := raw.Hash()
	_, err = tx.Exec(
		"insert into blocks (hash, parent_hash, parent_total_work, timestamp, num_transactions, transactions_merkle_root, nonce, graffiti, height, epoch, size_bytes, acc_work) values (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
		blockhash[:],
		raw.ParentHash[:],
		raw.ParentTotalWork[:],
		raw.Timestamp,
		raw.NumTransactions,
		raw.TransactionsMerkleRoot[:],
		raw.Nonce[:],
		raw.Graffiti[:],
		height,
		epoch.GetId(),
		0, // Block size is 0 until we get transactions.
		acc_work_buf[:],
	)
	if err != nil {
		tx.Rollback()
		return err
	}

	tx.Commit()

	// Update the tip.
	// TODO UPDATE LIGHT TIP.

	return nil
}

// Ingests a block's body, which is linked to a previously ingested block header.
func (dag *BlockDAG) IngestBlockBody(blockhash [32]byte, body []RawTransaction) error {
	// Lookup block header.
	block, err := dag.GetBlockByHash(blockhash)
	if err != nil {
		return err
	}
	if block == nil {
		return fmt.Errorf("Block header missing during body ingestion.")
	}
	raw := block.ToRawBlock()


	// 2. Verify timestamp is within bounds.
	// TODO: subjectivity.

	// 3. Verify num transactions is the same as the length of the transactions list.
	if int(raw.NumTransactions) != len(raw.Transactions) {
		return fmt.Errorf("Num transactions does not match length of transactions list.")
	}

	// 4. Verify transactions are valid.
	// TODO: We can parallelise this.
	// This is one of the most expensive operations of the blockchain node.
	for i, block_tx := range raw.Transactions {
		logger.Printf("Verifying transaction %d\n", i)
		isValid := core.VerifySignature(
			hex.EncodeToString(block_tx.FromPubkey[:]),
			block_tx.Sig[:],
			block_tx.Envelope(),
		)
		if !isValid {
			return fmt.Errorf("Transaction %d is invalid: signature invalid.", i)
		}

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

	// 7. Verify block size is within bounds.
	raw.Transactions = body
	if dag.consensus.MaxBlockSizeBytes < raw.SizeBytes() {
		return fmt.Errorf("Block size exceeds maximum block size.")
	}

	// 8. Ingest block into database store.
	tx, err := dag.db.Begin()
	if err != nil {
		return err
	}

	// Update block size.

	// Insert transactions, transactions_blocks.
	for i, block_tx := range raw.Transactions {
		txhash := block_tx.Hash()

		_, err = tx.Exec(
			`insert into transactions_blocks (block_hash, transaction_hash, txindex) values (?, ?, ?)`,
			blockhash[:],
			txhash[:],
			i,
		)
		if err != nil {
			tx.Rollback()
			return err
		}

		// Check if we already have the transaction.
		rows, err := tx.Query("select count(*) from transactions where hash = ?", txhash[:])
		if err != nil {
			tx.Rollback()
			return err
		}
		count := 0
		if rows.Next() {
			rows.Scan(&count)
		}
		rows.Close()

		if count > 0 {
			continue
		}

		// Insert the transaction.
		_, err = tx.Exec(
			"insert into transactions (hash, sig, from_pubkey, to_pubkey, amount, fee, nonce, version) values (?, ?, ?, ?, ?, ?, ?, ?)",
			txhash[:],
			blockhash[:],
			block_tx.Sig[:],
			block_tx.FromPubkey[:],
			block_tx.ToPubkey[:],
			block_tx.Amount,
			block_tx.Fee,
			block_tx.Nonce,
			block_tx.Version,
		)
		if err != nil {
			tx.Rollback()
			return err
		}
	}
	tx.Commit()

	// Update the tip.
	// TODO update full tip

	return nil
}

// Ingests a full block, and recomputes the full tip.
func (dag *BlockDAG) IngestBlock(raw RawBlock) error {
	// 1. Verify parent is known.
	parentBlock, err := dag.GetBlockByHash(raw.ParentHash)
	if err != nil {
		return err
	}
	if parentBlock == nil {
		return fmt.Errorf("Unknown parent block.")
	}

	// 2. Verify timestamp is within bounds.
	// TODO: subjectivity.

	// 3. Verify num transactions is the same as the length of the transactions list.
	if int(raw.NumTransactions) != len(raw.Transactions) {
		return fmt.Errorf("Num transactions does not match length of transactions list.")
	}

	// 4. Verify transactions are valid.
	// TODO: We can parallelise this.
	// This is one of the most expensive operations of the blockchain node.
	for i, block_tx := range raw.Transactions {
		logger.Printf("Verifying transaction %d\n", i)
		isValid := core.VerifySignature(
			hex.EncodeToString(block_tx.FromPubkey[:]),
			block_tx.Sig[:],
			block_tx.Envelope(),
		)
		if !isValid {
			return fmt.Errorf("Transaction %d is invalid: signature invalid.", i)
		}

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
	height := uint64(parentBlock.Height + 1)
	var epoch *Epoch

	// 6a. Compute the current difficulty epoch.
	//

	// Are we on an epoch boundary?
	if height%dag.consensus.EpochLengthBlocks == 0 {
		// Recompute difficulty and create new epoch.
		logger.Printf("Recomputing difficulty for epoch %d\n", height/dag.consensus.EpochLengthBlocks)

		// Get current epoch.
		epoch, err = dag.GetEpochForBlockHash(raw.ParentHash)
		if err != nil {
			return err
		}
		newDifficulty := RecomputeDifficulty(epoch.StartTime, raw.Timestamp, epoch.Difficulty, dag.consensus.TargetEpochLengthMillis, dag.consensus.EpochLengthBlocks, height)

		epoch = &Epoch{
			Number:         height / dag.consensus.EpochLengthBlocks,
			StartBlockHash: raw.Hash(),
			StartTime:      raw.Timestamp,
			StartHeight:    height,
			Difficulty:     newDifficulty,
		}
		_, err := dag.db.Exec(
			"insert into epochs (id, start_block_hash, start_time, start_height, difficulty) values (?, ?, ?, ?, ?)",
			epoch.GetId(),
			epoch.StartBlockHash[:],
			epoch.StartTime,
			epoch.StartHeight,
			newDifficulty.Bytes(),
		)
		if err != nil {
			return err
		}
	} else {
		// Lookup current epoch.
		epoch, err = dag.GetEpochForBlockHash(raw.ParentHash)
		if epoch == nil {
			return fmt.Errorf("Parent block epoch not found.")
		}
		if err != nil {
			return err
		}
	}

	// 6b. Verify POW solution.
	blockHash := raw.Hash()
	if !VerifyPOW(blockHash, epoch.Difficulty) {
		return fmt.Errorf("POW solution is invalid.")
	}

	// 6c. Verify parent total work is correct.
	parentTotalWork := Bytes32ToBigInt(raw.ParentTotalWork)
	if parentBlock.AccumulatedWork.Cmp(&parentTotalWork) != 0 {
		logger.Printf("Comparing parent total work. expected=%s actual=%s\n", parentBlock.AccumulatedWork.String(), parentTotalWork.String())
		return fmt.Errorf("Parent total work is incorrect.")
	}

	// 7. Verify block size is within bounds.
	if dag.consensus.MaxBlockSizeBytes < raw.SizeBytes() {
		return fmt.Errorf("Block size exceeds maximum block size.")
	}

	// 8. Ingest block into database store.
	tx, err := dag.db.Begin()
	if err != nil {
		return err
	}

	acc_work := new(big.Int)
	work := CalculateWork(Bytes32ToBigInt(blockHash))
	acc_work.Add(&parentBlock.AccumulatedWork, work)
	acc_work_buf := BigIntToBytes32(*acc_work)

	// Insert block.
	blockhash := raw.Hash()
	_, err = tx.Exec(
		"insert into blocks (hash, parent_hash, parent_total_work, timestamp, num_transactions, transactions_merkle_root, nonce, graffiti, height, epoch, size_bytes, acc_work) values (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
		blockhash[:],
		raw.ParentHash[:],
		raw.ParentTotalWork[:],
		raw.Timestamp,
		raw.NumTransactions,
		raw.TransactionsMerkleRoot[:],
		raw.Nonce[:],
		raw.Graffiti[:],
		height,
		epoch.GetId(),
		raw.SizeBytes(),
		acc_work_buf[:],
	)
	if err != nil {
		tx.Rollback()
		return err
	}

	// Insert transactions, transactions_blocks.
	for i, block_tx := range raw.Transactions {
		txhash := block_tx.Hash()

		_, err = tx.Exec(
			`insert into transactions_blocks (block_hash, transaction_hash, txindex) values (?, ?, ?)`,
			blockhash[:],
			txhash[:],
			i,
		)
		if err != nil {
			tx.Rollback()
			return err
		}

		// Check if we already have the transaction.
		rows, err := tx.Query("select count(*) from transactions where hash = ?", txhash[:])
		if err != nil {
			tx.Rollback()
			return err
		}
		count := 0
		if rows.Next() {
			rows.Scan(&count)
		}
		rows.Close()

		if count > 0 {
			continue
		}

		// Insert the transaction.
		_, err = tx.Exec(
			"insert into transactions (hash, sig, from_pubkey, to_pubkey, amount, fee, nonce, version) values (?, ?, ?, ?, ?, ?, ?, ?)",
			txhash[:],
			blockhash[:],
			block_tx.Sig[:],
			block_tx.FromPubkey[:],
			block_tx.ToPubkey[:],
			block_tx.Amount,
			block_tx.Fee,
			block_tx.Nonce,
			block_tx.Version,
		)
		if err != nil {
			tx.Rollback()
			return err
		}
	}
	tx.Commit()

	// Update the tip.
	// TODO this is bad for performance.
	// TODO also this is not atomic.
	prev_tip := dag.FullTip
	curr_tip, err := dag.GetLatestTip()
	if err != nil {
		return err
	}

	if prev_tip.Hash != curr_tip.Hash {
		logger.Printf("New tip: height=%d hash=%s\n", curr_tip.Height, curr_tip.HashStr())
		dag.FullTip = curr_tip
		if dag.OnNewFullTip != nil {
			dag.OnNewFullTip(curr_tip, prev_tip)
		}
	}

	return nil
}

// Gets the epoch for a given block hash.
func (dag *BlockDAG) GetEpochForBlockHash(blockhash [32]byte) (*Epoch, error) {
	// Lookup the parent block.
	parentBlockEpochId := ""
	rows, err := dag.db.Query("select epoch from blocks where hash = ? limit 1", blockhash[:])
	if err != nil {
		return nil, err
	}
	if rows.Next() {
		rows.Scan(&parentBlockEpochId)
	} else {
		return nil, fmt.Errorf("Parent block not found.")
	}
	rows.Close()

	// Get the epoch.
	epoch := Epoch{}
	rows, err = dag.db.Query("select id, start_block_hash, start_time, start_height, difficulty from epochs where id = ? limit 1", parentBlockEpochId)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	if rows.Next() {
		startBlockHash := []byte{}
		difficulty := []byte{}
		err := rows.Scan(&epoch.Id, &startBlockHash, &epoch.StartTime, &epoch.StartHeight, &difficulty)
		if err != nil {
			return nil, err
		}

		copy(epoch.StartBlockHash[:], startBlockHash)
		diffBytes32 := [32]byte{}
		copy(diffBytes32[:], difficulty)
		epoch.Difficulty = Bytes32ToBigInt(diffBytes32)
	} else {
		return nil, fmt.Errorf("Epoch not found.")
	}

	return &epoch, nil
}

func (dag *BlockDAG) GetBlockByHash(hash [32]byte) (*Block, error) {
	block := Block{}

	// Query database.
	rows, err := dag.db.Query("select hash, parent_hash, parent_total_work, timestamp, num_transactions, transactions_merkle_root, nonce, graffiti, height, epoch, size_bytes, acc_work from blocks where hash = ? limit 1", hash[:])
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	if rows.Next() {
		hash := []byte{}
		parentHash := []byte{}
		transactionsMerkleRoot := []byte{}
		nonce := []byte{}
		graffiti := []byte{}
		accWorkBuf := []byte{}
		parentTotalWorkBuf := []byte{}

		err := rows.Scan(
			&hash,
			&parentHash,
			&parentTotalWorkBuf,
			&block.Timestamp,
			&block.NumTransactions,
			&transactionsMerkleRoot,
			&nonce,
			&graffiti,
			&block.Height,
			&block.Epoch,
			&block.SizeBytes,
			&accWorkBuf,
		)

		if err != nil {
			return nil, err
		}

		copy(block.Hash[:], hash)
		copy(block.ParentHash[:], parentHash)
		copy(block.TransactionsMerkleRoot[:], transactionsMerkleRoot)
		copy(block.Nonce[:], nonce)
		copy(block.Graffiti[:], graffiti)

		accWork := [32]byte{}
		copy(accWork[:], accWorkBuf)
		block.AccumulatedWork = Bytes32ToBigInt(accWork)

		parentTotalWork := [32]byte{}
		copy(parentTotalWork[:], parentTotalWorkBuf)
		block.ParentTotalWork = Bytes32ToBigInt(parentTotalWork)

		return &block, nil
	} else {
		return nil, err
	}
}

func (dag *BlockDAG) GetBlockTransactions(hash [32]byte) (*[]Transaction, error) {
	// Query database, get transactions count for blockhash.
	rows, err := dag.db.Query("select count(*) from transactions where block_hash = ?", hash[:])
	if err != nil {
		return nil, err
	}

	count := 0
	if rows.Next() {
		rows.Scan(&count)
	}
	rows.Close()

	// Construct the buffer.
	txs := make([]Transaction, count)

	// Load the transactions in.
	rows, err = dag.db.Query(`
		SELECT t.hash, t.sig, t.from_pubkey, t.to_pubkey, t.amount, t.fee, t.nonce, tb.txindex, t.version
		FROM transactions t
		JOIN transactions_blocks tb ON t.hash = tb.transaction_hash
		WHERE tb.block_hash = ?
	`, hash[:])
	if err != nil {
		return nil, err
	}

	for rows.Next() {
		tx := Transaction{}
		hash := []byte{}
		sig := []byte{}
		fromPubkey := []byte{}
		toPubkey := []byte{}
		amount := uint64(0)
		fee := uint64(0)
		nonce := uint64(0)
		var index uint64 = 0
		version := 0

		err := rows.Scan(&hash, &sig, &fromPubkey, &toPubkey, &amount, &fee, &nonce, &index, &version)
		if err != nil {
			return nil, err
		}

		copy(tx.Hash[:], hash)
		copy(tx.Sig[:], sig)
		copy(tx.FromPubkey[:], fromPubkey)
		copy(tx.ToPubkey[:], toPubkey)
		tx.Amount = amount
		tx.Fee = fee
		tx.Nonce = nonce
		tx.TxIndex = index
		tx.Version = byte(version)

		txs[index] = tx
	}

	return &txs, nil
}

func (dag *BlockDAG) GetRawBlockDataByHash(hash [32]byte) ([]byte, error) {
	// TODO.
	// get block from disk
	// get txs from disk
	// load into raw block
	// return
}

func (dag *BlockDAG) HasBlock(hash [32]byte) bool {
	rows, err := dag.db.Query("select count(*) from blocks where hash = ?", hash[:])
	if err != nil {
		return false
	}
	count := 0
	if rows.Next() {
		rows.Scan(&count)
	}
	rows.Close()

	return count > 0
}

// Gets the latest block in the longest chain.
func (dag *BlockDAG) GetLatestTip() (Block, error) {
	// The tip of the chain is defined as the chain with the longest proof-of-work.
	// Simply put, given a DAG of blocks, where each block has an accumulated work, we want to find the path with the highest accumulated work.

	// Query the highest accumulated work block in the database.
	rows, err := dag.db.Query("select hash from blocks order by acc_work desc limit 1")
	if err != nil {
		return Block{}, err
	}
	if !rows.Next() {
		return Block{}, fmt.Errorf("No blocks found.")
	}

	hashBuf := []byte{}
	err = rows.Scan(&hashBuf)
	if err != nil {
		return Block{}, err
	}
	rows.Close()

	hash := [32]byte{}
	copy(hash[:], hashBuf)

	// Get the block.
	block, err := dag.GetBlockByHash(hash)
	if err != nil {
		return Block{}, err
	}

	return *block, nil
}

// Gets the list of hashes for the longest chain, traversing backwards from startHash and accumulating depthFromTip items.
func (dag *BlockDAG) GetLongestChainHashList(startHash [32]byte, depthFromTip uint64) ([][32]byte, error) {
	list := make([][32]byte, 0, depthFromTip)

	// Hey, I bet you didn't know SQL could do this, right?
	// Neither did I. It's called a recursive common table expression.
	// It's a way to traverse a tree structure in SQL.
	// Pretty cool, huh?
	rows, err := dag.db.Query(`
		WITH RECURSIVE block_path AS (
			SELECT hash, parent_hash, 1 AS depth
			FROM blocks
			WHERE hash = ?

			UNION ALL

			SELECT b.hash, b.parent_hash, bp.depth + 1
			FROM blocks b
			INNER JOIN block_path bp ON b.hash = bp.parent_hash
			WHERE bp.depth < ?
		)
		SELECT hash, parent_hash
		FROM block_path
		ORDER BY depth DESC;`,
		startHash[:],
		depthFromTip,
	)
	if err != nil {
		return list, err
	}

	for rows.Next() {
		hashBuf := []byte{}
		parentHashBuf := []byte{}

		hash := [32]byte{}
		parentHash := [32]byte{}

		err := rows.Scan(&hashBuf, &parentHashBuf)
		if err != nil {
			return list, err
		}

		copy(hash[:], hashBuf)
		copy(parentHash[:], parentHashBuf)

		list = append(list, hash)
	}

	return list, nil
}
