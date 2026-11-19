package encryption

import (
	bytes "bytes"
	binary "encoding/binary"
	errors "errors"
	io "io"

	chacha "golang.org/x/crypto/chacha20poly1305"
)

// Segmented files: how a WebDAV file is encrypted at rest when
// webdav.encrypt_at_rest is on.
//
// A file is a fixed header and then segments of SegmentSize plaintext bytes,
// each sealed on its own with XChaCha20-Poly1305 under the file's own random
// key. Each segment's nonce is the file's random prefix, the segment's number
// and a flag saying whether it is the last one -- the STREAM construction. So:
//
//   - nothing has to know the size up front: a PUT streams straight through;
//   - a segment moved, swapped with another file's, or a file cut short at a
//     segment boundary fails to open (the last segment has to carry the flag);
//   - any byte can be read without reading what comes before it, which is what
//     a Range request and archive/zip's central directory need.
//
// The file's key is sealed in the header by the WebDAV master key, which lives
// in the database (models.DAVMasterKey). Rotating secret.key re-seals that one
// key, not any file.
//
// Layout:
//
//	magic       8   "ZWDAVSG1"
//	prefix     16   random, the start of every segment nonce
//	file key   72   24-byte nonce, then the 32-byte key sealed, bound to magic+prefix
//	segments        SegmentSize+16 each; the last one 16..SegmentSize+16

const (
	SegmentSize       = 64 * 1024
	SegmentHeaderSize = 8 + 16 + 72
	segmentOverhead   = 16
	segmentSealed     = SegmentSize + segmentOverhead
)

var segmentMagic = []byte( "ZWDAVSG1" )

var (
	ErrNotSegmented   = errors.New( "encryption: not a segmented file" )
	ErrSegmentCorrupt = errors.New( "encryption: segmented file is damaged or was cut short" )
)

// IsSegmented says whether a file's first bytes are a segmented file's
// header. It only looks at the magic: OpenSegments is what proves it.
func IsSegmented( head []byte ) ( result bool ) {
	result = len( head ) >= len( segmentMagic ) && bytes.Equal( head[ :len( segmentMagic ) ] , segmentMagic )
	return
}

// SegmentedPlainSize is the plaintext size of a segmented file of disk_size
// bytes, worked out from the size alone. ok is false for a size no segmented
// file can have.
func SegmentedPlainSize( disk_size int64 ) ( plain int64 , ok bool ) {
	body := disk_size - SegmentHeaderSize
	if body < segmentOverhead { return }
	segments := ( body + segmentSealed - 1 ) / segmentSealed
	last := body - ( segments - 1 ) * segmentSealed
	if last < segmentOverhead { return }
	plain = ( segments - 1 ) * SegmentSize + last - segmentOverhead
	ok = true
	return
}

func segmentNonce( prefix []byte , index uint64 , final bool ) ( nonce []byte ) {
	nonce = make( []byte , chacha.NonceSizeX )
	copy( nonce , prefix )
	var counter [8]byte
	binary.BigEndian.PutUint64( counter[ : ] , index )
	// Seven bytes of counter: 2^56 segments of 64 KiB is far past any file.
	copy( nonce[ 16:23 ] , counter[ 1: ] )
	if final { nonce[ 23 ] = 1 }
	return
}

func headerBound( prefix []byte ) ( bound []byte ) {
	bound = append( append( []byte{} , segmentMagic... ) , prefix... )
	return
}

// SegmentWriter seals what is written to it. Close writes the last segment;
// a writer that is never closed leaves a file that will not open, which is
// the point.
type SegmentWriter struct {
	out     io.Writer
	aead    interface{ Seal( dst , nonce , plaintext , additional []byte ) []byte }
	prefix  []byte
	buffer  []byte
	index   uint64
	sealed  []byte
	closed  bool
}

// NewSegmentWriter writes the header to out and returns the writer for the
// rest. master_hex is the WebDAV master key.
func NewSegmentWriter( out io.Writer , master_hex string ) ( writer *SegmentWriter , err error ) {
	file_key := GenerateRandomBytes( 32 )
	prefix := GenerateRandomBytes( 16 )
	wrapped , err := ChaChaEncryptBound( master_hex , file_key , headerBound( prefix ) )
	if err != nil { return }
	aead , err := chacha.NewX( file_key )
	if err != nil { return }
	header := make( []byte , 0 , SegmentHeaderSize )
	header = append( header , segmentMagic... )
	header = append( header , prefix... )
	header = append( header , wrapped... )
	if _ , err = out.Write( header ); err != nil { return }
	writer = &SegmentWriter{ out: out , aead: aead , prefix: prefix , buffer: make( []byte , 0 , SegmentSize ) , sealed: make( []byte , 0 , segmentSealed ) }
	return
}

func ( writer *SegmentWriter ) flush( final bool ) ( err error ) {
	writer.sealed = writer.aead.Seal( writer.sealed[ :0 ] , segmentNonce( writer.prefix , writer.index , final ) , writer.buffer , nil )
	_ , err = writer.out.Write( writer.sealed )
	writer.index += 1
	writer.buffer = writer.buffer[ :0 ]
	return
}

