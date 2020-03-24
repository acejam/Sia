package renter

import (
	"fmt"
	"io"
	"sync"

	"gitlab.com/NebulousLabs/errors"

	"gitlab.com/NebulousLabs/Sia/build"
	"gitlab.com/NebulousLabs/Sia/crypto"
	"gitlab.com/NebulousLabs/Sia/modules"
	"gitlab.com/NebulousLabs/Sia/modules/renter/filesystem"
	"gitlab.com/NebulousLabs/Sia/modules/renter/filesystem/siafile"
	"gitlab.com/NebulousLabs/Sia/types"
)

// Upload Streaming Overview:
// Most of the logic that enables upload streaming can be found within
// UploadStreamFromReader and the StreamShard. As seen at the beginning of the
// big for - loop in UploadStreamFromReader, the streamer currently always
// assumes that the data provided by the user starts at index 0 of chunk 0. In
// every iteration the siafile is grown by a single chunk to prepare for the
// upload of the next chunk. To allow the upload code to repair a chunk from a
// stream, the stream is passed into the unfinished chunk as a new field. If the
// upload code detects a stream, it will use that instead of a local file to
// fetch the chunk's logical data. As soon as the upload code is done fetching
// the logical data, it will close that streamer to signal the loop that it's
// save to upload another chunk.
// This is possible due to the custom StreamShard type which is a wrapper for a
// io.Reader with a channel which is closed when the StreamShard is closed.

// StreamShard is a helper type that allows us to split an io.Reader up into
// multiple readers, wait for the shard to finish reading and then check the
// error for that Read. SignalChan will be closed when the shard has been
// closed.
type StreamShard struct {
	n    int
	peek []byte
	err  error

	r io.Reader

	closed     bool
	mu         sync.Mutex
	signalChan chan struct{}
}

// NewStreamShard creates a new stream shard from a reader.
func NewStreamShard(r io.Reader, peek []byte) *StreamShard {
	return &StreamShard{
		peek:       peek,
		r:          r,
		signalChan: make(chan struct{}),
	}
}

// Close closes the underlying channel of the shard.
func (ss *StreamShard) Close() error {
	close(ss.signalChan)
	ss.closed = true
	return nil
}

// Peek will check to see if there is more data in the stream.
func (ss *StreamShard) Peek() ([]byte, error) {
	ss.mu.Lock()
	defer ss.mu.Unlock()

	// If 'peek' already has data, then there is more data to consume.
	if len(ss.peek) > 0 {
		return ss.peek, nil
	}

	// Read a byte into peek.
	ss.peek = append(ss.peek, 0)
	_, err := io.ReadFull(ss.r, ss.peek)
	if err != nil {
		ss.err = err
		return nil, err
	}
	return ss.peek, nil
}

// Result returns the returned values of calling Read on the shard.
func (ss *StreamShard) Result() (int, error) {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	return ss.n, ss.err
}

// Read implements the io.Reader interface.
func (ss *StreamShard) Read(b []byte) (int, error) {
	if ss.closed {
		return 0, errors.New("StreamShard already closed")
	}
	ss.mu.Lock()
	defer ss.mu.Unlock()

	if len(b) < 1 {
		return 0, nil
	}
	var peekBytes int // will be 0 or 1
	if len(ss.peek) > 0 {
		// Sanity check - peek should never be more than 1 byte.
		if len(ss.peek) > 1 {
			build.Critical("stream shard has too many bytes in the peek field", len(ss.peek))
		}
		b[0] = ss.peek[0]
		b = b[1:]
		ss.n += 1
		ss.peek = ss.peek[:0]
		peekBytes++
	}
	n, err := ss.r.Read(b)
	ss.n += n
	ss.err = err
	return n + peekBytes, err
}

// UploadStreamFromReader reads from the provided reader until io.EOF is reached and
// upload the data to the Sia network.
func (r *Renter) UploadStreamFromReader(up modules.FileUploadParams, reader io.Reader) error {
	if err := r.tg.Add(); err != nil {
		return err
	}
	defer r.tg.Done()

	// Perform the upload, close the filenode, and return.
	fileNode, err := r.callUploadStreamFromReader(up, reader, false)
	if err != nil {
		return errors.AddContext(err, "unable to stream an upload from a reader")
	}
	return fileNode.Close()
}

