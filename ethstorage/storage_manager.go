// Copyright 2022-2023, EthStorage.
// For license information, see https://github.com/ethstorage/es-node/blob/main/LICENSE

package ethstorage

import (
	"bytes"
	"context"
	"errors"
	"math/big"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/rpc"
)

const (
	blobFillingMask    = byte(0b10000000)
	HashSizeInContract = 24
	MetaBatchSize      = 8000
	MetaDownloadThread = 32
)

var (
	errCommitMismatch = errors.New("commit from contract and input is not matched")
)

type Il1Source interface {
	GetKvMetas(kvIndices []uint64, blockNumber int64) ([][32]byte, error)

	GetStorageLastBlobIdx(blockNumber int64) (uint64, error)

	HeaderByNumber(ctx context.Context, number *big.Int) (*types.Header, error)
}

// StorageManager is a higher-level abstract of ShardManager which provides multi-thread safety to storage file read/write
// and a consistent view of most-recent-finalized L1 block.
type StorageManager struct {
	shardManager      *ShardManager
	localL1           int64      // local view of most-recent-finalized L1 block
	mu                sync.Mutex // protect lastKvIdx, shardManager and blobMeta read/write state
	lastKvIdx         uint64     // lastKvIndex in the most-recent-finalized L1 block
	l1Source          Il1Source
	DownloadThreadNum int
	blobMetas         map[uint64][32]byte
}

func NewStorageManager(sm *ShardManager, l1Source Il1Source) *StorageManager {
	return &StorageManager{
		shardManager: sm,
		l1Source:     l1Source,
		blobMetas:    map[uint64][32]byte{},
	}
}

// DownloadFinished This function will be called when the node found new block are finalized, and it will update the
// local L1 view and commit new blobs into local storage file.
func (s *StorageManager) DownloadFinished(newL1 int64, kvIndices []uint64, blobs [][]byte, commits []common.Hash) error {
	if len(kvIndices) != len(blobs) || len(blobs) != len(commits) {
		return errors.New("invalid params lens")
	}

	s.mu.Lock()

	// in most case, newL1 should be equal to s.localL1 + 32
	// but it is possible that the node was shutdown for some time, and when it restart and DownloadFinished for the first time
	// the new finalized L1 will be larger than that, so we just do the simple compare check here.
	if newL1 <= s.localL1 {
		s.mu.Unlock()
		return errors.New("new L1 is older than local L1")
	}

	lastKvIdx, err := s.l1Source.GetStorageLastBlobIdx(newL1)
	if err != nil {
		s.mu.Unlock()
		return err
	}
	s.lastKvIdx = lastKvIdx

	taskNum := s.DownloadThreadNum
	var wg sync.WaitGroup
	chanRes := make(chan error, taskNum)
	defer close(chanRes)

	taskIdx := 0
	for taskIdx < taskNum {
		if taskIdx >= len(kvIndices) {
			break
		}

		wg.Add(1)

		insertIdxInTask := make([]int, 0)
		for i := taskIdx; i < len(kvIndices); i += taskNum {
			insertIdxInTask = append(insertIdxInTask, i)
		}

		go func(insertIdx []int, out chan<- error) {
			defer wg.Done()

			var err error = nil
			for _, idx := range insertIdx {
				c := prepareCommit(commits[idx])
				// if return false, just ignore because we are not intersted in it
				_, err = s.shardManager.TryWrite(kvIndices[idx], blobs[idx], c)
				if err != nil {
					break
				}
			}

			chanRes <- err
		}(insertIdxInTask, chanRes)

		taskIdx++
	}

	wg.Wait()

	for i := 0; i < taskIdx; i++ {
		res := <- chanRes
		if (res != nil) {
			s.mu.Unlock()
			return res
		}
	}

	s.localL1 = newL1

	s.mu.Unlock()

	s.updateLocalMetas(kvIndices, commits)

	return nil
}

func prepareCommit(commit common.Hash) common.Hash {
	c := common.Hash{}
	copy(c[0:HashSizeInContract], commit[0:HashSizeInContract])

	// The first bit after data hash in the meta indicate whether this blob has been filled. 0 stands for NOT filled yet.
	// We want to make sure this bit to be 1 when filling data
	c[HashSizeInContract] = c[HashSizeInContract] | blobFillingMask

	return c
}

