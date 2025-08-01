// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package reader // import "github.com/open-telemetry/opentelemetry-collector-contrib/pkg/stanza/fileconsumer/internal/reader"

import (
	"bufio"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"os"
	"sync"

	"go.opentelemetry.io/collector/component"
	"go.uber.org/zap"
	"golang.org/x/text/encoding"

	"github.com/open-telemetry/opentelemetry-collector-contrib/internal/coreinternal/textutils"
	"github.com/open-telemetry/opentelemetry-collector-contrib/pkg/stanza/fileconsumer/emit"
	"github.com/open-telemetry/opentelemetry-collector-contrib/pkg/stanza/fileconsumer/internal/fingerprint"
	"github.com/open-telemetry/opentelemetry-collector-contrib/pkg/stanza/fileconsumer/internal/header"
	"github.com/open-telemetry/opentelemetry-collector-contrib/pkg/stanza/fileconsumer/internal/scanner"
	"github.com/open-telemetry/opentelemetry-collector-contrib/pkg/stanza/flush"
	"github.com/open-telemetry/opentelemetry-collector-contrib/pkg/stanza/tokenlen"
)

const gzipExtension = ".gz"

type Metadata struct {
	Fingerprint     *fingerprint.Fingerprint
	Offset          int64
	RecordNum       int64
	FileAttributes  map[string]any
	HeaderFinalized bool
	FlushState      flush.State
	TokenLenState   tokenlen.State
	FileType        string
}

// Reader manages a single file
type Reader struct {
	*Metadata
	set                    component.TelemetrySettings
	fileName               string
	file                   *os.File
	reader                 io.Reader
	fingerprintSize        int
	bufPool                *sync.Pool
	initialBufferSize      int
	maxLogSize             int
	headerSplitFunc        bufio.SplitFunc
	contentSplitFunc       bufio.SplitFunc
	decoder                *encoding.Decoder
	headerReader           *header.Reader
	emitFunc               emit.Callback
	deleteAtEOF            bool
	needsUpdateFingerprint bool
	compression            string
	acquireFSLock          bool
	maxBatchSize           int
}

// ReadToEnd will read until the end of the file
func (r *Reader) ReadToEnd(ctx context.Context) {
	if r.acquireFSLock {
		if !r.tryLockFile() {
			return
		}
		defer r.unlockFile()
	}

	switch r.compression {
	case "gzip":
		currentEOF, err := r.createGzipReader()
		if err != nil {
			return
		}
		// Offset tracking in an uncompressed file is based on the length of emitted tokens, but in this case
		// we need to set the offset to the end of the file.
		defer func() {
			r.Offset = currentEOF
		}()
	case "auto":
		// Identifying a filename by its extension may not always be correct. We could have a compressed file without the .gz extension
		if r.FileType == gzipExtension {
			currentEOF, err := r.createGzipReader()
			if err != nil {
				return
			}
			// Offset tracking in an uncompressed file is based on the length of emitted tokens, but in this case
			// we need to set the offset to the end of the file.
			defer func() {
				r.Offset = currentEOF
			}()
		} else {
			r.reader = r.file
		}
	default:
		r.reader = r.file
	}

	if _, err := r.file.Seek(r.Offset, 0); err != nil {
		r.set.Logger.Error("failed to seek", zap.Error(err))
		return
	}

	defer func() {
		if r.needsUpdateFingerprint {
			r.updateFingerprint()
		}
	}()

	if r.headerReader != nil {
		if r.readHeader(ctx) {
			return
		}
	}

	r.readContents(ctx)
}

// createGzipReader creates gzip reader and returns the file offset
func (r *Reader) createGzipReader() (int64, error) {
	// We need to create a gzip reader each time ReadToEnd is called because the underlying
	// SectionReader can only read a fixed window (from previous offset to EOF).
	info, err := r.file.Stat()
	if err != nil {
		r.set.Logger.Error("failed to stat", zap.Error(err))
		return 0, err
	}
	currentEOF := info.Size()
	// use a gzip Reader with an underlying SectionReader to pick up at the last
	// offset of a gzip compressed file
	gzipReader, err := gzip.NewReader(io.NewSectionReader(r.file, r.Offset, currentEOF))
	if err != nil {
		if !errors.Is(err, io.EOF) {
			r.set.Logger.Error("failed to create gzip reader", zap.Error(err))
		}
		return 0, err
	}
	r.reader = gzipReader
	return currentEOF, nil
}

func (r *Reader) readHeader(ctx context.Context) (doneReadingFile bool) {
	bufPtr := r.getBufPtrFromPool()
	defer r.bufPool.Put(bufPtr)
	s := scanner.New(r, r.maxLogSize, *bufPtr, r.Offset, r.headerSplitFunc)

	// Read the tokens from the file until no more header tokens are found or the end of file is reached.
	for {
		select {
		case <-ctx.Done():
			return true
		default:
		}

		ok := s.Scan()
		if !ok {
			if err := s.Error(); err != nil {
				r.set.Logger.Error("failed during header scan", zap.Error(err))
			} else {
				r.set.Logger.Debug("end of file reached", zap.Bool("delete_at_eof", r.deleteAtEOF))
				if r.deleteAtEOF {
					r.delete()
				}
			}
			// Either end of file was reached, or file cannot be scanned.
			return true
		}

		token, err := textutils.DecodeAsString(r.decoder, s.Bytes())
		if err != nil {
			r.set.Logger.Error("failed to decode header token", zap.Error(err))
			r.Offset = s.Pos() // move past the bad token or we may be stuck
			continue
		}

		err = r.headerReader.Process(ctx, token, r.FileAttributes)
		if err != nil {
			if errors.Is(err, header.ErrEndOfHeader) {
				// End of header reached.
				break
			}
			r.set.Logger.Error("failed to process header token", zap.Error(err))
		}

		r.Offset = s.Pos()
	}

	// Clean up the header machinery
	if err := r.headerReader.Stop(); err != nil {
		r.set.Logger.Error("failed to stop header pipeline during finalization", zap.Error(err))
	}
	r.headerReader = nil
	r.HeaderFinalized = true

	// Reset position in file to r.Offest after the header scanner might have moved it past a content token.
	if _, err := r.file.Seek(r.Offset, 0); err != nil {
		r.set.Logger.Error("failed to seek post-header", zap.Error(err))
		return true
	}

	return false
}