// managedInitUploadStream verifies the upload parameters and prepares an empty
// SiaFile for the upload.
func (r *Renter) managedInitUploadStream(up modules.FileUploadParams, backup bool) (*filesystem.FileNode, error) {
	siaPath, ec, force, repair, dryRun, cipherType := up.SiaPath, up.ErasureCode, up.Force, up.Repair, up.DryRun, up.CipherType
	// Check if ec was set. If not use defaults.
	var err error
	if ec == nil && !repair {
		up.ErasureCode, err = siafile.NewRSSubCode(DefaultDataPieces, DefaultParityPieces, 64)
		if err != nil {
			return nil, err
		}
		ec = up.ErasureCode
	} else if ec != nil && repair {
		return nil, errors.New("can't provide erasure code settings when doing repairs")
	}

	// Make sure that force and repair aren't both set.
	if force && repair {
		return nil, errors.New("'force' and 'repair' can't both be set")
	}

	// Make sure that dryRun is not used in combination with other flags.
	if dryRun && force {
		return nil, errors.New("'dryRun' and 'force' can't both be set")
	}
	if dryRun && repair {
		return nil, errors.New("'dryRun' and 'repair' can't both be set")
	}

	// Delete existing file if overwrite flag is set. Ignore ErrUnknownPath.
	if force {
		err := r.DeleteFile(siaPath)
		if err != nil && !errors.Contains(err, filesystem.ErrNotExist) {
			return nil, err
		}
	}
	// If repair is set open the existing file.
	if repair {
		entry, err := r.staticFileSystem.OpenSiaFile(siaPath)
		if err != nil {
			return nil, err
		}
		return entry, nil
	}
	// Check that we have contracts to upload to. We need at least data +
	// parity/2 contracts. NumPieces is equal to data+parity, and min pieces is
	// equal to parity. Therefore (NumPieces+MinPieces)/2 = (data+data+parity)/2
	// = data+parity/2.
	numContracts := len(r.hostContractor.Contracts())
	requiredContracts := (ec.NumPieces() + ec.MinPieces()) / 2
	if numContracts < requiredContracts && build.Release != "testing" {
		return nil, fmt.Errorf("not enough contracts to upload file: got %v, needed %v", numContracts, (ec.NumPieces()+ec.MinPieces())/2)
	}
	// Create the Siafile and add to renter
	sk := crypto.GenerateSiaKey(cipherType)
	err = r.staticFileSystem.NewSiaFile(siaPath, up.Source, up.ErasureCode, sk, 0, defaultFilePerm, up.DisablePartialChunk)
	if err != nil {
		return nil, err
	}
	return r.staticFileSystem.OpenSiaFile(siaPath)
}