// Reset This function must be called before calling any other funcs, it will setup a local L1 view for the node.
func (s *StorageManager) Reset(newL1 int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	lastKvIdx, err := s.l1Source.GetStorageLastBlobIdx(newL1)
	if err != nil {
		return err
	}
	s.lastKvIdx = lastKvIdx
	s.localL1 = newL1

	return nil
}

// CommitBlobs This function will be called when p2p sync received blobs. It will commit the blobs
// that match local L1 view and return the unmatched ones.
// Note that the caller must make sure the blobs data and the corresponding commit are matched.
func (s *StorageManager) CommitBlobs(kvIndices []uint64, blobs [][]byte, commits []common.Hash) ([]uint64, error) {
	if len(kvIndices) != len(blobs) || len(blobs) != len(commits) {
		return nil, errors.New("invalid params lens")
	}
	var (
		l            = len(kvIndices)
		encodedBlobs = make([][]byte, l)
		encoded      = make([]bool, l)
	)
	for i := 0; i < len(kvIndices); i++ {
		encodedBlob, success, err := s.shardManager.TryEncodeKV(kvIndices[i], blobs[i], commits[i])
		if !success || err != nil {
			log.Warn("Blob encode failed", "index", kvIndices[i], "err", err.Error())
			continue
		}
		encodedBlobs[i] = encodedBlob
		encoded[i] = true
	}

	metas := s.getKvMetas(kvIndices)

	s.mu.Lock()
	defer s.mu.Unlock()

	inserted := []uint64{}
	for i, contractMeta := range metas {
		if !encoded[i] {
			continue
		}
		err := s.commitEncodedBlob(kvIndices[i], encodedBlobs[i], commits[i], contractMeta)
		if err != nil {
			log.Info("Commit blobs fail", "kvIndex", kvIndices[i], "err", err.Error())
			continue
		}
		inserted = append(inserted, kvIndices[i])
	}
	return inserted, nil
}

// CommitEmptyBlobs use to commit batch empty blobs, return inserted blobs count, next index to fill
// and error GetKvMetas got. Any error (like encode or commit) happen to a blob, cancel to rest.
func (s *StorageManager) CommitEmptyBlobs(start, limit uint64) (uint64, uint64, error) {
	var (
		encodedBlobs = make([][]byte, 0)
		kvIndices    = make([]uint64, 0)
		inserted     = uint64(0)
		emptyBs      = make([]byte, 0)
		hash         = common.Hash{}
		next         = start
	)
	for i := start; i <= limit; i++ {
		encodedBlob, success, err := s.shardManager.TryEncodeKV(i, emptyBs, hash)
		if !success || err != nil {
			log.Warn("Blob encode failed", "index", i, "err", err.Error())
			break
		}
		encodedBlobs = append(encodedBlobs, encodedBlob)
		kvIndices = append(kvIndices, i)
	}

	metas:= s.getKvMetas(kvIndices)

	s.mu.Lock()
	defer s.mu.Unlock()

	for i, index := range kvIndices {
		err := s.commitEncodedBlob(index, encodedBlobs[i], hash, metas[i])
		if err == nil {
			inserted++
		} else if err != errCommitMismatch {
			log.Info("Commit blobs fail", "kvIndex", kvIndices[i], "err", err.Error())
			break
		}
		// if meta is not equal to empty hash, that mean the blob is not empty,
		// so cancel the fill empty for that index and continue the rest.
		next++
	}
	return inserted, next, nil
}

// CommitBlob This function will be called when p2p sync received a blob.
// Return err if the passed commit and the one queried from contract are not matched.
func (s *StorageManager) CommitBlob(kvIndex uint64, blob []byte, commit common.Hash) error {
	encodedBlob, success, err := s.shardManager.TryEncodeKV(kvIndex, blob, commit)
	if !success || err != nil {
		return errors.New("blob encode failed")
	}

	metas:= s.getKvMetas([]uint64{kvIndex})

	s.mu.Lock()
	defer s.mu.Unlock()

	if len(metas) != 1 {
		return errors.New("invalid params lens")
	}

	contractMeta := metas[0]
	return s.commitEncodedBlob(kvIndex, encodedBlob, commit, contractMeta)
}