func (r *Reader) readContents(ctx context.Context) {
	var buf []byte
	if r.TokenLenState.MinimumLength <= r.initialBufferSize {
		bufPtr := r.getBufPtrFromPool()
		buf = *bufPtr
		defer r.bufPool.Put(bufPtr)
	} else {
		// If we previously saw a potential token larger than the default buffer,
		// size the buffer to be at least one byte larger so we can see if there's more data.
		// Usually, expect this to be a rare event so that we don't bother pooling this special buffer size.
		buf = make([]byte, 0, r.TokenLenState.MinimumLength+1)
	}
	s := scanner.New(r, r.maxLogSize, buf, r.Offset, r.contentSplitFunc)

	tokenBodies := make([][]byte, r.maxBatchSize)
	tokenOffsets := make([]int64, r.maxBatchSize+1)

	numTokensBatched := 0
	tokenOffsets[0] = r.Offset
	// Iterate over the contents of the file.
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		ok := s.Scan()
		if !ok {
			if err := s.Error(); err != nil {
				r.set.Logger.Error("failed during scan", zap.Error(err))
			} else if r.deleteAtEOF {
				r.delete()
			}

			if numTokensBatched > 0 {
				err := r.emitFunc(ctx, tokenBodies[:numTokensBatched], r.FileAttributes, r.RecordNum, tokenOffsets)
				if err != nil {
					r.set.Logger.Error("failed to emit token", zap.Error(err))
				}
				r.Offset = s.Pos()
			}
			return
		}

		var err error
		tokenBodies[numTokensBatched], err = r.decoder.Bytes(s.Bytes())
		tokenOffsets[numTokensBatched+1] = s.Pos()
		if err != nil {
			r.set.Logger.Error("failed to decode token", zap.Error(err))
			r.Offset = s.Pos() // move past the bad token or we may be stuck
			continue
		}
		numTokensBatched++

		r.RecordNum++
		if r.maxBatchSize > 0 && numTokensBatched >= r.maxBatchSize {
			if err = r.emitFunc(ctx, tokenBodies[:numTokensBatched], r.FileAttributes, r.RecordNum, tokenOffsets); err != nil {
				r.set.Logger.Error("failed to emit token", zap.Error(err))
			}
			numTokensBatched = 0
			r.Offset, tokenOffsets[0] = s.Pos(), s.Pos()
		}
	}
}

// Delete will close and delete the file
func (r *Reader) delete() {
	r.close()
	if err := os.Remove(r.fileName); err != nil {
		r.set.Logger.Error("could not delete", zap.String("filename", r.fileName))
	}
}

// Close will close the file and return the metadata
func (r *Reader) Close() *Metadata {
	r.close()
	m := r.Metadata
	r.Metadata = nil
	return m
}

func (r *Reader) close() {
	if r.file != nil {
		if err := r.file.Close(); err != nil {
			r.set.Logger.Debug("Problem closing reader", zap.Error(err))
		}
		r.file = nil
	}

	if r.headerReader != nil {
		if err := r.headerReader.Stop(); err != nil {
			r.set.Logger.Error("Failed to stop header pipeline", zap.Error(err))
		}
	}
}

// Read from the file and update the fingerprint if necessary
func (r *Reader) Read(dst []byte) (n int, err error) {
	n, err = r.reader.Read(dst)
	if n == 0 || err != nil {
		return
	}

	if !r.needsUpdateFingerprint && r.Fingerprint.Len() < r.fingerprintSize {
		r.needsUpdateFingerprint = true
	}
	return
}

func (r *Reader) NameEquals(other *Reader) bool {
	return r.fileName == other.fileName
}

// Validate returns true if the reader still has a valid file handle, false otherwise.
func (r *Reader) Validate() bool {
	if r.file == nil {
		return false
	}
	refreshedFingerprint, err := fingerprint.NewFromFile(r.file, r.fingerprintSize, r.compression != "")
	if err != nil {
		return false
	}
	if refreshedFingerprint.StartsWith(r.Fingerprint) {
		return true
	}
	return false
}

func (r *Reader) GetFileName() string {
	return r.fileName
}

func (m Metadata) GetFingerprint() *fingerprint.Fingerprint {
	return m.Fingerprint
}

func (r *Reader) updateFingerprint() {
	r.needsUpdateFingerprint = false
	if r.file == nil {
		return
	}
	refreshedFingerprint, err := fingerprint.NewFromFile(r.file, r.fingerprintSize, r.compression != "")
	if err != nil {
		return
	}
	if r.Fingerprint.Len() > 0 && !refreshedFingerprint.StartsWith(r.Fingerprint) {
		return // fingerprint tampered, likely due to truncation
	}
	r.Fingerprint = refreshedFingerprint
}

func (r *Reader) getBufPtrFromPool() *[]byte {
	bufP := r.bufPool.Get()
	if bufP == nil {
		buf := make([]byte, 0, r.initialBufferSize)
		return &buf
	}
	return bufP.(*[]byte)
}
