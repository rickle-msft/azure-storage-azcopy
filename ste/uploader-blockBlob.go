// Copyright © 2017 Microsoft <wastore@microsoft.com>
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.

package ste

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"github.com/Azure/azure-pipeline-go/pipeline"
	"github.com/Azure/azure-storage-azcopy/common"
	"github.com/Azure/azure-storage-blob-go/azblob"
	"net/url"
	"sync"
	"sync/atomic"
	"time"
)

const (
	plNotNeeded   = -1
	plNeedUnknown = 0
	plNeeded      = 1
)

type blockBlobUploader struct {
	jptm         IJobPartTransferMgr
	blockBlobUrl azblob.BlockBlobURL
	chunkSize    uint32
	numChunks    uint32
	pipeline     pipeline.Pipeline
	pacer        *pacer
	leadingBytes []byte // no lock because is written before first chunk-func go routine is scheduled

	putListIndicator int32       // accessed via sync.atomic
	mu               *sync.Mutex // protects the fields below
	blockIds         []string
}

func newBlockBlobUploader(jptm IJobPartTransferMgr, destination string, p pipeline.Pipeline, pacer *pacer) (uploader, error) {
	// compute chunk count
	info := jptm.Info()
	fileSize := info.SourceSize
	chunkSize := info.BlockSize

	numChunks := getNumUploadChunks(fileSize, chunkSize)
	if numChunks > common.MaxNumberOfBlocksPerBlob {
		return nil, errors.New(
			fmt.Sprintf("BlockSize %d for uploading source of size %d is not correct. Number of blocks will exceed the limit",
				chunkSize,
				fileSize))
	}

	// make sure URL is parsable
	destURL, err := url.Parse(destination)
	if err != nil {
		return nil, err
	}

	return &blockBlobUploader{
		jptm:         jptm,
		blockBlobUrl: azblob.NewBlobURL(*destURL, p).ToBlockBlobURL(),
		chunkSize:    chunkSize,
		numChunks:    numChunks,
		pipeline:     p,
		pacer:        pacer,
		mu:           &sync.Mutex{},
		blockIds:     make([]string, numChunks),
	}, nil
}

func (u *blockBlobUploader) ChunkSize() uint32 {
	return u.chunkSize
}

func (u *blockBlobUploader) NumChunks() uint32 {
	return u.numChunks
}

func (u *blockBlobUploader) SetLeadingBytes(leadingBytes []byte) {
	u.leadingBytes = leadingBytes
}

func (u *blockBlobUploader) RemoteFileExists() (bool, error) {
	_, err := u.blockBlobUrl.GetProperties(u.jptm.Context(), azblob.BlobAccessConditions{})
	return err == nil, nil
	// TODO: is there a better, more robust way to do this check, rather than just taking ANY error as evidence of non-existence?
	//      Can't just look at the response object, because its null if error is non null (where does that null come from?  Wouldn't a non-null value be reasonable in the 404 case?)
}

func (u *blockBlobUploader) Prologue(leadingBytes []byte) {
	// block blobs don't need any work done at this stage
	// But we do need to remember the leading bytes because we'll need them later
	u.leadingBytes = leadingBytes
}

// Returns a chunk-func for blob uploads
func (u *blockBlobUploader) GenerateUploadFunc(id common.ChunkID, blockIndex int32, reader common.SingleChunkReader, chunkIsWholeFile bool) chunkFunc {

	if chunkIsWholeFile {
		if blockIndex > 0 {
			panic("chunk cannot be whole file where there is more than one chunk")
		}
		u.setPutListNeed(plNotNeeded)
		return u.generatePutWholeBlob(id, blockIndex, reader)
	} else {
		u.setPutListNeed(plNeeded)
		return u.generatePutBlock(id, blockIndex, reader)
	}
}

// generatePutBlock generates a func to uploads the block of src data from given startIndex till the given chunkSize.
func (u *blockBlobUploader) generatePutBlock(id common.ChunkID, blockIndex int32, reader common.SingleChunkReader) chunkFunc {

	return createUploadChunkFunc(u.jptm, id, func() {
		jptm := u.jptm

		// step 1: generate block ID
		blockId := common.NewUUID().String()
		encodedBlockId := base64.StdEncoding.EncodeToString([]byte(blockId))

		// step 2: save the block ID into the list of block IDs
		u.setBlockId(blockIndex, encodedBlockId)

		// step 3: perform put block
		u.jptm.LogChunkStatus(id, common.EWaitReason.Body())
		body := newLiteRequestBodyPacer(reader, u.pacer)
		_, err := u.blockBlobUrl.StageBlock(u.jptm.Context(), encodedBlockId, body, azblob.LeaseAccessConditions{}, nil)
		if err != nil {
			jptm.FailActiveUpload("Staging block", err)
			return
		}
	})
}