func (s *StorageManager) commitEncodedBlob(kvIndex uint64, encodedBlob []byte, commit common.Hash, contractMeta [32]byte) error {
	// the commit is different with what we got from the contract, so should not commit
	if !bytes.Equal(contractMeta[32-HashSizeInContract:32], commit[0:HashSizeInContract]) {
		return errCommitMismatch
	}

	m, success, err := s.shardManager.TryReadMeta(kvIndex)
	if !success || err != nil {
		return errors.New("metadata read failed")
	}

	contractKvIdx := new(big.Int).SetBytes(contractMeta[0:5]).Uint64()
	if contractKvIdx != kvIndex {
		return errors.New("kvIdx from contract and input is not matched")
	}

	localMeta := common.Hash{}
	copy(localMeta[:], m)

	// the local already have the data and we do not need to commit
	// empty filled case: if both of the hash is 0, but local meta shows this encodedBlob hasn't been filled yet, we should also commit
	if bytes.Equal(localMeta[0:HashSizeInContract], commit[0:HashSizeInContract]) && (localMeta[HashSizeInContract]&blobFillingMask) != 0 {
		return nil
	}

	c := prepareCommit(commit)

	success, err = s.shardManager.TryWriteEncoded(kvIndex, encodedBlob, c)
	if !success || err != nil {
		return errors.New("encodedBlob write failed")
	}
	return nil
}

func (s *StorageManager) syncCheck(kvIdx uint64) error {
	meta, success, err := s.shardManager.TryReadMeta(kvIdx)
	if !success || err != nil {
		return errors.New("meta reading failed")
	}

	// There are two cases that we do NOT want to return data: not synced and empty filled
	h0 := common.Hash{} // means not filled, e.g. haven't been synced yet

	h1 := common.Hash{}
	h1[HashSizeInContract] = h1[HashSizeInContract] | blobFillingMask // means empty filled

	hash := common.Hash{}
	copy(hash[:], meta)
	if hash == h0 || hash == h1 {
		return errors.New("syncing or just empty blob")
	}

	return nil
}

// DownloadAllMetas This function download the blob hashes of all the local storage shards from the smart contract
func (s *StorageManager) DownloadAllMetas() error {
	header, err := s.l1Source.HeaderByNumber(context.Background(), big.NewInt(rpc.FinalizedBlockNumber.Int64()))
	if err != nil {
		return err
	}
	l1 := header.Number.Int64()

	lastKvIdx, err := s.l1Source.GetStorageLastBlobIdx(l1)
	if err != nil {
		return err
	}

	for _, sid := range s.Shards() {
		first, limit := s.KvEntries()*sid, s.KvEntries()*(sid+1)

		// batch request metas until the lastKvIdx
		end := limit
		if end > lastKvIdx {
			end = lastKvIdx
		}
		log.Info("Begin to download metas", "shard", sid, "first", first, "end", end, "limit", limit, "lastKvIdx", lastKvIdx)
		ts := time.Now()

		err := s.downloadMetaInParallel(first, end)
		if err != nil {
			return err
		}

		log.Info("All the metas has been downloaded", "first", first, "end", end, "time", time.Since(ts).Seconds())
		ts = time.Now()

		// empty blobs
		for i := end; i < limit; i++ {
			meta := [32]byte{}
			new(big.Int).SetInt64(int64(i)).FillBytes(meta[0:5])

			s.blobMetas[i] = meta
		}

		log.Info("Empty metas has been filled", "first", end, "limit", limit, "time", time.Since(ts).Seconds())
	}

	return nil
}

func (s *StorageManager) downloadMetaInParallel(from, to uint64) error {
	var wg sync.WaitGroup
	taskNum := uint64(MetaDownloadThread)

	// We don't need to download in parallel if the meta amount is small
	if to - from < uint64(taskNum) * MetaBatchSize {
		return s.downloadMetaInRange(from, to, 0)
	}

	chanRes := make(chan error, taskNum)
	defer close(chanRes)

	rangeSize := (to - from) / uint64(taskNum)
	for taskIdx := uint64(0); taskIdx < taskNum; taskIdx++ {
		rangeStart := taskIdx * rangeSize
		rangeEnd := (taskIdx + 1) * rangeSize
		if taskIdx == taskNum - 1 {
			rangeEnd = to
		}
		wg.Add(1)

		go func(start, end, taskId uint64, out chan<- error) {
			defer wg.Done()
			err := s.downloadMetaInRange(start, end, taskId)

			chanRes <- err
		}(rangeStart, rangeEnd, taskIdx, chanRes)
	}

	wg.Wait()

	for i := uint64(0); i < taskNum; i++ {
		res := <- chanRes
		if (res != nil) {
			return res
		}
	}

	return nil
}

