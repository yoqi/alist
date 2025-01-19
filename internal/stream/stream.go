package stream

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"

	"github.com/alist-org/alist/v3/internal/errs"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/pkg/http_range"
	"github.com/alist-org/alist/v3/pkg/utils"
)

type FileStream struct {
	Ctx context.Context
	model.Obj
	io.Reader
	Mimetype          string
	WebPutAsTask      bool
	ForceStreamUpload bool
	Exist             model.Obj //the file existed in the destination, we can reuse some info since we wil overwrite it
	utils.Closers
	tmpFile  *os.File //if present, tmpFile has full content, it will be deleted at last
	peekBuff *bytes.Reader
}

func (f *FileStream) GetSize() int64 {
	if f.tmpFile != nil {
		info, err := f.tmpFile.Stat()
		if err == nil {
			return info.Size()
		}
	}
	return f.Obj.GetSize()
}

func (f *FileStream) GetMimetype() string {
	return f.Mimetype
}

func (f *FileStream) NeedStore() bool {
	return f.WebPutAsTask
}

func (f *FileStream) IsForceStreamUpload() bool {
	return f.ForceStreamUpload
}

func (f *FileStream) Close() error {
	var err1, err2 error

	err1 = f.Closers.Close()
	if errors.Is(err1, os.ErrClosed) {
		err1 = nil
	}
	if f.tmpFile != nil {
		err2 = os.RemoveAll(f.tmpFile.Name())
		if err2 != nil {
			err2 = errs.NewErr(err2, "failed to remove tmpFile [%s]", f.tmpFile.Name())
		} else {
			f.tmpFile = nil
		}
	}

	return errors.Join(err1, err2)
}

func (f *FileStream) GetExist() model.Obj {
	return f.Exist
}
func (f *FileStream) SetExist(obj model.Obj) {
	f.Exist = obj
}

// CacheFullInTempFile save all data into tmpFile. Not recommended since it wears disk,
// and can't start upload until the file is written. It's not thread-safe!
func (f *FileStream) CacheFullInTempFile() (model.File, error) {
	if f.tmpFile != nil {
		return f.tmpFile, nil
	}
	if file, ok := f.Reader.(model.File); ok {
		return file, nil
	}
	tmpF, err := utils.CreateTempFile(f.Reader, f.GetSize())
	if err != nil {
		return nil, err
	}
	f.Add(tmpF)
	f.tmpFile = tmpF
	f.Reader = tmpF
	return f.tmpFile, nil
}

func (f *FileStream) CacheFullInTempFileAndUpdateProgress(up model.UpdateProgress) (model.File, error) {
	if f.tmpFile != nil {
		return f.tmpFile, nil
	}
	if file, ok := f.Reader.(model.File); ok {
		return file, nil
	}
	tmpF, err := utils.CreateTempFile(&ReaderUpdatingProgress{
		Reader:         f,
		UpdateProgress: up,
	}, f.GetSize())
	if err != nil {
		return nil, err
	}
	f.Add(tmpF)
	f.tmpFile = tmpF
	f.Reader = tmpF
	return f.tmpFile, nil
}

const InMemoryBufMaxSize = 10 // Megabytes
const InMemoryBufMaxSizeBytes = InMemoryBufMaxSize * 1024 * 1024

// RangeRead have to cache all data first since only Reader is provided.
// also support a peeking RangeRead at very start, but won't buffer more than 10MB data in memory
func (f *FileStream) RangeRead(httpRange http_range.Range) (io.Reader, error) {
	if httpRange.Length == -1 {
		httpRange.Length = f.GetSize()
	}
	if f.peekBuff != nil && httpRange.Start < int64(f.peekBuff.Len()) && httpRange.Start+httpRange.Length-1 < int64(f.peekBuff.Len()) {
		return io.NewSectionReader(f.peekBuff, httpRange.Start, httpRange.Length), nil
	}
	if f.tmpFile == nil {
		if httpRange.Start == 0 && httpRange.Length <= InMemoryBufMaxSizeBytes && f.peekBuff == nil {
			bufSize := utils.Min(httpRange.Length, f.GetSize())
			newBuf := bytes.NewBuffer(make([]byte, 0, bufSize))
			n, err := utils.CopyWithBufferN(newBuf, f.Reader, bufSize)
			if err != nil {
				return nil, err
			}
			if n != bufSize {
				return nil, fmt.Errorf("stream RangeRead did not get all data in peek, expect =%d ,actual =%d", bufSize, n)
			}
			f.peekBuff = bytes.NewReader(newBuf.Bytes())
			f.Reader = io.MultiReader(f.peekBuff, f.Reader)
			return io.NewSectionReader(f.peekBuff, httpRange.Start, httpRange.Length), nil
		} else {
			_, err := f.CacheFullInTempFile()
			if err != nil {
				return nil, err
			}
		}
	}
	return io.NewSectionReader(f.tmpFile, httpRange.Start, httpRange.Length), nil
}