func ( writer *SegmentWriter ) Write( data []byte ) ( written int , err error ) {
	if writer.closed {
		err = errors.New( "encryption: write after close" )
		return
	}
	for len( data ) > 0 {
		// A full buffer is flushed only once more data arrives, so the
		// segment Close seals as the last one is never empty unless the
		// whole file is.
		if len( writer.buffer ) == SegmentSize {
			if err = writer.flush( false ); err != nil { return }
		}
		taken := min( SegmentSize - len( writer.buffer ) , len( data ) )
		writer.buffer = append( writer.buffer , data[ :taken ]... )
		data = data[ taken: ]
		written += taken
	}
	return
}

// Close seals the last segment. It does not close out.
func ( writer *SegmentWriter ) Close() ( err error ) {
	if writer.closed { return }
	writer.closed = true
	err = writer.flush( true )
	return
}

// SegmentReader reads a segmented file at any offset. It keeps one segment
// decrypted, so one reader belongs to one goroutine.
type SegmentReader struct {
	in       io.ReaderAt
	aead     interface{ Open( dst , nonce , ciphertext , additional []byte ) ( []byte , error ) }
	prefix   []byte
	size     int64
	segments int64
	// One segment is kept, since reads come in order far more often than not.
	cached   int64
	plain    []byte
	sealed   []byte
}

// OpenSegments checks the header of a segmented file of disk_size bytes and
// opens its key with the WebDAV master key.
func OpenSegments( in io.ReaderAt , disk_size int64 , master_hex string ) ( reader *SegmentReader , err error ) {
	size , ok := SegmentedPlainSize( disk_size )
	if ok == false {
		err = ErrNotSegmented
		return
	}
	header := make( []byte , SegmentHeaderSize )
	if _ , err = in.ReadAt( header , 0 ); err != nil { return }
	if IsSegmented( header ) == false {
		err = ErrNotSegmented
		return
	}
	prefix := header[ 8:24 ]
	file_key , open_err := ChaChaDecryptBound( master_hex , header[ 24: ] , headerBound( prefix ) )
	if open_err != nil {
		err = ErrNotSegmented
		return
	}
	aead , err := chacha.NewX( file_key )
	if err != nil { return }
	body := disk_size - SegmentHeaderSize
	reader = &SegmentReader{
		in:       in,
		aead:     aead,
		prefix:   append( []byte{} , prefix... ),
		size:     size,
		segments: ( body + segmentSealed - 1 ) / segmentSealed,
		cached:   -1,
		sealed:   make( []byte , segmentSealed ),
	}
	return
}

// Size is the plaintext size.
func ( reader *SegmentReader ) Size() ( result int64 ) {
	result = reader.size
	return
}

func ( reader *SegmentReader ) load( index int64 ) ( err error ) {
	if index == reader.cached { return }
	offset := SegmentHeaderSize + index * segmentSealed
	length := int64( segmentSealed )
	if index == reader.segments - 1 {
		length = ( reader.size - index * SegmentSize ) + segmentOverhead
	}
	sealed := reader.sealed[ :length ]
	if _ , err = reader.in.ReadAt( sealed , offset ); err != nil && errors.Is( err , io.EOF ) == false { return }
	err = nil
	final := index == reader.segments - 1
	plain , open_err := reader.aead.Open( reader.plain[ :0 ] , segmentNonce( reader.prefix , uint64( index ) , final ) , sealed , nil )
	if open_err != nil {
		reader.cached = -1
		err = ErrSegmentCorrupt
		return
	}
	reader.plain = plain
	reader.cached = index
	return
}

// ReadAt reads plaintext at offset, as io.ReaderAt says.
func ( reader *SegmentReader ) ReadAt( data []byte , offset int64 ) ( read int , err error ) {
	if offset < 0 {
		err = errors.New( "encryption: negative offset" )
		return
	}
	for len( data ) > 0 {
		if offset >= reader.size {
			err = io.EOF
			return
		}
		index := offset / SegmentSize
		if err = reader.load( index ); err != nil { return }
		within := offset - index * SegmentSize
		copied := copy( data , reader.plain[ within: ] )
		data = data[ copied: ]
		read += copied
		offset += int64( copied )
	}
	return
}

// RewriteSegments re-encrypts a segmented file from one master key to
// another, or decrypts or encrypts it: from_hex "" reads in as plaintext,
// to_hex "" writes plaintext. The caller handles the file around it.
func RewriteSegments( in io.ReaderAt , disk_size int64 , from_hex string , out io.Writer , to_hex string ) ( err error ) {
	var source io.Reader
	if from_hex == "" {
		source = io.NewSectionReader( in , 0 , disk_size )
	} else {
		reader , open_err := OpenSegments( in , disk_size , from_hex )
		if open_err != nil {
			err = open_err
			return
		}
		source = io.NewSectionReader( reader , 0 , reader.Size() )
	}
	if to_hex == "" {
		_ , err = io.Copy( out , source )
		return
	}
	writer , err := NewSegmentWriter( out , to_hex )
	if err != nil { return }
	if _ , err = io.Copy( writer , source ); err != nil { return }
	err = writer.Close()
	return
}