func (s *StorageManager) downloadMetaInRange(from, to uint64, taskId uint64) error {
	for from < to {
		batchLimit := from + MetaBatchSize
		if batchLimit > to {
			batchLimit = to
		}
		kvIndices := []uint64{}
		for i := from; i < batchLimit; i++ {
			kvIndices = append(kvIndices, i)
		}

		s.mu.Lock()
		localL1 := s.localL1
		s.mu.Unlock()

		metas, err := s.l1Source.GetKvMetas(kvIndices, localL1)
		if err != nil {
			// Retry the request again in case it could fail occasionally in poor network connection
			metas, err = s.l1Source.GetKvMetas(kvIndices, localL1)
			if err != nil {
				return err
			}
		}

		s.mu.Lock()
		if localL1 != s.localL1 {
			s.mu.Unlock()
			continue
		}
		for i, meta := range(metas) {
			s.blobMetas[kvIndices[i]] = meta
		}
		s.mu.Unlock()

		log.Info("One batch metas has been downloaded", "first", from, "batchLimit", batchLimit, "to", to, "taskId", taskId)

		from = batchLimit
	}
	return nil
}

func (s *StorageManager) updateLocalMetas(kvIndices []uint64, commits []common.Hash) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for i, idx := range kvIndices {
		meta := [32]byte{}
		new(big.Int).SetInt64(int64(idx)).FillBytes(meta[0:5])
		copy(meta[32-HashSizeInContract:32], commits[i][0:HashSizeInContract])

		s.blobMetas[idx] = meta
	}
}

func (s *StorageManager) getKvMetas(kvIndices []uint64) [][32]byte {
	s.mu.Lock()
	defer s.mu.Unlock()

	metas := [][32]byte{}
	for _, i := range kvIndices {
		metas = append(metas, s.blobMetas[i])
	}
	return metas
}

// TryReadEncoded This function will read the encoded data from the local storage file. It also check whether the blob is empty or not synced,
// if they are these two cases, it will return err.
func (s *StorageManager) TryReadEncoded(kvIdx uint64, readLen int) ([]byte, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	err := s.syncCheck(kvIdx)
	if err != nil {
		return nil, false, err
	}

	return s.shardManager.TryReadEncoded(kvIdx, readLen)
}

func (s *StorageManager) TryRead(kvIdx uint64, readLen int, commit common.Hash) ([]byte, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.shardManager.TryRead(kvIdx, readLen, commit)
}

func (s *StorageManager) TryReadMeta(kvIdx uint64) ([]byte, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.shardManager.TryReadMeta(kvIdx)
}

func (s *StorageManager) LastKvIndex() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastKvIdx
}

func (s *StorageManager) DecodeKV(kvIdx uint64, b []byte, hash common.Hash, providerAddr common.Address, encodeType uint64) ([]byte, bool, error) {
	return s.shardManager.DecodeKV(kvIdx, b, hash, providerAddr, encodeType)
}

func (s *StorageManager) KvEntries() uint64 {
	return s.shardManager.kvEntries
}

func (s *StorageManager) ContractAddress() common.Address {
	return s.shardManager.contractAddress
}

func (s *StorageManager) Shards() []uint64 {
	shards := make([]uint64, 0)
	for idx := range s.shardManager.ShardMap() {
		shards = append(shards, idx)
	}
	return shards
}

func (s *StorageManager) ReadSampleUnlocked(shardIdx, sampleIdx uint64) (common.Hash, error) {
	if ds, ok := s.shardManager.shardMap[shardIdx]; ok {
		return ds.ReadSample(sampleIdx)
	}
	return common.Hash{}, errors.New("shard not found")
}

func (s *StorageManager) GetShardMiner(shardIdx uint64) (common.Address, bool) {
	return s.shardManager.GetShardMiner(shardIdx)
}

func (s *StorageManager) GetShardEncodeType(shardIdx uint64) (uint64, bool) {
	return s.shardManager.GetShardEncodeType(shardIdx)
}

func (s *StorageManager) MaxKvSize() uint64 {
	return s.shardManager.kvSize
}

func (s *StorageManager) MaxKvSizeBits() uint64 {
	return s.shardManager.kvSizeBits
}
func (s *StorageManager) ChunksPerKvBits() uint64 {
	return s.shardManager.chunksPerKvBits
}

func (s *StorageManager) KvEntriesBits() uint64 {
	return s.shardManager.kvEntriesBits
}

func (s *StorageManager) Close() error {
	return s.shardManager.Close()
}