var _ model.FileStreamer = (*SeekableStream)(nil)
var _ model.FileStreamer = (*FileStream)(nil)

//var _ seekableStream = (*FileStream)(nil)

// for most internal stream, which is either RangeReadCloser or MFile
type SeekableStream struct {
	FileStream
	Link *model.Link
	// should have one of belows to support rangeRead
	rangeReadCloser model.RangeReadCloserIF
	mFile           model.File
}

func NewSeekableStream(fs FileStream, link *model.Link) (*SeekableStream, error) {
	if len(fs.Mimetype) == 0 {
		fs.Mimetype = utils.GetMimeType(fs.Obj.GetName())
	}
	ss := SeekableStream{FileStream: fs, Link: link}
	if ss.Reader != nil {
		result, ok := ss.Reader.(model.File)
		if ok {
			ss.mFile = result
			ss.Closers.Add(result)
			return &ss, nil
		}
	}
	if ss.Link != nil {
		if ss.Link.MFile != nil {
			ss.mFile = ss.Link.MFile
			ss.Reader = ss.Link.MFile
			ss.Closers.Add(ss.Link.MFile)
			return &ss, nil
		}

		if ss.Link.RangeReadCloser != nil {
			ss.rangeReadCloser = ss.Link.RangeReadCloser
			return &ss, nil
		}
		if len(ss.Link.URL) > 0 {
			rrc, err := GetRangeReadCloserFromLink(ss.GetSize(), link)
			if err != nil {
				return nil, err
			}
			ss.rangeReadCloser = rrc
			return &ss, nil
		}
	}

	return nil, fmt.Errorf("illegal seekableStream")
}

//func (ss *SeekableStream) Peek(length int) {
//
//}

// RangeRead is not thread-safe, pls use it in single thread only.
func (ss *SeekableStream) RangeRead(httpRange http_range.Range) (io.Reader, error) {
	if httpRange.Length == -1 {
		httpRange.Length = ss.GetSize()
	}
	if ss.mFile != nil {
		return io.NewSectionReader(ss.mFile, httpRange.Start, httpRange.Length), nil
	}
	if ss.tmpFile != nil {
		return io.NewSectionReader(ss.tmpFile, httpRange.Start, httpRange.Length), nil
	}
	if ss.rangeReadCloser != nil {
		rc, err := ss.rangeReadCloser.RangeRead(ss.Ctx, httpRange)
		if err != nil {
			return nil, err
		}
		return rc, nil
	}
	return nil, fmt.Errorf("can't find mFile or rangeReadCloser")
}

//func (f *FileStream) GetReader() io.Reader {
//	return f.Reader
//}

// only provide Reader as full stream when it's demanded. in rapid-upload, we can skip this to save memory
func (ss *SeekableStream) Read(p []byte) (n int, err error) {
	//f.mu.Lock()

	//f.peekedOnce = true
	//defer f.mu.Unlock()
	if ss.Reader == nil {
		if ss.rangeReadCloser == nil {
			return 0, fmt.Errorf("illegal seekableStream")
		}
		rc, err := ss.rangeReadCloser.RangeRead(ss.Ctx, http_range.Range{Length: -1})
		if err != nil {
			return 0, nil
		}
		ss.Reader = io.NopCloser(rc)
		ss.Closers.Add(rc)

	}
	return ss.Reader.Read(p)
}

func (ss *SeekableStream) CacheFullInTempFile() (model.File, error) {
	if ss.tmpFile != nil {
		return ss.tmpFile, nil
	}
	if ss.mFile != nil {
		return ss.mFile, nil
	}
	tmpF, err := utils.CreateTempFile(ss, ss.GetSize())
	if err != nil {
		return nil, err
	}
	ss.Add(tmpF)
	ss.tmpFile = tmpF
	ss.Reader = tmpF
	return ss.tmpFile, nil
}

func (ss *SeekableStream) CacheFullInTempFileAndUpdateProgress(up model.UpdateProgress) (model.File, error) {
	if ss.tmpFile != nil {
		return ss.tmpFile, nil
	}
	if ss.mFile != nil {
		return ss.mFile, nil
	}
	tmpF, err := utils.CreateTempFile(&ReaderUpdatingProgress{
		Reader:         ss,
		UpdateProgress: up,
	}, ss.GetSize())
	if err != nil {
		return nil, err
	}
	ss.Add(tmpF)
	ss.tmpFile = tmpF
	ss.Reader = tmpF
	return ss.tmpFile, nil
}

func (f *FileStream) SetTmpFile(r *os.File) {
	f.Reader = r
	f.tmpFile = r
}