// generates PUT Blob (for a blob that fits in a single put request)
func (u *blockBlobUploader) generatePutWholeBlob(id common.ChunkID, blockIndex int32, reader common.SingleChunkReader) chunkFunc {

	return createUploadChunkFunc(u.jptm, id, func() {
		jptm := u.jptm

		// Get blob http headers and metadata.
		blobHttpHeader, metaData := jptm.BlobDstData(u.leadingBytes)

		// Upload the blob
		u.jptm.LogChunkStatus(id, common.EWaitReason.Body())
		var err error
		if jptm.Info().SourceSize == 0 {
			_, err = u.blockBlobUrl.Upload(jptm.Context(), bytes.NewReader(nil), blobHttpHeader, metaData, azblob.BlobAccessConditions{})
		} else {
			body := newLiteRequestBodyPacer(reader, u.pacer)
			_, err = u.blockBlobUrl.Upload(jptm.Context(), body, blobHttpHeader, metaData, azblob.BlobAccessConditions{})
		}

		// if the put blob is a failure, update the transfer status to failed
		if err != nil {
			jptm.FailActiveUpload("Uploading block", err)
			return
		}
	})
}

func (u *blockBlobUploader) Epilogue() {
	u.mu.Lock()
	shouldPutBlockList := u.putListIndicator
	blockIds := u.blockIds
	u.mu.Unlock()
	if shouldPutBlockList == plNeedUnknown {
		panic("'put list' need flag was never set")
	}

	jptm := u.jptm

	// TODO: finalize and wrap in functions whether 0 is included or excluded in status comparisons

	// commit the blocks, if necessary
	if jptm.TransferStatus() > 0 && shouldPutBlockList == plNeeded {
		jptm.Log(pipeline.LogDebug, fmt.Sprintf("Conclude Transfer with BlockList %s", u.blockIds))

		// fetching the blob http headers with content-type, content-encoding attributes
		// fetching the metadata passed with the JobPartOrder
		blobHttpHeader, metaData := jptm.BlobDstData(u.leadingBytes)

		_, err := u.blockBlobUrl.CommitBlockList(jptm.Context(), blockIds, blobHttpHeader, metaData, azblob.BlobAccessConditions{})
		if err != nil {
			jptm.FailActiveUpload("Committing block list", err)
			// don't return, since need cleanup below
		} else {
			jptm.Log(pipeline.LogInfo, "UPLOAD SUCCESSFUL ")
		}
	}

	// set tier
	if jptm.TransferStatus() > 0 {
		blockBlobTier, _ := jptm.BlobTiers()
		if blockBlobTier != common.EBlockBlobTier.None() {
			// for blob tier, set the latest service version from sdk as service version in the context.
			ctxWithValue := context.WithValue(jptm.Context(), ServiceAPIVersionOverride, azblob.ServiceVersion)
			_, err := u.blockBlobUrl.SetTier(ctxWithValue, blockBlobTier.ToAccessTierType(), azblob.LeaseAccessConditions{})
			if err != nil {
				jptm.FailActiveUploadWithStatus("Setting BlockBlob tier", err, common.ETransferStatus.BlobTierFailure())
				// don't return, because need cleanup below
			}
		}
	}

	// Cleanup
	if jptm.TransferStatus() <= 0 { // TODO: <=0 or <0?
		// If the transfer status value < 0, then transfer failed with some failure
		// there is a possibility that some uncommitted blocks will be there
		// Delete the uncommitted blobs
		// TODO: should we really do this deletion?  What if we are in an overwrite-existing-blob
		//    situation. Deletion has very different semantics then, compared to not deleting.
		deletionContext, _ := context.WithTimeout(context.Background(), 30*time.Second)
		_, _ = u.blockBlobUrl.Delete(deletionContext, azblob.DeleteSnapshotsOptionNone, azblob.BlobAccessConditions{})
		// TODO: question, is it OK to remoe this logging of failures (since there's no adverse effect of failure)
		//  if stErr, ok := err.(azblob.StorageError); ok && stErr.Response().StatusCode != http.StatusNotFound {
		// If the delete failed with Status Not Found, then it means there were no uncommitted blocks.
		// Other errors report that uncommitted blocks are there
		// bbu.jptm.LogError(bbu.blobURL.String(), "Deleting uncommitted blocks", err)
		//  }

	}

}

func (u *blockBlobUploader) setPutListNeed(value int32) {
	// atomic because uploaders are used by multiple threads at the same time
	previous := atomic.SwapInt32(&u.putListIndicator, value)
	if previous != plNeedUnknown && previous != value {
		panic("'put list' need cannot be set twice")
	}
}

func (u *blockBlobUploader) setBlockId(index int32, value string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if len(u.blockIds[index]) > 0 {
		panic("block id set twice for one block")
	}
	u.blockIds[index] = value
}