// callUploadStreamFromReader reads from the provided reader until io.EOF is
// reached and upload the data to the Sia network. Depending on whether backup
// is true or false, the siafile for the upload will be stored in the siafileset
// or backupfileset.
//
// callUploadStreamFromReader will return as soon as the data is available on
// the Sia network, this will happen faster than the entire upload is complete -
// the streamer may continue uploading in the background after returning while
// it is boosting redundancy.
func (r *Renter) callUploadStreamFromReader(up modules.FileUploadParams, reader io.Reader, backup bool) (fileNode *filesystem.FileNode, err error) {
	// Check the upload params first.
	fileNode, err = r.managedInitUploadStream(up, backup)
	if err != nil {
		return nil, err
	}
	// Need to make a copy of this value for the defer statement. Because
	// 'fileNode' is a named value, if you run the call `return nil, err`, then
	// 'fileNode' will be set to 'nil' when defer calls 'fileNode.Close()'.
	fn := fileNode
	defer func() {
		// Ensure the fileNode is closed if there is an error upon return.
		if err != nil {
			err = errors.Compose(err, fn.Close())
		}
	}()

	// In case of a dry-run we don't want to actually push upload chunks onto
	// the heap, but rather only read the pieces from the stream and decorate
	// the siafile with the roots.
	var peek []byte
	if up.DryRun {
		hpk := types.SiaPublicKey{} // blank host key
		ec := fileNode.ErasureCode()
		psize := fileNode.PieceSize()
		csize := fileNode.ChunkSize()

		for chunkIndex := uint64(0); ; chunkIndex++ {
			// Grow the SiaFile to the right size.
			err := fileNode.SiaFile.GrowNumChunks(chunkIndex + 1)
			if err != nil {
				return nil, err
			}

			// Allocate data pieces and fill them with data from r.
			ss := NewStreamShard(reader, peek)
			err = func() error {
				defer ss.Close()

				dataPieces, total, errRead := readDataPieces(ss, ec, psize)
				if errRead != nil {
					return errRead
				}

				dataEncoded, _ := ec.EncodeShards(dataPieces)
				for pieceIndex, dataPieceEnc := range dataEncoded {
					if err := fileNode.SiaFile.AddPiece(hpk, chunkIndex, uint64(pieceIndex), crypto.MerkleRoot(dataPieceEnc)); err != nil {
						return err
					}
				}

				adjustedSize := fileNode.Size() - csize + total
				if err := fileNode.SetFileSize(adjustedSize); err != nil {
					return errors.AddContext(err, "failed to adjust FileSize")
				}
				return nil
			}()
			if err != nil {
				return nil, err
			}

			_, err = ss.Result()
			if err == io.EOF {
				break
			}
			if err != nil {
				return nil, err
			}
		}
		return fileNode, nil
	}

	// Build a map of host public keys.
	pks := make(map[string]types.SiaPublicKey)
	for _, pk := range fileNode.HostPublicKeys() {
		pks[string(pk.Key)] = pk
	}

	// Get the most recent workers.
	hosts := r.managedRefreshHostsAndWorkers()

	// Check if we currently have enough workers for the specified redundancy.
	minWorkers := fileNode.ErasureCode().MinPieces()
	r.staticWorkerPool.mu.RLock()
	availableWorkers := len(r.staticWorkerPool.workers)
	r.staticWorkerPool.mu.RUnlock()
	if availableWorkers < minWorkers {
		return nil, fmt.Errorf("Need at least %v workers for upload but got only %v",
			minWorkers, availableWorkers)
	}

	// Read the chunks we want to upload one by one from the input stream using
	// shards. A shard will signal completion after reading the input but
	// before the upload is done.
	var chunks []*unfinishedUploadChunk
	for chunkIndex := uint64(0); ; chunkIndex++ {
		// Disrupt the upload by closing the reader and simulating losing
		// connectivity during the upload.
		if r.deps.Disrupt("DisruptUploadStream") {
			c, ok := reader.(io.Closer)
			if ok {
				c.Close()
			}
		}
		// Grow the SiaFile to the right size. Otherwise buildUnfinishedChunk
		// won't realize that there are pieces which haven't been repaired yet.
		if err := fileNode.SiaFile.GrowNumChunks(chunkIndex + 1); err != nil {
			return nil, err
		}

		// Start the chunk upload.
		offline, goodForRenew, _ := r.managedContractUtilityMaps()
		uuc, err := r.managedBuildUnfinishedChunk(fileNode, chunkIndex, hosts, pks, true, offline, goodForRenew)
		if err != nil {
			return nil, errors.AddContext(err, "unable to fetch chunk for stream")
		}

		// Create a new shard set it to be the source reader of the chunk.
		ss := NewStreamShard(reader, peek)
		uuc.sourceReader = ss

		// Check if the chunk needs any work or if we can skip it.
		if uuc.piecesCompleted < uuc.piecesNeeded {
			// Add the chunk to the upload heap.
			if !r.uploadHeap.managedPush(uuc) {
				// The chunk can't be added to the heap. It's probably already being
				// repaired. Flush the shard and move on to the next one.
				_, _ = io.ReadFull(ss, make([]byte, fileNode.ChunkSize()))
				if err := ss.Close(); err != nil {
					return nil, err
				}
			}
			// Notify the upload loop.
			chunks = append(chunks, uuc)
			select {
			case r.uploadHeap.newUploads <- struct{}{}:
			default:
			}
		} else {
			// The chunk doesn't need any work. We still need to read a chunk
			// from the shard though. Otherwise we will upload the wrong chunk
			// for the next chunkIndex. We don't need to check the error though
			// since we check that anyway at the end of the loop.
			_, _ = io.ReadFull(ss, make([]byte, fileNode.ChunkSize()))
			if err := ss.Close(); err != nil {
				return nil, err
			}
		}
		// Wait for the shard to be read.
		select {
		case <-r.tg.StopChan():
			return nil, errors.New("interrupted by shutdown")
		case <-ss.signalChan:
		}

		// If an io.EOF error occurred or less than chunkSize was read, we are
		// done. Otherwise we report the error.
		if _, err := ss.Result(); err == io.EOF {
			// All chunks successfully submitted.
			break
		} else if ss.err != nil {
			return nil, ss.err
		}

		// Call Peek to make sure that there's more data for another shard.
		peek, err = ss.Peek()
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			break
		} else if err != nil {
			return nil, ss.err
		}
	}
	// Wait for all chunks to finish, then return.
	for _, chunk := range chunks {
		<-chunk.availableChan
		chunk.mu.Lock()
		err := chunk.err
		chunk.mu.Unlock()
		if err != nil {
			return nil, errors.AddContext(err, "upload streamer failed to get all data available")
		}
	}

	// Disrupt to force an error and ensure the fileNode is being closed
	// correctly.
	if r.deps.Disrupt("failUploadStreamFromReader") {
		return nil, errors.New("disrupted by failUploadStreamFromReader")
	}
	return fileNode, nil
}