type ReaderWithSize interface {
	io.Reader
	GetSize() int64
}

type SimpleReaderWithSize struct {
	io.Reader
	Size int64
}

func (r *SimpleReaderWithSize) GetSize() int64 {
	return r.Size
}

type ReaderUpdatingProgress struct {
	Reader ReaderWithSize
	model.UpdateProgress
	offset int
}

func (r *ReaderUpdatingProgress) Read(p []byte) (n int, err error) {
	n, err = r.Reader.Read(p)
	r.offset += n
	r.UpdateProgress(math.Min(100.0, float64(r.offset)/float64(r.Reader.GetSize())*100.0))
	return n, err
}

type SStreamReadAtSeeker interface {
	model.File
	GetRawStream() *SeekableStream
}

type readerCur struct {
	reader io.Reader
	cur    int64
}

type RangeReadReadAtSeeker struct {
	ss        *SeekableStream
	masterOff int64
	readers   []*readerCur
}

type FileReadAtSeeker struct {
	ss *SeekableStream
}

func NewReadAtSeeker(ss *SeekableStream, offset int64, forceRange ...bool) (SStreamReadAtSeeker, error) {
	if ss.mFile != nil {
		_, err := ss.mFile.Seek(offset, io.SeekStart)
		if err != nil {
			return nil, err
		}
		return &FileReadAtSeeker{ss: ss}, nil
	}
	var r io.Reader
	var err error
	if offset != 0 || utils.IsBool(forceRange...) {
		if offset < 0 || offset > ss.GetSize() {
			return nil, errors.New("offset out of range")
		}
		r, err = ss.RangeRead(http_range.Range{Start: offset, Length: -1})
		if err != nil {
			return nil, err
		}
		if rc, ok := r.(io.Closer); ok {
			ss.Closers.Add(rc)
		}
	} else {
		r = ss
	}
	return &RangeReadReadAtSeeker{
		ss:        ss,
		masterOff: offset,
		readers:   []*readerCur{{reader: r, cur: offset}},
	}, nil
}

func (r *RangeReadReadAtSeeker) GetRawStream() *SeekableStream {
	return r.ss
}

func (r *RangeReadReadAtSeeker) getReaderAtOffset(off int64) (*readerCur, error) {
	for _, reader := range r.readers {
		if reader.cur == off {
			return reader, nil
		}
	}
	reader, err := r.ss.RangeRead(http_range.Range{Start: off, Length: -1})
	if err != nil {
		return nil, err
	}
	if c, ok := reader.(io.Closer); ok {
		r.ss.Closers.Add(c)
	}
	rc := &readerCur{reader: reader, cur: off}
	r.readers = append(r.readers, rc)
	return rc, nil
}

func (r *RangeReadReadAtSeeker) ReadAt(p []byte, off int64) (int, error) {
	rc, err := r.getReaderAtOffset(off)
	if err != nil {
		return 0, err
	}
	num := 0
	for num < len(p) {
		n, err := rc.reader.Read(p[num:])
		rc.cur += int64(n)
		num += n
		if err != nil {
			return num, err
		}
	}
	return num, nil
}

func (r *RangeReadReadAtSeeker) Seek(offset int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
	case io.SeekCurrent:
		if offset == 0 {
			return r.masterOff, nil
		}
		offset += r.masterOff
	case io.SeekEnd:
		offset += r.ss.GetSize()
	default:
		return 0, errs.NotSupport
	}
	if offset < 0 {
		return r.masterOff, errors.New("invalid seek: negative position")
	}
	if offset > r.ss.GetSize() {
		return r.masterOff, io.EOF
	}
	r.masterOff = offset
	return offset, nil
}

func (r *RangeReadReadAtSeeker) Read(p []byte) (n int, err error) {
	rc, err := r.getReaderAtOffset(r.masterOff)
	if err != nil {
		return 0, err
	}
	n, err = rc.reader.Read(p)
	rc.cur += int64(n)
	r.masterOff += int64(n)
	return n, err
}

func (r *RangeReadReadAtSeeker) Close() error {
	return r.ss.Close()
}

func (f *FileReadAtSeeker) GetRawStream() *SeekableStream {
	return f.ss
}

func (f *FileReadAtSeeker) Read(p []byte) (n int, err error) {
	return f.ss.mFile.Read(p)
}

func (f *FileReadAtSeeker) ReadAt(p []byte, off int64) (n int, err error) {
	return f.ss.mFile.ReadAt(p, off)
}

func (f *FileReadAtSeeker) Seek(offset int64, whence int) (int64, error) {
	return f.ss.mFile.Seek(offset, whence)
}

func (f *FileReadAtSeeker) Close() error {
	return f.ss.Close()
}
